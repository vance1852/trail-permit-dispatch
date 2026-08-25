// Package catalog implements the trail and permit window governance used by the
// trail authority: route registration, seasonal status and daily quota windows.
package catalog

import (
	"context"
	"strings"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/audit"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/repository"
)

// Audit actions produced by this service.
const (
	ActionTrailCreate   = "trail.create"
	ActionTrailStatus   = "trail.status"
	ActionWindowOpen    = "permit.window_open"
	ActionWindowClose   = "permit.window_close"
	ActionCheckpointAdd = "trail.checkpoint_add"
)

// Deps carries the collaborators of the catalog service.
type Deps struct {
	Tx      repository.TxRunner
	Trails  repository.TrailRepository
	Permits repository.PermitRepository
	Audit   *audit.Recorder
	Clock   clock.Clock
}

// Service owns the trail catalog.
type Service struct {
	tx      repository.TxRunner
	trails  repository.TrailRepository
	permits repository.PermitRepository
	audit   *audit.Recorder
	clock   clock.Clock
}

// New builds a catalog service.
func New(deps Deps) *Service {
	clk := deps.Clock
	if clk == nil {
		clk = clock.System{}
	}
	return &Service{tx: deps.Tx, trails: deps.Trails, permits: deps.Permits, audit: deps.Audit, clock: clk}
}

// CheckpointView is the public projection of a reporting node.
type CheckpointView struct {
	Seq           int    `json:"seq"`
	Name          string `json:"name"`
	CutoffMinutes int    `json:"cutoff_minutes"`
	Mandatory     bool   `json:"mandatory"`
}

// TrailView is the public projection of a trail.
type TrailView struct {
	Code              string           `json:"code"`
	Name              string           `json:"name"`
	Region            string           `json:"region"`
	Difficulty        int              `json:"difficulty"`
	DistanceKM        float64          `json:"distance_km"`
	DailyQuota        int              `json:"daily_quota"`
	MinPartySize      int              `json:"min_party_size"`
	MaxPartySize      int              `json:"max_party_size"`
	PermitCutoffHours int              `json:"permit_cutoff_hours"`
	Status            string           `json:"status"`
	Version           int64            `json:"version"`
	Checkpoints       []CheckpointView `json:"checkpoints,omitempty"`
}

// WindowView is the public projection of a permit window.
type WindowView struct {
	HikeDay    string `json:"hike_day"`
	QuotaTotal int    `json:"quota_total"`
	Reserved   int    `json:"reserved"`
	Remaining  int    `json:"remaining"`
	Closed     bool   `json:"closed"`
	Version    int64  `json:"version"`
}

// CheckpointInput describes one reporting node of a new trail.
type CheckpointInput struct {
	Seq           int    `json:"seq"`
	Name          string `json:"name"`
	CutoffMinutes int    `json:"cutoff_minutes"`
	Mandatory     bool   `json:"mandatory"`
}

// CreateTrailInput describes a new governed route.
type CreateTrailInput struct {
	Code              string
	Name              string
	Region            string
	Difficulty        int
	DistanceKM        float64
	DailyQuota        int
	MinPartySize      int
	MaxPartySize      int
	PermitCutoffHours int
	Checkpoints       []CheckpointInput
}

