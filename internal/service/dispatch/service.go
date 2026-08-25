// Package dispatch implements the party lifecycle: planning, member
// registration, permit reservation, ranger approval and trail release.
package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/idempotency"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
	"github.com/vance1852/trail-permit-dispatch/internal/security"
)

// Audit actions produced by this service.
const (
	ActionPartyCreate     = "party.create"
	ActionMemberRegister  = "party.member_register"
	ActionPermitRequest   = "party.permit_request"
	ActionPartyApprove    = "party.approve"
	ActionPartyDispatch   = "party.dispatch"
	ActionCheckpoint      = "party.checkpoint_report"
	ActionPartyComplete   = "party.complete"
	ActionPartyCancel     = "party.cancel"
	ActionPartyAbort      = "party.abort"
	ActionOverdueDetected = "party.overdue_detected"
)

// IdemScopePermitRequest namespaces the idempotency keys of permit requests.
const IdemScopePermitRequest = "party.permit_request"

// IncidentOpener lets the dispatch service raise an incident without importing
// the incident package, which keeps the two services free of an import cycle.
type IncidentOpener interface {
	OpenOverdue(ctx context.Context, partyID int64, checkpointSeq int, summary string) (int64, error)
}

// Options carries the tunable business parameters of the dispatch service.
type Options struct {
	CheckpointGraceMinutes int
	PermitUnitFeeCents     int64
	JobMaxAttempts         int
}

// Deps carries the collaborators of the dispatch service.
type Deps struct {
	Tx          repository.TxRunner
	Parties     repository.PartyRepository
	Trails      repository.TrailRepository
	Permits     repository.PermitRepository
	Reports     repository.CheckpointReportRepository
	Settlements repository.SettlementRepository
	Jobs        repository.JobRepository
	Audit       *audit.Recorder
	Idempotency *idempotency.Manager
	Clock       clock.Clock
	Options     Options
}

// Service owns the party lifecycle.
type Service struct {
	tx          repository.TxRunner
	parties     repository.PartyRepository
	trails      repository.TrailRepository
	permits     repository.PermitRepository
	reports     repository.CheckpointReportRepository
	settlements repository.SettlementRepository
	jobs        repository.JobRepository
	audit       *audit.Recorder
	idem        *idempotency.Manager
	clock       clock.Clock
	options     Options
	incidents   IncidentOpener
}

// New builds a dispatch service.
func New(deps Deps) *Service {
	options := deps.Options
	if options.CheckpointGraceMinutes < 0 {
		options.CheckpointGraceMinutes = 0
	}
	if options.PermitUnitFeeCents <= 0 {
		options.PermitUnitFeeCents = 4500
	}
	if options.JobMaxAttempts <= 0 {
		options.JobMaxAttempts = 5
	}
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	return &Service{
		tx:          deps.Tx,
		parties:     deps.Parties,
		trails:      deps.Trails,
		permits:     deps.Permits,
		reports:     deps.Reports,
		settlements: deps.Settlements,
		jobs:        deps.Jobs,
		audit:       deps.Audit,
		idem:        deps.Idempotency,
		clock:       clk,
		options:     options,
	}
}

// AttachIncidentOpener wires the incident service after construction.
func (s *Service) AttachIncidentOpener(opener IncidentOpener) {
	s.incidents = opener
}

// PermitWindowView is the public projection of a permit window.
type PermitWindowView struct {
	HikeDay    string `json:"hike_day"`
	QuotaTotal int    `json:"quota_total"`
	Reserved   int    `json:"reserved"`
	Remaining  int    `json:"remaining"`
	Closed     bool   `json:"closed"`
}

