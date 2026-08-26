package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// PermitRepo is the SQLite backed permit window store. Seat accounting is done
// with conditional updates so the database itself enforces the quota invariant.
type PermitRepo struct {
	base
}

// NewPermitRepo builds a permit window repository.
func NewPermitRepo(tx *sqlite.TxManager) *PermitRepo {
	return &PermitRepo{base{tx: tx}}
}

const permitColumns = `id, trail_id, hike_day, quota_total, quota_reserved, version, closed_at, created_at, updated_at`

// EnsureWindow returns the permit window of a trail day, creating it with the
// given quota when the day was not opened yet. Concurrent creation is resolved
// by the unique index and results in a second read instead of an error.
func (r *PermitRepo) EnsureWindow(ctx context.Context, trailID int64, hikeDay string, quotaTotal int, now time.Time) (*domain.PermitWindow, error) {
	existing, err := r.WindowByTrailDay(ctx, trailID, hikeDay)
	if err == nil {
		return existing, nil
	}
	if !apperr.Is(err, apperr.CodeNotFound) {
		return nil, err
	}
	if quotaTotal <= 0 {
		return nil, apperr.New(apperr.CodeInvalidArgument, "许可窗口的配额必须大于 0")
	}
	_, execErr := r.q(ctx).ExecContext(ctx,
		`INSERT INTO permit_windows (trail_id, hike_day, quota_total, quota_reserved, version, closed_at, created_at, updated_at)
		 VALUES (?, ?, ?, 0, 1, NULL, ?, ?)`,
		trailID, hikeDay, quotaTotal, unixOrZero(now), unixOrZero(now))
	if execErr != nil {
		if translated := translate("创建许可窗口", execErr); !apperr.Is(translated, apperr.CodeConflict) {
			return nil, translated
		}
	}
	return r.WindowByTrailDay(ctx, trailID, hikeDay)
}

// WindowByID loads a permit window by identifier.
func (r *PermitRepo) WindowByID(ctx context.Context, id int64) (*domain.PermitWindow, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+permitColumns+` FROM permit_windows WHERE id = ?`, id)
	return scanPermitWindowRow(row)
}

// WindowByTrailDay loads the permit window of one trail day.
func (r *PermitRepo) WindowByTrailDay(ctx context.Context, trailID int64, hikeDay string) (*domain.PermitWindow, error) {
	row := r.q(ctx).QueryRowContext(ctx,
		`SELECT `+permitColumns+` FROM permit_windows WHERE trail_id = ? AND hike_day = ?`, trailID, hikeDay)
	return scanPermitWindowRow(row)
}

// ListWindows lists the permit windows of a trail inside a day range.
func (r *PermitRepo) ListWindows(ctx context.Context, trailID int64, fromDay, toDay string) ([]domain.PermitWindow, error) {
	query := `SELECT ` + permitColumns + ` FROM permit_windows WHERE trail_id = ?`
	args := []any{trailID}
	if fromDay != "" {
		query += ` AND hike_day >= ?`
		args = append(args, fromDay)
	}
	if toDay != "" {
		query += ` AND hike_day <= ?`
		args = append(args, toDay)
	}
	query += ` ORDER BY hike_day ASC`

	rows, err := r.q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, translate("查询许可窗口", err)
	}
	defer func() { _ = rows.Close() }()

	windows := make([]domain.PermitWindow, 0, 16)
	for rows.Next() {
		var window domain.PermitWindow
		var closedAt sql.NullInt64
		var createdAt, updatedAt int64
		if err := rows.Scan(&window.ID, &window.TrailID, &window.HikeDay, &window.QuotaTotal,
			&window.QuotaReserved, &window.Version, &closedAt, &createdAt, &updatedAt); err != nil {
			return nil, translate("解析许可窗口", err)
		}
		window.ClosedAt = fromNullUnix(closedAt)
		window.CreatedAt = fromUnix(createdAt)
		window.UpdatedAt = fromUnix(updatedAt)
		windows = append(windows, window)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历许可窗口", err)
	}
	return windows, nil
}

// ReserveSeats occupies seats atomically. The WHERE clause carries both the
// optimistic version guard and the quota invariant, so a lost race is reported
// as a version conflict and an over-booking attempt as an exhausted quota.
func (r *PermitRepo) ReserveSeats(ctx context.Context, windowID int64, expectedVersion int64, seats int, now time.Time) error {
	if seats <= 0 {
		return apperr.New(apperr.CodeInvalidArgument, "占用座位数必须大于 0")
	}
	affected, err := affectedRows("占用许可名额", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE permit_windows
			 SET quota_reserved = quota_reserved + ?, version = version + 1, updated_at = ?
			 WHERE id = ?
			   AND version = ?
			   AND closed_at IS NULL
			   AND quota_reserved + ? <= quota_total`,
			seats, unixOrZero(now), windowID, expectedVersion, seats)
	})
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	current, loadErr := r.WindowByID(ctx, windowID)
	if loadErr != nil {
		return loadErr
	}
	if current.Version != expectedVersion {
		return apperr.Newf(apperr.CodeVersionConflict,
			"%s 的许可窗口已被其他队伍更新，请重新提交", current.HikeDay)
	}
	if err := current.CanReserve(seats); err != nil {
		return err
	}
	return apperr.New(apperr.CodeConflict, "占用许可名额失败，请重试")
}

// ReleaseSeats returns seats to a window when a party leaves the permit holding
// states. The guard keeps the reserved counter from going negative.
func (r *PermitRepo) ReleaseSeats(ctx context.Context, windowID int64, seats int, now time.Time) error {
	if seats <= 0 {
		return apperr.New(apperr.CodeInvalidArgument, "释放座位数必须大于 0")
	}
	// 归还是幂等操作：即使同一笔占用被重复归还，也把结果收敛到不小于 0，
	// 避免并发归还时抛出难以处理的冲突错误。
	affected, err := affectedRows("释放许可名额", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE permit_windows
			 SET quota_reserved = MAX(quota_reserved - ?, 0), version = version + 1, updated_at = ?
			 WHERE id = ?`,
			seats, unixOrZero(now), windowID)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeNotFound, "许可窗口不存在，无法归还名额")
	}
	return nil
}

// Close stops a permit window from accepting further reservations.
func (r *PermitRepo) Close(ctx context.Context, windowID int64, at time.Time) error {
	affected, err := affectedRows("关闭许可窗口", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE permit_windows SET closed_at = ?, version = version + 1, updated_at = ?
			 WHERE id = ? AND closed_at IS NULL`,
			unixOrZero(at), unixOrZero(at), windowID)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeStateInvalid, "许可窗口不存在或已关闭")
	}
	return nil
}

func scanPermitWindowRow(row *sql.Row) (*domain.PermitWindow, error) {
	var window domain.PermitWindow
	var closedAt sql.NullInt64
	var createdAt, updatedAt int64
	err := row.Scan(&window.ID, &window.TrailID, &window.HikeDay, &window.QuotaTotal,
		&window.QuotaReserved, &window.Version, &closedAt, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("许可窗口")
	case err != nil:
		return nil, translate("读取许可窗口", err)
	}
	window.ClosedAt = fromNullUnix(closedAt)
	window.CreatedAt = fromUnix(createdAt)
	window.UpdatedAt = fromUnix(updatedAt)
	return &window, nil
}

var _ repository.PermitRepository = (*PermitRepo)(nil)
