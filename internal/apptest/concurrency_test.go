package apptest

import (
	"fmt"
	"sync"
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

func TestConcurrentPermitRequestsNeverOverbookAndRollBack(t *testing.T) {
	h := newHarness(t)
	ranger := h.ranger()

	// AOMEN-RIDGE 的每日配额为 24，单队上限 8 人，因此 4 支 8 人队伍中
	// 只能有 3 支拿到许可。
	const parties = 4
	const size = 8
	type entry struct {
		actor domain.Actor
		code  string
	}
	entries := make([]entry, 0, parties)
	for i := 0; i < parties; i++ {
		actor := h.newLeader(fmt.Sprintf("leader%d@trailpermit.test", i))
		party := h.readyParty(actor, ridgeTrail, size, 1)
		entries = append(entries, entry{actor: actor, code: party.Code})
	}

	var (
		barrier  sync.WaitGroup
		finished sync.WaitGroup
		mu       sync.Mutex
		granted  []string
		refused  []error
	)
	barrier.Add(1)
	for _, item := range entries {
		finished.Add(1)
		go func(item entry) {
			defer finished.Done()
			barrier.Wait()
			view, err := h.app.Dispatch.RequestPermit(h.ctx(), item.actor, item.code, "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused = append(refused, err)
				return
			}
			granted = append(granted, view.Code)
		}(item)
	}
	barrier.Done()
	finished.Wait()

	occupied, total := h.permitWindow(ridgeTrail, 1)
	if occupied > total {
		t.Fatalf("并发申请造成超卖: reserved=%d total=%d", occupied, total)
	}
	if len(granted)*size != occupied {
		t.Fatalf("成功申请数与占用名额不一致: %d*%d != %d", len(granted), size, occupied)
	}
	if len(granted)+len(refused) != parties {
		t.Fatalf("每个申请都应有明确结果: 成功 %d 失败 %d", len(granted), len(refused))
	}
	for _, err := range refused {
		switch apperr.CodeOf(err) {
		case apperr.CodeQuotaExhausted, apperr.CodeVersionConflict, apperr.CodeConflict:
		default:
			t.Fatalf("失败原因应为名额或并发冲突，实际 %s (%v)", apperr.CodeOf(err), err)
		}
	}

	// 事务原子性：失败的申请不得留下待结算记录，也不得推进队伍状态。
	page, err := domain.NewPageRequest(1, 50, "created_at", false, []string{"created_at", "total_cents", "state"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	settlements, err := h.app.Settlements.List(h.ctx(), ranger, domain.SettlementFilter{}, page, "")
	if err != nil {
		t.Fatalf("查询结算失败: %v", err)
	}
	if settlements.Total != len(granted) {
		t.Fatalf("结算记录数应与成功申请数一致: %d != %d", settlements.Total, len(granted))
	}

	grantedSet := make(map[string]bool, len(granted))
	for _, code := range granted {
		grantedSet[code] = true
	}
	for _, item := range entries {
		view, err := h.app.Dispatch.GetParty(h.ctx(), ranger, item.code)
		if err != nil {
			t.Fatalf("读取队伍失败: %v", err)
		}
		if grantedSet[item.code] {
			if view.State != string(domain.PartyPermitReserved) {
				t.Fatalf("成功申请的队伍状态错误: %s", view.State)
			}
			continue
		}
		if view.State != string(domain.PartyDraft) {
			t.Fatalf("失败申请的队伍应保持草稿状态，实际 %s", view.State)
		}
		if view.Permit != nil {
			t.Fatalf("失败申请不应绑定许可窗口: %+v", view.Permit)
		}
	}
}

func TestPermitRequestIdempotencyReplaysStoredResponse(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 3, 1)

	first, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "permit-key-1")
	if err != nil {
		t.Fatalf("首次申请失败: %v", err)
	}
	second, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "permit-key-1")
	if err != nil {
		t.Fatalf("重放相同幂等键应成功: %v", err)
	}
	if first.Code != second.Code || first.Version != second.Version {
		t.Fatalf("重放应返回完全相同的响应: %+v vs %+v", first, second)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 3 {
		t.Fatalf("重放不得重复占用名额，实际 %d", occupied)
	}

	other := h.readyParty(leader, valleyTrail, 2, 2)
	_, err = h.app.Dispatch.RequestPermit(h.ctx(), leader, other.Code, "permit-key-1")
	if !apperr.Is(err, apperr.CodeConflict) {
		t.Fatalf("同一幂等键用于不同请求应返回 conflict，实际 %v", err)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 2); occupied != 0 {
		t.Fatalf("被拒绝的请求不应占用名额，实际 %d", occupied)
	}

	// 不带幂等键时不建立重放记录，但重复提交仍受状态机保护。
	fresh := h.readyParty(leader, valleyTrail, 2, 3)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, fresh.Code, ""); err != nil {
		t.Fatalf("无幂等键的申请应成功: %v", err)
	}
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, fresh.Code, ""); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("重复申请应被状态机拒绝，实际 %v", err)
	}
}

