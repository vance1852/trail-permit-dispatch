package domain

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestTrailAcceptsNewPartiesOnlyWhenOpen(t *testing.T) {
	open := &Trail{Status: TrailOpen}
	if !open.AcceptsNewParties() {
		t.Fatal("开放线路应接受新队伍")
	}
	for _, status := range []TrailStatus{TrailSeasonClosed, TrailSuspended} {
		trail := &Trail{Status: status}
		if trail.AcceptsNewParties() {
			t.Fatalf("%s 状态不应接受新队伍", status)
		}
	}
}

func TestTrailValidatePartySize(t *testing.T) {
	trail := &Trail{Code: "AOMEN-RIDGE", MinPartySize: 3, MaxPartySize: 8}
	if err := trail.ValidatePartySize(3); err != nil {
		t.Fatalf("下限人数应通过: %v", err)
	}
	if err := trail.ValidatePartySize(8); err != nil {
		t.Fatalf("上限人数应通过: %v", err)
	}
	if err := trail.ValidatePartySize(2); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("低于下限应被拒绝，实际 %v", err)
	}
	if err := trail.ValidatePartySize(9); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("超过上限应被拒绝，实际 %v", err)
	}
	var missing *Trail
	if err := missing.ValidatePartySize(4); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("空线路应返回 internal，实际 %v", err)
	}
}

func TestTrailPermitDeadline(t *testing.T) {
	dayStart := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	trail := &Trail{PermitCutoffHours: 12}
	deadline := trail.PermitDeadline(dayStart)
	if !deadline.Equal(dayStart.Add(-12 * time.Hour)) {
		t.Fatalf("申报截止时间计算错误: %s", deadline)
	}
	immediate := &Trail{PermitCutoffHours: 0}
	if !immediate.PermitDeadline(dayStart).Equal(dayStart) {
		t.Fatal("零提前量时截止时间应等于出行日零点")
	}
}

func TestValidateTrailCode(t *testing.T) {
	if err := ValidateTrailCode("AOMEN-RIDGE_2"); err != nil {
		t.Fatalf("合法编码被拒绝: %v", err)
	}
	for _, invalid := range []string{"", "   ", "北岭线路", "code with space", "code/slash"} {
		if err := ValidateTrailCode(invalid); err == nil {
			t.Fatalf("非法编码 %q 应被拒绝", invalid)
		}
	}
}

func TestPermitWindowReservationGuards(t *testing.T) {
	window := &PermitWindow{HikeDay: "2026-09-12", QuotaTotal: 10, QuotaReserved: 6}
	if window.Remaining() != 4 {
		t.Fatalf("剩余名额计算错误: %d", window.Remaining())
	}
	if err := window.CanReserve(4); err != nil {
		t.Fatalf("恰好占满应允许: %v", err)
	}
	err := window.CanReserve(5)
	if !apperr.Is(err, apperr.CodeQuotaExhausted) {
		t.Fatalf("超额占用应返回 quota_exhausted，实际 %v", err)
	}
	if err := window.CanReserve(0); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("零座位应返回 invalid_argument，实际 %v", err)
	}

	closedAt := time.Now()
	window.ClosedAt = &closedAt
	if err := window.CanReserve(1); !apperr.Is(err, apperr.CodeStateInvalid) {
		t.Fatalf("已关闭窗口应返回 state_invalid，实际 %v", err)
	}

	var absent *PermitWindow
	if err := absent.CanReserve(1); !apperr.Is(err, apperr.CodeNotFound) {
		t.Fatalf("缺失窗口应返回 not_found，实际 %v", err)
	}
	overbooked := &PermitWindow{QuotaTotal: 3, QuotaReserved: 5}
	if overbooked.Remaining() != 0 {
		t.Fatalf("剩余名额不应为负数: %d", overbooked.Remaining())
	}
}

func TestCheckpointDeadlineAndReportClassification(t *testing.T) {
	plannedStart := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	checkpoint := &Checkpoint{CutoffMinutes: 90}
	if !checkpoint.Deadline(plannedStart).Equal(plannedStart.Add(90 * time.Minute)) {
		t.Fatal("打点截止时间计算错误")
	}

	cases := []struct {
		name     string
		reported time.Time
		grace    int
		expected ReportStatus
	}{
		{"提前到达", plannedStart.Add(30 * time.Minute), 30, ReportOnTime},
		{"恰好在截止时刻", plannedStart.Add(90 * time.Minute), 30, ReportOnTime},
		{"超出截止但在宽限内", plannedStart.Add(100 * time.Minute), 30, ReportLate},
		{"恰好在宽限边界", plannedStart.Add(120 * time.Minute), 30, ReportLate},
		{"超出宽限", plannedStart.Add(121 * time.Minute), 30, ReportMissed},
		{"无宽限期即超时", plannedStart.Add(91 * time.Minute), 0, ReportMissed},
	}
	for _, testCase := range cases {
		got := ClassifyReport(plannedStart, testCase.reported, 90, testCase.grace)
		if got != testCase.expected {
			t.Fatalf("%s: 期望 %s，实际 %s", testCase.name, testCase.expected, got)
		}
	}
}
