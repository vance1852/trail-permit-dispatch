package domain

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestPartyStateMachineAllowsOnlyPlannedRoutes(t *testing.T) {
	allowed := []struct {
		from PartyState
		to   PartyState
	}{
		{PartyDraft, PartyPermitReserved},
		{PartyDraft, PartyCancelled},
		{PartyPermitReserved, PartyConfirmed},
		{PartyPermitReserved, PartyCancelled},
		{PartyConfirmed, PartyOnTrail},
		{PartyConfirmed, PartyAborted},
		{PartyOnTrail, PartyCompleted},
		{PartyOnTrail, PartyAborted},
	}
	for _, transition := range allowed {
		if err := ValidatePartyTransition(transition.from, transition.to); err != nil {
			t.Fatalf("期望 %s -> %s 合法，实际返回 %v", transition.from, transition.to, err)
		}
	}

	rejected := []struct {
		from PartyState
		to   PartyState
	}{
		{PartyDraft, PartyConfirmed},
		{PartyDraft, PartyOnTrail},
		{PartyPermitReserved, PartyOnTrail},
		{PartyOnTrail, PartyCancelled},
		{PartyCompleted, PartyOnTrail},
		{PartyCancelled, PartyPermitReserved},
		{PartyAborted, PartyCompleted},
	}
	for _, transition := range rejected {
		err := ValidatePartyTransition(transition.from, transition.to)
		if err == nil {
			t.Fatalf("期望 %s -> %s 被拒绝", transition.from, transition.to)
		}
		if !apperr.Is(err, apperr.CodeStateInvalid) {
			t.Fatalf("非法转换应返回 state_invalid，实际 %s", apperr.CodeOf(err))
		}
	}
}

func TestPartyTransitionRejectsSameStateAndUnknownState(t *testing.T) {
	if err := ValidatePartyTransition(PartyOnTrail, PartyOnTrail); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复进入同一状态应返回 state_invalid，实际 %v", err)
	}
	if err := ValidatePartyTransition(PartyDraft, PartyState("teleported")); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知目标状态应返回 invalid_argument，实际 %v", err)
	}
	if err := ValidatePartyTransition(PartyState("ghost"), PartyDraft); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("未知来源状态应返回 internal，实际 %v", err)
	}
}

func TestPartyStateTerminalAndPermitHolding(t *testing.T) {
	terminal := []PartyState{PartyCompleted, PartyCancelled, PartyAborted}
	for _, state := range terminal {
		if !state.Terminal() {
			t.Fatalf("%s 应为终态", state)
		}
		if state.HoldsPermit() {
			t.Fatalf("终态 %s 不应继续占用许可名额", state)
		}
	}
	holding := []PartyState{PartyPermitReserved, PartyConfirmed, PartyOnTrail}
	for _, state := range holding {
		if !state.HoldsPermit() {
			t.Fatalf("%s 应占用许可名额", state)
		}
		if state.Terminal() {
			t.Fatalf("%s 不应为终态", state)
		}
	}
	if PartyDraft.HoldsPermit() {
		t.Fatal("草稿状态不应占用许可名额")
	}
}

func TestEnsureLeaderProtectsOtherParties(t *testing.T) {
	party := &Party{LeaderID: 7}
	if err := party.EnsureLeader(7); err != nil {
		t.Fatalf("本人领队应通过校验，实际 %v", err)
	}
	err := party.EnsureLeader(8)
	if !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人操作应返回 permission_denied，实际 %v", err)
	}
	var missing *Party
	if err := missing.EnsureLeader(1); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("空队伍应返回 not_found，实际 %v", err)
	}
}

func TestValidateScheduleBoundaries(t *testing.T) {
	start := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	if err := ValidateSchedule(start, start.Add(10*time.Hour)); err != nil {
		t.Fatalf("合法计划时间被拒绝: %v", err)
	}
	if err := ValidateSchedule(start, start); err == nil {
		t.Fatal("返回时间等于出发时间应被拒绝")
	}
	if err := ValidateSchedule(start, start.Add(-time.Hour)); err == nil {
		t.Fatal("返回时间早于出发时间应被拒绝")
	}
	if err := ValidateSchedule(start, start.Add(73*time.Hour)); err == nil {
		t.Fatal("超过 72 小时的行程应被拒绝")
	}
	if err := ValidateSchedule(time.Time{}, start); err == nil {
		t.Fatal("空出发时间应被拒绝")
	}
}

