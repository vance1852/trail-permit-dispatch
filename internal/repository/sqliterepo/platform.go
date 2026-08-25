package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// AuditRepo is the SQLite backed audit trail.
type AuditRepo struct {
	base
}

// NewAuditRepo builds an audit repository.
func NewAuditRepo(tx *sqlite.TxManager) *AuditRepo {
	return &AuditRepo{base{tx: tx}}
}

const auditColumns = `id, request_id, actor_id, actor_role, action, object_type, object_id, result, detail, created_at`

// auditSortKeys whitelists the sortable columns of the audit endpoint.
var auditSortKeys = []string{"created_at", "action"}

// AuditSortKeys exposes the whitelist to the HTTP layer.
func AuditSortKeys() []string { return append([]string(nil), auditSortKeys...) }

// Append writes one immutable audit event.
func (r *AuditRepo) Append(ctx context.Context, event *domain.AuditEvent) (int64, error) {
	if event == nil {
		return 0, apperr.New(apperr.CodeInternal, "审计事件为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO audit_events (request_id, actor_id, actor_role, action, object_type, object_id, result, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.RequestID, event.ActorID, string(event.ActorRole), event.Action,
		event.ObjectType, event.ObjectID, string(event.Result), event.Detail, unixOrZero(event.CreatedAt),
	)
	id, err := insertedID("写入审计事件", result, err)
	if err != nil {
		return 0, err
	}
	event.ID = id
	return id, nil
}

// List returns a filtered, sorted page of audit events.
func (r *AuditRepo) List(ctx context.Context, filter domain.AuditFilter, page domain.PageRequest) (domain.Page[domain.AuditEvent], error) {
	where := " WHERE 1 = 1"
	var args []any
	if filter.ActorID > 0 {
		where += " AND actor_id = ?"
		args = append(args, filter.ActorID)
	}
	if filter.Action != "" {
		where += " AND action = ?"
		args = append(args, filter.Action)
	}
	if filter.ObjectType != "" {
		where += " AND object_type = ?"
		args = append(args, filter.ObjectType)
	}
	if filter.ObjectID != "" {
		where += " AND object_id = ?"
		args = append(args, filter.ObjectID)
	}
	if filter.Result != "" {
		where += " AND result = ?"
		args = append(args, string(filter.Result))
	}
	total, err := countQuery(ctx, r.q(ctx), "统计审计事件", `SELECT COUNT(1) FROM audit_events`+where, args...)
	if err != nil {
		return domain.Page[domain.AuditEvent]{}, err
	}
	query := fmt.Sprintf(`SELECT %s FROM audit_events%s ORDER BY %s %s, id DESC LIMIT ? OFFSET ?`,
		auditColumns, where, page.SortBy, page.Direction())
	rows, err := r.q(ctx).QueryContext(ctx, query, append(args, page.Limit(), page.Offset())...)
	if err != nil {
		return domain.Page[domain.AuditEvent]{}, translate("查询审计事件", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.AuditEvent, 0, page.Size)
	for rows.Next() {
		var event domain.AuditEvent
		var role, result string
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.RequestID, &event.ActorID, &role, &event.Action,
			&event.ObjectType, &event.ObjectID, &result, &event.Detail, &createdAt); err != nil {
			return domain.Page[domain.AuditEvent]{}, translate("解析审计事件", err)
		}
		event.ActorRole = domain.Role(role)
		event.Result = domain.AuditResult(result)
		event.CreatedAt = fromUnix(createdAt)
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		return domain.Page[domain.AuditEvent]{}, translate("遍历审计事件", err)
	}
	return domain.NewPage(items, total, page), nil
}

// CountByObject counts the audit events of one business object.
func (r *AuditRepo) CountByObject(ctx context.Context, objectType, objectID string) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计对象审计事件",
		`SELECT COUNT(1) FROM audit_events WHERE object_type = ? AND object_id = ?`, objectType, objectID)
}

// JobRepo is the SQLite backed background work queue.
type JobRepo struct {
	base
}

// NewJobRepo builds a job repository.
func NewJobRepo(tx *sqlite.TxManager) *JobRepo {
	return &JobRepo{base{tx: tx}}
}

const jobColumns = `id, kind, payload, state, attempts, max_attempts, run_at, locked_by, locked_until, last_error, created_at, updated_at`