// PartyView is the public projection of a party.
type PartyView struct {
	Code         string                 `json:"code"`
	TrailCode    string                 `json:"trail_code"`
	TrailName    string                 `json:"trail_name"`
	HikeDay      string                 `json:"hike_day"`
	Size         int                    `json:"size"`
	State        string                 `json:"state"`
	Version      int64                  `json:"version"`
	PlannedStart time.Time              `json:"planned_start"`
	PlannedEnd   time.Time              `json:"planned_end"`
	ContactPhone string                 `json:"contact_phone"`
	Notes        string                 `json:"notes,omitempty"`
	Readiness    domain.ReadinessReport `json:"readiness"`
	Permit       *PermitWindowView      `json:"permit,omitempty"`
	ConfirmedAt  *time.Time             `json:"confirmed_at,omitempty"`
	DispatchedAt *time.Time             `json:"dispatched_at,omitempty"`
	ClosedAt     *time.Time             `json:"closed_at,omitempty"`
	Members      []MemberView           `json:"members,omitempty"`
	Reports      []CheckpointReportView `json:"reports,omitempty"`
}

// MemberView is the public projection of a registered hiker.
type MemberView struct {
	MemberRef    string `json:"member_ref"`
	DisplayName  string `json:"display_name"`
	Kind         string `json:"kind"`
	WaiverSigned bool   `json:"waiver_signed"`
	Phone        string `json:"phone"`
}

// CheckpointReportView is the public projection of a progress report.
type CheckpointReportView struct {
	Seq        int       `json:"seq"`
	Status     string    `json:"status"`
	HeadCount  int       `json:"head_count"`
	Note       string    `json:"note,omitempty"`
	ReportedAt time.Time `json:"reported_at"`
}

// CreatePartyInput describes a new party plan.
type CreatePartyInput struct {
	TrailCode    string
	HikeDay      string
	PlannedStart time.Time
	PlannedEnd   time.Time
	Size         int
	ContactPhone string
	Notes        string
	LeaderRef    string
}

// CreateParty registers a new party plan in the draft state.
func (s *Service) CreateParty(ctx context.Context, actor domain.Actor, input CreatePartyInput) (PartyView, error) {
	if err := actor.RequireRole(domain.RoleLeader); err != nil {
		return PartyView{}, err
	}
	if err := domain.ValidateTrailCode(input.TrailCode); err != nil {
		return PartyView{}, err
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(input.TrailCode))
	if err != nil {
		return PartyView{}, err
	}
	if !trail.AcceptsNewParties() {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid, "线路 %s 当前状态为 %s，暂不接受新队伍", trail.Code, trail.Status)
	}
	if err := trail.ValidatePartySize(input.Size); err != nil {
		return PartyView{}, err
	}
	dayStart, err := clock.ParseHikeDay(input.HikeDay)
	if err != nil {
		return PartyView{}, err
	}
	if err := domain.ValidateSchedule(input.PlannedStart, input.PlannedEnd); err != nil {
		return PartyView{}, err
	}
	if input.PlannedStart.Before(dayStart) || !input.PlannedStart.Before(dayStart.Add(24*time.Hour)) {
		return PartyView{}, apperr.New(apperr.CodeInvalidArgument, "计划出发时间必须落在所选出行日内").WithField("planned_start")
	}
	if err := domain.ValidatePhone(input.ContactPhone); err != nil {
		return PartyView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	if !now.Before(trail.PermitDeadline(dayStart)) {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"线路 %s 要求提前 %d 小时申报，%s 的计划已超过申报截止时间",
			trail.Code, trail.PermitCutoffHours, input.HikeDay)
	}
	code, err := newPartyCode(dayStart)
	if err != nil {
		return PartyView{}, err
	}
	leaderRef := strings.TrimSpace(input.LeaderRef)
	if leaderRef == "" {
		leaderRef = fmt.Sprintf("LEADER-%d", actor.UserID)
	}

	party := &domain.Party{
		Code:         code,
		TrailID:      trail.ID,
		LeaderID:     actor.UserID,
		HikeDay:      input.HikeDay,
		PlannedStart: clock.Truncate(input.PlannedStart),
		PlannedEnd:   clock.Truncate(input.PlannedEnd),
		Size:         input.Size,
		State:        domain.PartyDraft,
		ContactPhone: strings.TrimSpace(input.ContactPhone),
		Notes:        strings.TrimSpace(input.Notes),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if _, err := s.parties.Create(txCtx, party); err != nil {
			return err
		}
		leader := &domain.PartyMember{
			PartyID:      party.ID,
			MemberRef:    leaderRef,
			DisplayName:  actor.Email,
			Kind:         domain.MemberLeader,
			WaiverSigned: true,
			Phone:        party.ContactPhone,
			JoinedAt:     now,
		}
		if _, err := s.parties.AddMember(txCtx, leader); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionPartyCreate, audit.ObjectParty, party.Code,
			fmt.Sprintf("线路 %s 出行日 %s 规模 %d", trail.Code, party.HikeDay, party.Size))
	})
	if err != nil {
		return PartyView{}, err
	}
	return s.buildView(ctx, party, trail, true)
}

