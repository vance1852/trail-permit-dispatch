package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/auth"
)

func TestSessionExpiryAndSweep(t *testing.T) {
	h := newHarness(t)
	token := h.token(leaderEmail, leaderPassword)

	if _, err := h.app.Auth.Authenticate(h.ctx(), token); err != nil {
		t.Fatalf("新签发令牌应可用: %v", err)
	}

	// 会话有效期为 72 小时，推进 73 小时后必须失效。
	h.clock.Advance(73 * time.Hour)
	if _, err := h.app.Auth.Authenticate(h.ctx(), token); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("过期令牌应返回 unauthenticated，实际 %v", err)
	}

	revoked, err := h.app.Auth.ExpireSessions(h.ctx())
	if err != nil {
		t.Fatalf("清理过期会话失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应清理 1 个过期会话，实际 %d", revoked)
	}
	again, err := h.app.Auth.ExpireSessions(h.ctx())
	if err != nil {
		t.Fatalf("重复清理失败: %v", err)
	}
	if again != 0 {
		t.Fatalf("重复清理不应再撤销会话，实际 %d", again)
	}

	fresh := h.token(leaderEmail, leaderPassword)
	actor, err := h.app.Auth.Authenticate(h.ctx(), fresh)
	if err != nil {
		t.Fatalf("重新登录后应可用: %v", err)
	}
	active, err := h.app.Auth.ActiveSessions(h.ctx(), actor)
	if err != nil {
		t.Fatalf("统计活跃会话失败: %v", err)
	}
	if active != 1 {
		t.Fatalf("活跃会话应为 1，实际 %d", active)
	}
}

func TestLogoutAndRevokeAllSessions(t *testing.T) {
	h := newHarness(t)
	firstToken := h.token(leaderEmail, leaderPassword)
	secondToken := h.token(leaderEmail, leaderPassword)

	firstActor, err := h.app.Auth.Authenticate(h.ctx(), firstToken)
	if err != nil {
		t.Fatalf("解析会话失败: %v", err)
	}
	if err := h.app.Auth.Logout(h.ctx(), firstActor); err != nil {
		t.Fatalf("退出失败: %v", err)
	}
	if _, err := h.app.Auth.Authenticate(h.ctx(), firstToken); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("退出后令牌应失效，实际 %v", err)
	}
	if err := h.app.Auth.Logout(h.ctx(), firstActor); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复退出应返回 state_invalid，实际 %v", err)
	}

	secondActor, err := h.app.Auth.Authenticate(h.ctx(), secondToken)
	if err != nil {
		t.Fatalf("另一会话应仍可用: %v", err)
	}
	revoked, err := h.app.Auth.RevokeAll(h.ctx(), secondActor)
	if err != nil {
		t.Fatalf("撤销全部会话失败: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("应撤销 1 个剩余会话，实际 %d", revoked)
	}
	if _, err := h.app.Auth.Authenticate(h.ctx(), secondToken); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("撤销后令牌应失效，实际 %v", err)
	}
	if err := h.app.Auth.Logout(h.ctx(), domain.Actor{UserID: secondActor.UserID}); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("缺少会话标识时应返回 unauthenticated，实际 %v", err)
	}
	if _, err := h.app.Auth.RevokeAll(h.ctx(), domain.Actor{}); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("匿名撤销应被拒绝，实际 %v", err)
	}
}

func TestAccountProvisioningRules(t *testing.T) {
	h := newHarness(t)
	ranger := h.ranger()
	leader := h.leader()

	if _, err := h.app.Auth.Register(h.ctx(), leader, auth.RegisterInput{
		Email: "shadow@trailpermit.test", DisplayName: "越权建号",
		Password: "shadow-pass-2026", Role: "ranger",
	}); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("领队不应创建账号，实际 %v", err)
	}

	created, err := h.app.Auth.Register(h.ctx(), ranger, auth.RegisterInput{
		Email: "  Backup.Ranger@TrailPermit.test ", DisplayName: "备班值守员",
		Password: "backup-pass-2026", Role: "RANGER",
	})
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	if created.Email != "backup.ranger@trailpermit.test" {
		t.Fatalf("邮箱应被归一化，实际 %q", created.Email)
	}
	if created.Role != string(domain.RoleRanger) || created.Status != string(domain.UserActive) {
		t.Fatalf("新账号角色或状态错误: %+v", created)
	}

	if _, err := h.app.Auth.Register(h.ctx(), ranger, auth.RegisterInput{
		Email: "backup.ranger@trailpermit.test", DisplayName: "重复",
		Password: "backup-pass-2026", Role: "ranger",
	}); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复邮箱应返回 conflict，实际 %v", err)
	}
	if _, err := h.app.Auth.Register(h.ctx(), ranger, auth.RegisterInput{
		Email: "bad-role@trailpermit.test", DisplayName: "未知角色",
		Password: "role-pass-2026", Role: "volunteer",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知角色应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Auth.Register(h.ctx(), ranger, auth.RegisterInput{
		Email: "weak@trailpermit.test", DisplayName: "弱密码",
		Password: "weakpass", Role: "leader",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("弱密码应被拒绝，实际 %v", err)
	}

	rangers, err := h.app.Auth.CountRole(h.ctx(), domain.RoleRanger)
	if err != nil {
		t.Fatalf("统计角色失败: %v", err)
	}
	if rangers != 2 {
		t.Fatalf("线路管理员数量应为 2，实际 %d", rangers)
	}

	if _, err := h.app.Auth.Login(h.ctx(), "backup.ranger@trailpermit.test", "backup-pass-2026", "go-test"); err != nil {
		t.Fatalf("新账号应可登录: %v", err)
	}
	if _, err := h.app.Auth.Login(h.ctx(), "backup.ranger@trailpermit.test", "wrong-pass-1", "go-test"); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("错误密码应返回 unauthenticated，实际 %v", err)
	}
	if _, err := h.app.Auth.Login(h.ctx(), "ghost@trailpermit.test", "ghost-pass-2026", "go-test"); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("未知账号应返回 unauthenticated，实际 %v", err)
	}
	if _, err := h.app.Auth.Login(h.ctx(), "not-an-email", "whatever2026", "go-test"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法邮箱应返回 invalid_argument，实际 %v", err)
	}
	if _, err := h.app.Auth.Login(h.ctx(), leaderEmail, "", "go-test"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("空密码应返回 invalid_argument，实际 %v", err)
	}
	if _, err := h.app.Auth.Authenticate(h.ctx(), ""); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("空令牌应返回 unauthenticated，实际 %v", err)
	}
	if _, err := h.app.Auth.Account(h.ctx(), domain.Actor{}); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("匿名读取账号应被拒绝，实际 %v", err)
	}
	account, err := h.app.Auth.Account(h.ctx(), ranger)
	if err != nil {
		t.Fatalf("读取账号失败: %v", err)
	}
	if account.Email != rangerEmail {
		t.Fatalf("账号信息错误: %+v", account)
	}
}

func TestSeedIsIdempotentAndSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	result, err := h.app.Seed(h.ctx())
	if err != nil {
		t.Fatalf("重复初始化失败: %v", err)
	}
	if !result.Skipped {
		t.Fatalf("已有数据时初始化应跳过: %+v", result)
	}

	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if err := h.app.Close(); err != nil {
		t.Fatalf("关闭应用失败: %v", err)
	}

	restarted := openHarness(t, h.dsn)
	rangers, err := restarted.app.Auth.CountRole(restarted.ctx(), domain.RoleRanger)
	if err != nil {
		t.Fatalf("重启后统计角色失败: %v", err)
	}
	if rangers != 1 {
		t.Fatalf("重启后不应重复创建账号，实际 %d", rangers)
	}
	trails, err := restarted.app.Catalog.TrailCount(restarted.ctx())
	if err != nil {
		t.Fatalf("重启后统计线路失败: %v", err)
	}
	if trails != 2 {
		t.Fatalf("重启后不应重复创建线路，实际 %d", trails)
	}
	restored, err := restarted.app.Dispatch.GetParty(restarted.ctx(), restarted.ranger(), party.Code)
	if err != nil {
		t.Fatalf("重启后读取队伍失败: %v", err)
	}
	if restored.State != string(domain.PartyPermitReserved) {
		t.Fatalf("重启后队伍状态丢失，实际 %s", restored.State)
	}
	if occupied, _ := restarted.permitWindow(valleyTrail, 1); occupied != 2 {
		t.Fatalf("重启后占用名额丢失，实际 %d", occupied)
	}

	report, err := restarted.app.Ready(restarted.ctx())
	if err != nil {
		t.Fatalf("重启后就绪检查失败: %v", err)
	}
	if report.Status != "ready" || report.SchemaVersion != 3 {
		t.Fatalf("重启后就绪检查结果错误: %+v", report)
	}
}

func TestWorkerRunsRecurringJobsAndRecoversLeases(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.dispatchParty(leader, ranger, valleyTrail, 2, 1)

	if err := h.app.StartWorker(h.ctx()); err != nil {
		t.Fatalf("启动后台执行器失败: %v", err)
	}
	h.clock.Set(h.plannedStart(1, 6).Add(2 * time.Hour))

	deadline := time.Now().Add(5 * time.Second)
	var opened int
	for time.Now().Before(deadline) {
		count, err := h.app.Incidents.CountOpen(h.ctx(), mustPartyID(t, h, party.Code))
		if err != nil {
			t.Fatalf("统计未处置事故失败: %v", err)
		}
		if count > 0 {
			opened = count
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.app.Worker.Stop()
	if opened == 0 {
		t.Fatal("后台执行器应在打点超时后登记事故")
	}

	pending, err := h.app.Worker.PendingSummary(h.ctx())
	if err != nil {
		t.Fatalf("读取队列深度失败: %v", err)
	}
	if _, ok := pending[domain.JobSweepOverdue]; !ok {
		t.Fatalf("周期性扫描作业应持续排期: %+v", pending)
	}
}

// mustPartyID resolves the internal identifier of a party code.
func mustPartyID(t *testing.T, h *harness, code string) int64 {
	t.Helper()
	id, err := h.app.Dispatch.PartyIDByCode(h.ctx(), code)
	if err != nil {
		t.Fatalf("解析队伍标识失败: %v", err)
	}
	return id
}
