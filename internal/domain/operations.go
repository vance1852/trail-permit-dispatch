package domain

import (
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// Severity is the triage level of a trail incident.
type Severity string

const (
	// SeverityMinor covers delays that the party can still resolve alone.
	SeverityMinor Severity = "minor"
	// SeverityMajor covers overdue checkpoints and lost route situations.
	SeverityMajor Severity = "major"
	// SeverityCritical covers injuries and rescue operations.
	SeverityCritical Severity = "critical"
)

// ParseSeverity validates an external severity value.
func ParseSeverity(value string) (Severity, error) {
	switch Severity(strings.TrimSpace(strings.ToLower(value))) {
	case SeverityMinor:
		return SeverityMinor, nil
	case SeverityMajor:
		return SeverityMajor, nil
	case SeverityCritical:
		return SeverityCritical, nil
	default:
		return "", apperr.Newf(apperr.CodeInvalidArgument, "未知事故级别 %q", value).WithField("severity")
	}
}

// EscalationDelay is the wait before the next escalation attempt.
func (s Severity) EscalationDelay() time.Duration {
	switch s {
	case SeverityCritical:
		return 2 * time.Minute
	case SeverityMajor:
		return 10 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// MaxEscalations caps how often an unresolved incident is escalated.
func (s Severity) MaxEscalations() int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityMajor:
		return 3
	default:
		return 1
	}
}

// ForcesPartyAbort reports whether opening the incident must stop the party.
func (s Severity) ForcesPartyAbort() bool { return s == SeverityCritical }

// IncidentState is the lifecycle state of an incident.
type IncidentState string

const (
	// IncidentOpen was just reported and waits for the first escalation.
	IncidentOpen IncidentState = "open"
	// IncidentEscalated was handed to the rescue coordination channel.
	IncidentEscalated IncidentState = "escalated"
	// IncidentResolved has an operator resolution note.
	IncidentResolved IncidentState = "resolved"
	// IncidentClosed is archived after the resolution was reviewed.
	IncidentClosed IncidentState = "closed"
)

var incidentTransitions = map[IncidentState][]IncidentState{
	IncidentOpen:      {IncidentEscalated, IncidentResolved},
	IncidentEscalated: {IncidentEscalated, IncidentResolved},
	IncidentResolved:  {IncidentClosed},
	IncidentClosed:    nil,
}

// Valid reports whether the value is a known incident state.
func (s IncidentState) Valid() bool {
	_, ok := incidentTransitions[s]
	return ok
}

// Terminal reports whether the incident accepts no further transition.
func (s IncidentState) Terminal() bool { return len(incidentTransitions[s]) == 0 }

// ValidateIncidentTransition rejects illegal incident transitions. Repeated
// escalation is legal on purpose because each attempt is a separate hand-off.
func ValidateIncidentTransition(from, to IncidentState) error {
	if !from.Valid() {
		return apperr.Newf(apperr.CodeInternal, "事故状态 %q 未定义", from)
	}
	if !to.Valid() {
		return apperr.Newf(apperr.CodeInvalidArgument, "目标事故状态 %q 未定义", to)
	}
	for _, allowed := range incidentTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return apperr.Newf(apperr.CodeStateInvalid, "事故状态不允许从 %s 变更为 %s", from, to)
}

// Incident is a trail safety event attached to one party.
type Incident struct {
	ID              int64
	PartyID         int64
	Kind            string
	Severity        Severity
	State           IncidentState
	Summary         string
	CheckpointSeq   int
	EscalationCount int
	OpenedBy        int64
	OpenedAt        time.Time
	ResolvedAt      *time.Time
	Resolution      string
	Version         int64
	UpdatedAt       time.Time
}

// EscalationExhausted reports whether the incident reached its attempt cap.
func (i *Incident) EscalationExhausted() bool {
	return i != nil && i.EscalationCount >= i.Severity.MaxEscalations()
}

// SettlementState is the lifecycle state of a permit fee settlement.
type SettlementState string

const (
	// SettlementPending waits for the background settlement job.
	SettlementPending SettlementState = "pending"
	// SettlementSettled recorded a successful payment reference.
	SettlementSettled SettlementState = "settled"
	// SettlementWaived was written off by the trail authority.
	SettlementWaived SettlementState = "waived"
	// SettlementFailed exhausted its retries and needs manual handling.
	SettlementFailed SettlementState = "failed"
)

var settlementTransitions = map[SettlementState][]SettlementState{
	SettlementPending: {SettlementSettled, SettlementWaived, SettlementFailed},
	SettlementFailed:  {SettlementSettled, SettlementWaived},
	SettlementSettled: nil,
	SettlementWaived:  nil,
}

// Valid reports whether the value is a known settlement state.
func (s SettlementState) Valid() bool {
	_, ok := settlementTransitions[s]
	return ok
}

// ValidateSettlementTransition rejects illegal settlement transitions.
func ValidateSettlementTransition(from, to SettlementState) error {
	if !from.Valid() {
		return apperr.Newf(apperr.CodeInternal, "结算状态 %q 未定义", from)
	}
	if !to.Valid() {
		return apperr.Newf(apperr.CodeInvalidArgument, "目标结算状态 %q 未定义", to)
	}
	for _, allowed := range settlementTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return apperr.Newf(apperr.CodeStateInvalid, "结算状态不允许从 %s 变更为 %s", from, to)
}

// Settlement is the permit fee record of one party.
type Settlement struct {
	ID           int64
	PartyID      int64
	Seats        int
	UnitFeeCents int64
	TotalCents   int64
	State        SettlementState
	Reference    string
	FailureNote  string
	SettledAt    *time.Time
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PermitFee computes the permit fee in cents. Harder trails and weekend permits
// cost more, and large parties receive a group rebate.
func PermitFee(unitFeeCents int64, difficulty, seats int, weekend bool) int64 {
	if seats <= 0 {
		return 0
	}
	unit := unitFeeCents + int64(difficulty-1)*1500
	if weekend {
		unit += unit / 5
	}
	total := unit * int64(seats)
	if seats >= 8 {
		total -= total / 10
	}
	return total
}

// AuditResult records whether an audited action succeeded.
type AuditResult string

const (
	// AuditSuccess marks an accepted action.
	AuditSuccess AuditResult = "success"
	// AuditFailure marks a rejected action.
	AuditFailure AuditResult = "failure"
)

// AuditEvent is one immutable audit trail entry.
type AuditEvent struct {
	ID         int64
	RequestID  string
	ActorID    int64
	ActorRole  Role
	Action     string
	ObjectType string
	ObjectID   string
	Result     AuditResult
	Detail     string
	CreatedAt  time.Time
}

// JobKind enumerates the background job types of the platform.
type JobKind string

const (
	// JobNotifyDispatch informs the rescue base about a confirmed party.
	JobNotifyDispatch JobKind = "notify_dispatch"
	// JobSettleParty settles the permit fee of a finished party.
	JobSettleParty JobKind = "settle_party"
	// JobEscalateIncident hands an unresolved incident to the next level.
	JobEscalateIncident JobKind = "escalate_incident"
	// JobSweepOverdue scans parties that missed a mandatory checkpoint.
	JobSweepOverdue JobKind = "sweep_overdue_checkpoints"
	// JobExpireSessions revokes sessions that outlived their time to live.
	JobExpireSessions JobKind = "expire_sessions"
)

// ParseJobKind validates an external job kind value.
func ParseJobKind(value string) (JobKind, error) {
	switch JobKind(strings.TrimSpace(value)) {
	case JobNotifyDispatch:
		return JobNotifyDispatch, nil
	case JobSettleParty:
		return JobSettleParty, nil
	case JobEscalateIncident:
		return JobEscalateIncident, nil
	case JobSweepOverdue:
		return JobSweepOverdue, nil
	case JobExpireSessions:
		return JobExpireSessions, nil
	default:
		return "", apperr.Newf(apperr.CodeInvalidArgument, "未知后台作业类型 %q", value).WithField("kind")
	}
}

// JobState is the lifecycle state of a background job.
type JobState string

const (
	// JobQueued waits for a worker lease.
	JobQueued JobState = "queued"
	// JobRunning is leased by exactly one worker.
	JobRunning JobState = "running"
	// JobDone finished successfully.
	JobDone JobState = "done"
	// JobFailed exhausted its attempts permanently.
	JobFailed JobState = "failed"
)

// Job is one unit of background work.
type Job struct {
	ID          int64
	Kind        JobKind
	Payload     string
	State       JobState
	Attempts    int
	MaxAttempts int
	RunAt       time.Time
	LockedBy    string
	LockedUntil *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AttemptsRemaining reports how many attempts the job may still consume.
func (j *Job) AttemptsRemaining() int {
	if j == nil {
		return 0
	}
	remaining := j.MaxAttempts - j.Attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Exhausted reports whether the job may no longer be retried.
func (j *Job) Exhausted() bool { return j != nil && j.Attempts >= j.MaxAttempts }

// Backoff returns the delay before the given attempt is retried. The delay grows
// exponentially and is capped so a poisoned job cannot starve the queue.
func Backoff(attempts int, base, max time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := base
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}
