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
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// IncidentKindOverdue is the incident kind raised by the overdue sweep.
const IncidentKindOverdue = "checkpoint_overdue"

// ReportInput describes one checkpoint report of a party.
type ReportInput struct {
	Seq       int
	HeadCount int
	Note      string
}

// ReportCheckpoint records the progress of a party on the trail. Reporting the
// same checkpoint twice is rejected, and the report status is derived from the
// planned start, the checkpoint cutoff and the operating grace period.
func (s *Service) ReportCheckpoint(ctx context.Context, actor domain.Actor, code string, input ReportInput) (PartyView, error) {
	if err := actor.RequireRole(domain.RoleLeader); err != nil {
		return PartyView{}, err
	}
	party, trail, err := s.loadPartyForLeader(ctx, actor, code)
	if err != nil {
		return PartyView{}, err
	}
	if party.State != domain.PartyOnTrail {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"队伍处于 %s 状态，只有在途队伍可以上报打点", party.State)
	}
	if input.HeadCount < 0 || input.HeadCount > party.Size {
		return PartyView{}, apperr.Newf(apperr.CodeInvalidArgument,
			"上报人数必须在 0 到 %d 之间", party.Size).WithField("head_count")
	}
	checkpoint, err := s.trails.CheckpointBySeq(ctx, trail.ID, input.Seq)
	if err != nil {
		return PartyView{}, err
	}
	exists, err := s.reports.Exists(ctx, party.ID, checkpoint.ID)
	if err != nil {
		return PartyView{}, err
	}
	if exists {
		return PartyView{}, apperr.Newf(apperr.CodeConflict, "打点 %s 已经上报过", checkpoint.Name)
	}
	highest, err := s.reports.HighestSeq(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	if input.Seq <= highest {
		return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
			"打点顺序不正确，已上报至第 %d 个节点", highest)
	}

	now := clock.Truncate(s.clock.Now())
	status := domain.ClassifyReport(party.PlannedStart, now, checkpoint.CutoffMinutes, s.options.CheckpointGraceMinutes)
	report := &domain.CheckpointReport{
		PartyID:      party.ID,
		CheckpointID: checkpoint.ID,
		Seq:          checkpoint.Seq,
		Status:       status,
		HeadCount:    input.HeadCount,
		Note:         strings.TrimSpace(input.Note),
		ReportedAt:   now,
		ReportedBy:   actor.UserID,
	}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if _, err := s.reports.Create(txCtx, report); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionCheckpoint, audit.ObjectParty, party.Code,
			fmt.Sprintf("上报打点 %s，状态 %s，人数 %d", checkpoint.Name, status, input.HeadCount))
	})
	if err != nil {
		return PartyView{}, err
	}
	refreshed, err := s.parties.ByID(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	return s.buildView(ctx, refreshed, trail, true)
}

// CompleteParty closes a finished trip. Every mandatory checkpoint must be
// reported first, the permit seats are released and the settlement job is queued.
func (s *Service) CompleteParty(ctx context.Context, actor domain.Actor, code string) (PartyView, error) {
	if err := actor.RequireRole(domain.RoleLeader); err != nil {
		return PartyView{}, err
	}
	party, trail, err := s.loadPartyForLeader(ctx, actor, code)
	if err != nil {
		return PartyView{}, err
	}
	if err := domain.ValidatePartyTransition(party.State, domain.PartyCompleted); err != nil {
		return PartyView{}, err
	}
	checkpoints, err := s.trails.Checkpoints(ctx, trail.ID)
	if err != nil {
		return PartyView{}, err
	}
	reports, err := s.reports.ByParty(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	reported := make(map[int]bool, len(reports))
	for _, report := range reports {
		reported[report.Seq] = true
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.Mandatory && !reported[checkpoint.Seq] {
			return PartyView{}, apperr.Newf(apperr.CodeStateInvalid,
				"必经打点 %s 尚未上报，无法结束行程", checkpoint.Name)
		}
	}

	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.parties.ApplyStateChange(txCtx, repository.PartyStateChange{
			PartyID:         party.ID,
			ExpectedState:   party.State,
			ExpectedVersion: party.Version,
			NextState:       domain.PartyCompleted,
			ClosedAt:        &now,
			Now:             now,
		}); err != nil {
			return err
		}
		if err := s.releasePermit(txCtx, party, now); err != nil {
			return err
		}
		if err := s.enqueue(txCtx, domain.JobSettleParty, party.ID, now); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionPartyComplete, audit.ObjectParty, party.Code,
			fmt.Sprintf("完成行程，释放 %s 许可名额 %d 个", party.HikeDay, party.Size))
	})
	if err != nil {
		return PartyView{}, err
	}
	refreshed, err := s.parties.ByID(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	return s.buildView(ctx, refreshed, trail, true)
}