// CreateTrail registers a route together with its mandatory checkpoints.
func (s *Service) CreateTrail(ctx context.Context, actor domain.Actor, input CreateTrailInput) (TrailView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return TrailView{}, err
	}
	if err := domain.ValidateTrailCode(input.Code); err != nil {
		return TrailView{}, err
	}
	if err := domain.ValidateDisplayName(input.Name); err != nil {
		return TrailView{}, err
	}
	if strings.TrimSpace(input.Region) == "" {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "所属区域不能为空").WithField("region")
	}
	if input.Difficulty < 1 || input.Difficulty > 5 {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "难度等级必须在 1 到 5 之间").WithField("difficulty")
	}
	if input.DistanceKM <= 0 {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "线路里程必须大于 0").WithField("distance_km")
	}
	if input.DailyQuota <= 0 {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "每日许可配额必须大于 0").WithField("daily_quota")
	}
	if input.MinPartySize <= 0 || input.MaxPartySize < input.MinPartySize {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "队伍人数区间配置无效").WithField("min_party_size")
	}
	if input.MaxPartySize > input.DailyQuota {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "单队上限不能超过每日许可配额").WithField("max_party_size")
	}
	if input.PermitCutoffHours < 0 || input.PermitCutoffHours > 720 {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "申报截止提前小时数配置无效").WithField("permit_cutoff_hours")
	}
	if len(input.Checkpoints) == 0 {
		return TrailView{}, apperr.New(apperr.CodeInvalidArgument, "线路至少需要一个打点节点").WithField("checkpoints")
	}
	if err := validateCheckpoints(input.Checkpoints); err != nil {
		return TrailView{}, err
	}

	now := clock.Truncate(s.clock.Now())
	trail := &domain.Trail{
		Code:              strings.TrimSpace(input.Code),
		Name:              strings.TrimSpace(input.Name),
		Region:            strings.TrimSpace(input.Region),
		Difficulty:        input.Difficulty,
		DistanceKM:        input.DistanceKM,
		DailyQuota:        input.DailyQuota,
		MinPartySize:      input.MinPartySize,
		MaxPartySize:      input.MaxPartySize,
		PermitCutoffHours: input.PermitCutoffHours,
		Status:            domain.TrailOpen,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	err := s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if _, err := s.trails.Create(txCtx, trail); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				return apperr.Newf(apperr.CodeConflict, "线路编码 %s 已存在", trail.Code).WithField("code")
			}
			return err
		}
		for _, item := range input.Checkpoints {
			checkpoint := &domain.Checkpoint{
				TrailID:       trail.ID,
				Seq:           item.Seq,
				Name:          strings.TrimSpace(item.Name),
				CutoffMinutes: item.CutoffMinutes,
				Mandatory:     item.Mandatory,
				CreatedAt:     now,
			}
			if _, err := s.trails.AddCheckpoint(txCtx, checkpoint); err != nil {
				return err
			}
		}
		return s.audit.Success(txCtx, actor, ActionTrailCreate, audit.ObjectTrail, trail.Code,
			"登记线路并配置打点节点")
	})
	if err != nil {
		return TrailView{}, err
	}
	return s.trailView(ctx, trail, true)
}

// validateCheckpoints enforces strictly increasing sequences and cutoffs.
func validateCheckpoints(items []CheckpointInput) error {
	previousSeq := 0
	previousCutoff := 0
	mandatory := 0
	for _, item := range items {
		if item.Seq <= previousSeq {
			return apperr.New(apperr.CodeInvalidArgument, "打点序号必须从 1 开始严格递增").WithField("checkpoints")
		}
		if strings.TrimSpace(item.Name) == "" {
			return apperr.New(apperr.CodeInvalidArgument, "打点名称不能为空").WithField("checkpoints")
		}
		if item.CutoffMinutes <= previousCutoff {
			return apperr.New(apperr.CodeInvalidArgument, "打点的截止分钟数必须随序号递增").WithField("checkpoints")
		}
		if item.Mandatory {
			mandatory++
		}
		previousSeq = item.Seq
		previousCutoff = item.CutoffMinutes
	}
	if mandatory == 0 {
		return apperr.New(apperr.CodeInvalidArgument, "线路至少需要一个必经打点").WithField("checkpoints")
	}
	return nil
}

// SetTrailStatus opens, closes or suspends a route.
func (s *Service) SetTrailStatus(ctx context.Context, actor domain.Actor, code, status string) (TrailView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return TrailView{}, err
	}
	next := domain.TrailStatus(strings.TrimSpace(status))
	switch next {
	case domain.TrailOpen, domain.TrailSeasonClosed, domain.TrailSuspended:
	default:
		return TrailView{}, apperr.Newf(apperr.CodeInvalidArgument, "未知线路状态 %q", status).WithField("status")
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return TrailView{}, err
	}
	if trail.Status == next {
		return TrailView{}, apperr.Newf(apperr.CodeStateInvalid, "线路已经处于 %s 状态", next)
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.trails.UpdateStatus(txCtx, trail.ID, trail.Version, next, now); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionTrailStatus, audit.ObjectTrail, trail.Code,
			"线路状态调整为 "+string(next))
	})
	if err != nil {
		return TrailView{}, err
	}
	refreshed, err := s.trails.ByID(ctx, trail.ID)
	if err != nil {
		return TrailView{}, err
	}
	return s.trailView(ctx, refreshed, false)
}

