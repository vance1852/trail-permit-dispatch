package apptest

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

// TestOverdueSweepKeepsTrailCheckpointsIntact 在一支队伍已经上报部分打点后触发
// 在途超时扫描，校验线路的打点定义不会被扫描过程改写，必经打点约束依旧生效。
func TestOverdueSweepKeepsTrailCheckpointsIntact(t *testing.T) {
	h := newHarness(t)
	ranger := h.ranger()
	reporting := h.leader()
	silent := h.newLeader("club-silent-0006@trailpermit.test")

	before, err := h.app.Catalog.GetTrail(h.ctx(), valleyTrail)
	if err != nil {
		t.Fatalf("读取线路详情失败: %v", err)
	}
	if len(before.Checkpoints) != 3 {
		t.Fatalf("前置条件要求线路有 3 个打点，实际 %d", len(before.Checkpoints))
	}

	reportingParty := h.dispatchParty(reporting, ranger, valleyTrail, 2, 1)
	silentParty := h.dispatchParty(silent, ranger, valleyTrail, 2, 1)

	h.clock.Set(h.plannedStart(1, 6).Add(40 * time.Minute))
	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), reporting, reportingParty.Code, dispatch.ReportInput{Seq: 1, HeadCount: 2}); err != nil {
		t.Fatalf("上报第一个打点失败: %v", err)
	}

	h.clock.Set(h.plannedStart(1, 6).Add(95 * time.Minute))
	sweep, err := h.app.Dispatch.SweepOverdueCheckpoints(h.ctx(), 10)
	if err != nil {
		t.Fatalf("超时扫描失败: %v", err)
	}
	if sweep.Scanned != 2 {
		t.Fatalf("应扫描到 2 支在途队伍，实际 %d", sweep.Scanned)
	}
	if sweep.IncidentsOpened != 1 {
		t.Fatalf("只有未上报的队伍应触发超时事故，实际登记 %d 起", sweep.IncidentsOpened)
	}

	after, err := h.app.Catalog.GetTrail(h.ctx(), valleyTrail)
	if err != nil {
		t.Fatalf("读取线路详情失败: %v", err)
	}
	if len(after.Checkpoints) != len(before.Checkpoints) {
		t.Fatalf("扫描不得改变线路打点数量: 期望 %d 实际 %d", len(before.Checkpoints), len(after.Checkpoints))
	}
	for i := range before.Checkpoints {
		if after.Checkpoints[i].Seq != before.Checkpoints[i].Seq {
			t.Fatalf("扫描后第 %d 个打点序号被改写: 期望 %d 实际 %d", i+1, before.Checkpoints[i].Seq, after.Checkpoints[i].Seq)
		}
		if after.Checkpoints[i].Mandatory != before.Checkpoints[i].Mandatory {
			t.Fatalf("扫描后打点 %d 的必经标记被改写", before.Checkpoints[i].Seq)
		}
	}

	h.clock.Set(h.plannedStart(1, 6).Add(290 * time.Minute))
	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), silent, silentParty.Code, dispatch.ReportInput{Seq: 3, HeadCount: 2}); err != nil {
		t.Fatalf("上报最后一个打点失败: %v", err)
	}
	_, err = h.app.Dispatch.CompleteParty(h.ctx(), silent, silentParty.Code)
	if !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("漏报必经打点的队伍不得结束行程，实际 %v", err)
	}

	if _, err := h.app.Dispatch.ReportCheckpoint(h.ctx(), reporting, reportingParty.Code, dispatch.ReportInput{Seq: 3, HeadCount: 2}); err != nil {
		t.Fatalf("补齐打点失败: %v", err)
	}
	if _, err := h.app.Dispatch.CompleteParty(h.ctx(), reporting, reportingParty.Code); err != nil {
		t.Fatalf("必经打点齐备的队伍应能正常结束行程: %v", err)
	}
}