// CancelParty withdraws a party before it enters the trail. A leader may cancel
// their own party, a ranger may cancel any party.
func (s *Service) CancelParty(ctx context.Context, actor domain.Actor, code, reason string) (PartyView, error) {
	party, trail, err := s.loadParty(ctx, code)
	if err != nil {
		return PartyView{}, err
	}
	if actor.Role != domain.RoleRanger {
		if err := actor.RequireRole(domain.RoleLeader); err != nil {
			return PartyView{}, err
		}
		if err := party.EnsureLeader(actor.UserID); err != nil {
			return PartyView{}, err
		}
	}
	if err := domain.ValidatePartyTransition(party.State, domain.PartyCancelled); err != nil {
		return PartyView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	detail := "取消出行"
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		detail += "：" + trimmed
	}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.parties.ApplyStateChange(txCtx, repository.PartyStateChange{
			PartyID:         party.ID,
			ExpectedState:   party.State,
			ExpectedVersion: party.Version,
			NextState:       domain.PartyCancelled,
			ClosedAt:        &now,
			Now:             now,
		}); err != nil {
			return err
		}
		if err := s.releasePermit(txCtx, party, now); err != nil {
			return err
		}
		if err := s.waiveSettlement(txCtx, party.ID, "队伍取消出行", now); err != nil {
			return err
		}
		// releasePermit above has already returned this party's seats. Releasing
		// a second time would claw back other parties' reservations too, because
		// ReleaseSeats clamps the counter down without per-party bookkeeping. Do
		// not release again.
		return s.audit.Success(txCtx, actor, ActionPartyCancel, audit.ObjectParty, party.Code, detail)
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

// AbortParty stops a party after a critical incident. It is called by the
// incident service and by rangers, releases the permit seats and waives the
// outstanding permit fee.
func (s *Service) AbortParty(ctx context.Context, partyID int64, reason string) error {
	party, err := s.parties.ByID(ctx, partyID)
	if err != nil {
		return err
	}
	if party.State.Terminal() {
		return nil
	}
	if err := domain.ValidatePartyTransition(party.State, domain.PartyAborted); err != nil {
		return err
	}
	now := clock.Truncate(s.clock.Now())
	return s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.parties.ApplyStateChange(txCtx, repository.PartyStateChange{
			PartyID:         party.ID,
			ExpectedState:   party.State,
			ExpectedVersion: party.Version,
			NextState:       domain.PartyAborted,
			ClosedAt:        &now,
			Now:             now,
		}); err != nil {
			return err
		}
		if err := s.releasePermit(txCtx, party, now); err != nil {
			return err
		}
		if err := s.waiveSettlement(txCtx, party.ID, reason, now); err != nil {
			return err
		}
		return s.audit.Success(txCtx, audit.SystemActor(), ActionPartyAbort, audit.ObjectParty, party.Code, reason)
	})
}

// releasePermit gives the reserved seats back when a party leaves the permit
// holding states. Parties that never reserved seats are ignored.
func (s *Service) releasePermit(ctx context.Context, party *domain.Party, now time.Time) error {
	if party.PermitWindowID == nil || !party.State.HoldsPermit() {
		return nil
	}
	return s.permits.ReleaseSeats(ctx, *party.PermitWindowID, party.Size, now)
}

