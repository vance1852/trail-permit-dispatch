package apptest

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestFailedSettlementJobIsParkedForRetry 让一笔结算作业在业务上必然失败，
// 校验失败的后台作业不会被当成已完成，并且对应的结算记录会被明确停放。
func TestFailedSettlementJobIsParkedForRetry(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	partyID, err := h.app.Dispatch.PartyIDByCode(h.ctx(), party.Code)
	if err != nil {
		t.Fatalf("解析队伍标识失败: %v", err)
	}

	before, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if before.State != string(domain.SettlementPending) {
		t.Fatalf("前置条件要求结算处于待结算状态，实际 %s", before.State)
	}

	now := h.clock.Now()
	job := &domain.Job{
		Kind:        domain.JobSettleParty,
		Payload:     `{"party_id":` + itoa(partyID) + `}`,
		MaxAttempts: 3,
		RunAt:       now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := h.app.Jobs.Enqueue(h.ctx(), job); err != nil {
		t.Fatalf("入队结算作业失败: %v", err)
	}

	if _, err := h.app.Worker.Poll(h.ctx(), "settlement-test-worker"); err != nil {
		t.Fatalf("执行后台作业失败: %v", err)
	}

	executed, err := h.app.Jobs.ByID(h.ctx(), job.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if executed.State == domain.JobDone {
		t.Fatalf("业务上失败的结算作业不得被标记为已完成: state=%s last_error=%q", executed.State, executed.LastError)
	}
	if executed.State != domain.JobFailed {
		t.Fatalf("不可重试的结算失败应停放为 failed，实际 %s", executed.State)
	}
	if executed.LastError == "" {
		t.Fatal("停放的作业必须记录失败原因")
	}
	if executed.Attempts != 1 {
		t.Fatalf("首次执行后尝试次数应为 1，实际 %d", executed.Attempts)
	}

	after, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if after.State != string(domain.SettlementFailed) {
		t.Fatalf("结算作业永久失败后必须把结算记录标记为 failed，实际 %s", after.State)
	}
	if after.FailureNote == "" {
		t.Fatal("失败的结算记录必须保留失败说明，便于人工跟进")
	}
}

// itoa 避免测试文件引入额外依赖。
func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
