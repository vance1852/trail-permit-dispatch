// Package incident implements the trail safety event lifecycle: reporting,
// escalation hand-off and resolution.
package incident

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// Audit actions produced by this service.
const (
	ActionOpen     = "incident.open"
	ActionEscalate = "incident.escalate"
	ActionResolve  = "incident.resolve"
	ActionClose    = "incident.close"
)

// PartyAborter stops a party after a critical incident. The dispatch service
// implements it; declaring the narrow interface here avoids an import cycle.
type PartyAborter interface {
	AbortParty(ctx context.Context, partyID int64, reason string) error
}

// Deps carries the collaborators of the incident service.
type Deps struct {
	Tx             repository.TxRunner
	Incidents      repository.IncidentRepository
	Parties        repository.PartyRepository
	Jobs           repository.JobRepository
	Audit          *audit.Recorder
	Clock          clock.Clock
	JobMaxAttempts int
}

// Service owns the incident lifecycle.
type Service struct {
	tx             repository.TxRunner
	incidents      repository.IncidentRepository
	parties        repository.PartyRepository
	jobs           repository.JobRepository
	audit          *audit.Recorder
	clock          clock.Clock
	jobMaxAttempts int
	aborter        PartyAborter
}

// New builds an incident service.
func New(deps Deps) *Service {
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	attempts := deps.JobMaxAttempts
	if attempts <= 0 {
		attempts = 5
	}
	return &Service{
		tx:             deps.Tx,
		incidents:      deps.Incidents,
		parties:        deps.Parties,
		jobs:           deps.Jobs,
		audit:          deps.Audit,
		clock:          clk,
		jobMaxAttempts: attempts,
	}
}

// AttachPartyAborter wires the dispatch service after construction.
func (s *Service) AttachPartyAborter(aborter PartyAborter) {
	s.aborter = aborter
}

