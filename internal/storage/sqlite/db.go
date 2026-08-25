// Package sqlite owns the database handle, the pragma configuration and the
// context scoped transaction manager used by every repository.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	// modernc.org/sqlite is a pure Go driver, which keeps CGO disabled and lets
	// the same binary build for linux/amd64 and linux/arm64.
	_ "modernc.org/sqlite"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// driverName is the database/sql driver registered by modernc.org/sqlite.
const driverName = "sqlite"

// pragmas are appended to every DSN. Write transactions start as IMMEDIATE so a
// read-modify-write sequence inside one transaction cannot interleave, and the
// busy timeout removes spurious SQLITE_BUSY errors under worker contention.
var pragmas = []string{
	"_pragma=busy_timeout(8000)",
	"_pragma=foreign_keys(1)",
	"_pragma=journal_mode(wal)",
	"_pragma=synchronous(1)",
	"_txlock=immediate",
}

// Open prepares the database file, applies the platform pragmas and verifies
// connectivity. The caller owns the returned handle.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, apperr.New(apperr.CodeInvalidArgument, "数据库 DSN 不能为空")
	}
	if err := ensureParentDir(dsn); err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, buildDSN(dsn))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "打开数据库失败", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, apperr.Wrap(apperr.CodeUnavailable, "数据库连接不可用", err)
	}
	return db, nil
}

// buildDSN appends the platform pragmas that are not already present.
func buildDSN(dsn string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	var builder strings.Builder
	builder.WriteString(dsn)
	for _, pragma := range pragmas {
		key := pragma
		if index := strings.IndexByte(pragma, '='); index > 0 {
			key = pragma[:index]
		}
		if key == "_pragma" {
			name := pragma[strings.IndexByte(pragma, '=')+1:]
			if index := strings.IndexByte(name, '('); index > 0 {
				name = name[:index]
			}
			if strings.Contains(dsn, "_pragma="+name) {
				continue
			}
		} else if strings.Contains(dsn, key+"=") {
			continue
		}
		builder.WriteString(separator)
		builder.WriteString(pragma)
		separator = "&"
	}
	return builder.String()
}

// ensureParentDir creates the directory of a file backed DSN.
func ensureParentDir(dsn string) error {
	path := filePathFromDSN(dsn)
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return apperr.Wrapf(apperr.CodeUnavailable, err, "创建数据库目录 %s 失败", dir)
	}
	return nil
}

// filePathFromDSN extracts the on disk path of a file DSN, returning an empty
// string for memory backed databases.
func filePathFromDSN(dsn string) string {
	trimmed := strings.TrimPrefix(dsn, "file:")
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		if strings.Contains(trimmed[index:], "mode=memory") {
			return ""
		}
		trimmed = trimmed[:index]
	}
	if trimmed == "" || trimmed == ":memory:" {
		return ""
	}
	return trimmed
}

// Querier is the subset of database/sql shared by *sql.DB and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txKey carries the ambient transaction through the request context.
type txKey struct{}

// TxManager runs functions inside a single database transaction and exposes the
// ambient querier so repositories never need to know whether they are called
// inside a transaction.
type TxManager struct {
	db *sql.DB
}

// NewTxManager builds a transaction manager for the given handle.
func NewTxManager(db *sql.DB) *TxManager {
	return &TxManager{db: db}
}

// DB returns the underlying handle for health checks and migrations.
func (m *TxManager) DB() *sql.DB { return m.db }

// Querier returns the ambient transaction when one is active, otherwise the
// pooled handle.
func (m *TxManager) Querier(ctx context.Context) Querier {
	if tx, ok := ctx.Value(txKey{}).(*sql.Tx); ok && tx != nil {
		return tx
	}
	return m.db
}

// InTx reports whether the context already carries a transaction.
func InTx(ctx context.Context) bool {
	tx, ok := ctx.Value(txKey{}).(*sql.Tx)
	return ok && tx != nil
}

// WithTx executes fn inside one transaction. Nested calls join the existing
// transaction so a service can compose repository operations freely. Any error
// or panic rolls the whole unit of work back.
func (m *TxManager) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if InTx(ctx) {
		return fn(ctx)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeUnavailable, "开启数据库事务失败", err)
	}
	txCtx := context.WithValue(ctx, txKey{}, tx)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(txCtx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if errors.Is(err, sql.ErrTxDone) {
			return apperr.Wrap(apperr.CodeConflict, "事务已结束，提交被拒绝", err)
		}
		return apperr.Wrap(apperr.CodeUnavailable, "提交数据库事务失败", err)
	}
	committed = true
	return nil
}

// Stats renders pool statistics for the readiness endpoint.
func (m *TxManager) Stats() string {
	stats := m.db.Stats()
	return fmt.Sprintf("open=%d in_use=%d idle=%d wait=%d", stats.OpenConnections, stats.InUse, stats.Idle, stats.WaitCount)
}
