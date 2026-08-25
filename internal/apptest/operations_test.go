package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
	"github.com/vance1852/trail-permit-dispatch/internal/service/incident"
)

// dispatchParty walks a party all the way onto the trail.
func (h *harness) dispatchParty(leader, ranger domain.Actor, trailCode string, size, offsetDays int) dispatch.PartyView {
	h.t.Helper()
	party := h.readyParty(leader, trailCode, size, offsetDays)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		h.t.Fatalf("申请许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.ApproveParty(h.ctx(), ranger, party.Code); err != nil {
		h.t.Fatalf("复核失败: %v", err)
	}
	view, err := h.app.Dispatch.DispatchParty(h.ctx(), ranger, party.Code)
	if err != nil {
		h.t.Fatalf("放行失败: %v", err)
	}
	return view
}

func TestOverdueSweepOpensIncidentOnceAndEscalates(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.dispatchParty(leader, ranger, valleyTrail, 2, 1)

	// 第一个必经打点截止 60 分钟，加 30 分钟宽限；91 分钟后仍未上报即超时。
	h.clock.Set(h.plannedStart(1, 6).Add(91 * time.Minute))
	result, err := h.app.Dispatch.SweepOverdueCheckpoints(h.ctx(), 10)
	if err != nil {
		t.Fatalf("超时扫描失败: %v", err)
	}
	if result.Scanned != 1 || result.IncidentsOpened != 1 {
		t.Fatalf("超时扫描结果错误: %+v", result)
	}

	again, err := h.app.Dispatch.SweepOverdueCheckpoints(h.ctx(), 10)
	if err != nil {
		t.Fatalf("重复扫描失败: %v", err)
	}
	if again.IncidentsOpened != 0 {
		t.Fatalf("同一超时不应重复登记事故: %+v", again)
	}

	page, err := domain.NewPageRequest(1, 10, "opened_at", true, []string{"opened_at", "severity", "state"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	incidents, err := h.app.Incidents.List(h.ctx(), ranger, domain.IncidentFilter{}, page, party.Code)
	if err != nil {
		t.Fatalf("查询事故失败: %v", err)
	}
	if incidents.Total != 1 {
		t.Fatalf("应存在 1 条事故记录，实际 %d", incidents.Total)
	}
	opened := incidents.Items[0]
	if opened.Severity != string(domain.SeverityMajor) || opened.State != string(domain.IncidentOpen) {
		t.Fatalf("超时事故级别或状态错误: %+v", opened)
	}
	if opened.CheckpointSeq != 1 {
		t.Fatalf("事故应关联第一个打点，实际 %d", opened.CheckpointSeq)
	}

	// 升级作业按级别延迟执行，推进时钟后由后台执行器接手。
	h.clock.Set(h.clock.Now().Add(domain.SeverityMajor.EscalationDelay() + time.Second))
	if processed := h.drainJobs(20); processed == 0 {
		t.Fatal("升级作业应被执行")
	}
	escalated, err := h.app.Incidents.Get(h.ctx(), ranger, opened.ID)
	if err != nil {
		t.Fatalf("读取事故失败: %v", err)
	}
	if escalated.State != string(domain.IncidentEscalated) || escalated.EscalationCount < 1 {
		t.Fatalf("事故应完成至少一次升级: %+v", escalated)
	}

	resolved, err := h.app.Incidents.Resolve(h.ctx(), ranger, opened.ID, "已电话联系领队，队伍安全下撤")
	if err != nil {
		t.Fatalf("处置事故失败: %v", err)
	}
	if resolved.State != string(domain.IncidentResolved) || resolved.ResolvedAt == nil {
		t.Fatalf("处置结果错误: %+v", resolved)
	}
	closed, err := h.app.Incidents.Close(h.ctx(), ranger, opened.ID)
	if err != nil {
		t.Fatalf("归档事故失败: %v", err)
	}
	if closed.State != string(domain.IncidentClosed) {
		t.Fatalf("归档结果错误: %+v", closed)
	}
	if _, err := h.app.Incidents.Close(h.ctx(), ranger, opened.ID); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复归档应被拒绝，实际 %v", err)
	}
}

func TestCriticalIncidentAbortsPartyAndReleasesQuota(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.dispatchParty(leader, ranger, valleyTrail, 3, 1)
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 3 {
		t.Fatalf("放行后应占用 3 个名额，实际 %d", occupied)
	}

	view, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode:     party.Code,
		Kind:          "injury",
		Severity:      string(domain.SeverityCritical),
		Summary:       "一名队员在石滩滑倒，脚踝疑似骨折，需要救援",
		CheckpointSeq: 2,
	})
	if err != nil {
		t.Fatalf("登记事故失败: %v", err)
	}
	if view.Severity != string(domain.SeverityCritical) {
		t.Fatalf("事故级别错误: %+v", view)
	}

	aborted, err := h.app.Dispatch.GetParty(h.ctx(), ranger, party.Code)
	if err != nil {
		t.Fatalf("读取队伍失败: %v", err)
	}
	if aborted.State != string(domain.PartyAborted) {
		t.Fatalf("严重事故应强制中止队伍，实际 %s", aborted.State)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 0 {
		t.Fatalf("中止后应释放许可名额，实际 %d", occupied)
	}
	settlement, err := h.app.Settlements.ForParty(h.ctx(), ranger, party.Code)
	if err != nil {
		t.Fatalf("读取结算失败: %v", err)
	}
	if settlement.State != string(domain.SettlementWaived) {
		t.Fatalf("中止后应减免许可费用，实际 %s", settlement.State)
	}

	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 3, HeadCount: 2}); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("已中止的队伍不应继续上报打点，实际 %v", err)
	}
	if _, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode: party.Code, Kind: "lost", Severity: string(domain.SeverityMinor),
		Summary: "队伍已中止，不应再登记事故",
	}); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("已中止的队伍不应登记新事故，实际 %v", err)
	}
}

