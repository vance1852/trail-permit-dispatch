package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestExpiredJobLeaseIsRecoveredByNextWorker 模拟一个 worker 崩溃后留下的过期租约，
// 校验后续 worker 会把该作业重新领取并执行完成。
func TestExpiredJobLeaseIsRecoveredByNextWorker(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 2, 1)
	if _, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, ""); err != nil {
		t.Fatalf("申请许可失败: %v", err)
	}

	claimed, err := h.app.Jobs.ClaimDue(h.ctx(), "crashed-worker", 5*time.Second, h.clock.Now(), 5)
	if err != nil {
		t.Fatalf("模拟 worker 领取作业失败: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("前置条件要求存在一条到期的派单通报作业")
	}
	stuck := claimed[0]
	if stuck.State != domain.JobRunning {
		t.Fatalf("被领取的作业应处于执行中，实际 %s", stuck.State)
	}

	// worker 进程在租约到期前退出，作业留在执行中状态。
	h.clock.Advance(2 * time.Minute)

	processed, err := h.app.Worker.Poll(h.ctx(), "healthy-worker")
	if err != nil {
		t.Fatalf("后续 worker 轮询失败: %v", err)
	}
	if processed == 0 {
		t.Fatal("租约到期后作业必须被后续 worker 重新领取执行")
	}

	recovered, err := h.app.Jobs.ByID(h.ctx(), stuck.ID)
	if err != nil {
		t.Fatalf("读取作业失败: %v", err)
	}
	if recovered.State != domain.JobDone {
		t.Fatalf("恢复后的作业应执行完成，实际 %s (locked_by=%q)", recovered.State, recovered.LockedBy)
	}
	if recovered.LockedBy != "" || recovered.LockedUntil != nil {
		t.Fatalf("完成后必须释放租约: locked_by=%q locked_until=%v", recovered.LockedBy, recovered.LockedUntil)
	}
	if recovered.Attempts < 2 {
		t.Fatalf("重新领取应累计尝试次数，实际 %d", recovered.Attempts)
	}

	running, err := h.app.Jobs.CountByState(h.ctx(), domain.JobRunning)
	if err != nil {
		t.Fatalf("统计执行中作业失败: %v", err)
	}
	if running != 0 {
		t.Fatalf("不应残留处于执行中的作业，实际 %d 条", running)
	}
}