// Enqueue appends a job to the queue.
func (r *JobRepo) Enqueue(ctx context.Context, job *domain.Job) (int64, error) {
	if job == nil {
		return 0, apperr.New(apperr.CodeInternal, "后台作业为空")
	}
	if job.MaxAttempts <= 0 {
		return 0, apperr.New(apperr.CodeInvalidArgument, "后台作业的最大重试次数必须大于 0")
	}
	payload := job.Payload
	if payload == "" {
		payload = "{}"
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO jobs (kind, payload, state, attempts, max_attempts, run_at, locked_by, locked_until, last_error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', NULL, '', ?, ?)`,
		string(job.Kind), payload, string(domain.JobQueued), job.Attempts, job.MaxAttempts,
		unixOrZero(job.RunAt), unixOrZero(job.CreatedAt), unixOrZero(job.UpdatedAt),
	)
	id, err := insertedID("入队后台作业", result, err)
	if err != nil {
		return 0, err
	}
	job.ID = id
	job.State = domain.JobQueued
	return id, nil
}

// ByID loads a job by identifier.
func (r *JobRepo) ByID(ctx context.Context, id int64) (*domain.Job, error) {
	var job domain.Job
	var kind, state string
	var lockedUntil sql.NullInt64
	var runAt, createdAt, updatedAt int64
	err := r.q(ctx).QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id).
		Scan(&job.ID, &kind, &job.Payload, &state, &job.Attempts, &job.MaxAttempts,
			&runAt, &job.LockedBy, &lockedUntil, &job.LastError, &createdAt, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("后台作业")
	case err != nil:
		return nil, translate("读取后台作业", err)
	}
	job.Kind = domain.JobKind(kind)
	job.State = domain.JobState(state)
	job.RunAt = fromUnix(runAt)
	job.LockedUntil = fromNullUnix(lockedUntil)
	job.CreatedAt = fromUnix(createdAt)
	job.UpdatedAt = fromUnix(updatedAt)
	return &job, nil
}

// ClaimDue leases due jobs for one worker. Each candidate is claimed with a
// conditional update, so two workers polling at the same instant never obtain
// the same job.
func (r *JobRepo) ClaimDue(ctx context.Context, workerID string, lease time.Duration, now time.Time, limit int) ([]domain.Job, error) {
	if workerID == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "worker 标识不能为空")
	}
	if limit <= 0 {
		limit = 1
	}
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT id FROM jobs WHERE state = ? AND run_at <= ? ORDER BY run_at ASC, id ASC LIMIT ?`,
		string(domain.JobQueued), unixOrZero(now), limit)
	if err != nil {
		return nil, translate("查询到期作业", err)
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, translate("解析到期作业", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, translate("遍历到期作业", err)
	}
	if err := rows.Close(); err != nil {
		return nil, translate("关闭到期作业游标", err)
	}

	lockedUntil := now.Add(lease)
	claimed := make([]domain.Job, 0, len(candidates))
	for _, id := range candidates {
		affected, err := affectedRows("租约后台作业", func() (sql.Result, error) {
			return r.q(ctx).ExecContext(ctx,
				`UPDATE jobs SET state = ?, attempts = attempts + 1, locked_by = ?, locked_until = ?, updated_at = ?
				 WHERE id = ? AND state = ? AND run_at <= ?`,
				string(domain.JobRunning), workerID, lockedUntil.Unix(), unixOrZero(now),
				id, string(domain.JobQueued), unixOrZero(now))
		})
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			continue
		}
		job, err := r.ByID(ctx, id)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, *job)
	}
	return claimed, nil
}

// MarkDone completes a leased job.
func (r *JobRepo) MarkDone(ctx context.Context, jobID int64, now time.Time) error {
	affected, err := affectedRows("完成后台作业", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE jobs SET state = ?, locked_by = '', locked_until = NULL, last_error = '', updated_at = ?
			 WHERE id = ? AND state = ?`,
			string(domain.JobDone), unixOrZero(now), jobID, string(domain.JobRunning))
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeStateInvalid, "后台作业不在执行状态，无法标记完成")
	}
	return nil
}