func TestValidateMemberChecksEveryField(t *testing.T) {
	valid := PartyMember{
		MemberRef:   "HK-20260912-001",
		DisplayName: "周岚",
		Kind:        MemberCompanion,
		Phone:       "13800001234",
	}
	if err := ValidateMember(valid); err != nil {
		t.Fatalf("合法队员被拒绝: %v", err)
	}

	shortRef := valid
	shortRef.MemberRef = "ab"
	if field := apperr.FieldOf(ValidateMember(shortRef)); field != "member_ref" {
		t.Fatalf("过短编号应定位到 member_ref，实际 %q", field)
	}

	badPhone := valid
	badPhone.Phone = "13800"
	if field := apperr.FieldOf(ValidateMember(badPhone)); field != "phone" {
		t.Fatalf("非法电话应定位到 phone，实际 %q", field)
	}

	badPhoneChars := valid
	badPhoneChars.Phone = "138-abc-0000"
	if err := ValidateMember(badPhoneChars); err == nil {
		t.Fatal("包含字母的电话应被拒绝")
	}

	badKind := valid
	badKind.Kind = MemberKind("guide")
	if err := ValidateMember(badKind); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知队员类型应被拒绝，实际 %v", err)
	}

	emptyName := valid
	emptyName.DisplayName = "   "
	if field := apperr.FieldOf(ValidateMember(emptyName)); field != "display_name" {
		t.Fatalf("空姓名应定位到 display_name，实际 %q", field)
	}
}

func TestEvaluateReadinessCountsWaivers(t *testing.T) {
	members := []PartyMember{
		{MemberRef: "a", WaiverSigned: true},
		{MemberRef: "b", WaiverSigned: false},
		{MemberRef: "c", WaiverSigned: true},
	}
	report := EvaluateReadiness(3, members)
	if report.Registered != 3 || report.Required != 3 {
		t.Fatalf("登记人数统计错误: %+v", report)
	}
	if report.WaiverMissing != 1 {
		t.Fatalf("未签署免责人数应为 1，实际 %d", report.WaiverMissing)
	}
	if report.Ready {
		t.Fatal("仍有队员未签署免责声明时不应判定为齐备")
	}

	members[1].WaiverSigned = true
	if ready := EvaluateReadiness(3, members); !ready.Ready {
		t.Fatalf("全部签署且人数齐备时应判定齐备: %+v", ready)
	}
	if partial := EvaluateReadiness(4, members); partial.Ready {
		t.Fatal("人数不足计划规模时不应判定齐备")
	}
}

func TestRoleCapabilitiesAreSeparated(t *testing.T) {
	if !RoleRanger.CanGovernPermits() || RoleLeader.CanGovernPermits() {
		t.Fatal("许可治理能力应只属于线路管理员")
	}
	if !RoleLeader.CanLeadParties() || RoleRanger.CanLeadParties() {
		t.Fatal("带队能力应只属于领队")
	}
	if !RoleRanger.CanReadAudit() || RoleLeader.CanReadAudit() {
		t.Fatal("审计读取应只属于线路管理员")
	}
	if _, err := ParseRole("volunteer"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知角色应被拒绝，实际 %v", err)
	}
	role, err := ParseRole("  RANGER ")
	if err != nil || role != RoleRanger {
		t.Fatalf("角色解析应忽略大小写与空白，实际 %v %v", role, err)
	}
}

func TestActorRequireRole(t *testing.T) {
	anonymous := Actor{}
	if err := anonymous.RequireRole(RoleLeader); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("匿名调用应返回 unauthenticated，实际 %v", err)
	}
	leader := Actor{UserID: 3, Role: RoleLeader}
	if err := leader.RequireRole(RoleRanger); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("角色不匹配应返回 permission_denied，实际 %v", err)
	}
	if err := leader.RequireRole(RoleLeader); err != nil {
		t.Fatalf("角色匹配应通过，实际 %v", err)
	}
}

func TestSessionLifecycleFlags(t *testing.T) {
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	session := &Session{IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	if !session.Active(now) {
		t.Fatal("新签发会话应可用")
	}
	if session.Active(now.Add(time.Hour)) {
		t.Fatal("到达过期时间的会话不应可用")
	}
	revokedAt := now.Add(10 * time.Minute)
	session.RevokedAt = &revokedAt
	if session.Active(now.Add(20 * time.Minute)) {
		t.Fatal("已撤销会话不应可用")
	}
	if !session.Revoked() || !session.Expired(now.Add(2*time.Hour)) {
		t.Fatal("撤销与过期标记计算错误")
	}
}

func TestValidatePasswordAndEmailRules(t *testing.T) {
	if err := ValidatePassword("short1"); err == nil {
		t.Fatal("过短密码应被拒绝")
	}
	if err := ValidatePassword("onlyletterspassword"); err == nil {
		t.Fatal("缺少数字的密码应被拒绝")
	}
	if err := ValidatePassword("1234567890123"); err == nil {
		t.Fatal("缺少字母的密码应被拒绝")
	}
	if err := ValidatePassword("trailpermit2026"); err != nil {
		t.Fatalf("合法密码被拒绝: %v", err)
	}
	if got := NormalizeEmail("  Leader@Trail.LOCAL "); got != "leader@trail.local" {
		t.Fatalf("邮箱归一化错误: %q", got)
	}
	for _, invalid := range []string{"", "leader", "leader@", "@trail.local", "a@b@c.local", "leader@local"} {
		if err := ValidateEmail(invalid); err == nil {
			t.Fatalf("非法邮箱 %q 应被拒绝", invalid)
		}
	}
}
