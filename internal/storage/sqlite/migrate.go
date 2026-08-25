package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/migrations"
)

// schemaTableDDL tracks which migrations were applied and with which content.
const schemaTableDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
)`

// AppliedMigration is one row of the schema_migrations table.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// MigrateResult reports what a migration run actually changed.
type MigrateResult struct {
	Applied       []int
	AlreadyLatest bool
	Version       int
}

// Migrate brings the database up to the latest embedded schema version. Running
// it twice is a no-op. A checksum drift on an already applied version is a hard
// error: the platform refuses to silently reinterpret historical data.
func Migrate(ctx context.Context, db *sql.DB) (MigrateResult, error) {
	if db == nil {
		return MigrateResult{}, apperr.New(apperr.CodeInternal, "数据库句柄为空")
	}
	if _, err := db.ExecContext(ctx, schemaTableDDL); err != nil {
		return MigrateResult{}, apperr.Wrap(apperr.CodeUnavailable, "创建迁移记录表失败", err)
	}
	all, err := migrations.All()
	if err != nil {
		return MigrateResult{}, apperr.Wrap(apperr.CodeInternal, "加载内置迁移失败", err)
	}
	applied, err := AppliedMigrations(ctx, db)
	if err != nil {
		return MigrateResult{}, err
	}
	appliedByVersion := make(map[int]AppliedMigration, len(applied))
	for _, record := range applied {
		appliedByVersion[record.Version] = record
	}

	result := MigrateResult{}
	for _, migration := range all {
		if record, exists := appliedByVersion[migration.Version]; exists {
			if record.Checksum != migration.Checksum {
				return MigrateResult{}, apperr.Newf(apperr.CodeConflict,
					"迁移 %04d_%s 的内容与已应用记录不一致，请人工核对后再启动",
					migration.Version, migration.Name)
			}
			continue
		}
		if err := applyMigration(ctx, db, migration); err != nil {
			return MigrateResult{}, err
		}
		result.Applied = append(result.Applied, migration.Version)
	}
	result.Version = all[len(all)-1].Version
	result.AlreadyLatest = len(result.Applied) == 0
	return result, nil
}

// applyMigration runs one migration and records it in the same transaction so a
// partially applied schema can never be marked as complete.
func applyMigration(ctx context.Context, db *sql.DB, migration migrations.Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return apperr.Wrapf(apperr.CodeUnavailable, err, "开启迁移事务失败 (%s)", migration.FileName)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return apperr.Wrapf(apperr.CodeUnavailable, err, "执行迁移 %s 失败", migration.FileName)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		migration.Version, migration.Name, migration.Checksum, time.Now().Unix(),
	); err != nil {
		return apperr.Wrapf(apperr.CodeUnavailable, err, "记录迁移 %s 失败", migration.FileName)
	}
	if err := tx.Commit(); err != nil {
		return apperr.Wrapf(apperr.CodeUnavailable, err, "提交迁移 %s 失败", migration.FileName)
	}
	return nil
}

// AppliedMigrations lists the migrations recorded in the database.
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]AppliedMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "读取迁移记录失败", err)
	}
	defer func() { _ = rows.Close() }()

	var records []AppliedMigration
	for rows.Next() {
		var record AppliedMigration
		var appliedAt int64
		if err := rows.Scan(&record.Version, &record.Name, &record.Checksum, &appliedAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeUnavailable, "解析迁移记录失败", err)
		}
		record.AppliedAt = time.Unix(appliedAt, 0)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeUnavailable, "遍历迁移记录失败", err)
	}
	return records, nil
}

// SchemaVersion returns the highest applied schema version, or zero for an
// empty database.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, apperr.Wrap(apperr.CodeUnavailable, "读取当前 schema 版本失败", err)
	case !version.Valid:
		return 0, nil
	default:
		return int(version.Int64), nil
	}
}

// VerifySchema reports whether the database matches the embedded schema version.
func VerifySchema(ctx context.Context, db *sql.DB) error {
	current, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	expected, err := migrations.LatestVersion()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "读取内置迁移版本失败", err)
	}
	if current != expected {
		return apperr.Newf(apperr.CodeUnavailable, "数据库 schema 版本为 %d，期望 %d", current, expected)
	}
	return nil
}
