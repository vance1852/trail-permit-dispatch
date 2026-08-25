package apptest

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestPermitFeeIsNotSettledBeforeTripEnds 校验队伍刚占到名额、尚未出行时后台作业
// 不会提前把许可费用结算掉，并且此时取消出行仍然能够正常减免费用。
func TestPermitFeeIsNotSettledBeforeTripEnds(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 3, 1)

	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	pending, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if pending.State != string(domain.SettlementPending) {
		t.Fatalf("刚占到名额时结算应为待结算，实际 %s", pending.State)
	}

	if processed := h.drainJobs(20); processed == 0 {
		t.Fatal("申请许可后应有派单通报作业被执行")
	}

	afterJobs, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if afterJobs.State != string(domain.SettlementPending) {
		t.Fatalf("队伍尚未出行结束，后台作业不得提前结算，实际 %s", afterJobs.State)
	}
	if afterJobs.SettledAt != nil {
		t.Fatalf("尚未收队的队伍不应有结算时间: %v", afterJobs.SettledAt)
	}
	if afterJobs.Reference != "" {
		t.Fatalf("尚未收队的队伍不应生成结算凭证: %q", afterJobs.Reference)
	}

	cancelled, err := h.app.Dispatch.CancelParty(h.ctx(), leader, party.Code, "队员临时受伤")
	if err != nil {
		t.Fatalf("取消出行失败: %v", err)
	}
	if cancelled.State != string(domain.PartyCancelled) {
		t.Fatalf("取消后状态应为 cancelled，实际 %s", cancelled.State)
	}

	waived, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if waived.State != string(domain.SettlementWaived) {
		t.Fatalf("出行前取消必须能够减免费用，实际 %s", waived.State)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 0 {
		t.Fatalf("取消后应归还全部名额，实际仍占用 %d", reserved)
	}
}