// AddCheckpoint appends a reporting node to an existing route.
func (s *Service) AddCheckpoint(ctx context.Context, actor domain.Actor, code string, input CheckpointInput) (TrailView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return TrailView{}, err
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return TrailView{}, err
	}
	existing, err := s.trails.Checkpoints(ctx, trail.ID)
	if err != nil {
		return TrailView{}, err
	}
	merged := make([]CheckpointInput, 0, len(existing)+1)
	for _, item := range existing {
		merged = append(merged, CheckpointInput{
			Seq: item.Seq, Name: item.Name, CutoffMinutes: item.CutoffMinutes, Mandatory: item.Mandatory,
		})
	}
	merged = append(merged, input)
	if err := validateCheckpoints(merged); err != nil {
		return TrailView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		checkpoint := &domain.Checkpoint{
			TrailID:       trail.ID,
			Seq:           input.Seq,
			Name:          strings.TrimSpace(input.Name),
			CutoffMinutes: input.CutoffMinutes,
			Mandatory:     input.Mandatory,
			CreatedAt:     now,
		}
		if _, err := s.trails.AddCheckpoint(txCtx, checkpoint); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				return apperr.Newf(apperr.CodeConflict, "线路 %s 已存在第 %d 个打点", trail.Code, input.Seq)
			}
			return err
		}
		return s.audit.Success(txCtx, actor, ActionCheckpointAdd, audit.ObjectTrail, trail.Code,
			"新增打点节点 "+strings.TrimSpace(input.Name))
	})
	if err != nil {
		return TrailView{}, err
	}
	return s.trailView(ctx, trail, true)
}

// OpenWindow opens or reuses the permit window of a trail day.
func (s *Service) OpenWindow(ctx context.Context, actor domain.Actor, code, hikeDay string, quota int) (WindowView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return WindowView{}, err
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return WindowView{}, err
	}
	if _, err := clock.ParseHikeDay(hikeDay); err != nil {
		return WindowView{}, err
	}
	if quota <= 0 {
		quota = trail.DailyQuota
	}
	if quota < trail.MaxPartySize {
		return WindowView{}, apperr.Newf(apperr.CodeInvalidArgument,
			"窗口配额不能小于线路单队上限 %d", trail.MaxPartySize).WithField("quota_total")
	}
	now := clock.Truncate(s.clock.Now())
	var window *domain.PermitWindow
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		created, err := s.permits.EnsureWindow(txCtx, trail.ID, hikeDay, quota, now)
		if err != nil {
			return err
		}
		window = created
		return s.audit.Success(txCtx, actor, ActionWindowOpen, audit.ObjectPermit,
			trail.Code+"/"+hikeDay, "开放许可窗口")
	})
	if err != nil {
		return WindowView{}, err
	}
	return toWindowView(window), nil
}

// CloseWindow stops a permit window from accepting new reservations.
func (s *Service) CloseWindow(ctx context.Context, actor domain.Actor, code, hikeDay string) (WindowView, error) {
	if err := actor.RequireRole(domain.RoleRanger); err != nil {
		return WindowView{}, err
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return WindowView{}, err
	}
	window, err := s.permits.WindowByTrailDay(ctx, trail.ID, hikeDay)
	if err != nil {
		return WindowView{}, err
	}
	now := clock.Truncate(s.clock.Now())
	err = s.tx.WithTx(ctx, func(txCtx context.Context) error {
		if err := s.permits.Close(txCtx, window.ID, now); err != nil {
			return err
		}
		return s.audit.Success(txCtx, actor, ActionWindowClose, audit.ObjectPermit,
			trail.Code+"/"+hikeDay, "关闭许可窗口")
	})
	if err != nil {
		return WindowView{}, err
	}
	refreshed, err := s.permits.WindowByID(ctx, window.ID)
	if err != nil {
		return WindowView{}, err
	}
	return toWindowView(refreshed), nil
}

