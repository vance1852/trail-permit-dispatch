package domain

import (
	"strings"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// TrailStatus controls whether a trail accepts new hiking parties.
type TrailStatus string

const (
	// TrailOpen accepts new parties within the permit quota.
	TrailOpen TrailStatus = "open"
	// TrailSeasonClosed rejects new parties until the season reopens.
	TrailSeasonClosed TrailStatus = "season_closed"
	// TrailSuspended rejects new parties because of an active hazard.
	TrailSuspended TrailStatus = "suspended"
)

// Trail is a governed hiking route with a daily permit quota.
type Trail struct {
	ID                int64
	Code              string
	Name              string
	Region            string
	Difficulty        int
	DistanceKM        float64
	DailyQuota        int
	MinPartySize      int
	MaxPartySize      int
	PermitCutoffHours int
	Status            TrailStatus
	Version           int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// AcceptsNewParties reports whether the trail currently allows permit requests.
func (t *Trail) AcceptsNewParties() bool {
	return t != nil && t.Status == TrailOpen
}

// ValidatePartySize checks the requested seat count against the trail rules.
func (t *Trail) ValidatePartySize(size int) error {
	if t == nil {
		return apperr.New(apperr.CodeInternal, "线路数据缺失")
	}
	if size < t.MinPartySize {
		return apperr.Newf(apperr.CodeInvalidArgument, "线路 %s 要求队伍不少于 %d 人", t.Code, t.MinPartySize).WithField("size")
	}
	if size > t.MaxPartySize {
		return apperr.Newf(apperr.CodeInvalidArgument, "线路 %s 单队上限 %d 人", t.Code, t.MaxPartySize).WithField("size")
	}
	return nil
}

// PermitDeadline returns the last instant at which a party may still lock a
// permit for the given hike day.
func (t *Trail) PermitDeadline(dayStart time.Time) time.Time {
	if t == nil {
		return dayStart
	}
	return dayStart.Add(-time.Duration(t.PermitCutoffHours) * time.Hour)
}

// ValidateTrailCode checks the external trail identifier.
func ValidateTrailCode(code string) error {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return apperr.New(apperr.CodeInvalidArgument, "线路编码不能为空").WithField("trail_code")
	}
	if len(trimmed) > 32 {
		return apperr.New(apperr.CodeInvalidArgument, "线路编码超过 32 个字符").WithField("trail_code")
	}
	for _, r := range trimmed {
		if !(r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return apperr.New(apperr.CodeInvalidArgument, "线路编码只能包含字母、数字、短横线和下划线").WithField("trail_code")
		}
	}
	return nil
}

// PermitWindow is the quota of one trail on one hike day. Seat accounting lives
// here so that concurrent confirmations contend on a single row.
type PermitWindow struct {
	ID            int64
	TrailID       int64
	HikeDay       string
	QuotaTotal    int
	QuotaReserved int
	Version       int64
	ClosedAt      *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Remaining returns the seats still available on the window.
func (w *PermitWindow) Remaining() int {
	if w == nil {
		return 0
	}
	remaining := w.QuotaTotal - w.QuotaReserved
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Closed reports whether the window no longer accepts reservations.
func (w *PermitWindow) Closed() bool { return w != nil && w.ClosedAt != nil }

// CanReserve checks the window level preconditions of a seat reservation. The
// authoritative check is the conditional SQL update; this method produces the
// operator facing error before the transaction starts.
func (w *PermitWindow) CanReserve(seats int) error {
	if w == nil {
		return apperr.New(apperr.CodeNotFound, "该出行日尚未开放许可窗口")
	}
	if seats <= 0 {
		return apperr.New(apperr.CodeInvalidArgument, "占用座位数必须大于 0").WithField("size")
	}
	if w.Closed() {
		return apperr.Newf(apperr.CodeStateInvalid, "%s 的许可窗口已关闭", w.HikeDay)
	}
	if w.Remaining() < seats {
		return apperr.Newf(apperr.CodeQuotaExhausted, "%s 仅剩 %d 个许可名额，无法容纳 %d 人", w.HikeDay, w.Remaining(), seats)
	}
	return nil
}

// Checkpoint is a mandatory or optional reporting node along a trail.
type Checkpoint struct {
	ID            int64
	TrailID       int64
	Seq           int
	Name          string
	CutoffMinutes int
	Mandatory     bool
	CreatedAt     time.Time
}

// Deadline returns the instant by which the party must report this checkpoint.
func (c *Checkpoint) Deadline(plannedStart time.Time) time.Time {
	if c == nil {
		return plannedStart
	}
	return plannedStart.Add(time.Duration(c.CutoffMinutes) * time.Minute)
}

// ReportStatus classifies a checkpoint report against its cutoff.
type ReportStatus string

const (
	// ReportOnTime means the party reported before the cutoff.
	ReportOnTime ReportStatus = "on_time"
	// ReportLate means the party reported inside the grace period.
	ReportLate ReportStatus = "late"
	// ReportMissed means the party reported after the grace period elapsed.
	ReportMissed ReportStatus = "missed"
)

// ClassifyReport derives the report status from the planned start, the cutoff
// and the operating grace period.
func ClassifyReport(plannedStart, reportedAt time.Time, cutoffMinutes, graceMinutes int) ReportStatus {
	deadline := plannedStart.Add(time.Duration(cutoffMinutes) * time.Minute)
	if !reportedAt.After(deadline) {
		return ReportOnTime
	}
	if !reportedAt.After(deadline.Add(time.Duration(graceMinutes) * time.Minute)) {
		return ReportLate
	}
	return ReportMissed
}

// CheckpointReport is one accepted progress report of a party.
type CheckpointReport struct {
	ID           int64
	PartyID      int64
	CheckpointID int64
	Seq          int
	Status       ReportStatus
	HeadCount    int
	Note         string
	ReportedAt   time.Time
	ReportedBy   int64
}
