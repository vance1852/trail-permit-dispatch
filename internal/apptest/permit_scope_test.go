package apptest

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TestPermitRequestKeyIsScopedPerLeader 覆盖两位领队使用相同申请编号的场景：
// 每个人的申请编号必须只对自己生效，同时同一领队的重放仍然只占用一次名额。
func TestPermitRequestKeyIsScopedPerLeader(t *testing.T) {
	h := newHarness(t)
	first := h.leader()
	second := h.newLeader("club-second@trailpermit.test")

	firstParty := h.readyParty(first, valleyTrail, 3, 1)
	secondParty := h.readyParty(second, valleyTrail, 2, 1)
	const sharedKey = "club-2026-0912"

	firstView, err := h.app.Dispatch.RequestPermit(h.ctx(), first, firstParty.Code, sharedKey)
	if err != nil {
		t.Fatalf("第一位领队使用自己的申请编号提交许可申请失败: %v", err)
	}
	if firstView.Code != firstParty.Code || firstView.State != string(domain.PartyPermitReserved) {
		t.Fatalf("第一位领队应拿到自己队伍的占用结果: code=%s state=%s", firstView.Code, firstView.State)
	}

	secondView, err := h.app.Dispatch.RequestPermit(h.ctx(), second, secondParty.Code, sharedKey)
	if err != nil {
		t.Fatalf("第二位领队使用相同申请编号提交自己队伍的申请时被拒绝: %v", err)
	}
	if secondView.Code != secondParty.Code {
		t.Fatalf("第二位领队拿到了他人队伍的信息: 期望 %s 实际 %s", secondParty.Code, secondView.Code)
	}
	if secondView.State != string(domain.PartyPermitReserved) {
		t.Fatalf("第二位领队的队伍应进入已占用名额状态，实际 %s", secondView.State)
	}

	stored, err := h.app.Dispatch.GetParty(h.ctx(), second, secondParty.Code)
	if err != nil {
		t.Fatalf("读取第二位领队的队伍失败: %v", err)
	}
	if stored.State != string(domain.PartyPermitReserved) {
		t.Fatalf("第二位领队的队伍未真正占用名额，仍处于 %s", stored.State)
	}

	if reserved, total := h.permitWindow(valleyTrail, 1); reserved != 5 || total != 30 {
		t.Fatalf("当天已占用名额应为两支队伍人数之和 5，实际 reserved=%d total=%d", reserved, total)
	}

	replay, err := h.app.Dispatch.RequestPermit(h.ctx(), first, firstParty.Code, sharedKey)
	if err != nil {
		t.Fatalf("同一领队重复提交同一申请编号应原样返回首次结果: %v", err)
	}
	if replay.Code != firstView.Code || replay.Version != firstView.Version {
		t.Fatalf("重放结果与首次结果不一致: %+v vs %+v", replay, firstView)
	}
	if reserved, _ := h.permitWindow(valleyTrail, 1); reserved != 5 {
		t.Fatalf("重放不得重复占用名额，实际已占用 %d", reserved)
	}
}
