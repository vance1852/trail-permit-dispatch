package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

func TestPartyLifecycleFromPlanToSettlement(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()

	party := h.readyParty(leader, valleyTrail, 3, 1)
	if party.State != string(domain.PartyDraft) {
		t.Fatalf("新建队伍应为草稿状态，实际 %s", party.State)
	}
	if !party.Readiness.Ready {
		t.Fatalf("补齐队员后应判定齐备: %+v", party.Readiness)
	}

	reserved, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "idem-lifecycle")
	if err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if reserved.State != string(domain.PartyPermitReserved) {
		t.Fatalf("申请许可后状态应为 permit_reserved，实际 %s", reserved.State)
	}
	if reserved.Permit == nil || reserved.Permit.Reserved != 3 {
		t.Fatalf("许可窗口占用未生效: %+v", reserved.Permit)
	}
	if occupied, total := h.permitWindow(valleyTrail, 1); occupied != 3 || total != 30 {
		t.Fatalf("许可窗口统计错误: reserved=%d total=%d", occupied, total)
	}

	settlement, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if settlement.State != string(domain.SettlementPending) {
		t.Fatalf("申请许可后应生成待结算记录，实际 %s", settlement.State)
	}
	// 2026-09-12 是周六，许可费用必须包含周末上浮。
	dayStart, err := clock.ParseHikeDay(h.hikeDay(1))
	if err != nil {
		t.Fatalf("解析出行日失败: %v", err)
	}
	if !clock.IsWeekend(dayStart) {
		t.Fatal("测试数据要求出行日落在周末")
	}
	expected := domain.PermitFee(4500, 2, 3, true)
	if settlement.TotalCents != expected {
		t.Fatalf("许可费用计算错误: 期望 %d 实际 %d", expected, settlement.TotalCents)
	}

	approved, err := h.app.Dispatch.ApproveParty(h.ctx(), ranger, party.Code)
	if err != nil {
		t.Fatalf("复核许可失败: %v", err)
	}
	if approved.State != string(domain.PartyConfirmed) {
		t.Fatalf("复核后状态应为 confirmed，实际 %s", approved.State)
	}
	dispatched, err := h.app.Dispatch.DispatchParty(h.ctx(), ranger, party.Code)
	if err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	if dispatched.State != string(domain.PartyOnTrail) || dispatched.DispatchedAt == nil {
		t.Fatalf("放行结果错误: %+v", dispatched)
	}

	// 出发后 30 分钟上报第一个打点，远早于 60 分钟的截止时间。
	h.clock.Set(h.plannedStart(1, 6).Add(30 * time.Minute))
	first, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 1, HeadCount: 3})
	if err != nil {
		t.Fatalf("上报第一个打点失败: %v", err)
	}
	if len(first.Reports) != 1 || first.Reports[0].Status != string(domain.ReportOnTime) {
		t.Fatalf("第一个打点应判定按时: %+v", first.Reports)
	}

	// 第三个打点截止为 300 分钟，310 分钟落在 30 分钟宽限期内，应判定迟到。
	h.clock.Set(h.plannedStart(1, 6).Add(310 * time.Minute))
	late, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 3, HeadCount: 3, Note: "谷尾出口"})
	if err != nil {
		t.Fatalf("上报第三个打点失败: %v", err)
	}
	if late.Reports[len(late.Reports)-1].Status != string(domain.ReportLate) {
		t.Fatalf("宽限期内的打点应判定迟到: %+v", late.Reports)
	}

	completed, err := h.app.Dispatch.CompleteParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("结束行程失败: %v", err)
	}
	if completed.State != string(domain.PartyCompleted) || completed.ClosedAt == nil {
		t.Fatalf("结束行程结果错误: %+v", completed)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 0 {
		t.Fatalf("结束行程后应释放许可名额，实际仍占用 %d", occupied)
	}

	if processed := h.drainJobs(10); processed == 0 {
		t.Fatal("完成行程后应有后台作业被执行")
	}
	settled, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if settled.State != string(domain.SettlementSettled) {
		t.Fatalf("后台作业执行后应完成结算，实际 %s", settled.State)
	}
	if settled.Reference == "" || settled.SettledAt == nil {
		t.Fatalf("结算凭证缺失: %+v", settled)
	}
}

func TestCompleteRequiresEveryMandatoryCheckpoint(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()

	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.ApproveParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("复核失败: %v", err)
	}
	if _, err := h.app.Dispatch.DispatchParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	h.clock.Set(h.plannedStart(1, 6).Add(30 * time.Minute))
	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 1, HeadCount: 2}); err != nil {
		t.Fatalf("上报打点失败: %v", err)
	}

	_, err := h.app.Dispatch.CompleteParty(h.ctx(), leader, party.Code)
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("必经打点未上报时结束行程应被拒绝，实际 %v", err)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 2 {
		t.Fatalf("结束行程被拒绝后不应释放名额，实际 %d", occupied)
	}
}

