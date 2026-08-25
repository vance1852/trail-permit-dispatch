// Package repository declares the persistence contracts of the platform. The
// interfaces live next to the services that consume them; the SQL bound
// implementations live in the sqliterepo sub package.
package repository

import (
	"context"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/domain"
)

// TxRunner executes a unit of work atomically. Repository calls made with the
// context passed to fn join the same transaction.
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// UserRepository persists accounts.
type UserRepository interface {
	Create(ctx context.Context, user *domain.User) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.User, error)
	ByEmail(ctx context.Context, email string) (*domain.User, error)
	UpdateCredential(ctx context.Context, userID int64, hash, salt string, iterations int, now time.Time) error
	UpdateStatus(ctx context.Context, userID int64, status domain.UserStatus, now time.Time) error
	CountByRole(ctx context.Context, role domain.Role) (int, error)
}

// SessionRepository persists revocable bearer sessions.
type SessionRepository interface {
	Create(ctx context.Context, session *domain.Session) (int64, error)
	ByTokenHash(ctx context.Context, tokenHash string) (*domain.Session, error)
	ByID(ctx context.Context, id int64) (*domain.Session, error)
	TouchLastSeen(ctx context.Context, id int64, at time.Time) error
	Revoke(ctx context.Context, id int64, at time.Time) (bool, error)
	RevokeAllForUser(ctx context.Context, userID int64, at time.Time) (int, error)
	RevokeExpired(ctx context.Context, now time.Time) (int, error)
	CountActive(ctx context.Context, userID int64, now time.Time) (int, error)
}

// TrailRepository persists governed routes and their checkpoints.
type TrailRepository interface {
	Create(ctx context.Context, trail *domain.Trail) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.Trail, error)
	ByCode(ctx context.Context, code string) (*domain.Trail, error)
	List(ctx context.Context, region string, status domain.TrailStatus, page domain.PageRequest) (domain.Page[domain.Trail], error)
	UpdateStatus(ctx context.Context, trailID int64, expectedVersion int64, status domain.TrailStatus, now time.Time) error
	AddCheckpoint(ctx context.Context, checkpoint *domain.Checkpoint) (int64, error)
	Checkpoints(ctx context.Context, trailID int64) ([]domain.Checkpoint, error)
	CheckpointBySeq(ctx context.Context, trailID int64, seq int) (*domain.Checkpoint, error)
}

// PermitRepository persists daily permit windows and the seat accounting.
type PermitRepository interface {
	EnsureWindow(ctx context.Context, trailID int64, hikeDay string, quotaTotal int, now time.Time) (*domain.PermitWindow, error)
	WindowByID(ctx context.Context, id int64) (*domain.PermitWindow, error)
	WindowByTrailDay(ctx context.Context, trailID int64, hikeDay string) (*domain.PermitWindow, error)
	ListWindows(ctx context.Context, trailID int64, fromDay, toDay string) ([]domain.PermitWindow, error)
	// ReserveSeats atomically occupies seats. It fails with
	// apperr.CodeVersionConflict when the window changed under the caller and
	// with apperr.CodeQuotaExhausted when the remaining quota is insufficient.
	ReserveSeats(ctx context.Context, windowID int64, expectedVersion int64, seats int, now time.Time) error
	// ReleaseSeats returns seats to the window when a party is cancelled or
	// aborted. It never lets the reserved counter drop below zero.
	ReleaseSeats(ctx context.Context, windowID int64, seats int, now time.Time) error
	Close(ctx context.Context, windowID int64, at time.Time) error
}

// PartyStateChange is a conditional party update. The update only applies when
// the stored state and version still match, which makes concurrent transitions
// fail loudly instead of overwriting each other.
type PartyStateChange struct {
	PartyID         int64
	ExpectedState   domain.PartyState
	ExpectedVersion int64
	NextState       domain.PartyState
	PermitWindowID  *int64
	ConfirmedAt     *time.Time
	DispatchedAt    *time.Time
	ClosedAt        *time.Time
	Now             time.Time
}