// View is the public projection of an incident.
type View struct {
	ID              int64      `json:"id"`
	PartyCode       string     `json:"party_code"`
	Kind            string     `json:"kind"`
	Severity        string     `json:"severity"`
	State           string     `json:"state"`
	Summary         string     `json:"summary"`
	CheckpointSeq   int        `json:"checkpoint_seq"`
	EscalationCount int        `json:"escalation_count"`
	OpenedAt        time.Time  `json:"opened_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	Resolution      string     `json:"resolution,omitempty"`
	Version         int64      `json:"version"`
}

// OpenInput describes a reported incident.
type OpenInput struct {
	PartyCode     string
	Kind          string
	Severity      string
	Summary       string
	CheckpointSeq int
}

// Open reports a new incident. A critical incident immediately stops the party,
// so the state of the party and the incident stay consistent.
func (s *Service) Open(ctx context.Context, actor domain.Actor, input OpenInput) (View, error) {
	if actor.UserID == 0 {
		return View{}, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	severity, err := domain.ParseSeverity(input.Severity)
	if err != nil {
		return View{}, err
	}
	kind := strings.TrimSpace(input.Kind)
	if kind == "" {
		return View{}, apperr.New(apperr.CodeInvalidArgument, "事故类型不能为空").WithField("kind")
	}
	summary := strings.TrimSpace(input.Summary)
	if len([]rune(summary)) < 5 {
		return View{}, apperr.New(apperr.CodeInvalidArgument, "事故描述至少需要 5 个字").WithField("summary")
	}
	party, err := s.parties.ByCode(ctx, strings.TrimSpace(input.PartyCode))
	if err != nil {
		return View{}, err
	}
	if actor.Role == domain.RoleLeader {
		if err := party.EnsureLeader(actor.UserID); err != nil {
			return View{}, err
		}
	}
	if party.State != domain.PartyConfirmed && party.State != domain.PartyOnTrail {
		return View{}, apperr.Newf(apperr.CodeStateInvalid,
			"队伍处于 %s 状态，无法登记事故", party.State)
	}

	incident, err := s.create(ctx, actor, party, kind, severity, summary, input.CheckpointSeq)
	if err != nil {
		return View{}, err
	}
	if severity.ForcesPartyAbort() {
		if s.aborter == nil {
			return View{}, apperr.New(apperr.CodeInternal, "队伍中止服务未接入")
		}
		if err := s.aborter.AbortParty(ctx, party.ID, "触发严重事故："+summary); err != nil {
			return View{}, err
		}
	}
	return s.toView(ctx, incident)
}

// OpenOverdue is the system entry point used by the overdue checkpoint sweep. It
// is idempotent per party and checkpoint: a still unresolved incident of the same
// kind is reported as a conflict instead of being duplicated.
func (s *Service) OpenOverdue(ctx context.Context, partyID int64, checkpointSeq int, summary string) (int64, error) {
	party, err := s.parties.ByID(ctx, partyID)
	if err != nil {
		return 0, err
	}
	if existing, err := s.incidents.OpenByPartyKind(ctx, partyID, kindOverdue, checkpointSeq); err == nil {
		return existing.ID, apperr.Newf(apperr.CodeConflict,
			"队伍 %s 的打点超时事故已存在", party.Code)
	} else if !apperr.Is(err, apperr.CodeNotFound) {
		return 0, err
	}
	incident, err := s.create(ctx, audit.SystemActor(), party, kindOverdue, domain.SeverityMajor, summary, checkpointSeq)
	if err != nil {
		return 0, err
	}
	return incident.ID, nil
}

// kindOverdue mirrors the dispatch service constant without importing it.
const kindOverdue = "checkpoint_overdue"

func (s *Service) create(ctx context.Context, actor domain.Actor, party *domain.Party,
	kind string, severity domain.Severity, summary string, checkpointSeq int) (*domain.Incident, error) {
	now := clock.Truncate(s.clock.Now())
	incident := &domain.Incident{
		PartyID:       party.ID,
		Kind:          kind,
		Severity:      severity,
		State:         domain.IncidentOpen,
		Summary:       summary,
		CheckpointSeq: checkpointSeq,
		OpenedBy:      actor.UserID,
		OpenedAt:      now,
		UpdatedAt:     now,
	}
	err := s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if _, err := s.incidents.Create(txCtx, incident); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				return apperr.Newf(apperr.CodeConflict, "队伍 %s 的同类事故已在处理中", party.Code)
			}
			return err
		}
		if err := s.enqueueEscalation(txCtx, incident, now.Add(severity.EscalationDelay())); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionOpen, audit.ObjectIncident, audit.ObjectID(incident.ID),
			fmt.Sprintf("队伍 %s 登记 %s 级事故：%s", party.Code, severity, summary))
	})
	if err != nil {
		return nil, err
	}
	return incident, nil
}

// Escalate performs one escalation hand-off. Once the attempt cap of the
// severity is reached the incident stops being re-queued.
func (s *Service) Escalate(ctx context.Context, incidentID int64) (View, error) {
	incident, err := s.incidents.ByID(ctx, incidentID)
	if err != nil {
		return View{}, err
	}
	if incident.State == domain.IncidentResolved || incident.State == domain.IncidentClosed {
		return s.toView(ctx, incident)
	}
	if incident.EscalationExhausted() {
		return View{}, apperr.Newf(apperr.CodeStateInvalid,
			"事故 %d 已达到 %s 级别的升级上限 %d 次", incident.ID, incident.Severity, incident.Severity.MaxEscalations())
	}
	if err := domain.ValidateIncidentTransition(incident.State, domain.IncidentEscalated); err != nil {
		return View{}, err
	}
	now := clock.Truncate(s.clock.Now())
	nextCount := incident.EscalationCount + 1
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.incidents.Apply(txCtx, repository.IncidentUpdate{
			IncidentID:      incident.ID,
			ExpectedVersion: incident.Version,
			NextState:       domain.IncidentEscalated,
			EscalationCount: nextCount,
			Resolution:      incident.Resolution,
			Now:             now,
		}); err != nil {
			return err
		}
		if nextCount < incident.Severity.MaxEscalations() {
			if err := s.enqueueEscalation(txCtx, incident, now.Add(incident.Severity.EscalationDelay())); err != nil {
				return err
			}
		}
		return s.audit.Success(txCtx, audit.SystemActor(), ActionEscalate, audit.ObjectIncident,
			audit.ObjectID(incident.ID), fmt.Sprintf("第 %d 次升级通报", nextCount))
	})
	if err != nil {
		return View{}, err
	}
	refreshed, err := s.incidents.ByID(ctx, incident.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, refreshed)
}

// Resolve records the operator resolution of an incident.
func (s *Service) Resolve(ctx context.Context, actor domain.Actor, incidentID int64, resolution string) (View, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return View{}, err
	}
	trimmed := strings.TrimSpace(resolution)
	if len([]rune(trimmed)) < 5 {
		return View{}, apperr.New(apperr.CodeInvalidArgument, "处置结论至少需要 5 个字").WithField("resolution")
	}
	incident, err := s.incidents.ByID(ctx, incidentID)
	if err != nil {
		return View{}, err
	}
	if err := domain.ValidateIncidentTransition(incident.State, domain.IncidentResolved); err != nil {
		return View{}, err
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.incidents.Apply(txCtx, repository.IncidentUpdate{
			IncidentID:      incident.ID,
			ExpectedVersion: incident.Version,
			NextState:       domain.IncidentResolved,
			EscalationCount: incident.EscalationCount,
			Resolution:      trimmed,
			ResolvedAt:      &now,
			Now:             now,
		}); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionResolve, audit.ObjectIncident,
			audit.ObjectID(incident.ID), "处置结论："+trimmed)
	})
	if err != nil {
		return View{}, err
	}
	refreshed, err := s.incidents.ByID(ctx, incident.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, refreshed)
}

// Close archives a resolved incident.
func (s *Service) Close(ctx context.Context, actor domain.Actor, incidentID int64) (View, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return View{}, err
	}
	incident, err := s.incidents.ByID(ctx, incidentID)
	if err != nil {
		return View{}, err
	}
	if err := domain.ValidateIncidentTransition(incident.State, domain.IncidentClosed); err != nil {
		return View{}, err
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.incidents.Apply(txCtx, repository.IncidentUpdate{
			IncidentID:      incident.ID,
			ExpectedVersion: incident.Version,
			NextState:       domain.IncidentClosed,
			EscalationCount: incident.EscalationCount,
			Resolution:      incident.Resolution,
			ResolvedAt:      incident.ResolvedAt,
			Now:             now,
		}); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionClose, audit.ObjectIncident,
			audit.ObjectID(incident.ID), "归档事故记录")
	})
	if err != nil {
		return View{}, err
	}
	refreshed, err := s.incidents.ByID(ctx, incident.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, refreshed)
}

// List returns a filtered page of incidents. Leaders only see their own parties.
func (s *Service) List(ctx context.Context, actor domain.Actor, filter domain.IncidentFilter,
	page domain.PageRequest, partyCode string) (domain.Page[View], error) {
	if actor.UserID == 0 {
		return domain.Page[View]{}, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	normalized, err := filter.Normalize()
	if err != nil {
		return domain.Page[View]{}, err
	}
	if code := strings.TrimSpace(partyCode); code != "" {
		party, err := s.parties.ByCode(ctx, code)
		if err != nil {
			return domain.Page[View]{}, err
		}
		if actor.Role == domain.RoleLeader {
			if err := party.EnsureLeader(actor.UserID); err != nil {
				return domain.Page[View]{}, err
			}
		}
		normalized.PartyID = party.ID
	} else if actor.Role == domain.RoleLeader {
		return domain.Page[View]{}, apperr.New(apperr.CodeInvalidArgument, "领队查询事故需要指定队伍编号").WithField("party_code")
	}
	result, err := s.incidents.List(ctx, normalized, page)
	if err != nil {
		return domain.Page[View]{}, err
	}
	views := make([]View, 0, len(result.Items))
	for i := range result.Items {
		view, err := s.toView(ctx, &result.Items[i])
		if err != nil {
			return domain.Page[View]{}, err
		}
		views = append(views, view)
	}
	return domain.NewPage(views, result.Total, page), nil
}

// Get returns one incident.
func (s *Service) Get(ctx context.Context, actor domain.Actor, incidentID int64) (View, error) {
	incident, err := s.incidents.ByID(ctx, incidentID)
	if err != nil {
		return View{}, err
	}
	if actor.Role == domain.RoleLeader {
		party, err := s.parties.ByID(ctx, incident.PartyID)
		if err != nil {
			return View{}, err
		}
		if err := party.EnsureLeader(actor.UserID); err != nil {
			return View{}, err
		}
	}
	return s.toView(ctx, incident)
}

// CountOpen reports the unresolved incidents of a party.
func (s *Service) CountOpen(ctx context.Context, partyID int64) (int, error) {
	return s.incidents.CountOpenByParty(ctx, partyID)
}

func (s *Service) enqueueEscalation(ctx context.Context, incident *domain.Incident, runAt time.Time) error {
	job := &domain.Job{
		Kind:        domain.JobEscalateIncident,
		Payload:     fmt.Sprintf(`{"incident_id":%d}`, incident.ID),
		MaxAttempts: s.jobMaxAttempts,
		RunAt:       runAt,
		CreatedAt:   clock.Truncate(s.clock.Now()),
		UpdatedAt:   clock.Truncate(s.clock.Now()),
	}
	_, err := s.jobs.Enqueue(ctx, job)
	return err
}

func (s *Service) toView(ctx context.Context, incident *domain.Incident) (View, error) {
	party, err := s.parties.ByID(ctx, incident.PartyID)
	if err != nil {
		return View{}, err
	}
	return View{
		ID:              incident.ID,
		PartyCode:       party.Code,
		Kind:            incident.Kind,
		Severity:        string(incident.Severity),
		State:           string(incident.State),
		Summary:         incident.Summary,
		CheckpointSeq:   incident.CheckpointSeq,
		EscalationCount: incident.EscalationCount,
		OpenedAt:        incident.OpenedAt,
		ResolvedAt:      incident.ResolvedAt,
		Resolution:      incident.Resolution,
		Version:         incident.Version,
	}, nil
}