// ListWindows lists the permit windows of a trail in a day range.
func (s *Service) ListWindows(ctx context.Context, code, fromDay, toDay string) ([]WindowView, error) {
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return nil, err
	}
	if fromDay != "" {
		if _, err := clock.ParseHikeDay(fromDay); err != nil {
			return nil, err
		}
	}
	if toDay != "" {
		if _, err := clock.ParseHikeDay(toDay); err != nil {
			return nil, err
		}
	}
	windows, err := s.permits.ListWindows(ctx, trail.ID, fromDay, toDay)
	if err != nil {
		return nil, err
	}
	views := make([]WindowView, 0, len(windows))
	for i := range windows {
		views = append(views, toWindowView(&windows[i]))
	}
	return views, nil
}

// ListTrails returns a filtered page of trails.
func (s *Service) ListTrails(ctx context.Context, region, status string, page domain.PageRequest) (domain.Page[TrailView], error) {
	trailStatus := domain.TrailStatus(strings.TrimSpace(status))
	switch trailStatus {
	case "", domain.TrailOpen, domain.TrailSeasonClosed, domain.TrailSuspended:
	default:
		return domain.Page[TrailView]{}, apperr.Newf(apperr.CodeInvalidArgument, "未知线路状态 %q", status).WithField("status")
	}
	result, err := s.trails.List(ctx, strings.TrimSpace(region), trailStatus, page)
	if err != nil {
		return domain.Page[TrailView]{}, err
	}
	views := make([]TrailView, 0, len(result.Items))
	for i := range result.Items {
		view, err := s.trailView(ctx, &result.Items[i], false)
		if err != nil {
			return domain.Page[TrailView]{}, err
		}
		views = append(views, view)
	}
	return domain.NewPage(views, result.Total, page), nil
}

// GetTrail returns one trail with its checkpoints.
func (s *Service) GetTrail(ctx context.Context, code string) (TrailView, error) {
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return TrailView{}, err
	}
	return s.trailView(ctx, trail, true)
}

// TrailIDByCode resolves a public trail code into its internal identifier so
// list endpoints can filter by route without exposing database identifiers.
func (s *Service) TrailIDByCode(ctx context.Context, code string) (int64, error) {
	if err := domain.ValidateTrailCode(code); err != nil {
		return 0, err
	}
	trail, err := s.trails.ByCode(ctx, strings.TrimSpace(code))
	if err != nil {
		return 0, err
	}
	return trail.ID, nil
}

// TrailCount reports how many routes are registered.
func (s *Service) TrailCount(ctx context.Context) (int, error) {
	page, err := domain.NewPageRequest(1, 1, "code", false, []string{"code"})
	if err != nil {
		return 0, err
	}
	result, err := s.trails.List(ctx, "", "", page)
	if err != nil {
		return 0, err
	}
	return result.Total, nil
}

func (s *Service) trailView(ctx context.Context, trail *domain.Trail, withCheckpoints bool) (TrailView, error) {
	view := TrailView{
		Code:              trail.Code,
		Name:              trail.Name,
		Region:            trail.Region,
		Difficulty:        trail.Difficulty,
		DistanceKM:        trail.DistanceKM,
		DailyQuota:        trail.DailyQuota,
		MinPartySize:      trail.MinPartySize,
		MaxPartySize:      trail.MaxPartySize,
		PermitCutoffHours: trail.PermitCutoffHours,
		Status:            string(trail.Status),
		Version:           trail.Version,
	}
	if !withCheckpoints {
		return view, nil
	}
	checkpoints, err := s.trails.Checkpoints(ctx, trail.ID)
	if err != nil {
		return TrailView{}, err
	}
	view.Checkpoints = make([]CheckpointView, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		view.Checkpoints = append(view.Checkpoints, CheckpointView{
			Seq:           checkpoint.Seq,
			Name:          checkpoint.Name,
			CutoffMinutes: checkpoint.CutoffMinutes,
			Mandatory:     checkpoint.Mandatory,
		})
	}
	return view, nil
}

func toWindowView(window *domain.PermitWindow) WindowView {
	return WindowView{
		HikeDay:    window.HikeDay,
		QuotaTotal: window.QuotaTotal,
		Reserved:   window.QuotaReserved,
		Remaining:  window.Remaining(),
		Closed:     window.Closed(),
		Version:    window.Version,
	}
}
