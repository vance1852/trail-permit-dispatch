package apptest

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/bootstrap"
	"github.com/vance1852/trail-permit-dispatch/internal/clock"
	"github.com/vance1852/trail-permit-dispatch/internal/config"
	"github.com/vance1852/trail-permit-dispatch/internal/domain"
	"github.com/vance1852/trail-permit-dispatch/internal/logging"
	"github.com/vance1852/trail-permit-dispatch/internal/service/catalog"
	"github.com/vance1852/trail-permit-dispatch/internal/service/dispatch"
)

// Seed accounts of the test harness.
const (
	rangerEmail    = "ranger@trailpermit.test"
	rangerPassword = "ranger-pass-2026"
	leaderEmail    = "leader@trailpermit.test"
	leaderPassword = "leader-pass-2026"
)

// Trails created by the bootstrap seed.
const (
	valleyTrail = "QINGXI-VALLEY"
	ridgeTrail  = "AOMEN-RIDGE"
)

// harness owns a fully assembled application backed by a temporary database.
type harness struct {
	t     *testing.T
	app   *bootstrap.App
	clock *clock.Fixed
	dsn   string
}

// baseTime is the frozen "now" of the harness: 2026-09-11 08:00 business time.
func baseTime() time.Time {
	return time.Date(2026, 9, 11, 8, 0, 0, 0, clock.Zone())
}

// testConfig builds a validated configuration with fast worker timings.
func testConfig(dsn string) config.Config {
	return config.Config{
		HTTPAddr:              "127.0.0.1:0",
		DatabaseDSN:           dsn,
		LogLevel:              "error",
		SessionTTL:            72 * time.Hour,
		ShutdownTimeout:       2 * time.Second,
		ReadHeaderTimeout:     2 * time.Second,
		WorkerCount:           1,
		WorkerPollInterval:    10 * time.Millisecond,
		WorkerJobTimeout:      5 * time.Second,
		JobLeaseDuration:      time.Second,
		JobBackoffBase:        time.Second,
		JobBackoffMax:         8 * time.Second,
		JobMaxAttempts:        3,
		CheckpointGraceMin:    30,
		PermitUnitFeeCents:    4500,
		IdempotencyTTL:        time.Hour,
		SeedEnabled:           true,
		SeedRangerEmail:       rangerEmail,
		SeedRangerPassword:    rangerPassword,
		SeedLeaderEmail:       leaderEmail,
		SeedLeaderPassword:    leaderPassword,
		PasswordKDFIterations: 10000,
	}
}

// newHarness assembles the application, migrates the schema and seeds it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "app.sqlite"))
	return openHarness(t, dsn)
}

