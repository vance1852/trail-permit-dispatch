package domain

import (
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// PartyState is the lifecycle state of a hiking party.
type PartyState string

const (
	// PartyDraft is a plan that still collects members.
	PartyDraft PartyState = "draft"
	// PartyPermitReserved holds seats on a permit window.
	PartyPermitReserved PartyState = "permit_reserved"
	// PartyConfirmed passed the ranger review and waits for release.
	PartyConfirmed PartyState = "confirmed"
	// PartyOnTrail is currently hiking and must report checkpoints.
	PartyOnTrail PartyState = "on_trail"
	// PartyCompleted finished the route and reported the last checkpoint.
	PartyCompleted PartyState = "completed"
	// PartyCancelled was withdrawn before entering the trail.
	PartyCancelled PartyState = "cancelled"
	// PartyAborted was stopped by the trail authority after an incident.
	PartyAborted PartyState = "aborted"
)

// partyTransitions is the single source of truth for the party state machine.
var partyTransitions = map[PartyState][]PartyState{
	PartyDraft:          {PartyPermitReserved, PartyCancelled},
	PartyPermitReserved: {PartyConfirmed, PartyCancelled},
	PartyConfirmed:      {PartyOnTrail, PartyCancelled, PartyAborted},
	PartyOnTrail:        {PartyCompleted, PartyAborted},
	PartyCompleted:      nil,
	PartyCancelled:      nil,
	PartyAborted:        nil,
}

// Valid reports whether the value is a known party state.
func (s PartyState) Valid() bool {
	_, ok := partyTransitions[s]
	return ok
}

// Terminal reports whether the state accepts no further transition.
func (s PartyState) Terminal() bool {
	return len(partyTransitions[s]) == 0
}

// HoldsPermit reports whether the state currently occupies permit seats. Seats
// are released exactly when the party leaves this set.
func (s PartyState) HoldsPermit() bool {
	switch s {
	case PartyPermitReserved, PartyConfirmed, PartyOnTrail:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether next is reachable from s.
func (s PartyState) CanTransitionTo(next PartyState) bool {
	for _, allowed := range partyTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// ValidatePartyTransition rejects illegal party state machine transitions.
func ValidatePartyTransition(from, to PartyState) error {
	if !from.Valid() {
		return apperr.Newf(apperr.CodeInternal, "队伍状态 %q 未定义", from)
	}
	if !to.Valid() {
		return apperr.Newf(apperr.CodeInvalidArgument, "目标状态 %q 未定义", to)
	}
	if from == to {
		return apperr.Newf(apperr.CodeStateInvalid, "队伍已处于 %s 状态", from)
	}
	if !from.CanTransitionTo(to) {
		return apperr.Newf(apperr.CodeStateInvalid, "队伍状态不允许从 %s 变更为 %s", from, to)
	}
	return nil
}

// Party is a planned or running hiking trip on one trail and one hike day.
type Party struct {
	ID             int64
	Code           string
	TrailID        int64
	LeaderID       int64
	PermitWindowID *int64
	HikeDay        string
	PlannedStart   time.Time
	PlannedEnd     time.Time
	Size           int
	State          PartyState
	Version        int64
	ContactPhone   string
	Notes          string
	ConfirmedAt    *time.Time
	DispatchedAt   *time.Time
	ClosedAt       *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// EnsureLeader verifies that the given account owns this party. Rangers use
// their own governance endpoints instead of this ownership check.
func (p *Party) EnsureLeader(userID int64) error {
	if p == nil {
		return apperr.New(apperr.CodeNotFound, "队伍不存在")
	}
	if p.LeaderID != userID {
		return apperr.New(apperr.CodePermissionDenied, "只有该队伍的领队可以操作")
	}
	return nil
}

// PlannedDuration returns the planned time on trail.
func (p *Party) PlannedDuration() time.Duration {
	if p == nil {
		return 0
	}
	return p.PlannedEnd.Sub(p.PlannedStart)
}

// ValidateSchedule checks the planned window of a party.
func ValidateSchedule(plannedStart, plannedEnd time.Time) error {
	if plannedStart.IsZero() || plannedEnd.IsZero() {
		return apperr.New(apperr.CodeInvalidArgument, "计划出发和返回时间必须填写").WithField("planned_start")
	}
	if !plannedEnd.After(plannedStart) {
		return apperr.New(apperr.CodeInvalidArgument, "计划返回时间必须晚于出发时间").WithField("planned_end")
	}
	if plannedEnd.Sub(plannedStart) > 72*time.Hour {
		return apperr.New(apperr.CodeInvalidArgument, "单次出行不得超过 72 小时").WithField("planned_end")
	}
	return nil
}

// MemberKind separates the leader record from ordinary companions.
type MemberKind string

const (
	// MemberLeader is the responsible leader entry of the party.
	MemberLeader MemberKind = "leader"
	// MemberCompanion is an ordinary hiking companion.
	MemberCompanion MemberKind = "companion"
)

// PartyMember is one registered hiker of a party.
type PartyMember struct {
	ID           int64
	PartyID      int64
	MemberRef    string
	DisplayName  string
	Kind         MemberKind
	WaiverSigned bool
	Phone        string
	JoinedAt     time.Time
}

// ValidateMemberRef checks the external member reference such as an id card
// suffix or a club membership number.
func ValidateMemberRef(ref string) error {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return apperr.New(apperr.CodeInvalidArgument, "队员编号不能为空").WithField("member_ref")
	}
	if len(trimmed) < 4 || len(trimmed) > 40 {
		return apperr.New(apperr.CodeInvalidArgument, "队员编号长度需在 4 到 40 个字符之间").WithField("member_ref")
	}
	return nil
}

// ValidatePhone checks an emergency contact number.
func ValidatePhone(phone string) error {
	trimmed := strings.TrimSpace(phone)
	if trimmed == "" {
		return apperr.New(apperr.CodeInvalidArgument, "紧急联系电话不能为空").WithField("phone")
	}
	digits := 0
	for _, r := range trimmed {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == '-' || r == ' ':
		default:
			return apperr.New(apperr.CodeInvalidArgument, "紧急联系电话包含非法字符").WithField("phone")
		}
	}
	if digits < 8 || digits > 15 {
		return apperr.New(apperr.CodeInvalidArgument, "紧急联系电话位数不正确").WithField("phone")
	}
	return nil
}

// ValidateMember runs every member level rule of the platform.
func ValidateMember(member PartyMember) error {
	if err := ValidateMemberRef(member.MemberRef); err != nil {
		return err
	}
	if err := ValidateDisplayName(member.DisplayName); err != nil {
		return err
	}
	if err := ValidatePhone(member.Phone); err != nil {
		return err
	}
	if member.Kind != MemberLeader && member.Kind != MemberCompanion {
		return apperr.Newf(apperr.CodeInvalidArgument, "队员类型 %q 未定义", member.Kind).WithField("kind")
	}
	return nil
}

// MemberBatchOutcome is the per item result of a batch member registration. The
// batch endpoint always reports every item so partial failures stay visible.
type MemberBatchOutcome struct {
	MemberRef string `json:"member_ref"`
	Accepted  bool   `json:"accepted"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ReadinessReport summarises whether a party may request its permit.
type ReadinessReport struct {
	Registered    int  `json:"registered"`
	Required      int  `json:"required"`
	WaiverMissing int  `json:"waiver_missing"`
	Ready         bool `json:"ready"`
}

// EvaluateReadiness derives the readiness of a party from its member records.
func EvaluateReadiness(size int, members []PartyMember) ReadinessReport {
	report := ReadinessReport{Required: size, Registered: len(members)}
	for i := range members {
		if !members[i].WaiverSigned {
			report.WaiverMissing++
		}
	}
	report.Ready = report.Registered == report.Required && report.WaiverMissing == 0
	return report
}
