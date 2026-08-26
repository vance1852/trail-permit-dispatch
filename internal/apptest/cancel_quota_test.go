package apptest

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestCancelOnlyReleasesItsOwnSeats 让同一天两支队伍各自占用名额后取消其中一支，
// 校验归还的名额只属于被取消的队伍，另一支队伍的占用不受影响。
func TestCancelOnlyReleasesItsOwnSeats(t *testing.T) {
	h := newHarness(t)
	leaving := h.leader()
	staying := h.newLeader("club-stay-0009@trailpermit.test")

	leavingParty := h.readyParty(leaving, valleyTrail, 4, 1)
	stayingParty := h.readyParty(staying, valleyTrail, 4, 1)

	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leaving, leavingParty.Code, ""); err != nil {
		t.Fatalf("第一支队伍申请许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), staying, stayingParty.Code, ""); err != nil {
		t.Fatalf("第二支队伍申请许可失败: %v", err)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 8 {
		t.Fatalf("两支队伍应共占用 8 个名额，实际 %d", reserved)
	}

	cancelled, err := h.app.Dispatch.CancelParty(h.ctx(), leaving, leavingParty.Code, "临时改期")
	if err != nil {
		t.Fatalf("取消出行失败: %v", err)
	}
	if cancelled.State != string(domain.PartyCancelled) {
		t.Fatalf("取消后状态应为 cancelled，实际 %s", cancelled.State)
	}

	reserved, total := h.permitWindow(valleyTrail, 1)
	if reserved != 4 {
		t.Fatalf("取消一支 4 人队伍后应只归还 4 个名额，实际剩余占用 %d", reserved)
	}
	if total != 30 {
		t.Fatalf("配额总量不应变化，实际 %d", total)
	}

	remaining, err := h.app.Dispatch.GetParty(h.ctx(), staying, stayingParty.Code)
	if err != nil {
		t.Fatalf("读取未取消队伍失败: %v", err)
	}
	if remaining.State != string(domain.PartyPermitReserved) {
		t.Fatalf("未取消的队伍状态应保持 permit_reserved，实际 %s", remaining.State)
	}
	if remaining.Permit == nil || remaining.Permit.Reserved != 4 {
		t.Fatalf("未取消的队伍应仍然看到自己的 4 个占用名额: %+v", remaining.Permit)
	}

	settlement, err := h.app.Settlements.ForParty(h.ctx(), leaving, leavingParty.Code)
	if err != nil {
		t.Fatalf("读取被取消队伍的结算记录失败: %v", err)
	}
	if settlement.State != string(domain.SettlementWaived) {
		t.Fatalf("取消后待结算费用应减免，实际 %s", settlement.State)
	}
}
