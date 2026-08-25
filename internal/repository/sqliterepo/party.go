package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// PartyRepo is the SQLite backed party and member store.
type PartyRepo struct {
	base
}

// NewPartyRepo builds a party repository.
func NewPartyRepo(tx *sqlite.TxManager) *PartyRepo {
	return &PartyRepo{base{tx: tx}}
}

const partyColumns = `id, code, trail_id, leader_id, permit_window_id, hike_day, planned_start, planned_end,
	size, state, version, contact_phone, notes, confirmed_at, dispatched_at, closed_at, created_at, updated_at`

// partySortKeys whitelists the sortable columns of the party list endpoint.
var partySortKeys = []string{"hike_day", "created_at", "planned_start", "size"}

// PartySortKeys exposes the whitelist to the HTTP layer.
func PartySortKeys() []string { return append([]string(nil), partySortKeys...) }

// Create inserts a party plan.
func (r *PartyRepo) Create(ctx context.Context, party *domain.Party) (int64, error) {
	if party == nil {
		return 0, apperr.New(apperr.CodeInternal, "队伍数据为空")
	}
	var windowID any
	if party.PermitWindowID != nil {
		windowID = *party.PermitWindowID
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO parties (code, trail_id, leader_id, permit_window_id, hike_day, planned_start, planned_end,
			size, state, version, contact_phone, notes, confirmed_at, dispatched_at, closed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, NULL, NULL, NULL, ?, ?)`,
		party.Code, party.TrailID, party.LeaderID, windowID, party.HikeDay,
		unixOrZero(party.PlannedStart), unixOrZero(party.PlannedEnd), party.Size, string(party.State),
		party.ContactPhone, party.Notes, unixOrZero(party.CreatedAt), unixOrZero(party.UpdatedAt),
	)
	id, err := insertedID("创建队伍", result, err)
	if err != nil {
		return 0, err
	}
	party.ID = id
	party.Version = 1
	return id, nil
}

// ByID loads a party by identifier.
func (r *PartyRepo) ByID(ctx context.Context, id int64) (*domain.Party, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+partyColumns+` FROM parties WHERE id = ?`, id)
	return scanPartyRow(row)
}

// ByCode loads a party by its public code.
func (r *PartyRepo) ByCode(ctx context.Context, code string) (*domain.Party, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+partyColumns+` FROM parties WHERE code = ?`, code)
	return scanPartyRow(row)
}

// List returns a filtered, sorted page of parties.
func (r *PartyRepo) List(ctx context.Context, filter domain.PartyFilter, page domain.PageRequest) (domain.Page[domain.Party], error) {
	where := " WHERE 1 = 1"
	var args []any
	if filter.LeaderID > 0 {
		where += " AND leader_id = ?"
		args = append(args, filter.LeaderID)
	}
	if filter.TrailID > 0 {
		where += " AND trail_id = ?"
		args = append(args, filter.TrailID)
	}
	if len(filter.States) > 0 {
		where += " AND state IN (" + placeholders(len(filter.States)) + ")"
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	if filter.HikeDayGE != "" {
		where += " AND hike_day >= ?"
		args = append(args, filter.HikeDayGE)
	}
	if filter.HikeDayLE != "" {
		where += " AND hike_day <= ?"
		args = append(args, filter.HikeDayLE)
	}
	if filter.Search != "" {
		where += " AND (code LIKE ? OR notes LIKE ?)"
		pattern := "%" + escapeLike(filter.Search) + "%"
		args = append(args, pattern, pattern)
	}

	total, err := countQuery(ctx, r.q(ctx), "统计队伍", `SELECT COUNT(1) FROM parties`+where, args...)
	if err != nil {
		return domain.Page[domain.Party]{}, err
	}
	query := fmt.Sprintf(`SELECT %s FROM parties%s ORDER BY %s %s, id ASC LIMIT ? OFFSET ?`,
		partyColumns, where, page.SortBy, page.Direction())
	rows, err := r.q(ctx).QueryContext(ctx, query, append(args, page.Limit(), page.Offset())...)
	if err != nil {
		return domain.Page[domain.Party]{}, translate("查询队伍", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.Party, 0, page.Size)
	for rows.Next() {
		party, err := scanParty(rows)
		if err != nil {
			return domain.Page[domain.Party]{}, err
		}
		items = append(items, *party)
	}
	if err := rows.Err(); err != nil {
		return domain.Page[domain.Party]{}, translate("遍历队伍", err)
	}
	return domain.NewPage(items, total, page), nil
}

// ApplyStateChange performs a conditional party transition. Both the expected
// state and the expected version must still match, otherwise the caller learns
// that another actor moved the party first.
func (r *PartyRepo) ApplyStateChange(ctx context.Context, change repository.PartyStateChange) error {
	assignments := []string{"state = ?", "version = version + 1", "updated_at = ?"}
	args := []any{string(change.NextState), unixOrZero(change.Now)}
	if change.PermitWindowID != nil {
		assignments = append(assignments, "permit_window_id = ?")
		args = append(args, *change.PermitWindowID)
	}
	if change.ConfirmedAt != nil {
		assignments = append(assignments, "confirmed_at = ?")
		args = append(args, change.ConfirmedAt.Unix())
	}
	if change.DispatchedAt != nil {
		assignments = append(assignments, "dispatched_at = ?")
		args = append(args, change.DispatchedAt.Unix())
	}
	if change.ClosedAt != nil {
		assignments = append(assignments, "closed_at = ?")
		args = append(args, change.ClosedAt.Unix())
	}
	query := `UPDATE parties SET ` + strings.Join(assignments, ", ") + ` WHERE id = ? AND state = ? AND version = ?`
	args = append(args, change.PartyID, string(change.ExpectedState), change.ExpectedVersion)

	affected, err := affectedRows("更新队伍状态", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx, query, args...)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.Newf(apperr.CodeVersionConflict,
			"队伍状态已被其他操作变更，期望 %s，请重新读取后重试", change.ExpectedState)
	}
	return nil
}

// AddMember registers one hiker on a party.
func (r *PartyRepo) AddMember(ctx context.Context, member *domain.PartyMember) (int64, error) {
	if member == nil {
		return 0, apperr.New(apperr.CodeInternal, "队员数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO party_members (party_id, member_ref, display_name, kind, waiver_signed, phone, joined_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		member.PartyID, member.MemberRef, member.DisplayName, string(member.Kind),
		boolToInt(member.WaiverSigned), member.Phone, unixOrZero(member.JoinedAt),
	)
	id, err := insertedID("登记队员", result, err)
	if err != nil {
		return 0, err
	}
	member.ID = id
	return id, nil
}

// Members lists the registered hikers of a party.
func (r *PartyRepo) Members(ctx context.Context, partyID int64) ([]domain.PartyMember, error) {
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT id, party_id, member_ref, display_name, kind, waiver_signed, phone, joined_at
		 FROM party_members WHERE party_id = ? ORDER BY id ASC`, partyID)
	if err != nil {
		return nil, translate("查询队员", err)
	}
	defer func() { _ = rows.Close() }()

	members := make([]domain.PartyMember, 0, 8)
	for rows.Next() {
		var member domain.PartyMember
		var kind string
		var waiver int
		var joinedAt int64
		if err := rows.Scan(&member.ID, &member.PartyID, &member.MemberRef, &member.DisplayName,
			&kind, &waiver, &member.Phone, &joinedAt); err != nil {
			return nil, translate("解析队员", err)
		}
		member.Kind = domain.MemberKind(kind)
		member.WaiverSigned = waiver == 1
		member.JoinedAt = fromUnix(joinedAt)
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历队员", err)
	}
	return members, nil
}