func TestQuotaExhaustionKeepsWindowConsistent(t *testing.T) {
	h := newHarness(t)
	ranger := h.ranger()
	first := h.newLeader("bulk1@trailpermit.test")
	second := h.newLeader("bulk2@trailpermit.test")
	third := h.newLeader("bulk3@trailpermit.test")
	fourth := h.newLeader("bulk4@trailpermit.test")

	for _, actor := range []domain.Actor{first, second, third} {
		party := h.readyParty(actor, ridgeTrail, 8, 1)
		if _, err := h.app.Dispatch.RequestPermit(h.ctx(), actor, party.Code, ""); err != nil {
			t.Fatalf("申请许可失败: %v", err)
		}
	}
	if occupied, total := h.permitWindow(ridgeTrail, 1); occupied != 24 || total != 24 {
		t.Fatalf("配额应被占满: reserved=%d total=%d", occupied, total)
	}

	blocked := h.readyParty(fourth, ridgeTrail, 8, 1)
	_, err := h.app.Dispatch.RequestPermit(h.ctx(), fourth, blocked.Code, "")
	if !apperr.Is(err, apperr.CodeQuotaExhausted) {
		t.Fatalf("配额占满后应返回 quota_exhausted，实际 %v", err)
	}
	if occupied, _ := h.permitWindow(ridgeTrail, 1); occupied != 24 {
		t.Fatalf("被拒绝的申请不得改变占用量，实际 %d", occupied)
	}

	// 线路管理员关闭窗口后，即使有余量也不再接受申请。
	if _, err := h.app.Catalog.OpenWindow(h.ctx(), ranger, ridgeTrail, h.hikeDay(2), 24); err != nil {
		t.Fatalf("开放许可窗口失败: %v", err)
	}
	if _, err := h.app.Catalog.CloseWindow(h.ctx(), ranger, ridgeTrail, h.hikeDay(2)); err != nil {
		t.Fatalf("关闭许可窗口失败: %v", err)
	}
	closedDay := h.readyParty(fourth, ridgeTrail, 4, 2)
	_, err = h.app.Dispatch.RequestPermit(h.ctx(), fourth, closedDay.Code, "")
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("窗口关闭后应返回 state_invalid，实际 %v", err)
	}
}

func TestListPartiesScopesLeadersToTheirOwnParties(t *testing.T) {
	h := newHarness(t)
	owner := h.leader()
	other := h.newLeader("other@trailpermit.test")
	ranger := h.ranger()

	mine := h.readyParty(owner, valleyTrail, 2, 1)
	theirs := h.readyParty(other, valleyTrail, 2, 2)

	page, err := domain.NewPageRequest(1, 10, "hike_day", false, []string{"hike_day", "created_at", "planned_start", "size"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	asLeader, err := h.app.Dispatch.ListParties(h.ctx(), owner, domain.PartyFilter{}, page)
	if err != nil {
		t.Fatalf("领队查询失败: %v", err)
	}
	if asLeader.Total != 1 || asLeader.Items[0].Code != mine.Code {
		t.Fatalf("领队只应看到自己的队伍: %+v", asLeader.Items)
	}
	spoofed, err := h.app.Dispatch.ListParties(h.ctx(), owner, domain.PartyFilter{LeaderID: 999}, page)
	if err != nil {
		t.Fatalf("领队查询失败: %v", err)
	}
	if spoofed.Total != 1 || spoofed.Items[0].Code != mine.Code {
		t.Fatal("领队伪造 leader_id 不应绕过归属限制")
	}
	asRanger, err := h.app.Dispatch.ListParties(h.ctx(), ranger, domain.PartyFilter{}, page)
	if err != nil {
		t.Fatalf("线路管理员查询失败: %v", err)
	}
	if asRanger.Total != 2 {
		t.Fatalf("线路管理员应看到全部队伍，实际 %d", asRanger.Total)
	}
	filtered, err := h.app.Dispatch.ListParties(h.ctx(), ranger, domain.PartyFilter{
		States:    []domain.PartyState{domain.PartyDraft},
		HikeDayGE: h.hikeDay(2),
	}, page)
	if err != nil {
		t.Fatalf("线路管理员查询失败: %v", err)
	}
	if filtered.Total != 1 || filtered.Items[0].Code != theirs.Code {
		t.Fatalf("状态与日期组合过滤失效: %+v", filtered.Items)
	}
	if _, err := h.app.Dispatch.ListParties(h.ctx(), domain.Actor{}, domain.PartyFilter{}, page); !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("匿名查询应被拒绝，实际 %v", err)
	}
	if _, err := h.app.Dispatch.ListParties(h.ctx(), ranger, domain.PartyFilter{
		States: []domain.PartyState{domain.PartyState("unknown")},
	}, page); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatal("未知状态过滤应被拒绝")
	}
}

func TestAuditTrailRecordsEveryStateChange(t *testing.T) {
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

	total, err := h.app.Audits.CountByObject(h.ctx(), "party", party.Code)
	if err != nil {
		t.Fatalf("统计审计事件失败: %v", err)
	}
	// 建队、登记队员、申请许可、复核，至少四条审计。
	if total < 4 {
		t.Fatalf("关键动作的审计事件不足: %d", total)
	}
	page, err := domain.NewPageRequest(1, 20, "created_at", true, []string{"created_at", "action"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	events, err := h.app.Audits.List(h.ctx(), domain.AuditFilter{ObjectType: "party", ObjectID: party.Code}, page)
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	actions := make(map[string]bool, len(events.Items))
	for _, event := range events.Items {
		actions[event.Action] = true
		if event.RequestID != "req-test" {
			t.Fatalf("审计事件应保留请求标识，实际 %q", event.RequestID)
		}
		if event.ActorID == 0 {
			t.Fatalf("审计事件应记录操作者: %+v", event)
		}
	}
	for _, action := range []string{dispatch.ActionPartyCreate, dispatch.ActionPermitRequest, dispatch.ActionPartyApprove} {
		if !actions[action] {
			t.Fatalf("缺少 %s 的审计记录: %+v", action, actions)
		}
	}
}