func TestCheckpointReportOrderAndDuplicateGuards(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}

	_, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 1, HeadCount: 2})
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("尚未放行的队伍不应上报打点，实际 %v", err)
	}

	if _, err := h.app.Dispatch.ApproveParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("复核失败: %v", err)
	}
	if _, err := h.app.Dispatch.DispatchParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	h.clock.Set(h.plannedStart(1, 6).Add(20 * time.Minute))

	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 2, HeadCount: 2}); err != nil {
		t.Fatalf("上报第二个打点失败: %v", err)
	}
	_, err = h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 1, HeadCount: 2})
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("回退上报更早的打点应被拒绝，实际 %v", err)
	}
	_, err = h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 2, HeadCount: 2})
	if !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复上报同一打点应返回 conflict，实际 %v", err)
	}
	_, err = h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 9, HeadCount: 2})
	if !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("不存在的打点应返回 not_found，实际 %v", err)
	}
	_, err = h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 3, HeadCount: 9})
	if !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("上报人数超过队伍规模应被拒绝，实际 %v", err)
	}
}

func TestMemberRegistrationReportsPartialFailure(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.createParty(leader, valleyTrail, 3, 1)

	result, err := h.app.Dispatch.RegisterMembers(h.ctx(), leader, party.Code, []dispatch.MemberInput{
		{MemberRef: "HK-001", DisplayName: "周岚", Phone: "13900000001", WaiverSigned: true},
		{MemberRef: "HK-001", DisplayName: "重复编号", Phone: "13900000002", WaiverSigned: true},
		{MemberRef: "no", DisplayName: "编号过短", Phone: "13900000003", WaiverSigned: true},
		{MemberRef: "HK-004", DisplayName: "电话非法", Phone: "abc", WaiverSigned: true},
		{MemberRef: "HK-005", DisplayName: "程野", Phone: "13900000005", WaiverSigned: false},
	})
	if err != nil {
		t.Fatalf("批量登记调用失败: %v", err)
	}
	if result.Accepted != 2 || result.Rejected != 3 {
		t.Fatalf("批量结果统计错误: accepted=%d rejected=%d", result.Accepted, result.Rejected)
	}
	if len(result.Outcomes) != 5 {
		t.Fatalf("必须逐项返回结果，实际 %d 项", len(result.Outcomes))
	}
	if result.Outcomes[1].Code != string(apperr.CodeConflict) {
		t.Fatalf("重复编号应返回 conflict，实际 %+v", result.Outcomes[1])
	}
	if result.Outcomes[2].Code != string(apperr.CodeInvalidArgument) {
		t.Fatalf("非法编号应返回 invalid_argument，实际 %+v", result.Outcomes[2])
	}
	if result.Party.Readiness.Registered != 3 {
		t.Fatalf("成功项应被保留: %+v", result.Party.Readiness)
	}
	if result.Party.Readiness.WaiverMissing != 1 || result.Party.Readiness.Ready {
		t.Fatalf("未签署免责的队员应阻断齐备判定: %+v", result.Party.Readiness)
	}

	_, err = h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "")
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("未齐备的队伍不应申请到许可，实际 %v", err)
	}

	overflow, err := h.app.Dispatch.RegisterMembers(h.ctx(), leader, party.Code, []dispatch.MemberInput{
		{MemberRef: "HK-006", DisplayName: "超额", Phone: "13900000006", WaiverSigned: true},
	})
	if err != nil {
		t.Fatalf("批量登记调用失败: %v", err)
	}
	if overflow.Accepted != 0 || overflow.Outcomes[0].Code != string(apperr.CodeConflict) {
		t.Fatalf("超过计划规模应被拒绝: %+v", overflow.Outcomes)
	}
	if _, err := h.app.Dispatch.RegisterMembers(h.ctx(), leader, party.Code, nil); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("空队员列表应被拒绝，实际 %v", err)
	}
}

func TestCancelReleasesQuotaAndWaivesFee(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 4, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 4 {
		t.Fatalf("占用名额应为 4，实际 %d", occupied)
	}

	cancelled, err := h.app.Dispatch.CancelParty(h.ctx(), leader, party.Code, "临时天气预警")
	if err != nil {
		t.Fatalf("取消出行失败: %v", err)
	}
	if cancelled.State != string(domain.PartyCancelled) {
		t.Fatalf("取消后状态错误: %s", cancelled.State)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 0 {
		t.Fatalf("取消后应释放全部名额，实际 %d", occupied)
	}
	settlement, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算失败: %v", err)
	}
	if settlement.State != string(domain.SettlementWaived) {
		t.Fatalf("取消后待结算费用应减免，实际 %s", settlement.State)
	}
	if _, err := h.app.Dispatch.CancelParty(h.ctx(), leader, party.Code, "重复取消"); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复取消应被拒绝，实际 %v", err)
	}
}

