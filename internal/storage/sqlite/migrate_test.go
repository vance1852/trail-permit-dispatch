package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// openTestDB creates a real SQLite database in a temporary directory.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "trail.sqlite"))
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateBuildsSchemaFromEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	version, err := SchemaVersion(ctx, db)
	if err == nil && version != 0 {
		t.Fatalf("空库的 schema 版本应为 0，实际 %d", version)
	}

	result, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if len(result.Applied) != 3 {
		t.Fatalf("期望应用 3 个迁移，实际 %v", result.Applied)
	}
	if result.AlreadyLatest {
		t.Fatal("首次迁移不应报告已是最新")
	}
	if result.Version != 3 {
		t.Fatalf("最新版本应为 3，实际 %d", result.Version)
	}
	if err := VerifySchema(ctx, db); err != nil {
		t.Fatalf("迁移后 schema 校验失败: %v", err)
	}

	tables := []string{
		"users", "sessions", "trails", "permit_windows", "checkpoints", "parties",
		"party_members", "checkpoint_reports", "incidents", "settlements",
		"audit_events", "jobs", "idempotency_records",
	}
	for _, table := range tables {
		var name string
		err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 未创建: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	second, err := Migrate(ctx, db)
	if err != nil {
		t.Fatalf("重复迁移应无副作用: %v", err)
	}
	if !second.AlreadyLatest || len(second.Applied) != 0 {
		t.Fatalf("重复迁移不应再次应用: %+v", second)
	}
	applied, err := AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("读取迁移记录失败: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("迁移记录数量错误: %d", len(applied))
	}
	for _, record := range applied {
		if record.Checksum == "" || record.AppliedAt.IsZero() {
			t.Fatalf("迁移记录缺少校验和或时间: %+v", record)
		}
	}
}

func TestMigrateBlocksChecksumDrift(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 2`); err != nil {
		t.Fatalf("篡改迁移记录失败: %v", err)
	}
	_, err := Migrate(ctx, db)
	if err == nil {
		t.Fatal("历史迁移内容不一致时必须阻断启动")
	}
	if !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("校验和漂移应返回 conflict，实际 %s (%v)", apperr.CodeOf(err), err)
	}
}

func TestVerifySchemaFailsOnStaleDatabase(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 3`); err != nil {
		t.Fatalf("删除迁移记录失败: %v", err)
	}
	err := VerifySchema(ctx, db)
	if !apperr.Is(err, apperr.CodeUnavailable) {
		t.Fatalf("落后的 schema 应返回 unavailable，实际 %v", err)
	}
}

func TestWithTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	manager := NewTxManager(db)

	err := manager.WithTx(ctx, func(txCtx context.Context) error {
		if !InTx(txCtx) {
			t.Fatal("事务上下文应被识别")
		}
		_, err := manager.Querier(txCtx).ExecContext(txCtx,
			`INSERT INTO trails (code, name, region, difficulty, distance_km, daily_quota, min_party_size,
				max_party_size, permit_cutoff_hours, status, version, created_at, updated_at)
			 VALUES ('COMMITTED', '提交线路', '北岭', 2, 10.0, 20, 2, 6, 6, 'open', 1, 1, 1)`)
		return err
	})
	if err != nil {
		t.Fatalf("事务提交失败: %v", err)
	}

	sentinel := errors.New("业务失败")
	err = manager.WithTx(ctx, func(txCtx context.Context) error {
		if _, execErr := manager.Querier(txCtx).ExecContext(txCtx,
			`INSERT INTO trails (code, name, region, difficulty, distance_km, daily_quota, min_party_size,
				max_party_size, permit_cutoff_hours, status, version, created_at, updated_at)
			 VALUES ('ROLLED-BACK', '回滚线路', '北岭', 2, 10.0, 20, 2, 6, 6, 'open', 1, 1, 1)`); execErr != nil {
			return execErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("业务错误应原样返回: %v", err)
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM trails`).Scan(&total); err != nil {
		t.Fatalf("统计线路失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("回滚后应只保留已提交的一条记录，实际 %d", total)
	}
}

func TestWithTxJoinsExistingTransaction(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	manager := NewTxManager(db)

	sentinel := errors.New("内层失败")
	err := manager.WithTx(ctx, func(outer context.Context) error {
		if _, execErr := manager.Querier(outer).ExecContext(outer,
			`INSERT INTO users (email, display_name, role, status, password_hash, password_salt, kdf_iterations, created_at, updated_at)
			 VALUES ('outer@trail.local', '外层', 'ranger', 'active', 'h', 's', 1000, 1, 1)`); execErr != nil {
			return execErr
		}
		return manager.WithTx(outer, func(inner context.Context) error {
			if _, execErr := manager.Querier(inner).ExecContext(inner,
				`INSERT INTO users (email, display_name, role, status, password_hash, password_salt, kdf_iterations, created_at, updated_at)
				 VALUES ('inner@trail.local', '内层', 'leader', 'active', 'h', 's', 1000, 1, 1)`); execErr != nil {
				return execErr
			}
			return sentinel
		})
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("嵌套事务应传播内层错误: %v", err)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&total); err != nil {
		t.Fatalf("统计账号失败: %v", err)
	}
	if total != 0 {
		t.Fatalf("嵌套事务失败应整体回滚，实际残留 %d 条", total)
	}
}

func TestForeignKeysAndPoolStats(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, user_agent, issued_at, expires_at, last_seen_at)
		 VALUES (999, 'orphan', '', 1, 2, 1)`)
	if err == nil {
		t.Fatal("外键约束应阻止孤立会话")
	}
	manager := NewTxManager(db)
	if stats := manager.Stats(); stats == "" {
		t.Fatal("连接池统计不应为空")
	}
	if manager.DB() != db {
		t.Fatal("事务管理器应暴露原始句柄")
	}
}

func TestOpenRejectsEmptyDSN(t *testing.T) {
	if _, err := Open(context.Background(), "  "); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("空 DSN 应返回 invalid_argument，实际 %v", err)
	}
}

func TestBuildDSNAppendsPragmasOnce(t *testing.T) {
	first := buildDSN("file:data/trail.sqlite")
	if !contains(first, "_txlock=immediate") || !contains(first, "_pragma=foreign_keys(1)") {
		t.Fatalf("默认 DSN 缺少必要参数: %s", first)
	}
	custom := buildDSN("file:data/trail.sqlite?_pragma=busy_timeout(100)&_txlock=deferred")
	if countOccurrences(custom, "_txlock=") != 1 {
		t.Fatalf("已显式设置的参数不应重复追加: %s", custom)
	}
	if countOccurrences(custom, "_pragma=busy_timeout") != 1 {
		t.Fatalf("已显式设置的 pragma 不应重复追加: %s", custom)
	}
}

func contains(haystack, needle string) bool {
	return countOccurrences(haystack, needle) > 0
}

func countOccurrences(haystack, needle string) int {
	count := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			count++
		}
	}
	return count
}
