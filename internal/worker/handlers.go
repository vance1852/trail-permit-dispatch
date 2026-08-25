package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/service/auth"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
	"github.com/vance1852/trail-permit-dispatch/internal/service/incident"
	"github.com/vance1852/trail-permit-dispatch/internal/service/settlement"
)

// payload is the shared JSON envelope of every job kind.
type payload struct {
	PartyID    int64 `json:"party_id,omitempty"`
	IncidentID int64 `json:"incident_id,omitempty"`
	Limit      int   `json:"limit,omitempty"`
}

// decodePayload parses the job payload.
func decodePayload(job domain.Job) (payload, error) {
	var parsed payload
	if job.Payload == "" {
		return parsed, nil
	}
	if err := json.Unmarshal([]byte(job.Payload), &parsed); err != nil {
		return payload{}, apperr.Wrapf(apperr.CodeInvalidArgument, err, "作业 %d 的载荷无法解析", job.ID)
	}
	return parsed, nil
}

// Services carries the business services the handlers delegate to.
type Services struct {
	Dispatch       *dispatch.Service
	Incidents      *incident.Service
	Settlements    *settlement.Service
	Auth           *auth.Service
	Jobs           repository.JobRepository
	Tx             repository.TxRunner
	Audit          *audit.Recorder
	Clock          clock.Clock
	Logger         *slog.Logger
	MaxAttempts    int
	SweepInterval  time.Duration
	ExpiryInterval time.Duration
	SweepLimit     int
}

// withDefaults fills unset scheduling parameters.
func (s Services) withDefaults() Services {
	if s.Clock == nil {
		s.Clock = clock.System{}
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = 5
	}
	if s.SweepInterval <= 0 {
		s.SweepInterval = time.Minute
	}
	if s.ExpiryInterval <= 0 {
		s.ExpiryInterval = 10 * time.Minute
	}
	if s.SweepLimit <= 0 {
		s.SweepLimit = 50
	}
	return s
}

// Register binds every job kind of the platform to its handler.
func Register(runner *Runner, deps Services) {
	svc := deps.withDefaults()

	runner.Register(domain.JobNotifyDispatch, svc.handleNotifyDispatch)
	runner.Register(domain.JobSettleParty, svc.handleSettleParty)
	runner.Register(domain.JobEscalateIncident, svc.handleEscalateIncident)
	runner.Register(domain.JobSweepOverdue, svc.handleSweepOverdue)
	runner.Register(domain.JobExpireSessions, svc.handleExpireSessions)

	runner.RegisterFailureHook(domain.JobSettleParty, func(ctx context.Context, job domain.Job, cause error) {
		parsed, err := decodePayload(job)
		if err != nil || parsed.PartyID == 0 {
			return
		}
		if markErr := svc.Settlements.MarkFailed(ctx, parsed.PartyID, cause.Error()); markErr != nil {
			svc.Logger.Error("结算永久失败后标记状态失败",
				slog.Int64("party_id", parsed.PartyID), slog.String("error", markErr.Error()))
		}
	})
	runner.RegisterFailureHook(domain.JobSweepOverdue, func(ctx context.Context, job domain.Job, _ error) {
		svc.reschedule(ctx, domain.JobSweepOverdue, svc.SweepInterval)
	})
	runner.RegisterFailureHook(domain.JobExpireSessions, func(ctx context.Context, job domain.Job, _ error) {
		svc.reschedule(ctx, domain.JobExpireSessions, svc.ExpiryInterval)
	})
}

// handleNotifyDispatch informs the rescue base that a party holds a permit.
func (s Services) handleNotifyDispatch(ctx context.Context, job domain.Job) error {
	parsed, err := decodePayload(job)
	if err != nil {
		return err
	}
	if parsed.PartyID == 0 {
		return apperr.New(apperr.CodeInvalidArgument, "派单通知作业缺少 party_id")
	}
	party, err := s.Dispatch.PartyByID(ctx, parsed.PartyID)
	if err != nil {
		return err
	}
	return s.Tx.WithTx(ctx, func(txCtx context.Context) error {
		return s.Audit.Success(txCtx, audit.SystemActor(), "job.notify_dispatch", audit.ObjectParty, party.Code,
			fmt.Sprintf("已向救援基地通报 %s 出行计划，规模 %d 人", party.HikeDay, party.Size))
	})
}