// MemberInput describes one hiker of a batch registration.
type MemberInput struct {
	MemberRef    string `json:"member_ref"`
	DisplayName  string `json:"display_name"`
	Phone        string `json:"phone"`
	WaiverSigned bool   `json:"waiver_signed"`
}

// BatchResult reports the per item outcome of a batch registration.
type BatchResult struct {
	Accepted int                         `json:"accepted"`
	Rejected int                         `json:"rejected"`
	Outcomes []domain.MemberBatchOutcome `json:"outcomes"`
	Party    PartyView                   `json:"party"`
}

// RegisterMembers adds hikers to a draft party. Every item is committed on its
// own so one invalid or duplicate entry cannot discard the accepted ones.
func (s *Service) RegisterMembers(ctx context.Context, actor domain.Actor, code string, items []MemberInput) (BatchResult, error) {
	if err := actor.RequireRole(domain.RoleLeader); err != nil {
		return BatchResult{}, err
	}
	if len(items) == 0 {
		return BatchResult{}, apperr.New(apperr.CodeInvalidArgument, "至少需要提交一名队员").WithField("members")
	}
	if len(items) > 50 {
		return BatchResult{}, apperr.New(apperr.CodeInvalidArgument, "单次登记不得超过 50 名队员").WithField("members")
	}
	party, trail, err := s.loadPartyForLeader(ctx, actor, code)
	if err != nil {
		return BatchResult{}, err
	}
	if party.State != domain.PartyDraft {
		return BatchResult{}, apperr.Newf(apperr.CodeStateInvalid, "队伍处于 %s 状态，无法继续登记队员", party.State)
	}

	// 批量登记前预读一次已登记人数，避免逐项重复查询数据库。
	registered, err := s.parties.CountMembers(ctx, party.ID)
	if err != nil {
		return BatchResult{}, err
	}
	remaining := party.Size - registered

	result := BatchResult{Outcomes: make([]domain.MemberBatchOutcome, 0, len(items))}
	for _, item := range items {
		outcome := domain.MemberBatchOutcome{MemberRef: strings.TrimSpace(item.MemberRef), Accepted: true}
		if err := s.registerOneMember(ctx, actor, party, item, remaining); err != nil {
			outcome.Accepted = false
			outcome.Code = string(apperr.CodeOf(err))
			outcome.Message = apperr.Message(err)
			result.Rejected++
		} else {
			result.Accepted++
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}

	refreshed, err := s.parties.ByID(ctx, party.ID)
	if err != nil {
		return BatchResult{}, err
	}
	view, err := s.buildView(ctx, refreshed, trail, true)
	if err != nil {
		return BatchResult{}, err
	}
	result.Party = view
	return result, nil
}

func (s *Service) registerOneMember(ctx context.Context, actor domain.Actor, party *domain.Party, item MemberInput, remaining int) error {
	member := domain.PartyMember{
		PartyID:      party.ID,
		MemberRef:    strings.TrimSpace(item.MemberRef),
		DisplayName:  strings.TrimSpace(item.DisplayName),
		Kind:         domain.MemberCompanion,
		WaiverSigned: item.WaiverSigned,
		Phone:        strings.TrimSpace(item.Phone),
		JoinedAt:     clock.Truncate(s.clock.Now()),
	}
	if err := domain.ValidateMember(member); err != nil {
		return err
	}
	return s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if remaining <= 0 {
			return apperr.Newf(apperr.CodeConflict, "队伍名额已满，计划规模为 %d 人", party.Size)
		}
		if _, err := s.parties.AddMember(txCtx, &member); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				return apperr.Newf(apperr.CodeConflict, "队员编号 %s 已登记", member.MemberRef)
			}
			return err
		}
		return s.audit.Success(txCtx, actor, ActionMemberRegister, audit.ObjectParty, party.Code,
			"登记队员 "+member.MemberRef)
	})
}

