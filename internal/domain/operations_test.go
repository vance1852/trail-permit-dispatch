package domain

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestIncidentTransitionsAllowRepeatedEscalation(t *testing.T) {
	if err := ValidateIncidentTransition(IncidentOpen, IncidentEscalated); err != nil {
		t.Fatalf("首次升级应合法: %v", err)
	}
	if err := ValidateIncidentTransition(IncidentEscalated, IncidentEscalated); err != nil {
		t.Fatalf("重复升级是独立的通报动作，应合法: %v", err)
	}
	if err := ValidateIncidentTransition(IncidentResolved, IncidentEscalated); err == nil {
		t.Fatal("已处置事故不应再次升级")
	}
	if err := ValidateIncidentTransition(IncidentClosed, IncidentResolved); err == nil {
		t.Fatal("归档事故不应回到处置状态")
	}
	if err := ValidateIncidentTransition(IncidentResolved, IncidentClosed); err != nil {
		t.Fatalf("处置后归档应合法: %v", err)
	}
	if !IncidentClosed.Terminal() {
		t.Fatal("归档应为终态")
	}
}

func TestSeverityEscalationPolicy(t *testing.T) {
	if SeverityCritical.EscalationDelay() >= SeverityMajor.EscalationDelay() {
		t.Fatal("严重事故的升级间隔应短于一般事故")
	}
	if SeverityMinor.MaxEscalations() != 1 {
		t.Fatalf("轻微事故应只升级一次，实际 %d", SeverityMinor.MaxEscalations())
	}
	if SeverityCritical.MaxEscalations() <= SeverityMajor.MaxEscalations() {
		t.Fatal("严重事故的升级上限应高于一般事故")
	}
	if !SeverityCritical.ForcesPartyAbort() || SeverityMajor.ForcesPartyAbort() {
		t.Fatal("只有严重事故才强制中止队伍")
	}
	if _, err := ParseSeverity("catastrophic"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知级别应被拒绝，实际 %v", err)
	}
	parsed, err := ParseSeverity(" Major ")
	if err != nil || parsed != SeverityMajor {
		t.Fatalf("级别解析失败: %v %v", parsed, err)
	}
}

func TestIncidentEscalationExhausted(t *testing.T) {
	incident := &Incident{Severity: SeverityMajor, EscalationCount: 2}
	if incident.EscalationExhausted() {
		t.Fatal("未达到上限时不应判定耗尽")
	}
	incident.EscalationCount = SeverityMajor.MaxEscalations()
	if !incident.EscalationExhausted() {
		t.Fatal("达到上限时应判定耗尽")
	}
}

func TestSettlementTransitions(t *testing.T) {
	if err := ValidateSettlementTransition(SettlementPending, SettlementSettled); err != nil {
		t.Fatalf("待结算到已结算应合法: %v", err)
	}
	if err := ValidateSettlementTransition(SettlementFailed, SettlementSettled); err != nil {
		t.Fatalf("失败后重试结算应合法: %v", err)
	}
	if err := ValidateSettlementTransition(SettlementSettled, SettlementWaived); err == nil {
		t.Fatal("已结算不应再减免")
	}
	if err := ValidateSettlementTransition(SettlementWaived, SettlementSettled); err == nil {
		t.Fatal("已减免不应再结算")
	}
	if err := ValidateSettlementTransition(SettlementState("refunded"), SettlementSettled); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("未知来源状态应返回 internal，实际 %v", err)
	}
}

func TestPermitFeeRules(t *testing.T) {
	base := PermitFee(4500, 1, 1, false)
	if base != 4500 {
		t.Fatalf("一级线路单人工作日费用应为 4500，实际 %d", base)
	}
	harder := PermitFee(4500, 3, 1, false)
	if harder != 4500+2*1500 {
		t.Fatalf("难度附加费计算错误，实际 %d", harder)
	}
	weekend := PermitFee(4500, 1, 1, true)
	if weekend != 5400 {
		t.Fatalf("周末应上浮两成，实际 %d", weekend)
	}
	group := PermitFee(4500, 1, 8, false)
	if group != 4500*8-(4500*8)/10 {
		t.Fatalf("8 人及以上应享受一成团队折扣，实际 %d", group)
	}
	small := PermitFee(4500, 1, 7, false)
	if small != 4500*7 {
		t.Fatalf("7 人不应享受团队折扣，实际 %d", small)
	}
	if PermitFee(4500, 2, 0, false) != 0 {
		t.Fatal("零座位应计费为 0")
	}
}

func TestBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	base := time.Second
	max := 10 * time.Second
	if got := Backoff(1, base, max); got != time.Second {
		t.Fatalf("首次重试应等于基准值，实际 %s", got)
	}
	if got := Backoff(2, base, max); got != 2*time.Second {
		t.Fatalf("第二次重试应加倍，实际 %s", got)
	}
	if got := Backoff(3, base, max); got != 4*time.Second {
		t.Fatalf("第三次重试应为 4s，实际 %s", got)
	}
	if got := Backoff(9, base, max); got != max {
		t.Fatalf("退避应被上限截断，实际 %s", got)
	}
	if got := Backoff(0, base, max); got != base {
		t.Fatalf("非法尝试次数应回退到基准值，实际 %s", got)
	}
}

func TestJobHelpers(t *testing.T) {
	job := &Job{Attempts: 2, MaxAttempts: 5}
	if job.AttemptsRemaining() != 3 {
		t.Fatalf("剩余尝试次数计算错误: %d", job.AttemptsRemaining())
	}
	if job.Exhausted() {
		t.Fatal("尚有剩余次数时不应判定耗尽")
	}
	job.Attempts = 5
	if !job.Exhausted() || job.AttemptsRemaining() != 0 {
		t.Fatal("达到上限时应判定耗尽且剩余为 0")
	}
	if _, err := ParseJobKind("send_sms"); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知作业类型应被拒绝，实际 %v", err)
	}
	for _, kind := range []JobKind{JobNotifyDispatch, JobSettleParty, JobEscalateIncident, JobSweepOverdue, JobExpireSessions} {
		parsed, err := ParseJobKind(string(kind))
		if err != nil || parsed != kind {
			t.Fatalf("作业类型 %s 应可解析: %v", kind, err)
		}
	}
}