func TestIncidentValidationAndVisibility(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	other := h.newLeader("bystander@trailpermit.test")
	ranger := h.ranger()
	party := h.dispatchParty(leader, ranger, valleyTrail, 2, 1)

	if _, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode: party.Code, Kind: "", Severity: string(domain.SeverityMinor), Summary: "缺少事故类型",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("缺少事故类型应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode: party.Code, Kind: "delay", Severity: string(domain.SeverityMinor), Summary: "太短",
	}); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("过短的事故描述应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Incidents.Open(h.ctx(), other, incident.OpenInput{
		PartyCode: party.Code, Kind: "delay", Severity: string(domain.SeverityMinor),
		Summary: "他人无权登记该队伍的事故",
	}); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人不应登记该队伍事故，实际 %v", err)
	}

	opened, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode: party.Code, Kind: "delay", Severity: string(domain.SeverityMinor),
		Summary: "溪水上涨导致行进速度明显变慢",
	})
	if err != nil {
		t.Fatalf("登记事故失败: %v", err)
	}
	if _, err := h.app.Incidents.Resolve(h.ctx(), leader, opened.ID, "领队自行处置完毕"); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("领队不应处置事故，实际 %v", err)
	}
	if _, err := h.app.Incidents.Resolve(h.ctx(), ranger, opened.ID, "短"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("过短的处置结论应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Incidents.Get(h.ctx(), other, opened.ID); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("他人不应查看该事故，实际 %v", err)
	}

	page, err := domain.NewPageRequest(1, 10, "opened_at", true, []string{"opened_at", "severity", "state"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	if _, err := h.app.Incidents.List(h.ctx(), leader, domain.IncidentFilter{}, page, ""); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("领队查询事故必须指定队伍，实际 %v", err)
	}
	mine, err := h.app.Incidents.List(h.ctx(), leader, domain.IncidentFilter{}, page, party.Code)
	if err != nil {
		t.Fatalf("查询事故失败: %v", err)
	}
	if mine.Total != 1 {
		t.Fatalf("领队应看到自己队伍的事故，实际 %d", mine.Total)
	}
}

func TestEscalationStopsAtSeverityCap(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.dispatchParty(leader, ranger, valleyTrail, 2, 1)

	opened, err := h.app.Incidents.Open(h.ctx(), leader, incident.OpenInput{
		PartyCode: party.Code, Kind: "delay", Severity: string(domain.SeverityMinor),
		Summary: "队伍在补水点等待同伴，进度落后",
	})
	if err != nil {
		t.Fatalf("登记事故失败: %v", err)
	}
	escalated, err := h.app.Incidents.Escalate(h.ctx(), opened.ID)
	if err != nil {
		t.Fatalf("首次升级失败: %v", err)
	}
	if escalated.EscalationCount != 1 {
		t.Fatalf("升级次数应为 1，实际 %d", escalated.EscalationCount)
	}
	_, err = h.app.Incidents.Escalate(h.ctx(), opened.ID)
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("达到轻微事故升级上限后应拒绝，实际 %v", err)
	}

	if _, err := h.app.Incidents.Resolve(h.ctx(), ranger, opened.ID, "同伴已归队，行进恢复正常"); err != nil {
		t.Fatalf("处置事故失败: %v", err)
	}
	// 已处置的事故再次收到升级请求时，应保持处置结果不变。
	after, err := h.app.Incidents.Escalate(h.ctx(), opened.ID)
	if err != nil {
		t.Fatalf("已处置事故的升级应被安全忽略: %v", err)
	}
	if after.State != string(domain.IncidentResolved) {
		t.Fatalf("已处置事故状态不应被升级覆盖，实际 %s", after.State)
	}
}