// RequestPermit reserves permit seats for a ready draft party. The seat
// occupation, the party transition, the settlement draft, the dispatch job and
// the audit entry commit together or not at all.
func (s *Service) RequestPermit(ctx context.Context, actor domain.Actor, code, idempotencyKey string) (PartyView, error) {
	if err := actor.RequireRole(domain.RoleLeader); err != nil {
		return PartyView{}, err
	}
	party, trail, err := s.loadPartyForLeader(ctx, actor, code)
	if err != nil {
		return PartyView{}, err
	}
	fingerprint, err := idempotency.Fingerprint(map[string]any{"party": party.Code, "size": party.Size})
	if err != nil {
		return PartyView{}, err
	}
	hit, err := s.idem.Lookup(ctx, IdemScopePermitRequest, idempotencyKey, actor.UserID, fingerprint)
	if err != nil {
		return PartyView{}, err
	}
	if hit.Replay {
		var replayed PartyView
		if err := idempotency.Decode(hit.Body, &replayed); err != nil {
			return PartyView{}, err
		}
		return replayed, nil
	}

	if err := domain.ValidatePartyTransition(party.State, domain.PartyPermitReserved); err != nil {
		return PartyView{}, err
	}
	members, err := s.parties.Members(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	readiness := domain.EvaluateReadiness(party.Size, members)
	if !readiness.Ready {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"队伍尚未齐备：已登记 %d/%d 人，%d 人未签署免责声明",
			readiness.Registered, readiness.Required, readiness.WaiverMissing)
	}
	dayStart, err := clock.ParseHikeDay(party.HikeDay)
	if err != nil {
		return PartyView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	if !now.Before(trail.PermitDeadline(dayStart)) {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"%s 的许可申报已于出行前 %d 小时截止", party.HikeDay, trail.PermitCutoffHours)
	}
	window, err := s.permits.EnsureWindow(ctx, trail.ID, party.HikeDay, trail.DailyQuota, now)
	if err != nil {
		return PartyView{}, err
	}
	if err := window.CanReserve(party.Size); err != nil {
		return PartyView{}, err
	}

	fee := domain.PermitFee(s.options.PermitUnitFeeCents, trail.Difficulty, party.Size, clock.IsWeekend(dayStart))
	var view PartyView
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.permits.ReserveSeats(txCtx, window.ID, window.Version, party.Size, now); err != nil {
			return err
		}
		windowID := window.ID
		if err := s.parties.ApplyStateChange(txCtx, repository.PartyStateChange{
			PartyID:         party.ID,
			ExpectedState:   party.State,
			ExpectedVersion: party.Version,
			NextState:       domain.PartyPermitReserved,
			PermitWindowID:  &windowID,
			ConfirmedAt:     &now,
			Now:             now,
		}); err != nil {
			return err
		}
		settlement := &domain.Settlement{
			PartyID:      party.ID,
			Seats:        party.Size,
			UnitFeeCents: s.options.PermitUnitFeeCents,
			TotalCents:   fee,
			State:        domain.SettlementPending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if _, err := s.settlements.Create(txCtx, settlement); err != nil {
			return err
		}
		if err := s.enqueue(txCtx, domain.JobNotifyDispatch, party.ID, now); err != nil {
			return err
		}
		if err := s.audit.Success(txCtx, actor, ActionPermitRequest, audit.ObjectParty, party.Code,
			fmt.Sprintf("占用 %s 许可名额 %d 个，应收 %d 分", party.HikeDay, party.Size, fee)); err != nil {
			return err
		}
		refreshed, err := s.parties.ByID(txCtx, party.ID)
		if err != nil {
			return err
		}
		built, err := s.buildView(txCtx, refreshed, trail, true)
		if err != nil {
			return err
		}
		view = built
		body, err := idempotency.Encode(built)
		if err != nil {
			return err
		}
		return s.idem.Remember(txCtx, IdemScopePermitRequest, idempotencyKey, actor.UserID, fingerprint, body)
	})
	if err != nil {
		return PartyView{}, err
	}
	return view, nil
}

