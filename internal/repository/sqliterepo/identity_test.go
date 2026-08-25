package sqliterepo

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/storage/sqlite"
)

// fixture bundles a real database and the repositories under test.
type fixture struct {
	t           *testing.T
	db          *sql.DB
	dsn         string
	tx          *sqlite.TxManager
	users       *UserRepo
	sessions    *SessionRepo
	trails      *TrailRepo
	permits     *PermitRepo
	parties     *PartyRepo
	reports     *CheckpointReportRepo
	incidents   *IncidentRepo
	settlements *SettlementRepo
	audits      *AuditRepo
	jobs        *JobRepo
	idem        *IdempotencyRepo
}

// newFixture builds a migrated database in a temporary directory.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "repo.sqlite"))
	return openFixture(t, dsn)
}

// openFixture opens an existing or new database at the given DSN. Reopening the
// same DSN is how the restart recovery tests verify durability.
func openFixture(t *testing.T, dsn string) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	if _, err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	tx := sqlite.NewTxManager(db)
	fix := &fixture{
		t: t, db: db, dsn: dsn, tx: tx,
		users:       NewUserRepo(tx),
		sessions:    NewSessionRepo(tx),
		trails:      NewTrailRepo(tx),
		permits:     NewPermitRepo(tx),
		parties:     NewPartyRepo(tx),
		reports:     NewCheckpointReportRepo(tx),
		incidents:   NewIncidentRepo(tx),
		settlements: NewSettlementRepo(tx),
		audits:      NewAuditRepo(tx),
		jobs:        NewJobRepo(tx),
		idem:        NewIdempotencyRepo(tx),
	}
	t.Cleanup(func() { _ = db.Close() })
	return fix
}

func testNow() time.Time {
	return clock.Truncate(time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone()))
}

func (f *fixture) newUser(email string, role domain.Role) *domain.User {
	f.t.Helper()
	user := &domain.User{
		Email:        email,
		DisplayName:  "测试用户",
		Role:         role,
		Status:       domain.UserActive,
		PasswordHash: "hash",
		PasswordSalt: "salt",
		KDFIter:      1000,
		CreatedAt:    testNow(),
		UpdatedAt:    testNow(),
	}
	if _, err := f.users.Create(context.Background(), user); err != nil {
		f.t.Fatalf("创建账号失败: %v", err)
	}
	return user
}

func (f *fixture) newTrail(code string) *domain.Trail {
	f.t.Helper()
	trail := &domain.Trail{
		Code:              code,
		Name:              "测试线路",
		Region:            "北岭",
		Difficulty:        3,
		DistanceKM:        18.5,
		DailyQuota:        12,
		MinPartySize:      2,
		MaxPartySize:      6,
		PermitCutoffHours: 12,
		Status:            domain.TrailOpen,
		CreatedAt:         testNow(),
		UpdatedAt:         testNow(),
	}
	if _, err := f.trails.Create(context.Background(), trail); err != nil {
		f.t.Fatalf("创建线路失败: %v", err)
	}
	for seq, cutoff := range []int{90, 240, 420} {
		checkpoint := &domain.Checkpoint{
			TrailID:       trail.ID,
			Seq:           seq + 1,
			Name:          "打点",
			CutoffMinutes: cutoff,
			Mandatory:     seq != 1,
			CreatedAt:     testNow(),
		}
		if _, err := f.trails.AddCheckpoint(context.Background(), checkpoint); err != nil {
			f.t.Fatalf("创建打点失败: %v", err)
		}
	}
	return trail
}

func (f *fixture) newParty(code string, trail *domain.Trail, leaderID int64, size int) *domain.Party {
	f.t.Helper()
	return f.newPartyOn(code, trail, leaderID, size, "2026-09-12")
}

func (f *fixture) newPartyOn(code string, trail *domain.Trail, leaderID int64, size int, hikeDay string) *domain.Party {
	f.t.Helper()
	party := &domain.Party{
		Code:         code,
		TrailID:      trail.ID,
		LeaderID:     leaderID,
		HikeDay:      hikeDay,
		PlannedStart: testNow().Add(24 * time.Hour),
		PlannedEnd:   testNow().Add(34 * time.Hour),
		Size:         size,
		State:        domain.PartyDraft,
		ContactPhone: "13800001234",
		CreatedAt:    testNow(),
		UpdatedAt:    testNow(),
	}
	if _, err := f.parties.Create(context.Background(), party); err != nil {
		f.t.Fatalf("创建队伍失败: %v", err)
	}
	return party
}

