package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// IncidentRepo is the SQLite backed incident store.
type IncidentRepo struct {
	base
}

// NewIncidentRepo builds an incident repository.
func NewIncidentRepo(tx *sqlite.TxManager) *IncidentRepo {
	return &IncidentRepo{base{tx: tx}}
}

const incidentColumns = `id, party_id, kind, severity, state, summary, checkpoint_seq, escalation_count,
	opened_by, opened_at, resolved_at, resolution, version, updated_at`

// incidentSortKeys whitelists the sortable columns of the incident endpoint.
var incidentSortKeys = []string{"opened_at", "severity", "state"}

// IncidentSortKeys exposes the whitelist to the HTTP layer.
func IncidentSortKeys() []string { return append([]string(nil), incidentSortKeys...) }

// Create stores a new incident.
func (r *IncidentRepo) Create(ctx context.Context, incident *domain.Incident) (int64, error) {
	if incident == nil {
		return 0, apperr.New(apperr.CodeInternal, "事故数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO incidents (party_id, kind, severity, state, summary, checkpoint_seq, escalation_count,
			opened_by, opened_at, resolved_at, resolution, version, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', 1, ?)`,
		incident.PartyID, incident.Kind, string(incident.Severity), string(incident.State),
		incident.Summary, incident.CheckpointSeq, incident.EscalationCount,
		incident.OpenedBy, unixOrZero(incident.OpenedAt), unixOrZero(incident.UpdatedAt),
	)
	id, err := insertedID("创建事故", result, err)
	if err != nil {
		return 0, err
	}
	incident.ID = id
	incident.Version = 1
	return id, nil
}

// ByID loads an incident by identifier.
func (r *IncidentRepo) ByID(ctx context.Context, id int64) (*domain.Incident, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = ?`, id)
	incident, err := scanIncidentRow(row)
	if err != nil {
		return nil, err
	}
	return incident, nil
}

// List returns a filtered, sorted page of incidents.
func (r *IncidentRepo) List(ctx context.Context, filter domain.IncidentFilter, page domain.PageRequest) (domain.Page[domain.Incident], error) {
	where := " WHERE 1 = 1"
	var args []any
	if filter.PartyID > 0 {
		where += " AND party_id = ?"
		args = append(args, filter.PartyID)
	}
	if len(filter.States) > 0 {
		where += " AND state IN (" + placeholders(len(filter.States)) + ")"
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	if len(filter.Severities) > 0 {
		where += " AND severity IN (" + placeholders(len(filter.Severities)) + ")"
		for _, severity := range filter.Severities {
			args = append(args, string(severity))
		}
	}
	total, err := countQuery(ctx, r.q(ctx), "统计事故", `SELECT COUNT(1) FROM incidents`+where, args...)
	if err != nil {
		return domain.Page[domain.Incident]{}, err
	}
	query := fmt.Sprintf(`SELECT %s FROM incidents%s ORDER BY %s %s, id ASC LIMIT ? OFFSET ?`,
		incidentColumns, where, page.SortBy, page.Direction())
	rows, err := r.q(ctx).QueryContext(ctx, query, append(args, page.Limit(), page.Offset())...)
	if err != nil {
		return domain.Page[domain.Incident]{}, translate("查询事故", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.Incident, 0, page.Size)
	for rows.Next() {
		incident, err := scanIncident(rows)
		if err != nil {
			return domain.Page[domain.Incident]{}, err
		}
		items = append(items, *incident)
	}
	if err := rows.Err(); err != nil {
		return domain.Page[domain.Incident]{}, translate("遍历事故", err)
	}
	return domain.NewPage(items, total, page), nil
}

// Apply performs a conditional incident update guarded by the version.
func (r *IncidentRepo) Apply(ctx context.Context, update repository.IncidentUpdate) error {
	affected, err := affectedRows("更新事故", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE incidents
			 SET state = ?, escalation_count = ?, resolution = ?, resolved_at = ?, version = version + 1, updated_at = ?
			 WHERE id = ? AND version = ?`,
			string(update.NextState), update.EscalationCount, update.Resolution,
			unixPtr(update.ResolvedAt), unixOrZero(update.Now), update.IncidentID, update.ExpectedVersion)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeVersionConflict, "事故记录已被其他操作更新，请重新读取后重试")
	}
	return nil
}