// ApproveParty is the ranger review that turns a reserved permit into a
// confirmed trip.
func (s *Service) ApproveParty(ctx context.Context, actor domain.Actor, code string) (PartyView, error) {
	return s.advance(ctx, actor, code, domain.PartyPermitReserved, domain.PartyConfirmed, ActionPartyApprove,
		"线路管理员通过许可复核", nil)
}

// DispatchParty releases a confirmed party onto the trail.
func (s *Service) DispatchParty(ctx context.Context, actor domain.Actor, code string) (PartyView, error) {
	now := clock.Truncate(s.clock.Now())
	return s.advance(ctx, actor, code, domain.PartyConfirmed, domain.PartyOnTrail, ActionPartyDispatch,
		"放行出行并开始在途监管", func(change *repository.PartyStateChange) {
			change.DispatchedAt = &now
		})
}

// advance performs a ranger governed party transition.
func (s *Service) advance(ctx context.Context, actor domain.Actor, code string,
	expected, next domain.PartyState, action, detail string, decorate func(*repository.PartyStateChange)) (PartyView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return PartyView{}, err
	}
	party, trail, err := s.loadParty(ctx, code)
	if err != nil {
		return PartyView{}, err
	}
	if party.State != expected {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"队伍当前状态为 %s，该操作要求 %s", party.State, expected)
	}
	if err := domain.ValidatePartyTransition(party.State, next); err != nil {
		return PartyView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	change := repository.PartyStateChange{
		PartyID:         party.ID,
		ExpectedState:   party.State,
		ExpectedVersion: party.Version,
		NextState:       next,
		Now:             now,
	}
	if decorate != nil {
		decorate(&change)
	}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.parties.ApplyStateChange(txCtx, change); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, action, audit.ObjectParty, party.Code, detail)
	})
	if err != nil {
		return PartyView{}, err
	}
	refreshed, err := s.parties.ByID(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	return s.buildView(ctx, refreshed, trail, false)
}

// enqueue appends a background job for one party.
func (s *Service) enqueue(ctx context.Context, kind domain.JobKind, partyID int64, now time.Time) error {
	job := &domain.Job{
		Kind:        kind,
		Payload:     fmt.Sprintf(`{"party_id":%d}`, partyID),
		MaxAttempts: s.options.JobMaxAttempts,
		RunAt:       now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err := s.jobs.Enqueue(ctx, job)
	return err
}

// newPartyCode mints a human readable, unique party code.
func newPartyCode(dayStart time.Time) (string, error) {
	suffix, err := security.NewReference("TP", 3)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(suffix, "-", 2)
	return fmt.Sprintf("TP-%s-%s", dayStart.Format("20060102"), parts[len(parts)-1]), nil
}
