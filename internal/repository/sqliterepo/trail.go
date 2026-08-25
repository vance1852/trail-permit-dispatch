package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// TrailRepo is the SQLite backed trail and checkpoint store. Checkpoint
// definitions change very rarely, so they are cached per trail to keep the
// on-trail supervision hot path from re-reading them on every sweep.
type TrailRepo struct {
	base
	checkpointMu    sync.Mutex
	checkpointCache map[int64][]domain.Checkpoint
}

// NewTrailRepo builds a trail repository.
func NewTrailRepo(tx *sqlite.TxManager) *TrailRepo {
	return &TrailRepo{
		base:            base{tx: tx},
		checkpointCache: make(map[int64][]domain.Checkpoint, 8),
	}
}

const trailColumns = `id, code, name, region, difficulty, distance_km, daily_quota, min_party_size,
	max_party_size, permit_cutoff_hours, status, version, created_at, updated_at`

// trailSortKeys whitelists the sortable columns of the trail list endpoint.
var trailSortKeys = []string{"code", "difficulty", "daily_quota", "distance_km"}

// TrailSortKeys exposes the whitelist to the HTTP layer.
func TrailSortKeys() []string { return append([]string(nil), trailSortKeys...) }

// Create inserts a governed trail.
func (r *TrailRepo) Create(ctx context.Context, trail *domain.Trail) (int64, error) {
	if trail == nil {
		return 0, apperr.New(apperr.CodeInternal, "线路数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO trails (code, name, region, difficulty, distance_km, daily_quota, min_party_size,
			max_party_size, permit_cutoff_hours, status, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		trail.Code, trail.Name, trail.Region, trail.Difficulty, trail.DistanceKM, trail.DailyQuota,
		trail.MinPartySize, trail.MaxPartySize, trail.PermitCutoffHours, string(trail.Status),
		unixOrZero(trail.CreatedAt), unixOrZero(trail.UpdatedAt),
	)
	id, err := insertedID("创建线路", result, err)
	if err != nil {
		return 0, err
	}
	trail.ID = id
	trail.Version = 1
	return id, nil
}

// ByID loads a trail by identifier.
func (r *TrailRepo) ByID(ctx context.Context, id int64) (*domain.Trail, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+trailColumns+` FROM trails WHERE id = ?`, id)
	return scanTrailRow(row)
}

// ByCode loads a trail by its public code.
func (r *TrailRepo) ByCode(ctx context.Context, code string) (*domain.Trail, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+trailColumns+` FROM trails WHERE code = ?`, code)
	return scanTrailRow(row)
}

// List returns a filtered, sorted page of trails.
func (r *TrailRepo) List(ctx context.Context, region string, status domain.TrailStatus, page domain.PageRequest) (domain.Page[domain.Trail], error) {
	where := " WHERE 1 = 1"
	var args []any
	if region != "" {
		where += " AND region = ?"
		args = append(args, region)
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, string(status))
	}
	total, err := countQuery(ctx, r.q(ctx), "统计线路", `SELECT COUNT(1) FROM trails`+where, args...)
	if err != nil {
		return domain.Page[domain.Trail]{}, err
	}
	query := fmt.Sprintf(`SELECT %s FROM trails%s ORDER BY %s %s, id ASC LIMIT ? OFFSET ?`,
		trailColumns, where, page.SortBy, page.Direction())
	rows, err := r.q(ctx).QueryContext(ctx, query, append(args, page.Limit(), page.Offset())...)
	if err != nil {
		return domain.Page[domain.Trail]{}, translate("查询线路", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.Trail, 0, page.Size)
	for rows.Next() {
		trail, err := scanTrail(rows)
		if err != nil {
			return domain.Page[domain.Trail]{}, err
		}
		items = append(items, *trail)
	}
	if err := rows.Err(); err != nil {
		return domain.Page[domain.Trail]{}, translate("遍历线路", err)
	}
	return domain.NewPage(items, total, page), nil
}

