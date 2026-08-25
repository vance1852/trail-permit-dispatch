// Package clock isolates wall clock access so business time rules, permit day
// boundaries and checkpoint cutoffs stay testable without sleeping.
package clock

import (
	"sync"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

// businessZone is the fixed operating zone of the trail authority. A fixed zone
// is used on purpose so scratch based containers without tzdata behave exactly
// like developer machines.
var businessZone = time.FixedZone("CST", 8*60*60)

// dayLayout is the canonical hike day representation used in storage and API.
const dayLayout = "2006-01-02"

// Zone returns the business time zone used for every permit day calculation.
func Zone() *time.Location { return businessZone }

// Clock supplies the current time. Production uses System, tests use Fixed.
type Clock interface {
	Now() time.Time
}

// System reads the operating system clock and normalises to the business zone.
type System struct{}

// Now returns the current business time.
func (System) Now() time.Time { return time.Now().In(businessZone) }

// Fixed is a deterministic clock for tests and replayable worker scenarios.
type Fixed struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixed builds a deterministic clock anchored at the given instant.
func NewFixed(now time.Time) *Fixed {
	return &Fixed{now: now.In(businessZone)}
}

// Now returns the currently configured instant.
func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Set replaces the configured instant.
func (f *Fixed) Set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now.In(businessZone)
}

// Advance moves the clock forward by d and returns the new instant.
func (f *Fixed) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

// HikeDay renders an instant as the canonical hike day of the business zone.
func HikeDay(at time.Time) string {
	return at.In(businessZone).Format(dayLayout)
}

// ParseHikeDay validates a canonical hike day and returns its local midnight.
func ParseHikeDay(day string) (time.Time, error) {
	parsed, err := time.ParseInLocation(dayLayout, day, businessZone)
	if err != nil {
		return time.Time{}, apperr.Wrap(apperr.CodeInvalidArgument, "出行日期必须为 YYYY-MM-DD 格式", err).WithField("hike_day")
	}
	return parsed, nil
}

// DayStart returns local midnight of the day containing at.
func DayStart(at time.Time) time.Time {
	local := at.In(businessZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, businessZone)
}

// IsWeekend reports whether the instant falls on a Saturday or Sunday in the
// business zone. Weekend permits carry a different fee.
func IsWeekend(at time.Time) bool {
	switch at.In(businessZone).Weekday() {
	case time.Saturday, time.Sunday:
		return true
	default:
		return false
	}
}

// Truncate normalises an instant to whole seconds in the business zone so that
// values written to SQLite round-trip without precision drift.
func Truncate(at time.Time) time.Time {
	return at.In(businessZone).Truncate(time.Second)
}