// TestCancelOnlyReleasesThatPartysSeats reproduces the Qingxi valley incident:
// two parties each held 4 seats (8 total); cancelling one must release only
// its own 4 seats and leave the other party's reservation and the quota total
// intact. The window must not be zeroed out.
func TestCancelOnlyReleasesThatPartysSeats(t *testing.T) {
	h := newHarness(t)
	first := h.leader()
	second := h.newLeader("second@trailpermit.test")

	partyA := h.readyParty(first, valleyTrail, 4, 1)
	partyB := h.readyParty(second, valleyTrail, 4, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), first, partyA.Code, ""); err != nil {
		t.Fatalf("队伍A申请许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), second, partyB.Code, ""); err != nil {
		t.Fatalf("队伍B申请许可失败: %v", err)
	}
	if occupied, total := h.permitWindow(valleyTrail, 1); occupied != 8 || total != 30 {
		t.Fatalf("两支队伍共应占用 8 个名额，实际 reserved=%d total=%d", occupied, total)
	}

	if _, err := h.app.Dispatch.CancelParty(h.ctx(), first, partyA.Code, "临时改期"); err != nil {
		t.Fatalf("取消队伍A失败: %v", err)
	}
	if occupied, total := h.permitWindow(valleyTrail, 1); occupied != 4 || total != 30 {
		t.Fatalf("取消一支队伍后应仅归还该队 4 个名额，另一队伍占用须保留，实际 reserved=%d total=%d", occupied, total)
	}

	// 取消的一方必须不再占用名额，被留下的队伍仍可被领队查看且状态不变。
	if _, err := h.app.Dispatch.GetParty(h.ctx(), second, partyB.Code); err != nil {
		t.Fatalf("队伍B仍应存在: %v", err)
	}
	if _, err := h.app.Dispatch.CancelParty(h.ctx(), second, partyB.Code, "之后取消"); err != nil {
		t.Fatalf("取消队伍B失败: %v", err)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 0 {
		t.Fatalf("两支队伍均取消后名额应清零，实际 %d", occupied)
	}
}

func TestLeaderCannotTouchAnotherLeadersParty(t *testing.T) {
	h := newHarness(t)
	owner := h.leader()
	intruder := h.newLeader("intruder@trailpermit.test")
	party := h.readyParty(owner, valleyTrail, 2, 1)

	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), intruder, party.Code, ""); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人不应申请该队伍的许可，实际 %v", err)
	}
	if _, err := h.app.Dispatch.GetParty(h.ctx(), intruder, party.Code); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人不应查看该队伍详情，实际 %v", err)
	}
	if _, err := h.app.Dispatch.CancelParty(h.ctx(), intruder, party.Code, "越权取消"); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人不应取消该队伍，实际 %v", err)
	}
	if _, err := h.app.Dispatch.ApproveParty(h.ctx(), owner, party.Code); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("领队不应执行线路管理员的复核动作，实际 %v", err)
	}
}

func TestPartyCreationRespectsTrailRules(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()

	if _, err := h.app.Dispatch.CreateParty(h.ctx(), leader, dispatch.CreatePartyInput{
		TrailCode: ridgeTrail, HikeDay: h.hikeDay(1),
		PlannedStart: h.plannedStart(1, 6), PlannedEnd: h.plannedStart(1, 15),
		Size: 2, ContactPhone: "13800001234",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("低于线路人数下限应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Dispatch.CreateParty(h.ctx(), leader, dispatch.CreatePartyInput{
		TrailCode: ridgeTrail, HikeDay: h.hikeDay(1),
		PlannedStart: h.plannedStart(2, 6), PlannedEnd: h.plannedStart(2, 15),
		Size: 4, ContactPhone: "13800001234",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("计划出发时间不在出行日内应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Dispatch.CreateParty(h.ctx(), leader, dispatch.CreatePartyInput{
		TrailCode: ridgeTrail, HikeDay: h.hikeDay(0),
		PlannedStart: h.plannedStart(0, 6), PlannedEnd: h.plannedStart(0, 15),
		Size: 4, ContactPhone: "13800001234",
	}); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("超过申报截止时间应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Dispatch.CreateParty(h.ctx(), leader, dispatch.CreatePartyInput{
		TrailCode: "NOT-EXIST", HikeDay: h.hikeDay(1),
		PlannedStart: h.plannedStart(1, 6), PlannedEnd: h.plannedStart(1, 15),
		Size: 4, ContactPhone: "13800001234",
	}); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("未知线路应返回 not_found，实际 %v", err)
	}

	if _, err := h.app.Catalog.SetTrailStatus(h.ctx(), ranger, ridgeTrail, string(domain.TrailSuspended)); err != nil {
		t.Fatalf("暂停线路失败: %v", err)
	}
	if _, err := h.app.Dispatch.CreateParty(h.ctx(), leader, dispatch.CreatePartyInput{
		TrailCode: ridgeTrail, HikeDay: h.hikeDay(1),
		PlannedStart: h.plannedStart(1, 6), PlannedEnd: h.plannedStart(1, 15),
		Size: 4, ContactPhone: "13800001234",
	}); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("暂停中的线路不应接受新队伍，实际 %v", err)
	}
}