// PartyRepository persists hiking parties and their registered members.
type PartyRepository interface {
	Create(ctx context.Context, party *domain.Party) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.Party, error)
	ByCode(ctx context.Context, code string) (*domain.Party, error)
	List(ctx context.Context, filter domain.PartyFilter, page domain.PageRequest) (domain.Page[domain.Party], error)
	ApplyStateChange(ctx context.Context, change PartyStateChange) error
	AddMember(ctx context.Context, member *domain.PartyMember) (int64, error)
	Members(ctx context.Context, partyID int64) ([]domain.PartyMember, error)
	CountMembers(ctx context.Context, partyID int64) (int, error)
	// OnTrailParties lists parties currently hiking, oldest planned start first.
	OnTrailParties(ctx context.Context, limit int) ([]domain.Party, error)
}

// CheckpointReportRepository persists accepted progress reports.
type CheckpointReportRepository interface {
	Create(ctx context.Context, report *domain.CheckpointReport) (int64, error)
	ByParty(ctx context.Context, partyID int64) ([]domain.CheckpointReport, error)
	Exists(ctx context.Context, partyID, checkpointID int64) (bool, error)
	HighestSeq(ctx context.Context, partyID int64) (int, error)
}

// IncidentUpdate is a conditional incident update guarded by the version.
type IncidentUpdate struct {
	IncidentID      int64
	ExpectedVersion int64
	NextState       domain.IncidentState
	EscalationCount int
	Resolution      string
	ResolvedAt      *time.Time
	Now             time.Time
}

// IncidentRepository persists trail safety events.
type IncidentRepository interface {
	Create(ctx context.Context, incident *domain.Incident) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.Incident, error)
	List(ctx context.Context, filter domain.IncidentFilter, page domain.PageRequest) (domain.Page[domain.Incident], error)
	Apply(ctx context.Context, update IncidentUpdate) error
	OpenByPartyKind(ctx context.Context, partyID int64, kind string, checkpointSeq int) (*domain.Incident, error)
	CountOpenByParty(ctx context.Context, partyID int64) (int, error)
}

// SettlementUpdate is a conditional settlement update guarded by the version.
type SettlementUpdate struct {
	SettlementID    int64
	ExpectedVersion int64
	NextState       domain.SettlementState
	Reference       string
	FailureNote     string
	SettledAt       *time.Time
	Now             time.Time
}

// SettlementRepository persists permit fee records.
type SettlementRepository interface {
	Create(ctx context.Context, settlement *domain.Settlement) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.Settlement, error)
	ByPartyID(ctx context.Context, partyID int64) (*domain.Settlement, error)
	List(ctx context.Context, filter domain.SettlementFilter, page domain.PageRequest) (domain.Page[domain.Settlement], error)
	Apply(ctx context.Context, update SettlementUpdate) error
}

// AuditRepository appends immutable audit events.
type AuditRepository interface {
	Append(ctx context.Context, event *domain.AuditEvent) (int64, error)
	List(ctx context.Context, filter domain.AuditFilter, page domain.PageRequest) (domain.Page[domain.AuditEvent], error)
	CountByObject(ctx context.Context, objectType, objectID string) (int, error)
}

// JobFailure describes how a failed attempt should be recorded.
type JobFailure struct {
	JobID     int64
	Message   string
	Permanent bool
	RetryAt   time.Time
	Now       time.Time
}

// JobRepository persists the background work queue.
type JobRepository interface {
	Enqueue(ctx context.Context, job *domain.Job) (int64, error)
	ByID(ctx context.Context, id int64) (*domain.Job, error)
	// ClaimDue leases at most limit due jobs for the given worker. Claiming is
	// atomic so two workers never run the same job concurrently.
	ClaimDue(ctx context.Context, workerID string, lease time.Duration, now time.Time, limit int) ([]domain.Job, error)
	MarkDone(ctx context.Context, jobID int64, now time.Time) error
	MarkFailure(ctx context.Context, failure JobFailure) error
	// ReclaimExpiredLeases returns jobs whose worker died back to the queue.
	ReclaimExpiredLeases(ctx context.Context, now time.Time) (int, error)
	CountByState(ctx context.Context, state domain.JobState) (int, error)
	PendingKinds(ctx context.Context) (map[domain.JobKind]int, error)
}

// IdempotencyRecord is a stored response of a previously accepted request.
type IdempotencyRecord struct {
	ID           int64
	Scope        string
	Key          string
	ActorID      int64
	RequestHash  string
	ResponseBody string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// IdempotencyRepository persists idempotency keys and their replies.
type IdempotencyRepository interface {
	Find(ctx context.Context, scope, key string, actorID int64) (*IdempotencyRecord, error)
	Save(ctx context.Context, record *IdempotencyRecord) (int64, error)
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}