func TestUserRepositoryLifecycle(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)

	user := fix.newUser("ranger@trail.local", domain.RoleRanger)
	if user.ID == 0 {
		t.Fatal("创建账号后应回写主键")
	}
	loaded, err := fix.users.ByEmail(ctx, "  RANGER@Trail.local ")
	if err != nil {
		t.Fatalf("按邮箱读取失败: %v", err)
	}
	if loaded.ID != user.ID || loaded.Role != domain.RoleRanger {
		t.Fatalf("读取到的账号不一致: %+v", loaded)
	}
	if loaded.CreatedAt.IsZero() {
		t.Fatal("创建时间应被持久化")
	}

	duplicate := &domain.User{
		Email: "ranger@trail.local", DisplayName: "重复", Role: domain.RoleLeader,
		Status: domain.UserActive, PasswordHash: "h", PasswordSalt: "s", KDFIter: 1000,
		CreatedAt: testNow(), UpdatedAt: testNow(),
	}
	if _, err := fix.users.Create(ctx, duplicate); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复邮箱应返回 conflict，实际 %v", err)
	}

	if err := fix.users.UpdateCredential(ctx, user.ID, "newhash", "newsalt", 5000, testNow()); err != nil {
		t.Fatalf("更新凭据失败: %v", err)
	}
	refreshed, err := fix.users.ByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if refreshed.PasswordHash != "newhash" || refreshed.KDFIter != 5000 {
		t.Fatalf("凭据未更新: %+v", refreshed)
	}
	if err := fix.users.UpdateStatus(ctx, user.ID, domain.UserSuspended, testNow()); err != nil {
		t.Fatalf("更新状态失败: %v", err)
	}
	suspended, err := fix.users.ByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if suspended.Active() {
		t.Fatal("停用账号不应判定为活跃")
	}
	if err := fix.users.UpdateStatus(ctx, 9999, domain.UserActive, testNow()); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("更新不存在账号应返回 not_found，实际 %v", err)
	}
	if _, err := fix.users.ByID(ctx, 9999); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("读取不存在账号应返回 not_found，实际 %v", err)
	}

	fix.newUser("leader@trail.local", domain.RoleLeader)
	rangers, err := fix.users.CountByRole(ctx, domain.RoleRanger)
	if err != nil {
		t.Fatalf("统计角色失败: %v", err)
	}
	if rangers != 1 {
		t.Fatalf("线路管理员数量应为 1，实际 %d", rangers)
	}
}

func TestSessionRepositoryRevocationAndExpiry(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	user := fix.newUser("leader@trail.local", domain.RoleLeader)
	now := testNow()

	active := &domain.Session{
		UserID: user.ID, TokenHash: "hash-active", UserAgent: "curl",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}
	if _, err := fix.sessions.Create(ctx, active); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	expired := &domain.Session{
		UserID: user.ID, TokenHash: "hash-expired",
		IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), LastSeenAt: now.Add(-2 * time.Hour),
	}
	if _, err := fix.sessions.Create(ctx, expired); err != nil {
		t.Fatalf("创建过期会话失败: %v", err)
	}

	count, err := fix.sessions.CountActive(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("统计活跃会话失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("活跃会话应为 1，实际 %d", count)
	}

	loaded, err := fix.sessions.ByTokenHash(ctx, "hash-active")
	if err != nil {
		t.Fatalf("按令牌哈希读取失败: %v", err)
	}
	if !loaded.Active(now) {
		t.Fatal("刚创建的会话应可用")
	}
	if err := fix.sessions.TouchLastSeen(ctx, loaded.ID, now.Add(10*time.Minute)); err != nil {
		t.Fatalf("更新活跃时间失败: %v", err)
	}
	touched, err := fix.sessions.ByID(ctx, loaded.ID)
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if !touched.LastSeenAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("活跃时间未更新: %s", touched.LastSeenAt)
	}

	revoked, err := fix.sessions.Revoke(ctx, loaded.ID, now.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("撤销会话失败: %v", err)
	}
	if !revoked {
		t.Fatal("首次撤销应返回 true")
	}
	again, err := fix.sessions.Revoke(ctx, loaded.ID, now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("重复撤销失败: %v", err)
	}
	if again {
		t.Fatal("重复撤销应返回 false")
	}

	swept, err := fix.sessions.RevokeExpired(ctx, now)
	if err != nil {
		t.Fatalf("清理过期会话失败: %v", err)
	}
	if swept != 1 {
		t.Fatalf("应清理 1 个过期会话，实际 %d", swept)
	}
	if _, err := fix.sessions.ByTokenHash(ctx, "missing"); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("未知令牌应返回 not_found，实际 %v", err)
	}

	fresh := &domain.Session{
		UserID: user.ID, TokenHash: "hash-fresh",
		IssuedAt: now, ExpiresAt: now.Add(2 * time.Hour), LastSeenAt: now,
	}
	if _, err := fix.sessions.Create(ctx, fresh); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	all, err := fix.sessions.RevokeAllForUser(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("撤销全部会话失败: %v", err)
	}
	if all != 1 {
		t.Fatalf("应撤销 1 个剩余会话，实际 %d", all)
	}
}

