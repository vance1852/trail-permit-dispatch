package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

// TestCompletedPartyReleasesPermitAfterWindowClosed 覆盖线路管理员在队伍出行当天
// 关闭许可窗口后队伍正常收队的场景：行程照常结束，占用的名额必须如实归还。
func TestCompletedPartyReleasesPermitAfterWindowClosed(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	ranger := h.ranger()

	party := h.readyParty(leader, valleyTrail, 3, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.ApproveParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("复核许可失败: %v", err)
	}
	if _, err := h.app.Dispatch.DispatchParty(h.ctx(), ranger, party.Code); err != nil {
		t.Fatalf("放行失败: %v", err)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 3 {
		t.Fatalf("放行后应占用 3 个名额，实际 %d", reserved)
	}

	closed, err := h.app.Catalog.CloseWindow(h.ctx(), ranger, valleyTrail, h.hikeDay(1))
	if err != nil {
		t.Fatalf("关闭当日许可窗口失败: %v", err)
	}
	if !closed.Closed {
		t.Fatalf("窗口应处于关闭状态: %+v", closed)
	}

	h.clock.Set(h.plannedStart(1, 6).Add(40 * time.Minute))
	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 1, HeadCount: 3}); err != nil {
		t.Fatalf("上报第一个打点失败: %v", err)
	}
	h.clock.Set(h.plannedStart(1, 6).Add(290 * time.Minute))
	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), leader, party.Code, dispatch.ReportInput{Seq: 3, HeadCount: 3}); err != nil {
		t.Fatalf("上报最后一个打点失败: %v", err)
	}

	completed, err := h.app.Dispatch.CompleteParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("窗口已关闭时结束行程仍应成功: %v", err)
	}
	if completed.State != string(domain.PartyCompleted) {
		t.Fatalf("结束行程后状态应为 completed，实际 %s", completed.State)
	}

	if reserved, total := h.permitWindow(valleyTrail, 1); reserved != 0 || total != 30 {
		t.Fatalf("完成行程后必须归还全部名额，实际 reserved=%d total=%d", reserved, total)
	}

	if processed := h.drainJobs(10); processed == 0 {
		t.Fatal("完成行程后应产生结算作业")
	}
	settled, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取结算记录失败: %v", err)
	}
	if settled.State != string(domain.SettlementSettled) {
		t.Fatalf("结算作业执行后应完成结算，实际 %s", settled.State)
	}
}
