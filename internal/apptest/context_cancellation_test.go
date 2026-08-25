package apptest

import (
	"context"
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestCancelledPermitRequestLeavesNoTrace 在调用方已经取消请求的情况下提交许可申请，
// 校验取消会被如实上报，并且队伍状态、名额与结算记录都不被改写。
func TestCancelledPermitRequestLeavesNoTrace(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.readyParty(leader, valleyTrail, 3, 1)

	cancelled, cancel := context.WithCancel(h.ctx())
	cancel()

	view, err := h.app.Dispatch.RequestPermit(cancelled, leader, party.Code, "cancelled-key-0004")
	if err == nil {
		t.Fatalf("请求已取消时必须返回错误，实际却成功返回 %+v", view)
	}
	if !apperr.Is(err, apperr.CodeDeadlineExceeded) {
		t.Fatalf("取消的请求应映射为 deadline_exceeded，实际错误码 %s (%v)", apperr.CodeOf(err), err)
	}

	stored, err := h.app.Dispatch.GetParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取队伍失败: %v", err)
	}
	if stored.State != string(domain.PartyDraft) {
		t.Fatalf("取消的申请不得推进队伍状态，实际 %s", stored.State)
	}
	if stored.Permit != nil {
		t.Fatalf("取消的申请不得为队伍绑定许可窗口: %+v", stored.Permit)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 0 {
		t.Fatalf("取消的申请不得占用当天名额，实际已占用 %d", reserved)
	}

	if _, err := h.app.Settlements.ForParty(h.ctx(), leader, party.Code); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("取消的申请不得生成结算记录，实际 %v", err)
	}

	retried, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "cancelled-key-0004")
	if err != nil {
		t.Fatalf("取消后使用同一编号重新提交应能正常占用名额: %v", err)
	}
	if retried.State != string(domain.PartyPermitReserved) {
		t.Fatalf("重新提交后队伍状态应为 permit_reserved，实际 %s", retried.State)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 3 {
		t.Fatalf("重新提交后应占用 3 个名额，实际 %d", reserved)
	}
}