// MarkFailure records a failed attempt. A permanent failure parks the job, a
// retryable failure returns it to the queue with a future run time.
func (r *JobRepo) MarkFailure(ctx context.Context, failure repository.JobFailure) error {
	nextState := domain.JobQueued
	runAt := failure.RetryAt
	if failure.Permanent {
		nextState = domain.JobFailed
		runAt = failure.Now
	}
	affected, err := affectedRows("记录后台作业失败", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE jobs SET state = ?, run_at = ?, locked_by = '', locked_until = NULL, last_error = ?, updated_at = ?
			 WHERE id = ? AND state = ?`,
			string(nextState), unixOrZero(runAt), truncateError(failure.Message), unixOrZero(failure.Now),
			failure.JobID, string(domain.JobRunning))
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return apperr.New(apperr.CodeStateInvalid, "后台作业不在执行状态，无法记录失败")
	}
	return nil
}

// ReclaimExpiredLeases returns jobs of crashed workers to the queue. It runs on
// startup and periodically, which is what makes the queue restart safe.
func (r *JobRepo) ReclaimExpiredLeases(ctx context.Context, now time.Time) (int, error) {
	affected, err := affectedRows("回收过期作业租约", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx,
			`UPDATE jobs SET state = ?, locked_by = '', locked_until = NULL, updated_at = ?
			 WHERE state = ? AND locked_until IS NOT NULL AND locked_until <= ?`,
			string(domain.JobQueued), unixOrZero(now), string(domain.JobRunning), unixOrZero(now))
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// CountByState counts jobs in one queue state.
func (r *JobRepo) CountByState(ctx context.Context, state domain.JobState) (int, error) {
	return countQuery(ctx, r.q(ctx), "统计后台作业", `SELECT COUNT(1) FROM jobs WHERE state = ?`, string(state))
}

// PendingKinds reports how many jobs of each kind still wait for execution.
func (r *JobRepo) PendingKinds(ctx context.Context) (map[domain.JobKind]int, error) {
	rows, err := r.q(ctx).QueryContext(ctx,
		`SELECT kind, COUNT(1) FROM jobs WHERE state IN (?, ?) GROUP BY kind`,
		string(domain.JobQueued), string(domain.JobRunning))
	if err != nil {
		return nil, translate("统计待执行作业", err)
	}
	defer func() { _ = rows.Close() }()

	pending := make(map[domain.JobKind]int, 5)
	for rows.Next() {
		var kind string
		var total int
		if err := rows.Scan(&kind, &total); err != nil {
			return nil, translate("解析待执行作业", err)
		}
		pending[domain.JobKind(kind)] = total
	}
	if err := rows.Err(); err != nil {
		return nil, translate("遍历待执行作业", err)
	}
	return pending, nil
}

// truncateError keeps stored error text bounded.
func truncateError(message string) string {
	const limit = 500
	if len(message) <= limit {
		return message
	}
	return message[:limit]
}

// IdempotencyRepo is the SQLite backed idempotency key store.
type IdempotencyRepo struct {
	base
}

// NewIdempotencyRepo builds an idempotency repository.
func NewIdempotencyRepo(tx *sqlite.TxManager) *IdempotencyRepo {
	return &IdempotencyRepo{base{tx: tx}}
}

// Find loads a stored response for a scope, key and actor triple.
func (r *IdempotencyRepo) Find(ctx context.Context, scope, key string, actorID int64) (*repository.IdempotencyRecord, error) {
	var record repository.IdempotencyRecord
	var createdAt, expiresAt int64
	// Records written by the shared request namespace carry actor_id 0, so the
	// lookup also accepts those rows for any caller.
	err := r.q(ctx).QueryRowContext(ctx,
		`SELECT id, scope, idem_key, actor_id, request_hash, response_body, created_at, expires_at
		 FROM idempotency_records
		 WHERE scope = ? AND idem_key = ? AND (actor_id = ? OR actor_id = 0)
		 ORDER BY actor_id DESC LIMIT 1`,
		scope, key, actorID).
		Scan(&record.ID, &record.Scope, &record.Key, &record.ActorID, &record.RequestHash,
			&record.ResponseBody, &createdAt, &expiresAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, notFound("幂等记录")
	case err != nil:
		return nil, translate("读取幂等记录", err)
	}
	record.CreatedAt = fromUnix(createdAt)
	record.ExpiresAt = fromUnix(expiresAt)
	return &record, nil
}

// Save stores the response of an accepted request.
func (r *IdempotencyRepo) Save(ctx context.Context, record *repository.IdempotencyRecord) (int64, error) {
	if record == nil {
		return 0, apperr.New(apperr.CodeInternal, "幂等记录为空")
	}
	result, err := r.q(ctx).ExecContext(ctx,
		`INSERT INTO idempotency_records (scope, idem_key, actor_id, request_hash, response_body, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.Scope, record.Key, record.ActorID, record.RequestHash, record.ResponseBody,
		unixOrZero(record.CreatedAt), unixOrZero(record.ExpiresAt),
	)
	id, err := insertedID("保存幂等记录", result, err)
	if err != nil {
		return 0, err
	}
	record.ID = id
	return id, nil
}

// DeleteExpired removes idempotency keys past their retention window.
func (r *IdempotencyRepo) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	affected, err := affectedRows("清理幂等记录", func() (sql.Result, error) {
		return r.q(ctx).ExecContext(ctx, `DELETE FROM idempotency_records WHERE expires_at <= ?`, unixOrZero(now))
	})
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

var (
	_ repository.AuditRepository       = (*AuditRepo)(nil)
	_ repository.JobRepository         = (*JobRepo)(nil)
	_ repository.IdempotencyRepository = (*IdempotencyRepo)(nil)
)