// handleSettleParty settles the permit fee of a completed party.
func (s Services) handleSettleParty(ctx context.Context, job domain.Job) error {
	parsed, err := decodePayload(job)
	if err != nil {
		return err
	}
	if parsed.PartyID == 0 {
		return apperr.New(apperr.CodeInvalidArgument, "结算作业缺少 party_id")
	}
	_, err = s.Settlements.SettleForParty(ctx, parsed.PartyID)
	return err
}

// handleEscalateIncident performs one escalation hand-off. Reaching the
// escalation cap is a normal terminal outcome, not a job failure.
func (s Services) handleEscalateIncident(ctx context.Context, job domain.Job) error {
	parsed, err := decodePayload(job)
	if err != nil {
		return err
	}
	if parsed.IncidentID == 0 {
		return apperr.New(apperr.CodeInvalidArgument, "事故升级作业缺少 incident_id")
	}
	if _, err := s.Incidents.Escalate(ctx, parsed.IncidentID); err != nil {
		if apperr.Is(err, apperr.CodeStateInvalid) {
			s.Logger.Info("事故升级链已终止",
				slog.Int64("incident_id", parsed.IncidentID), slog.String("reason", apperr.Message(err)))
			return nil
		}
		return err
	}
	return nil
}

// handleSweepOverdue scans for parties that missed a mandatory checkpoint.
func (s Services) handleSweepOverdue(ctx context.Context, job domain.Job) error {
	parsed, err := decodePayload(job)
	if err != nil {
		return err
	}
	limit := parsed.Limit
	if limit <= 0 {
		limit = s.SweepLimit
	}
	result, err := s.Dispatch.SweepOverdueCheckpoints(ctx, limit)
	if err != nil {
		return err
	}
	if result.IncidentsOpened > 0 {
		s.Logger.Warn("发现打点超时队伍",
			slog.Int("scanned", result.Scanned), slog.Int("incidents", result.IncidentsOpened))
	}
	s.reschedule(ctx, domain.JobSweepOverdue, s.SweepInterval)
	return nil
}

// handleExpireSessions revokes sessions past their time to live.
func (s Services) handleExpireSessions(ctx context.Context, job domain.Job) error {
	revoked, err := s.Auth.ExpireSessions(ctx)
	if err != nil {
		return err
	}
	if revoked > 0 {
		s.Logger.Info("清理过期会话", slog.Int("revoked", revoked))
	}
	s.reschedule(ctx, domain.JobExpireSessions, s.ExpiryInterval)
	return nil
}

// reschedule queues the next run of a recurring job.
func (s Services) reschedule(ctx context.Context, kind domain.JobKind, delay time.Duration) {
	now := s.Clock.Now()
	job := &domain.Job{
		Kind:        kind,
		Payload:     "{}",
		MaxAttempts: s.MaxAttempts,
		RunAt:       now.Add(delay),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.Jobs.Enqueue(ctx, job); err != nil {
		s.Logger.Error("重新排期周期作业失败",
			slog.String("kind", string(kind)), slog.String("error", err.Error()))
	}
}

// EnsureRecurring queues the recurring jobs of the platform when none is pending.
func (s Services) EnsureRecurring(ctx context.Context) error {
	svc := s.withDefaults()
	pending, err := svc.Jobs.PendingKinds(ctx)
	if err != nil {
		return err
	}
	now := svc.Clock.Now()
	for kind, delay := range map[domain.JobKind]time.Duration{
		domain.JobSweepOverdue:   svc.SweepInterval,
		domain.JobExpireSessions: svc.ExpiryInterval,
	} {
		if pending[kind] > 0 {
			continue
		}
		job := &domain.Job{
			Kind:        kind,
			Payload:     "{}",
			MaxAttempts: svc.MaxAttempts,
			RunAt:       now.Add(delay),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if _, err := svc.Jobs.Enqueue(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
