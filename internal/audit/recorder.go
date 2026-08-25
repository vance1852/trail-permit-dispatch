// Package audit records who changed which business object, with which result
// and under which request. Audit entries are business data, not log lines.
package audit

import (
	"context"
	"fmt"

	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// Object types used across the audit trail.
const (
	ObjectParty      = "party"
	ObjectPermit     = "permit_window"
	ObjectIncident   = "incident"
	ObjectSettlement = "settlement"
	ObjectSession    = "session"
	ObjectTrail      = "trail"
	ObjectJob        = "job"
)

// Recorder appends audit events through the repository layer.
type Recorder struct {
	repo  repository.AuditRepository
	clock clock.Clock
}

// New builds an audit recorder.
func New(repo repository.AuditRepository, clk clock.Clock) *Recorder {
	if clk == nil {
		clk = clock.System{}
	}
	return &Recorder{repo: repo, clock: clk}
}

// Record appends one audit event. It participates in the ambient transaction, so
// a rolled back business operation leaves no audit trace of a change that never
// happened.
func (r *Recorder) Record(ctx context.Context, actor domain.Actor, action, objectType, objectID string,
	result domain.AuditResult, detail string) error {
	event := &domain.AuditEvent{
		RequestID:  logging.RequestID(ctx),
		ActorID:    actor.UserID,
		ActorRole:  actor.Role,
		Action:     action,
		ObjectType: objectType,
		ObjectID:   objectID,
		Result:     result,
		Detail:     detail,
		CreatedAt:  r.clock.Now(),
	}
	_, err := r.repo.Append(ctx, event)
	return err
}

// Success appends a successful audit event.
func (r *Recorder) Success(ctx context.Context, actor domain.Actor, action, objectType, objectID, detail string) error {
	return r.Record(ctx, actor, action, objectType, objectID, domain.AuditSuccess, detail)
}

// Failure appends a rejected audit event.
func (r *Recorder) Failure(ctx context.Context, actor domain.Actor, action, objectType, objectID string, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return r.Record(ctx, actor, action, objectType, objectID, domain.AuditFailure, detail)
}

// SystemActor is the pseudo actor used by background workers.
func SystemActor() domain.Actor {
	return domain.Actor{UserID: 0, Email: "system@worker", Role: domain.RoleRanger}
}

// ObjectID renders a numeric identifier for the audit trail.
func ObjectID(id int64) string { return fmt.Sprintf("%d", id) }
