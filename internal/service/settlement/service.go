// Package settlement implements the permit fee lifecycle of finished parties.
package settlement

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
	"github.com/vance1852/trail-permit-dispatch/internal/security"
)

// Audit actions produced by this service.
const (
	ActionSettle = "settlement.settle"
	ActionWaive  = "settlement.waive"
	ActionFail   = "settlement.fail"
)

// Deps carries the collaborators of the settlement service.
type Deps struct {
	Tx          repository.TxRunner
	Settlements repository.SettlementRepository
	Parties     repository.PartyRepository
	Audit       *audit.Recorder
	Clock       clock.Clock
}

// Service owns the permit fee lifecycle.
type Service struct {
	tx          repository.TxRunner
	settlements repository.SettlementRepository
	parties     repository.PartyRepository
	audit       *audit.Recorder
	clock       clock.Clock
}

// New builds a settlement service.
func New(deps Deps) *Service {
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	return &Service{
		tx:          deps.Tx,
		settlements: deps.Settlements,
		parties:     deps.Parties,
		audit:       deps.Audit,
		clock:       clk,
	}
}

// View is the public projection of a settlement.
type View struct {
	ID           int64      `json:"id"`
	PartyCode    string     `json:"party_code"`
	Seats        int        `json:"seats"`
	UnitFeeCents int64      `json:"unit_fee_cents"`
	TotalCents   int64      `json:"total_cents"`
	State        string     `json:"state"`
	Reference    string     `json:"reference,omitempty"`
	FailureNote  string     `json:"failure_note,omitempty"`
	SettledAt    *time.Time `json:"settled_at,omitempty"`
	Version      int64      `json:"version"`
}

// SettleForParty settles the permit fee of a completed party. It is the worker
// entry point and refuses to charge a party that did not finish its trip.
func (s *Service) SettleForParty(ctx context.Context, partyID int64) (View, error) {
	if _, err := s.parties.ByID(ctx, partyID); err != nil {
		return View{}, err
	}
	current, err := s.settlements.ByPartyID(ctx, partyID)
	if err != nil {
		return View{}, err
	}
	if current.State == domain.SettlementSettled || current.State == domain.SettlementWaived {
		return s.toView(ctx, current)
	}
	reference, err := security.NewReference("PF", 4)
	if err != nil {
		return View{}, err
	}
	now := clock.Truncate(s.clock.Now())
	if err := domain.ValidateSettlementTransition(current.State, domain.SettlementSettled); err != nil {
		return View{}, err
	}
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.settlements.Apply(txCtx, repository.SettlementUpdate{
			SettlementID:    current.ID,
			ExpectedVersion: current.Version,
			NextState:       domain.SettlementSettled,
			Reference:       reference,
			SettledAt:       &now,
			Now:             now,
		}); err != nil {
			return err
		}
		return s.audit.Success(txCtx, audit.SystemActor(), ActionSettle, audit.ObjectSettlement,
			audit.ObjectID(current.ID), fmt.Sprintf("结算 %d 分，凭证 %s", current.TotalCents, reference))
	})
	if err != nil {
		return View{}, err
	}
	refreshed, err := s.settlements.ByID(ctx, current.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, refreshed)
}

// Waive writes off a permit fee. Only a ranger may waive a fee.
func (s *Service) Waive(ctx context.Context, actor domain.Actor, settlementID int64, note string) (View, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return View{}, err
	}
	trimmed := strings.TrimSpace(note)
	if len([]rune(trimmed)) < 5 {
		return View{}, apperr.New(apperr.CodeInvalidArgument, "减免说明至少需要 5 个字").WithField("note")
	}
	current, err := s.settlements.ByID(ctx, settlementID)
	if err != nil {
		return View{}, err
	}
	if err := domain.ValidateSettlementTransition(current.State, domain.SettlementWaived); err != nil {
		return View{}, err
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.settlements.Apply(txCtx, repository.SettlementUpdate{
			SettlementID:    current.ID,
			ExpectedVersion: current.Version,
			NextState:       domain.SettlementWaived,
			FailureNote:     trimmed,
			Now:             now,
		}); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionWaive, audit.ObjectSettlement,
			audit.ObjectID(current.ID), "减免许可费用："+trimmed)
	})
	if err != nil {
		return View{}, err
	}
	refreshed, err := s.settlements.ByID(ctx, current.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, refreshed)
}

// MarkFailed parks a settlement that exhausted its background retries.
func (s *Service) MarkFailed(ctx context.Context, partyID int64, reason string) error {
	current, err := s.settlements.ByPartyID(ctx, partyID)
	if err != nil {
		if apperr.Is(err, apperr.CodeNotFound) {
			return nil
		}
		return err
	}
	if current.State != domain.SettlementPending {
		return nil
	}
	now := clock.Truncate(s.clock.Now())
	return s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.settlements.Apply(txCtx, repository.SettlementUpdate{
			SettlementID:    current.ID,
			ExpectedVersion: current.Version,
			NextState:       domain.SettlementFailed,
			FailureNote:     reason,
			Now:             now,
		}); err != nil {
			return err
		}
		return s.audit.Success(txCtx, audit.SystemActor(), ActionFail, audit.ObjectSettlement,
			audit.ObjectID(current.ID), "结算失败："+reason)
	})
}

// List returns a filtered page of settlements. Leaders only see their own party.
func (s *Service) List(ctx context.Context, actor domain.Actor, filter domain.SettlementFilter,
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
		return domain.Page[View]{}, apperr.New(apperr.CodeInvalidArgument, "领队查询结算需要指定队伍编号").WithField("party_code")
	}
	result, err := s.settlements.List(ctx, normalized, page)
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

// ForParty returns the settlement of one party.
func (s *Service) ForParty(ctx context.Context, actor domain.Actor, partyCode string) (View, error) {
	party, err := s.parties.ByCode(ctx, strings.TrimSpace(partyCode))
	if err != nil {
		return View{}, err
	}
	if actor.Role == domain.RoleLeader {
		if err := party.EnsureLeader(actor.UserID); err != nil {
			return View{}, err
		}
	}
	current, err := s.settlements.ByPartyID(ctx, party.ID)
	if err != nil {
		return View{}, err
	}
	return s.toView(ctx, current)
}

func (s *Service) toView(ctx context.Context, current *domain.Settlement) (View, error) {
	party, err := s.parties.ByID(ctx, current.PartyID)
	if err != nil {
		return View{}, err
	}
	return View{
		ID:           current.ID,
		PartyCode:    party.Code,
		Seats:        current.Seats,
		UnitFeeCents: current.UnitFeeCents,
		TotalCents:   current.TotalCents,
		State:        string(current.State),
		Reference:    current.Reference,
		FailureNote:  current.FailureNote,
		SettledAt:    current.SettledAt,
		Version:      current.Version,
	}, nil
}