// OpenByPartyKind finds an unresolved incident of the same kind and checkpoint.
func (r *IncidentRepo) OpenByPartyKind(ctx context.Context, partyID int64, kind string, checkpointSeq int) (*domain.Incident, error) {
	row := r.q(ctx).QueryRowContext(ctx,
		`SELECT `+incidentColumns+` FROM incidents
		 WHERE party_id = ? AND kind = ? AND checkpoint_seq = ? AND state IN ('open', 'escalated')
		 ORDER BY id DESC LIMIT 1`, partyID, kind, checkpointSeq)
	return scanIncidentRow(row)
}

// CountOpenByParty counts unresolved incidents of a party.
func (r *IncidentRepo) CountOpenByParty(ctx context.Context, partyID int64) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计未处置事故",
		`SELECT COUNT(1) FROM incidents WHERE party_id = ? AND state IN ('open', 'escalated')`, partyID)
}

func scanIncidentRow(row *sql.Row) (*domain.Incident, error) {
	var incident domain.Incident
	var severity, state string
	var openedAt, updatedAt int64
	var resolvedAt sql.NullInt64
	err := row.Scan(&incident.ID, &incident.PartyID, &incident.Kind, &severity, &state, &incident.Summary,
		&incident.CheckpointSeq, &incident.EscalationCount, &incident.OpenedBy, &openedAt,
		&resolvedAt, &incident.Resolution, &incident.Version, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("事故记录")
	case err != nil:
		return nil, translate("读取事故记录", err)
	}
	incident.Severity = domain.Severity(severity)
	incident.State = domain.IncidentState(state)
	incident.OpenedAt = fromUnix(openedAt)
	incident.UpdatedAt = fromUnix(updatedAt)
	incident.ResolvedAt = fromNullUnix(resolvedAt)
	return &incident, nil
}

func scanIncident(rows *sql.Rows) (*domain.Incident, error) {
	var incident domain.Incident
	var severity, state string
	var openedAt, updatedAt int64
	var resolvedAt sql.NullInt64
	if err := rows.Scan(&incident.ID, &incident.PartyID, &incident.Kind, &severity, &state, &incident.Summary,
		&incident.CheckpointSeq, &incident.EscalationCount, &incident.OpenedBy, &openedAt,
		&resolvedAt, &incident.Resolution, &incident.Version, &updatedAt); err != nil {
		return nil, translate("解析事故记录", err)
	}
	incident.Severity = domain.Severity(severity)
	incident.State = domain.IncidentState(state)
	incident.OpenedAt = fromUnix(openedAt)
	incident.UpdatedAt = fromUnix(updatedAt)
	incident.ResolvedAt = fromNullUnix(resolvedAt)
	return &incident, nil
}

// SettlementRepo is the SQLite backed permit fee store.
type SettlementRepo struct {
	base
}

// NewSettlementRepo builds a settlement repository.
func NewSettlementRepo(tx *sqlite.TxManager) *SettlementRepo {
	return &SettlementRepo{base{tx: tx}}
}

const settlementColumns = `id, party_id, seats, unit_fee_cents, total_cents, state, reference,
	failure_note, settled_at, version, created_at, updated_at`

// settlementSortKeys whitelists the sortable columns of the settlement endpoint.
var settlementSortKeys = []string{"created_at", "total_cents", "state"}

// SettlementSortKeys exposes the whitelist to the HTTP layer.
func SettlementSortKeys() []string { return append([]string(nil), settlementSortKeys...) }

// Create stores a settlement record.
func (r *SettlementRepo) Create(ctx context.Context, settlement *domain.Settlement) (int64, error) {
	if settlement == nil {
		return 0, apperr.New(apperr.CodeInternal, "结算数据为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO settlements (party_id, seats, unit_fee_cents, total_cents, state, reference,
			failure_note, settled_at, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', NULL, 1, ?, ?)`,
		settlement.PartyID, settlement.Seats, settlement.UnitFeeCents, settlement.TotalCents,
		string(settlement.State), settlement.Reference,
		unixOrZero(settlement.CreatedAt), unixOrZero(settlement.UpdatedAt),
	)
	id, err := insertedID("创建结算记录", result, err)
	if err != nil {
		return 0, err
	}
	settlement.ID = id
	settlement.Version = 1
	return id, nil
}

// ByID loads a settlement by identifier.
func (r *SettlementRepo) ByID(ctx context.Context, id int64) (*domain.Settlement, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+settlementColumns+` FROM settlements WHERE id = ?`, id)
	return scanSettlementRow(row)
}

