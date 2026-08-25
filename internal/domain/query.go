package domain

import (
	"strings"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// Default and maximum page sizes of every list endpoint.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// PageRequest is a validated pagination and sorting instruction. Sort keys are
// whitelisted per repository so no caller can inject a SQL fragment.
type PageRequest struct {
	Page     int
	Size     int
	SortBy   string
	SortDesc bool
}

// NewPageRequest validates external pagination input against the allowed sort
// keys of the target repository.
func NewPageRequest(page, size int, sortBy string, desc bool, allowed []string) (PageRequest, error) {
	if page < 0 {
		return PageRequest{}, apperr.New(apperr.CodeInvalidArgument, "页码不能为负数").WithField("page")
	}
	if page == 0 {
		page = 1
	}
	if size < 0 {
		return PageRequest{}, apperr.New(apperr.CodeInvalidArgument, "每页数量不能为负数").WithField("size")
	}
	if size == 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		return PageRequest{}, apperr.Newf(apperr.CodeInvalidArgument, "每页数量不能超过 %d", maxPageSize).WithField("size")
	}
	key := strings.TrimSpace(sortBy)
	if key == "" {
		if len(allowed) == 0 {
			return PageRequest{}, apperr.New(apperr.CodeInternal, "未配置排序字段白名单")
		}
		key = allowed[0]
	}
	if !containsString(allowed, key) {
		return PageRequest{}, apperr.Newf(apperr.CodeInvalidArgument, "不支持按 %q 排序", sortBy).WithField("sort_by")
	}
	return PageRequest{Page: page, Size: size, SortBy: key, SortDesc: desc}, nil
}

// Offset returns the SQL offset of the request.
func (p PageRequest) Offset() int { return (p.Page - 1) * p.Size }

// Limit returns the SQL limit of the request.
func (p PageRequest) Limit() int { return p.Size }

// Direction renders the SQL sort direction.
func (p PageRequest) Direction() string {
	if p.SortDesc {
		return "DESC"
	}
	return "ASC"
}

// Page is one page of list results plus the total count.
type Page[T any] struct {
	Items      []T `json:"items"`
	Total      int `json:"total"`
	Page       int `json:"page"`
	Size       int `json:"size"`
	TotalPages int `json:"total_pages"`
}

// NewPage assembles a page result and never returns a nil item slice so JSON
// clients always receive an array.
func NewPage[T any](items []T, total int, req PageRequest) Page[T] {
	if items == nil {
		items = []T{}
	}
	pages := 0
	if req.Size > 0 {
		pages = (total + req.Size - 1) / req.Size
	}
	return Page[T]{Items: items, Total: total, Page: req.Page, Size: req.Size, TotalPages: pages}
}

// PartyFilter is the combined filter of the party list endpoint.
type PartyFilter struct {
	LeaderID  int64
	TrailID   int64
	States    []PartyState
	HikeDayGE string
	HikeDayLE string
	Search    string
}

// Normalize validates and canonicalises the filter values.
func (f PartyFilter) Normalize() (PartyFilter, error) {
	normalized := f
	normalized.Search = strings.TrimSpace(f.Search)
	if len(normalized.Search) > 60 {
		return PartyFilter{}, apperr.New(apperr.CodeInvalidArgument, "搜索关键词过长").WithField("search")
	}
	seen := make(map[PartyState]bool, len(f.States))
	states := make([]PartyState, 0, len(f.States))
	for _, state := range f.States {
		if !state.Valid() {
			return PartyFilter{}, apperr.Newf(apperr.CodeInvalidArgument, "未知队伍状态 %q", state).WithField("state")
		}
		if seen[state] {
			continue
		}
		seen[state] = true
		states = append(states, state)
	}
	normalized.States = states
	if normalized.HikeDayGE != "" && normalized.HikeDayLE != "" && normalized.HikeDayGE > normalized.HikeDayLE {
		return PartyFilter{}, apperr.New(apperr.CodeInvalidArgument, "出行日期区间的起始不能晚于结束").WithField("hike_day_from")
	}
	return normalized, nil
}

// IncidentFilter is the combined filter of the incident list endpoint.
type IncidentFilter struct {
	PartyID    int64
	States     []IncidentState
	Severities []Severity
}

// Normalize validates the incident filter values.
func (f IncidentFilter) Normalize() (IncidentFilter, error) {
	normalized := f
	for _, state := range f.States {
		if !state.Valid() {
			return IncidentFilter{}, apperr.Newf(apperr.CodeInvalidArgument, "未知事故状态 %q", state).WithField("state")
		}
	}
	for _, severity := range f.Severities {
		switch severity {
		case SeverityMinor, SeverityMajor, SeverityCritical:
		default:
			return IncidentFilter{}, apperr.Newf(apperr.CodeInvalidArgument, "未知事故级别 %q", severity).WithField("severity")
		}
	}
	return normalized, nil
}

// SettlementFilter is the combined filter of the settlement list endpoint.
type SettlementFilter struct {
	PartyID int64
	States  []SettlementState
}

// Normalize validates the settlement filter values.
func (f SettlementFilter) Normalize() (SettlementFilter, error) {
	for _, state := range f.States {
		if !state.Valid() {
			return SettlementFilter{}, apperr.Newf(apperr.CodeInvalidArgument, "未知结算状态 %q", state).WithField("state")
		}
	}
	return f, nil
}

// AuditFilter is the combined filter of the audit trail endpoint.
type AuditFilter struct {
	ActorID    int64
	Action     string
	ObjectType string
	ObjectID   string
	Result     AuditResult
}

// Normalize validates the audit filter values.
func (f AuditFilter) Normalize() (AuditFilter, error) {
	normalized := f
	normalized.Action = strings.TrimSpace(f.Action)
	normalized.ObjectType = strings.TrimSpace(f.ObjectType)
	normalized.ObjectID = strings.TrimSpace(f.ObjectID)
	switch normalized.Result {
	case "", AuditSuccess, AuditFailure:
	default:
		return AuditFilter{}, apperr.Newf(apperr.CodeInvalidArgument, "未知审计结果 %q", f.Result).WithField("result")
	}
	return normalized, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