// UpdateStatus changes the trail status under an optimistic version guard.
func (r *TrailRepo) UpdateStatus(ctx context.Context, trailID int64, expectedVersion int64, status domain.TrailStatus, now time.Time) error {
	affected, err := affectedRows("更新线路状态", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE trails SET status = ?, version = version + 1, updated_at = ?
			 WHERE id = ? AND version = ?`,
			string(status), unixOrZero(now), trailID, expectedVersion)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeVersionConflict, "线路已被其他操作更新，请重新读取后重试")
	}
	return nil
}

// AddCheckpoint appends a reporting node to a trail.
func (r *TrailRepo) AddCheckpoint(ctx context.Context, checkpoint *domain.Checkpoint) (int64, error) {
	if checkpoint == nil {
		return 0, apperr.New(apperr.CodeInternal, "打点数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO checkpoints (trail_id, seq, name, cutoff_minutes, mandatory, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		checkpoint.TrailID, checkpoint.Seq, checkpoint.Name, checkpoint.CutoffMinutes,
		boolToInt(checkpoint.Mandatory), unixOrZero(checkpoint.CreatedAt),
	)
	id, err := insertedID("创建打点", result, err)
	if err != nil {
		return 0, err
	}
	checkpoint.ID = id
	r.checkpointMu.Lock()
	delete(r.checkpointCache, checkpoint.TrailID)
	r.checkpointMu.Unlock()
	return id, nil
}

const checkpointColumns = `id, trail_id, seq, name, cutoff_minutes, mandatory, created_at`

// Checkpoints lists the reporting nodes of a trail ordered by sequence.
func (r *TrailRepo) Checkpoints(ctx context.Context, trailID int64) ([]domain.Checkpoint, error) {
	r.checkpointMu.Lock()
	cached, ok := r.checkpointCache[trailID]
	r.checkpointMu.Unlock()
	if ok {
		return cached, nil
	}
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT `+checkpointColumns+` FROM checkpoints WHERE trail_id = ? ORDER BY seq ASC`, trailID)
	if err != nil {
		return nil, translate("查询打点", err)
	}
	defer func() { _ = rows.Close() }()

	checkpoints := make([]domain.Checkpoint, 0, 8)
	for rows.Next() {
		var checkpoint domain.Checkpoint
		var mandatory int
		var createdAt int64
		if err := rows.Scan(&checkpoint.ID, &checkpoint.TrailID, &checkpoint.Seq, &checkpoint.Name,
			&checkpoint.CutoffMinutes, &mandatory, &createdAt); err != nil {
			return nil, translate("解析打点", err)
		}
		checkpoint.Mandatory = mandatory == 1
		checkpoint.CreatedAt = fromUnix(createdAt)
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历打点", err)
	}
	r.checkpointMu.Lock()
	r.checkpointCache[trailID] = checkpoints
	r.checkpointMu.Unlock()
	return checkpoints, nil
}

// CheckpointBySeq loads one reporting node of a trail.
func (r *TrailRepo) CheckpointBySeq(ctx context.Context, trailID int64, seq int) (*domain.Checkpoint, error) {
	var checkpoint domain.Checkpoint
	var mandatory int
	var createdAt int64
	err := r.q(ctx).QueryRowContext(ctx,
		`SELECT `+checkpointColumns+` FROM checkpoints WHERE trail_id = ? AND seq = ?`, trailID, seq).
		Scan(&checkpoint.ID, &checkpoint.TrailID, &checkpoint.Seq, &checkpoint.Name,
			&checkpoint.CutoffMinutes, &mandatory, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("打点节点")
	case err != nil:
		return nil, translate("读取打点节点", err)
	}
	checkpoint.Mandatory = mandatory == 1
	checkpoint.CreatedAt = fromUnix(createdAt)
	return &checkpoint, nil
}

func scanTrailRow(row *sql.Row) (*domain.Trail, error) {
	var trail domain.Trail
	var status string
	var createdAt, updatedAt int64
	err := row.Scan(&trail.ID, &trail.Code, &trail.Name, &trail.Region, &trail.Difficulty,
		&trail.DistanceKM, &trail.DailyQuota, &trail.MinPartySize, &trail.MaxPartySize,
		&trail.PermitCutoffHours, &status, &trail.Version, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("线路")
	case err != nil:
		return nil, translate("读取线路", err)
	}
	trail.Status = domain.TrailStatus(status)
	trail.CreatedAt = fromUnix(createdAt)
	trail.UpdatedAt = fromUnix(updatedAt)
	return &trail, nil
}

func scanTrail(rows *sql.Rows) (*domain.Trail, error) {
	var trail domain.Trail
	var status string
	var createdAt, updatedAt int64
	if err := rows.Scan(&trail.ID, &trail.Code, &trail.Name, &trail.Region, &trail.Difficulty,
		&trail.DistanceKM, &trail.DailyQuota, &trail.MinPartySize, &trail.MaxPartySize,
		&trail.PermitCutoffHours, &status, &trail.Version, &createdAt, &updatedAt); err != nil {
		return nil, translate("解析线路", err)
	}
	trail.Status = domain.TrailStatus(status)
	trail.CreatedAt = fromUnix(createdAt)
	trail.UpdatedAt = fromUnix(updatedAt)
	return &trail, nil
}

var _ repository.TrailRepository = (*TrailRepo)(nil)