// waiveSettlement writes off a still pending permit fee.
func (s *Service) waiveSettlement(ctx context.Context, partyID int64, note string, now time.Time) error {
	settlement, err := s.settlements.ByPartyID(ctx, partyID)
	if err != nil {
		if apperr.Is(err, apperr.CodeNotFound) {
			return nil
		}
		return err
	}
	if settlement.State != domain.SettlementPending {
		return nil
	}
	return s.settlements.Apply(ctx, repository.SettlementUpdate{
		SettlementID:    settlement.ID,
		ExpectedVersion: settlement.Version,
		NextState:       domain.SettlementWaived,
		FailureNote:     note,
		Now:             now,
	})
}

// OverdueSweepResult reports what one sweep round detected.
type OverdueSweepResult struct {
	Scanned         int     `json:"scanned"`
	IncidentsOpened int     `json:"incidents_opened"`
	PartyIDs        []int64 `json:"party_ids"`
}

// SweepOverdueCheckpoints finds parties that missed a mandatory checkpoint
// deadline including the grace period and raises one incident per party.
func (s *Service) SweepOverdueCheckpoints(ctx context.Context, limit int) (OverdueSweepResult, error) {
	if s.incidents == nil {
		return OverdueSweepResult{}, apperr.New(apperr.CodeInternal, "事故服务未接入，无法执行超时扫描")
	}
	parties, err := s.parties.OnTrailParties(ctx, limit)
	if err != nil {
		return OverdueSweepResult{}, err
	}
	now := s.clock.Now()
	grace := time.Duration(s.options.CheckpointGraceMinutes) * time.Minute
	result := OverdueSweepResult{Scanned: len(parties)}
	for i := range parties {
		party := parties[i]
		checkpoints, err := s.trails.Checkpoints(ctx, party.TrailID)
		if err != nil {
			return OverdueSweepResult{}, err
		}
		reports, err := s.reports.ByParty(ctx, party.ID)
		if err != nil {
			return OverdueSweepResult{}, err
		}
		reported := make(map[int]bool, len(reports))
		for _, report := range reports {
			reported[report.Seq] = true
		}
		overdue, deadline := firstOverdueCheckpoint(checkpoints, reported, party.PlannedStart, grace, now)
		if overdue == nil {
			continue
		}
		summary := fmt.Sprintf("队伍 %s 未在 %s 前上报必经打点 %s",
			party.Code, deadline.Format(time.RFC3339), overdue.Name)
		if _, err := s.incidents.OpenOverdue(ctx, party.ID, overdue.Seq, summary); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				continue
			}
			return OverdueSweepResult{}, err
		}
		result.IncidentsOpened++
		result.PartyIDs = append(result.PartyIDs, party.ID)
	}
	return result, nil
}

// firstOverdueCheckpoint returns the earliest mandatory checkpoint whose
// deadline plus grace period elapsed without a report.
func firstOverdueCheckpoint(checkpoints []domain.Checkpoint, reported map[int]bool,
	plannedStart time.Time, grace time.Duration, now time.Time) (*domain.Checkpoint, time.Time) {
	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if !checkpoint.Mandatory || reported[checkpoint.Seq] {
			continue
		}
		deadline := checkpoint.Deadline(plannedStart).Add(grace)
		if now.After(deadline) {
			return &checkpoint, deadline
		}
	}
	return nil, time.Time{}
}

// ListParties returns a filtered page of parties. Leaders only ever see their
// own parties, regardless of the requested filter.
func (s *Service) ListParties(ctx context.Context, actor domain.Actor, filter domain.PartyFilter, page domain.PageRequest) (domain.Page[PartyView], error) {
	if actor.UserID == 0 {
		return domain.Page[PartyView]{}, apperr.New(apperr.CodeUnauthenticated, "请先登录")
	}
	normalized, err := filter.Normalize()
	if err != nil {
		return domain.Page[PartyView]{}, err
	}
	if actor.Role == domain.RoleLeader {
		normalized.LeaderID = actor.UserID
	}
	result, err := s.parties.List(ctx, normalized, page)
	if err != nil {
		return domain.Page[PartyView]{}, err
	}
	views := make([]PartyView, 0, len(result.Items))
	for i := range result.Items {
		trail, err := s.trails.ByID(ctx, result.Items[i].TrailID)
		if err != nil {
			return domain.Page[PartyView]{}, err
		}
		view, err := s.buildView(ctx, &result.Items[i], trail, false)
		if err != nil {
			return domain.Page[PartyView]{}, err
		}
		views = append(views, view)
	}
	return domain.NewPage(views, result.Total, page), nil
}

