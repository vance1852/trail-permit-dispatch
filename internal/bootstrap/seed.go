package bootstrap

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/security"
	"github.com/vance1852/trail-permit-dispatch/internal/service/catalog"
)

// SeedResult reports what the seed created.
type SeedResult struct {
	Accounts int
	Trails   int
	Windows  int
	Skipped  bool
}

// Seed prepares an empty database with the minimum data the platform needs to
// operate: one ranger, one leader and two governed routes with checkpoints. It is
// idempotent and skips everything as soon as a ranger already exists.
func (a *App) Seed(ctx context.Context) (SeedResult, error) {
	rangers, err := a.Auth.CountRole(ctx, domain.RoleRanger)
	if err != nil {
		return SeedResult{}, err
	}
	if rangers > 0 {
		return SeedResult{Skipped: true}, nil
	}
	cfg := a.Config
	rangerPassword, err := a.seedPassword(cfg.SeedRangerPassword, cfg.SeedRangerEmail)
	if err != nil {
		return SeedResult{}, err
	}
	leaderPassword, err := a.seedPassword(cfg.SeedLeaderPassword, cfg.SeedLeaderEmail)
	if err != nil {
		return SeedResult{}, err
	}
	ranger, err := a.Auth.Provision(ctx, cfg.SeedRangerEmail, "线路值守调度员", rangerPassword, domain.RoleRanger)
	if err != nil {
		return SeedResult{}, err
	}
	if _, err := a.Auth.Provision(ctx, cfg.SeedLeaderEmail, "资深徒步领队", leaderPassword, domain.RoleLeader); err != nil {
		return SeedResult{}, err
	}
	actor := domain.Actor{UserID: ranger.ID, Email: ranger.Email, Role: domain.RoleRanger}

	result := SeedResult{Accounts: 2}
	for _, trail := range seedTrails() {
		if _, err := a.Catalog.CreateTrail(ctx, actor, trail); err != nil {
			if apperr.Is(err, apperr.CodeConflict) {
				continue
			}
			return SeedResult{}, err
		}
		result.Trails++
		today := clock.DayStart(a.Clock.Now())
		for offset := 1; offset <= 3; offset++ {
			day := clock.HikeDay(today.Add(time.Duration(offset) * 24 * time.Hour))
			if _, err := a.Catalog.OpenWindow(ctx, actor, trail.Code, day, trail.DailyQuota); err != nil {
				return SeedResult{}, err
			}
			result.Windows++
		}
	}
	a.Logger.Info("初始化基础数据",
		slog.Int("accounts", result.Accounts),
		slog.Int("trails", result.Trails),
		slog.Int("permit_windows", result.Windows),
	)
	return result, nil
}

// seedPassword returns the configured seed password, or mints a one-time
// credential and logs it once. The platform never ships a default password.
func (a *App) seedPassword(configured, email string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, nil
	}
	generated, err := security.NewReference("trail9", 12)
	if err != nil {
		return "", err
	}
	a.Logger.Warn("未配置初始口令，已生成一次性凭据，请立即登录后修改并妥善保存",
		slog.String("account", email),
		slog.String("one_time_password", generated),
	)
	return generated, nil
}

// seedTrails returns the routes created on an empty database.
func seedTrails() []catalog.CreateTrailInput {
	return []catalog.CreateTrailInput{
		{
			Code:              "AOMEN-RIDGE",
			Name:              "熬门山脊纵走线",
			Region:            "北岭保护区",
			Difficulty:        4,
			DistanceKM:        27.5,
			DailyQuota:        24,
			MinPartySize:      3,
			MaxPartySize:      8,
			PermitCutoffHours: 12,
			Checkpoints: []catalog.CheckpointInput{
				{Seq: 1, Name: "西口检查站", CutoffMinutes: 90, Mandatory: true},
				{Seq: 2, Name: "断崖岔路", CutoffMinutes: 240, Mandatory: true},
				{Seq: 3, Name: "风口营地", CutoffMinutes: 420, Mandatory: true},
				{Seq: 4, Name: "东坡下撤口", CutoffMinutes: 600, Mandatory: true},
			},
		},
		{
			Code:              "QINGXI-VALLEY",
			Name:              "清溪峡谷穿越线",
			Region:            "南溪林场",
			Difficulty:        2,
			DistanceKM:        14.2,
			DailyQuota:        30,
			MinPartySize:      2,
			MaxPartySize:      10,
			PermitCutoffHours: 6,
			Checkpoints: []catalog.CheckpointInput{
				{Seq: 1, Name: "溪口木桥", CutoffMinutes: 60, Mandatory: true},
				{Seq: 2, Name: "石滩补水点", CutoffMinutes: 150, Mandatory: false},
				{Seq: 3, Name: "谷尾出口", CutoffMinutes: 300, Mandatory: true},
			},
		},
	}
}