func TestTrailRepositoryListingSortingAndVersionGuard(t *testing.T) {
	ctx := context.Background()
	fix := newFixture(t)
	first := fix.newTrail("AOMEN-RIDGE")
	second := fix.newTrail("QINGXI-VALLEY")

	page, err := domain.NewPageRequest(1, 1, "code", true, TrailSortKeys())
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	result, err := fix.trails.List(ctx, "", domain.TrailOpen, page)
	if err != nil {
		t.Fatalf("查询线路失败: %v", err)
	}
	if result.Total != 2 || len(result.Items) != 1 {
		t.Fatalf("分页结果错误: total=%d items=%d", result.Total, len(result.Items))
	}
	if result.Items[0].Code != second.Code {
		t.Fatalf("倒序排序应先返回 %s，实际 %s", second.Code, result.Items[0].Code)
	}
	if result.TotalPages != 2 {
		t.Fatalf("总页数应为 2，实际 %d", result.TotalPages)
	}

	filtered, err := fix.trails.List(ctx, "不存在的区域", "", page)
	if err != nil {
		t.Fatalf("查询线路失败: %v", err)
	}
	if filtered.Total != 0 || len(filtered.Items) != 0 {
		t.Fatalf("区域过滤失效: %+v", filtered)
	}

	if err := fix.trails.UpdateStatus(ctx, first.ID, first.Version, domain.TrailSuspended, testNow()); err != nil {
		t.Fatalf("更新线路状态失败: %v", err)
	}
	err = fix.trails.UpdateStatus(ctx, first.ID, first.Version, domain.TrailOpen, testNow())
	if !apperr.Is(err, apperr.CodeVersionConflict) {
		t.Fatalf("过期版本号应返回 version_conflict，实际 %v", err)
	}
	updated, err := fix.trails.ByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("读取线路失败: %v", err)
	}
	if updated.Status != domain.TrailSuspended || updated.Version != first.Version+1 {
		t.Fatalf("状态或版本未按预期更新: %+v", updated)
	}

	checkpoints, err := fix.trails.Checkpoints(ctx, first.ID)
	if err != nil {
		t.Fatalf("读取打点失败: %v", err)
	}
	if len(checkpoints) != 3 {
		t.Fatalf("打点数量应为 3，实际 %d", len(checkpoints))
	}
	if checkpoints[0].Seq != 1 || checkpoints[2].Seq != 3 {
		t.Fatalf("打点应按序号升序返回: %+v", checkpoints)
	}
	if checkpoints[1].Mandatory {
		t.Fatal("第二个打点在夹具中应为非必经")
	}
	single, err := fix.trails.CheckpointBySeq(ctx, first.ID, 2)
	if err != nil {
		t.Fatalf("按序号读取打点失败: %v", err)
	}
	if single.CutoffMinutes != 240 {
		t.Fatalf("打点截止分钟数错误: %d", single.CutoffMinutes)
	}
	if _, err := fix.trails.CheckpointBySeq(ctx, first.ID, 9); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的打点应返回 not_found，实际 %v", err)
	}
	if _, err := fix.trails.ByCode(ctx, "MISSING"); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的线路应返回 not_found，实际 %v", err)
	}
}

func TestContextCancellationIsTranslated(t *testing.T) {
	fix := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fix.users.CountByRole(ctx, domain.RoleRanger)
	if !apperr.Is(err, apperr.CodeDeadlineExceeded) {
		t.Fatalf("已取消的上下文应返回 deadline_exceeded，实际 %s (%v)", apperr.CodeOf(err), err)
	}
}
