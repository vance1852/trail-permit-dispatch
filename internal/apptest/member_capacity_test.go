package apptest

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

// TestMemberBatchRespectsPlannedPartySize 在一次批量登记里提交超过计划规模的队员，
// 校验剩余名额在整批范围内被正确收敛，并且队伍随后仍能正常申请许可。
func TestMemberBatchRespectsPlannedPartySize(t *testing.T) {
	h := newHarness(t)
	leader := h.leader()
	party := h.createParty(leader, valleyTrail, 3, 1)

	result, err := h.app.Dispatch.RegisterMembers(h.ctx(), leader, party.Code, []dispatch.MemberInput{
		{MemberRef: "HK-2001", DisplayName: "周岚", Phone: "13900002001", WaiverSigned: true},
		{MemberRef: "HK-2002", DisplayName: "程野", Phone: "13900002002", WaiverSigned: true},
		{MemberRef: "HK-2003", DisplayName: "许清", Phone: "13900002003", WaiverSigned: true},
		{MemberRef: "HK-2004", DisplayName: "田舟", Phone: "13900002004", WaiverSigned: true},
	})
	if err != nil {
		t.Fatalf("批量登记调用失败: %v", err)
	}
	if result.Accepted != 2 {
		t.Fatalf("计划规模 3 人、领队已占 1 席时只应接受 2 名队员，实际接受 %d 名", result.Accepted)
	}
	if result.Rejected != 2 {
		t.Fatalf("超出计划规模的 2 名队员必须被拒绝，实际拒绝 %d 名", result.Rejected)
	}
	if len(result.Outcomes) != 4 {
		t.Fatalf("必须逐项返回 4 条结果，实际 %d 条", len(result.Outcomes))
	}
	for _, outcome := range result.Outcomes[2:] {
		if outcome.Accepted {
			t.Fatalf("队员 %s 超出计划规模却被接受", outcome.MemberRef)
		}
		if outcome.Code != string(apperr.CodeConflict) {
			t.Fatalf("超额队员 %s 的拒绝原因应为名额冲突，实际 %s", outcome.MemberRef, outcome.Code)
		}
	}
	if result.Party.Readiness.Registered != 3 {
		t.Fatalf("已登记人数必须等于计划规模 3，实际 %d", result.Party.Readiness.Registered)
	}
	if !result.Party.Readiness.Ready {
		t.Fatalf("人数齐备且全部签署免责后应判定齐备: %+v", result.Party.Readiness)
	}

	detail, err := h.app.Dispatch.GetParty(h.ctx(), leader, party.Code)
	if err != nil {
		t.Fatalf("读取队伍详情失败: %v", err)
	}
	if len(detail.Members) != 3 {
		t.Fatalf("持久化的队员数量必须等于计划规模 3，实际 %d", len(detail.Members))
	}

	reserved, err := h.app.Dispatch.RequestPermit(h.ctx(), leader, party.Code, "")
	if err != nil {
		t.Fatalf("人数收敛到计划规模后应能正常申请许可: %v", err)
	}
	if reserved.State != string(domain.PartyPermitReserved) {
		t.Fatalf("申请许可后队伍状态应为 permit_reserved，实际 %s", reserved.State)
	}
	if occupied, _ := h.permitWindow(valleyTrail, 1); occupied != 3 {
		t.Fatalf("当天已占用名额应为计划规模 3，实际 %d", occupied)
	}
}
