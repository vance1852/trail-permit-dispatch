package clock

import (
	"testing"
	"time"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestHikeDayUsesBusinessZone(t *testing.T) {
	// 2026-09-11T17:30:00Z is already 2026-09-12 in the business zone.
	instant := time.Date(2026, 9, 11, 17, 30, 0, 0, time.UTC)
	if got := HikeDay(instant); got != "2026-09-12" {
		t.Fatalf("跨时区出行日计算错误: %s", got)
	}
	if got := HikeDay(instant.Add(-2 * time.Hour)); got != "2026-09-11" {
		t.Fatalf("业务时区边界前应仍属前一天: %s", got)
	}
}

func TestParseHikeDayAndDayStart(t *testing.T) {
	parsed, err := ParseHikeDay("2026-09-12")
	if err != nil {
		t.Fatalf("解析出行日失败: %v", err)
	}
	if parsed.Hour() != 0 || parsed.Minute() != 0 {
		t.Fatalf("出行日应解析为当日零点: %s", parsed)
	}
	if parsed.Location().String() != Zone().String() {
		t.Fatalf("出行日应位于业务时区: %s", parsed.Location())
	}
	for _, invalid := range []string{"", "2026/09/12", "20260912", "2026-13-01"} {
		err := func() error {
			_, err := ParseHikeDay(invalid)
			return err
		}()
		if !apperr.Is(err, apperr.CodeInvalidArgument) {
			t.Fatalf("非法出行日 %q 应返回 invalid_argument，实际 %v", invalid, err)
		}
		if field := apperr.FieldOf(err); field != "hike_day" {
			t.Fatalf("非法出行日应定位到 hike_day 字段，实际 %q", field)
		}
	}
	instant := time.Date(2026, 9, 12, 15, 45, 30, 0, Zone())
	if start := DayStart(instant); !start.Equal(parsed) {
		t.Fatalf("当日零点计算错误: %s", start)
	}
}

func TestIsWeekend(t *testing.T) {
	saturday := time.Date(2026, 9, 12, 8, 0, 0, 0, Zone())
	if saturday.Weekday() != time.Saturday {
		t.Fatalf("测试数据错误，2026-09-12 应为周六，实际 %s", saturday.Weekday())
	}
	if !IsWeekend(saturday) {
		t.Fatal("周六应判定为周末")
	}
	if !IsWeekend(saturday.Add(24 * time.Hour)) {
		t.Fatal("周日应判定为周末")
	}
	if IsWeekend(saturday.Add(48 * time.Hour)) {
		t.Fatal("周一不应判定为周末")
	}
}

func TestFixedClockIsDeterministic(t *testing.T) {
	start := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	fixed := NewFixed(start)
	if !fixed.Now().Equal(start) {
		t.Fatalf("固定时钟初始值错误: %s", fixed.Now())
	}
	advanced := fixed.Advance(90 * time.Minute)
	if !advanced.Equal(start.Add(90 * time.Minute)) {
		t.Fatalf("推进后的时间错误: %s", advanced)
	}
	if !fixed.Now().Equal(advanced) {
		t.Fatal("推进结果应被固定时钟记住")
	}
	reset := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	fixed.Set(reset)
	if !fixed.Now().Equal(reset) {
		t.Fatalf("重设时间失败: %s", fixed.Now())
	}
	if fixed.Now().Location().String() != Zone().String() {
		t.Fatal("固定时钟应归一化到业务时区")
	}
}

func TestTruncateDropsSubSecondPrecision(t *testing.T) {
	instant := time.Date(2026, 9, 12, 6, 30, 45, 987654321, time.UTC)
	truncated := Truncate(instant)
	if truncated.Nanosecond() != 0 {
		t.Fatalf("截断后不应保留纳秒: %d", truncated.Nanosecond())
	}
	if truncated.Unix() != instant.Unix() {
		t.Fatalf("截断不应改变秒级时间戳: %d != %d", truncated.Unix(), instant.Unix())
	}
}

func TestSystemClockReturnsBusinessZone(t *testing.T) {
	now := System{}.Now()
	if now.Location().String() != Zone().String() {
		t.Fatalf("系统时钟应返回业务时区时间，实际 %s", now.Location())
	}
	if time.Since(now) > time.Minute {
		t.Fatal("系统时钟返回的时间明显偏离当前时刻")
	}
}
