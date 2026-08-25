package domain

import (
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestNewPageRequestDefaultsAndWhitelist(t *testing.T) {
	allowed := []string{"hike_day", "created_at"}
	page, err := NewPageRequest(0, 0, "", false, allowed)
	if err != nil {
		t.Fatalf("默认分页应可用: %v", err)
	}
	if page.Page != 1 || page.Size != 20 || page.SortBy != "hike_day" {
		t.Fatalf("默认分页取值错误: %+v", page)
	}
	if page.Offset() != 0 || page.Limit() != 20 || page.Direction() != "ASC" {
		t.Fatalf("分页派生值错误: offset=%d limit=%d dir=%s", page.Offset(), page.Limit(), page.Direction())
	}

	second, err := NewPageRequest(3, 15, "created_at", true, allowed)
	if err != nil {
		t.Fatalf("合法分页被拒绝: %v", err)
	}
	if second.Offset() != 30 || second.Direction() != "DESC" {
		t.Fatalf("第三页偏移或方向错误: %+v", second)
	}

	if _, err := NewPageRequest(1, 20, "leader_id", false, allowed); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("白名单外排序字段应被拒绝，实际 %v", err)
	}
	if _, err := NewPageRequest(-1, 20, "hike_day", false, allowed); err == nil {
		t.Fatal("负页码应被拒绝")
	}
	if _, err := NewPageRequest(1, 101, "hike_day", false, allowed); err == nil {
		t.Fatal("超过上限的每页数量应被拒绝")
	}
	if _, err := NewPageRequest(1, -5, "hike_day", false, allowed); err == nil {
		t.Fatal("负的每页数量应被拒绝")
	}
	if _, err := NewPageRequest(1, 10, "", false, nil); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("缺少白名单应返回 internal，实际 %v", err)
	}
}

func TestNewPageAlwaysReturnsArrayAndPageCount(t *testing.T) {
	request, err := NewPageRequest(1, 10, "hike_day", false, []string{"hike_day"})
	if err != nil {
		t.Fatalf("构造分页失败: %v", err)
	}
	empty := NewPage[string](nil, 0, request)
	if empty.Items == nil {
		t.Fatal("空结果的条目必须是空数组而不是 nil")
	}
	if empty.TotalPages != 0 {
		t.Fatalf("零总数的总页数应为 0，实际 %d", empty.TotalPages)
	}
	filled := NewPage([]string{"a", "b"}, 25, request)
	if filled.TotalPages != 3 {
		t.Fatalf("25 条按每页 10 条应为 3 页，实际 %d", filled.TotalPages)
	}
	if filled.Total != 25 || filled.Page != 1 || filled.Size != 10 {
		t.Fatalf("分页元数据错误: %+v", filled)
	}
}

func TestPartyFilterNormalizeDeduplicatesStates(t *testing.T) {
	filter := PartyFilter{
		States: []PartyState{PartyDraft, PartyDraft, PartyOnTrail},
		Search: "  TP-2026  ",
	}
	normalized, err := filter.Normalize()
	if err != nil {
		t.Fatalf("合法筛选被拒绝: %v", err)
	}
	if len(normalized.States) != 2 {
		t.Fatalf("重复状态应被去重，实际 %v", normalized.States)
	}
	if normalized.Search != "TP-2026" {
		t.Fatalf("搜索词应去除首尾空白，实际 %q", normalized.Search)
	}

	if _, err := (PartyFilter{States: []PartyState{"flying"}}).Normalize(); !apperr.Is(err, apperr.CodeInvalidArgument) {
		t.Fatalf("未知状态应被拒绝，实际 %v", err)
	}
	if _, err := (PartyFilter{HikeDayGE: "2026-09-20", HikeDayLE: "2026-09-01"}).Normalize(); err == nil {
		t.Fatal("倒置的日期区间应被拒绝")
	}
	long := PartyFilter{Search: string(make([]byte, 61))}
	if _, err := long.Normalize(); err == nil {
		t.Fatal("过长搜索词应被拒绝")
	}
}

func TestIncidentAndSettlementFilterNormalize(t *testing.T) {
	if _, err := (IncidentFilter{States: []IncidentState{"pending"}}).Normalize(); err == nil {
		t.Fatal("未知事故状态应被拒绝")
	}
	if _, err := (IncidentFilter{Severities: []Severity{"fatal"}}).Normalize(); err == nil {
		t.Fatal("未知事故级别应被拒绝")
	}
	valid := IncidentFilter{States: []IncidentState{IncidentOpen}, Severities: []Severity{SeverityMajor}}
	if _, err := valid.Normalize(); err != nil {
		t.Fatalf("合法事故筛选被拒绝: %v", err)
	}
	if _, err := (SettlementFilter{States: []SettlementState{"refunded"}}).Normalize(); err == nil {
		t.Fatal("未知结算状态应被拒绝")
	}
	if _, err := (SettlementFilter{States: []SettlementState{SettlementPending}}).Normalize(); err != nil {
		t.Fatal("合法结算筛选被拒绝")
	}
}

func TestAuditFilterNormalize(t *testing.T) {
	filter := AuditFilter{Action: "  party.create  ", ObjectType: " party ", ObjectID: " TP-1 ", Result: AuditSuccess}
	normalized, err := filter.Normalize()
	if err != nil {
		t.Fatalf("合法审计筛选被拒绝: %v", err)
	}
	if normalized.Action != "party.create" || normalized.ObjectType != "party" || normalized.ObjectID != "TP-1" {
		t.Fatalf("审计筛选未正确去空白: %+v", normalized)
	}
	if _, err := (AuditFilter{Result: AuditResult("unknown")}).Normalize(); err == nil {
		t.Fatal("未知审计结果应被拒绝")
	}
	if _, err := (AuditFilter{}).Normalize(); err != nil {
		t.Fatalf("空审计筛选应合法: %v", err)
	}
}