// GetParty returns one party with its members and reports.
func (s *Service) GetParty(ctx context.Context, actor domain.Actor, code string) (PartyView, error) {
	party, trail, err := s.loadParty(ctx, code)
	if err != nil {
		return PartyView{}, err
	}
	if actor.Role == domain.RoleLeader {
		if err := party.EnsureLeader(actor.UserID); err != nil {
			return PartyView{}, err
		}
	}
	return s.buildView(ctx, party, trail, true)
}

// PartyByID returns the internal party record, used by the worker handlers.
func (s *Service) PartyByID(ctx context.Context, partyID int64) (*domain.Party, error) {
	return s.parties.ByID(ctx, partyID)
}

// PartyIDByCode resolves a public party code into its internal identifier so
// operational tooling and background jobs can address a party without exposing
// database identifiers on the API surface.
func (s *Service) PartyIDByCode(ctx context.Context, code string) (int64, error) {
	party, err := s.parties.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return 0, err
	}
	return party.ID, nil
}

func (s *Service) loadParty(ctx context.Context, code string) (*domain.Party, *domain.Trail, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return nil, nil, apperr.New(apperr.CodeInvalidArgument, "队伍编号不能为空").WithField("code")
	}
	party, err := s.parties.ByCode(ctx, trimmed)
	if err != nil {
		return nil, nil, err
	}
	trail, err := s.trails.ByID(ctx, party.TrailID)
	if err != nil {
		return nil, nil, err
	}
	return party, trail, nil
}

func (s *Service) loadPartyForLeader(ctx context.Context, actor domain.Actor, code string) (*domain.Party, *domain.Trail, error) {
	party, trail, err := s.loadParty(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	if err := party.EnsureLeader(actor.UserID); err != nil {
		return nil, nil, err
	}
	return party, trail, nil
}

// buildView assembles the public projection of a party.
func (s *Service) buildView(ctx context.Context, party *domain.Party, trail *domain.Trail, detailed bool) (PartyView, error) {
	view := PartyView{
		Code:         party.Code,
		TrailCode:    trail.Code,
		TrailName:    trail.Name,
		HikeDay:      party.HikeDay,
		Size:         party.Size,
		State:        string(party.State),
		Version:      party.Version,
		PlannedStart: party.PlannedStart,
		PlannedEnd:   party.PlannedEnd,
		ContactPhone: party.ContactPhone,
		Notes:        party.Notes,
		ConfirmedAt:  party.ConfirmedAt,
		DispatchedAt: party.DispatchedAt,
		ClosedAt:     party.ClosedAt,
	}
	members, err := s.parties.Members(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	view.Readiness = domain.EvaluateReadiness(party.Size, members)
	if party.PermitWindowID != nil {
		window, err := s.permits.WindowByID(ctx, *party.PermitWindowID)
		if err != nil {
			return PartyView{}, err
		}
		view.Permit = &PermitWindowView{
			HikeDay:    window.HikeDay,
			QuotaTotal: window.QuotaTotal,
			Reserved:   window.QuotaReserved,
			Remaining:  window.Remaining(),
			Closed:     window.Closed(),
		}
	}
	if !detailed {
		return view, nil
	}
	view.Members = make([]MemberView, 0, len(members))
	for _, member := range members {
		view.Members = append(view.Members, MemberView{
			MemberRef:    member.MemberRef,
			DisplayName:  member.DisplayName,
			Kind:         string(member.Kind),
			WaiverSigned: member.WaiverSigned,
			Phone:        member.Phone,
		})
	}
	reports, err := s.reports.ByParty(ctx, party.ID)
	if err != nil {
		return PartyView{}, err
	}
	view.Reports = make([]CheckpointReportView, 0, len(reports))
	for _, report := range reports {
		view.Reports = append(view.Reports, CheckpointReportView{
			Seq:        report.Seq,
			Status:     string(report.Status),
			HeadCount:  report.HeadCount,
			Note:       report.Note,
			ReportedAt: report.ReportedAt,
		})
	}
	return view, nil
}
