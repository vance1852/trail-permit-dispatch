// Package sqliterepo implements the repository contracts on top of SQLite. Every
// method takes a context, resolves the ambient transaction and returns copies of
// its data so callers can never mutate shared state.
package sqliterepo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// base carries the transaction manager shared by all repositories.
type base struct {
	tx *sqlite.TxManager
}

// q resolves the querier of the current context.
func (b base) q(ctx context.Context) sqlite.Querier {
	return b.tx.Querier(ctx)
}

// unixOrZero renders a time as unix seconds, mapping the zero time to 0.
func unixOrZero(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.Unix()
}

// unixPtr renders an optional time as a nullable unix seconds value.
func unixPtr(at *time.Time) any {
	if at == nil || at.IsZero() {
		return nil
	}
	return at.Unix()
}

// fromUnix rebuilds a business zone time from unix seconds.
func fromUnix(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).In(businessZone())
}

// fromNullUnix rebuilds an optional time from a nullable column.
func fromNullUnix(value sql.NullInt64) *time.Time {
	if !value.Valid || value.Int64 == 0 {
		return nil
	}
	at := time.Unix(value.Int64, 0).In(businessZone())
	return &at
}

// boolToInt maps a bool onto the SQLite integer representation.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// translate maps driver level failures onto the platform error vocabulary so
// services and HTTP handlers never inspect SQLite specific strings.
func translate(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apperr.Wrapf(apperr.CodeDeadlineExceeded, err, "%s 已取消", operation)
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "UNIQUE constraint failed"):
		return apperr.Wrapf(apperr.CodeConflict, err, "%s 违反唯一约束", operation)
	case strings.Contains(message, "FOREIGN KEY constraint failed"):
		return apperr.Wrapf(apperr.CodeConflict, err, "%s 违反外键约束", operation)
	case strings.Contains(message, "CHECK constraint failed"):
		return apperr.Wrapf(apperr.CodeInvalidArgument, err, "%s 违反数据校验约束", operation)
	case strings.Contains(message, "database is locked"):
		return apperr.Wrapf(apperr.CodeUnavailable, err, "%s 因数据库锁竞争超时", operation)
	default:
		return apperr.Wrapf(apperr.CodeUnavailable, err, "%s 失败", operation)
	}
}

// notFound builds the canonical missing resource error.
func notFound(entity string) error {
	return apperr.Newf(apperr.CodeNotFound, "%s不存在", entity)
}

// affectedRows runs a write and extracts the affected row count.
func affectedRows(operation string, exec func() (sql.Result, error)) (int64, error) {
	result, err := exec()
	if err != nil {
		return 0, translate(operation, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, translate(operation, err)
	}
	return affected, nil
}

// insertedID extracts the auto increment identifier of an insert.
func insertedID(operation string, result sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, translate(operation, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, translate(operation, err)
	}
	return id, nil
}

// countQuery runs a scalar count query.
func countQuery(ctx context.Context, q sqlite.Querier, operation, query string, args ...any) (int, error) {
	var total int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, translate(operation, err)
	}
	return total, nil
}

// placeholders renders a comma separated placeholder list of the given size.
func placeholders(size int) string {
	if size <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", size), ",")
}