// CountMembers counts the registered hikers of a party.
func (r *PartyRepo) CountMembers(ctx context.Context, partyID int64) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计队员", `SELECT COUNT(1) FROM party_members WHERE party_id = ?`, partyID)
}

// OnTrailParties lists parties currently hiking, oldest planned start first.
func (r *PartyRepo) OnTrailParties(ctx context.Context, limit int) ([]domain.Party, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT `+partyColumns+` FROM parties WHERE state = ? ORDER BY planned_start ASC, id ASC LIMIT ?`,
		string(domain.PartyOnTrail), limit)
	if err != nil {
		return nil, translate("查询在途队伍", err)
	}
	defer func() { _ = rows.Close() }()

	parties := make([]domain.Party, 0, limit)
	for rows.Next() {
		party, err := scanParty(rows)
		if err != nil {
			return nil, err
		}
		parties = append(parties, *party)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历在途队伍", err)
	}
	return parties, nil
}

func scanPartyRow(row *sql.Row) (*domain.Party, error) {
	party, state, windowID, times := newPartyScanTargets()
	scanErr := row.Scan(&party.ID, &party.Code, &party.TrailID, &party.LeaderID, windowID, &party.HikeDay,
		&times.plannedStart, &times.plannedEnd, &party.Size, state, &party.Version,
		&party.ContactPhone, &party.Notes, &times.confirmedAt, &times.dispatchedAt, &times.closedAt,
		&times.createdAt, &times.updatedAt)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return nil, notFound("队伍")
	case scanErr != nil:
		return nil, translate("读取队伍", scanErr)
	}
	finishParty(party, state, windowID, times)
	return party, nil
}

func scanParty(rows *sql.Rows) (*domain.Party, error) {
	party, state, windowID, times := newPartyScanTargets()
	if scanErr := rows.Scan(&party.ID, &party.Code, &party.TrailID, &party.LeaderID, windowID, &party.HikeDay,
		&times.plannedStart, &times.plannedEnd, &party.Size, state, &party.Version,
		&party.ContactPhone, &party.Notes, &times.confirmedAt, &times.dispatchedAt, &times.closedAt,
		&times.createdAt, &times.updatedAt); scanErr != nil {
		return nil, translate("解析队伍", scanErr)
	}
	finishParty(party, state, windowID, times)
	return party, nil
}

// partyTimes groups the raw timestamp columns of a party row.
type partyTimes struct {
	plannedStart int64
	plannedEnd   int64
	createdAt    int64
	updatedAt    int64
	confirmedAt  sql.NullInt64
	dispatchedAt sql.NullInt64
	closedAt     sql.NullInt64
}

// newPartyScanTargets allocates the scan destinations shared by the row and the
// rows based party readers.
func newPartyScanTargets() (*domain.Party, *string, *sql.NullInt64, *partyTimes) {
	return &domain.Party{}, new(string), &sql.NullInt64{}, &partyTimes{}
}

func finishParty(party *domain.Party, state *string, windowID *sql.NullInt64, times *partyTimes) {
	party.State = domain.PartyState(*state)
	if windowID.Valid {
		id := windowID.Int64
		party.PermitWindowID = &id
	}
	party.PlannedStart = fromUnix(times.plannedStart)
	party.PlannedEnd = fromUnix(times.plannedEnd)
	party.CreatedAt = fromUnix(times.createdAt)
	party.UpdatedAt = fromUnix(times.updatedAt)
	party.ConfirmedAt = fromNullUnix(times.confirmedAt)
	party.DispatchedAt = fromNullUnix(times.dispatchedAt)
	party.ClosedAt = fromNullUnix(times.closedAt)
}

// escapeLike neutralises the LIKE wildcards of a user supplied search term.
func escapeLike(value string) string {
	replaced := strings.ReplaceAll(value, "%", "")
	return strings.ReplaceAll(replaced, "_", "")
}

// CheckpointReportRepo is the SQLite backed progress report store.
type CheckpointReportRepo struct {
	base
}

// NewCheckpointReportRepo builds a progress report repository.
func NewCheckpointReportRepo(tx *sqlite.TxManager) *CheckpointReportRepo {
	return &CheckpointReportRepo{base{tx: tx}}
}

// Create stores one accepted progress report.
func (r *CheckpointReportRepo) Create(ctx context.Context, report *domain.CheckpointReport) (int64, error) {
	if report == nil {
		return 0, apperr.New(apperr.CodeInternal, "打点记录为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO checkpoint_reports (party_id, checkpoint_id, seq, status, head_count, note, reported_at, reported_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		report.PartyID, report.CheckpointID, report.Seq, string(report.Status),
		report.HeadCount, report.Note, unixOrZero(report.ReportedAt), report.ReportedBy,
	)
	id, err := insertedID("记录打点", result, err)
	if err != nil {
		return 0, err
	}
	report.ID = id
	return id, nil
}

// ByParty lists the progress reports of a party ordered by sequence.
func (r *CheckpointReportRepo) ByParty(ctx context.Context, partyID int64) ([]domain.CheckpointReport, error) {
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT id, party_id, checkpoint_id, seq, status, head_count, note, reported_at, reported_by
		 FROM checkpoint_reports WHERE party_id = ? ORDER BY seq ASC, id ASC`, partyID)
	if err != nil {
		return nil, translate("查询打点记录", err)
	}
	defer func() { _ = rows.Close() }()

	reports := make([]domain.CheckpointReport, 0, 8)
	for rows.Next() {
		var report domain.CheckpointReport
		var status string
		var reportedAt int64
		if err := rows.Scan(&report.ID, &report.PartyID, &report.CheckpointID, &report.Seq,
			&status, &report.HeadCount, &report.Note, &reportedAt, &report.ReportedBy); err != nil {
			return nil, translate("解析打点记录", err)
		}
		report.Status = domain.ReportStatus(status)
		report.ReportedAt = fromUnix(reportedAt)
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历打点记录", err)
	}
	return reports, nil
}

// Exists reports whether a checkpoint was already reported by a party.
func (r *CheckpointReportRepo) Exists(ctx context.Context, partyID, checkpointID int64) (bool, error) {
	total, err := countQuery(ctx, r.q(ctx), "检查打点记录",
		`SELECT COUNT(1) FROM checkpoint_reports WHERE party_id = ? AND checkpoint_id = ?`, partyID, checkpointID)
	if err != nil {
		return false, err
	}
	return total > 0, nil
}

// HighestSeq returns the highest reported checkpoint sequence of a party.
func (r *CheckpointReportRepo) HighestSeq(ctx context.Context, partyID int64) (int, error) {
	var seq sql.NullInt64
	err := r.q(ctx).QueryRowContext(ctx,
		`SELECT MAX(seq) FROM checkpoint_reports WHERE party_id = ?`, partyID).Scan(&seq)
	if err != nil {
		return 0, translate("读取最新打点序号", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return int(seq.Int64), nil
}

var (
	_ repository.PartyRepository            = (*PartyRepo)(nil)
	_ repository.CheckpointReportRepository = (*CheckpointReportRepo)(nil)
)