// openHarness assembles the application against the given DSN, which lets the
// restart test reopen the same database file.
func openHarness(t *testing.T, dsn string) *harness {
	t.Helper()
	fixed := clock.NewFixed(baseTime())
	logger := slog.New(slog.NewTextHandler(&discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	app, err := bootstrap.New(context.Background(), testConfig(dsn), logger, fixed)
	if err != nil {
		t.Fatalf("装配应用失败: %v", err)
	}
	if _, err := app.Seed(context.Background()); err != nil {
		t.Fatalf("初始化基础数据失败: %v", err)
	}
	h := &harness{t: t, app: app, clock: fixed, dsn: dsn}
	t.Cleanup(func() { _ = app.Close() })
	return h
}

// discardWriter silences the application logger during tests.
type discardWriter struct{}

func (*discardWriter) Write(payload []byte) (int, error) { return len(payload), nil }

// ctx returns a request scoped context carrying a correlation identifier so the
// audit trail of the tests looks like production traffic.
func (h *harness) ctx() context.Context {
	return logging.WithRequestID(context.Background(), "req-test")
}

// token logs in and returns the bearer token.
func (h *harness) token(email, password string) string {
	h.t.Helper()
	result, err := h.app.Auth.Login(h.ctx(), email, password, "go-test")
	if err != nil {
		h.t.Fatalf("登录 %s 失败: %v", email, err)
	}
	return result.Token
}

// actor logs in and resolves the authenticated actor.
func (h *harness) actor(email, password string) domain.Actor {
	h.t.Helper()
	actor, err := h.app.Auth.Authenticate(h.ctx(), h.token(email, password))
	if err != nil {
		h.t.Fatalf("解析 %s 的会话失败: %v", email, err)
	}
	return actor
}

// leader returns the seeded leader actor.
func (h *harness) leader() domain.Actor { return h.actor(leaderEmail, leaderPassword) }

// ranger returns the seeded ranger actor.
func (h *harness) ranger() domain.Actor { return h.actor(rangerEmail, rangerPassword) }

// newLeader provisions an additional leader account and returns its actor.
func (h *harness) newLeader(email string) domain.Actor {
	h.t.Helper()
	if _, err := h.app.Auth.Provision(h.ctx(), email, "备用领队", leaderPassword, domain.RoleLeader); err != nil {
		h.t.Fatalf("创建领队 %s 失败: %v", email, err)
	}
	return h.actor(email, leaderPassword)
}

// hikeDay returns the seeded permit day at the given offset from the base date.
func (h *harness) hikeDay(offsetDays int) string {
	return clock.HikeDay(clock.DayStart(baseTime()).Add(time.Duration(offsetDays) * 24 * time.Hour))
}

// plannedStart returns a planned departure inside the given hike day.
func (h *harness) plannedStart(offsetDays int, hour int) time.Time {
	return clock.DayStart(baseTime()).Add(time.Duration(offsetDays)*24*time.Hour + time.Duration(hour)*time.Hour)
}

// createParty registers a draft party on the given trail.
func (h *harness) createParty(actor domain.Actor, trailCode string, size int, offsetDays int) dispatch.PartyView {
	h.t.Helper()
	start := h.plannedStart(offsetDays, 6)
	view, err := h.app.Dispatch.CreateParty(h.ctx(), actor, dispatch.CreatePartyInput{
		TrailCode:    trailCode,
		HikeDay:      h.hikeDay(offsetDays),
		PlannedStart: start,
		PlannedEnd:   start.Add(9 * time.Hour),
		Size:         size,
		ContactPhone: "13800001234",
		Notes:        "常规穿越计划",
	})
	if err != nil {
		h.t.Fatalf("创建队伍失败: %v", err)
	}
	return view
}

// fillMembers registers companions until the party reaches its planned size.
func (h *harness) fillMembers(actor domain.Actor, code string, missing int) dispatch.BatchResult {
	h.t.Helper()
	members := make([]dispatch.MemberInput, 0, missing)
	for i := 0; i < missing; i++ {
		members = append(members, dispatch.MemberInput{
			MemberRef:    code + "-M" + string(rune('A'+i)),
			DisplayName:  "队员" + string(rune('A'+i)),
			Phone:        "1390000000" + string(rune('0'+i%10)),
			WaiverSigned: true,
		})
	}
	result, err := h.app.Dispatch.RegisterMembers(h.ctx(), actor, code, members)
	if err != nil {
		h.t.Fatalf("登记队员失败: %v", err)
	}
	return result
}

// readyParty creates a party and registers every remaining member.
func (h *harness) readyParty(actor domain.Actor, trailCode string, size, offsetDays int) dispatch.PartyView {
	h.t.Helper()
	party := h.createParty(actor, trailCode, size, offsetDays)
	if size <= 1 {
		return party
	}
	result := h.fillMembers(actor, party.Code, size-1)
	if result.Rejected != 0 {
		h.t.Fatalf("登记队员出现拒绝项: %+v", result.Outcomes)
	}
	return result.Party
}

// drainJobs runs the worker until the queue is empty or the budget is spent.
func (h *harness) drainJobs(maxRounds int) int {
	h.t.Helper()
	processed := 0
	for i := 0; i < maxRounds; i++ {
		count, err := h.app.Worker.Poll(h.ctx(), "test-worker")
		if err != nil {
			h.t.Fatalf("执行后台作业失败: %v", err)
		}
		if count == 0 {
			break
		}
		processed += count
	}
	return processed
}

// permitWindow reads the permit window of a trail day.
func (h *harness) permitWindow(trailCode string, offsetDays int) (reserved, total int) {
	h.t.Helper()
	day := h.hikeDay(offsetDays)
	windows, err := h.app.Catalog.ListWindows(h.ctx(), trailCode, day, day)
	if err != nil {
		h.t.Fatalf("读取许可窗口失败: %v", err)
	}
	if len(windows) != 1 {
		h.t.Fatalf("期望 1 个许可窗口，实际 %d", len(windows))
	}
	return windows[0].Reserved, windows[0].QuotaTotal
}

// catalogCheckpoint builds one checkpoint definition for the catalog tests.
func catalogCheckpoint(seq int, name string, cutoff int, mandatory bool) catalog.CheckpointInput {
	return catalog.CheckpointInput{Seq: seq, Name: name, CutoffMinutes: cutoff, Mandatory: mandatory}
}

// validTrailInput returns a trail definition that satisfies every catalog rule.
func validTrailInput() catalog.CreateTrailInput {
	return catalog.CreateTrailInput{
		Code:              "BEILING-TRAVERSE",
		Name:              "北岭横切线",
		Region:            "北岭保护区",
		Difficulty:        3,
		DistanceKM:        21.4,
		DailyQuota:        18,
		MinPartySize:      2,
		MaxPartySize:      6,
		PermitCutoffHours: 8,
		Checkpoints: []catalog.CheckpointInput{
			catalogCheckpoint(1, "北岭入口", 80, true),
			catalogCheckpoint(2, "云雾台", 220, true),
		},
	}
}