// ByPartyID loads the settlement of a party.
func (r *SettlementRepo) ByPartyID(ctx context.Context, partyID int64) (*domain.Settlement, error) {
	row := r.q(ctx).QueryRowContext(ctx, `SELECT `+settlementColumns+` FROM settlements WHERE party_id = ?`, partyID)
	return scanSettlementRow(row)
}

// List returns a filtered, sorted page of settlements.
func (r *SettlementRepo) List(ctx context.Context, filter domain.SettlementFilter, page domain.PageRequest) (domain.Page[domain.Settlement], error) {
	where := " WHERE 1 = 1"
	var args []any
	if filter.PartyID > 0 {
		where += " AND party_id = ?"
		args = append(args, filter.PartyID)
	}
	if len(filter.States) > 0 {
		where += " AND state IN (" + placeholders(len(filter.States)) + ")"
		for _, state := range filter.States {
			args = append(args, string(state))
		}
	}
	total, err := countQuery(ctx, r.q(ctx), "统计结算记录", `SELECT COUNT(1) FROM settlements`+where, args...)
	if err != nil {
		return domain.Page[domain.Settlement]{}, err
	}
	query := fmt.Sprintf(`SELECT %s FROM settlements%s ORDER BY %s %s, id ASC LIMIT ? OFFSET ?`,
		settlementColumns, where, page.SortBy, page.Direction())
	rows, err := r.q(ctx).QueryContext(ctx, query, append(args, page.Limit(), page.Offset())...)
	if err != nil {
		return domain.Page[domain.Settlement]{}, translate("查询结算记录", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.Settlement, 0, page.Size)
	for rows.Next() {
		settlement, err := scanSettlement(rows)
		if err != nil {
			return domain.Page[domain.Settlement]{}, err
		}
		items = append(items, *settlement)
	}
	if err := rows.Err(); err != nil {
		return domain.Page[domain.Settlement]{}, translate("遍历结算记录", err)
	}
	return domain.NewPage(items, total, page), nil
}

// Apply performs a conditional settlement update guarded by the version.
func (r *SettlementRepo) Apply(ctx context.Context, update repository.SettlementUpdate) error {
	affected, err := affectedRows("更新结算记录", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE settlements
			 SET state = ?, reference = ?, failure_note = ?, settled_at = ?, version = version + 1, updated_at = ?
			 WHERE id = ? AND version = ?`,
			string(update.NextState), update.Reference, update.FailureNote,
			unixPtr(update.SettledAt), unixOrZero(update.Now), update.SettlementID, update.ExpectedVersion)
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeVersionConflict, "结算记录已被其他操作更新，请重新读取后重试")
	}
	return nil
}

func scanSettlementRow(row *sql.Row) (*domain.Settlement, error) {
	var settlement domain.Settlement
	var state string
	var createdAt, updatedAt int64
	var settledAt sql.NullInt64
	err := row.Scan(&settlement.ID, &settlement.PartyID, &settlement.Seats, &settlement.UnitFeeCents,
		&settlement.TotalCents, &state, &settlement.Reference, &settlement.FailureNote,
		&settledAt, &settlement.Version, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("结算记录")
	case err != nil:
		return nil, translate("读取结算记录", err)
	}
	settlement.State = domain.SettlementState(state)
	settlement.SettledAt = fromNullUnix(settledAt)
	settlement.CreatedAt = fromUnix(createdAt)
	settlement.UpdatedAt = fromUnix(updatedAt)
	return &settlement, nil
}

func scanSettlement(rows *sql.Rows) (*domain.Settlement, error) {
	var settlement domain.Settlement
	var state string
	var createdAt, updatedAt int64
	var settledAt sql.NullInt64
	if err := rows.Scan(&settlement.ID, &settlement.PartyID, &settlement.Seats, &settlement.UnitFeeCents,
		&settlement.TotalCents, &state, &settlement.Reference, &settlement.FailureNote,
		&settledAt, &settlement.Version, &createdAt, &updatedAt); err != nil {
		return nil, translate("解析结算记录", err)
	}
	settlement.State = domain.SettlementState(state)
	settlement.SettledAt = fromNullUnix(settledAt)
	settlement.CreatedAt = fromUnix(createdAt)
	settlement.UpdatedAt = fromUnix(updatedAt)
	return &settlement, nil
}

var (
	_ repository.IncidentRepository   = (*IncidentRepo)(nil)
	_ repository.SettlementRepository = (*SettlementRepo)(nil)
)