func TestSettlementGovernance(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()
	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	settlement, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算失败: %v", err)
	}

	partyID, err := h.app.Dispatch.PartyIDByCode(h.ctx(), party.Code)
	if err != nil {
		t.Fatalf("解析队伍标识失败: %v", err)
	}
	if _, err := h.app.Settlements.SettleForParty(h.ctx(), partyID); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("未完成行程的队伍不应结算，实际 %v", err)
	}
	if _, err := h.app.Settlements.Waive(h.ctx(), leader, settlement.ID, "领队请求减免"); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("领队不应减免费用，实际 %v", err)
	}
	if _, err := h.app.Settlements.Waive(h.ctx(), ranger, settlement.ID, "短"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("过短的减免说明应被拒绝，实际 %v", err)
	}
	waived, err := h.app.Settlements.Waive(h.ctx(), ranger, settlement.ID, "线路临时封闭造成的行程变更")
	if err != nil {
		t.Fatalf("减免费用失败: %v", err)
	}
	if waived.State != string(domain.SettlementWaived) {
		t.Fatalf("减免结果错误: %+v", waived)
	}
	if _, err := h.app.Settlements.Waive(h.ctx(), ranger, settlement.ID, "重复减免同一笔费用"); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复减免应被拒绝，实际 %v", err)
	}

	page, err := domain.NewPageRequest(1, 10, "created_at", false, []string{"created_at", "total_cents", "state"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	if _, err := h.app.Settlements.List(h.ctx(), leader, domain.SettlementFilter{}, page, ""); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("领队查询结算必须指定队伍，实际 %v", err)
	}
	list, err := h.app.Settlements.List(h.ctx(), ranger, domain.SettlementFilter{
		States: []domain.SettlementState{domain.SettlementWaived},
	}, page, "")
	if err != nil {
		t.Fatalf("查询结算失败: %v", err)
	}
	if list.Total != 1 {
		t.Fatalf("减免记录数量错误: %d", list.Total)
	}
}

func TestCatalogGovernanceRules(t *testing.T) {
	h := newHarness(t)
	ranger := h.ranger()
	leader := h.leader()

	if _, err := h.app.Catalog.CreateTrail(h.ctx(), leader, validTrailInput()); !apperr.Is(err, apperr.CodePermissionDenied) {
		t.Fatalf("领队不应创建线路，实际 %v", err)
	}
	created, err := h.app.Catalog.CreateTrail(h.ctx(), ranger, validTrailInput())
	if err != nil {
		t.Fatalf("创建线路失败: %v", err)
	}
	if len(created.Checkpoints) != 2 {
		t.Fatalf("线路打点数量错误: %+v", created.Checkpoints)
	}
	if _, err := h.app.Catalog.CreateTrail(h.ctx(), ranger, validTrailInput()); !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("重复线路编码应被拒绝，实际 %v", err)
	}

	broken := validTrailInput()
	broken.Code = "BROKEN-SEQ"
	broken.Checkpoints[1].Seq = 1
	if _, err := h.app.Catalog.CreateTrail(h.ctx(), ranger, broken); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("打点序号必须递增，实际 %v", err)
	}
	optional := validTrailInput()
	optional.Code = "NO-MANDATORY"
	optional.Checkpoints[0].Mandatory = false
	optional.Checkpoints[1].Mandatory = false
	if _, err := h.app.Catalog.CreateTrail(h.ctx(), ranger, optional); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("线路必须包含必经打点，实际 %v", err)
	}
	oversize := validTrailInput()
	oversize.Code = "OVERSIZE"
	oversize.MaxPartySize = 100
	if _, err := h.app.Catalog.CreateTrail(h.ctx(), ranger, oversize); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("单队上限不得超过每日配额，实际 %v", err)
	}

	added, err := h.app.Catalog.AddCheckpoint(h.ctx(), ranger, created.Code, catalogCheckpoint(3, "终点站", 400, true))
	if err != nil {
		t.Fatalf("追加打点失败: %v", err)
	}
	if len(added.Checkpoints) != 3 {
		t.Fatalf("追加后打点数量错误: %+v", added.Checkpoints)
	}
	if _, err := h.app.Catalog.AddCheckpoint(h.ctx(), ranger, created.Code, catalogCheckpoint(3, "重复序号", 500, true)); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("重复打点序号应被拒绝，实际 %v", err)
	}

	if _, err := h.app.Catalog.OpenWindow(h.ctx(), ranger, created.Code, "2026-13-40", 20); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("非法出行日应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Catalog.OpenWindow(h.ctx(), ranger, created.Code, h.hikeDay(2), 2); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("窗口配额小于单队上限应被拒绝，实际 %v", err)
	}
	window, err := h.app.Catalog.OpenWindow(h.ctx(), ranger, created.Code, h.hikeDay(2), 0)
	if err != nil {
		t.Fatalf("开放窗口失败: %v", err)
	}
	if window.QuotaTotal != created.DailyQuota {
		t.Fatalf("默认配额应取线路每日配额，实际 %d", window.QuotaTotal)
	}
	if _, err := h.app.Catalog.SetTrailStatus(h.ctx(), ranger, created.Code, "closed"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知线路状态应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Catalog.SetTrailStatus(h.ctx(), ranger, created.Code, string(domain.TrailOpen)); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复设置同一状态应被拒绝，实际 %v", err)
	}
	trails, err := h.app.Catalog.TrailCount(h.ctx())
	if err != nil {
		t.Fatalf("统计线路失败: %v", err)
	}
	if trails != 3 {
		t.Fatalf("线路总数应为 3，实际 %d", trails)
	}
}
