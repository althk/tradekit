package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
)

func ist(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

// at builds an IST timestamp on 2026-01-15, a Thursday.
func at(t *testing.T, hour, min int) time.Time {
	t.Helper()
	return time.Date(2026, 1, 15, hour, min, 0, 0, ist(t))
}

func TestInSession(t *testing.T) {
	s := NSE()
	cases := []struct {
		hour, min int
		want      bool
	}{
		{9, 14, false},
		{9, 15, true},
		{12, 0, true},
		{15, 30, true},
		{15, 31, false},
	}
	for _, c := range cases {
		got := s.InSession(at(t, c.hour, c.min))
		if got != c.want {
			t.Errorf("InSession(%02d:%02d) = %v, want %v", c.hour, c.min, got, c.want)
		}
	}
}

func TestInSessionRejectsWeekend(t *testing.T) {
	s := NSE()
	saturday := time.Date(2026, 1, 17, 12, 0, 0, 0, ist(t))
	if s.InSession(saturday) {
		t.Error("Saturday noon reported as in session")
	}
}

func TestBucketStartAnchorsToOpen(t *testing.T) {
	s := NSE()
	cases := []struct {
		hour, min    int
		tf           domain.Timeframe
		wantH, wantM int
	}{
		{9, 15, domain.M5, 9, 15},
		{9, 19, domain.M5, 9, 15},
		{9, 20, domain.M5, 9, 20},
		{9, 29, domain.M15, 9, 15},
		{9, 30, domain.M15, 9, 30},
		{10, 7, domain.M15, 10, 0},
		// A pre-open print clamps into the first bar rather than
		// creating a bar of its own.
		{9, 0, domain.M5, 9, 15},
	}
	for _, c := range cases {
		got := s.BucketStart(at(t, c.hour, c.min), c.tf)
		want := at(t, c.wantH, c.wantM)
		if !got.Equal(want) {
			t.Errorf("BucketStart(%02d:%02d, %s) = %s, want %s",
				c.hour, c.min, c.tf, got.Format("15:04"), want.Format("15:04"))
		}
	}
}

func TestBucketStartDailyIsSessionDate(t *testing.T) {
	s := NSE()
	got := s.BucketStart(at(t, 14, 37), domain.D1)
	want := time.Date(2026, 1, 15, 0, 0, 0, 0, ist(t))
	if !got.Equal(want) {
		t.Errorf("daily BucketStart = %s, want %s", got, want)
	}
}

func TestSlotIndexAndCount(t *testing.T) {
	s := NSE()
	if got := s.SlotIndex(at(t, 9, 15), domain.M5); got != 0 {
		t.Errorf("first bar slot = %d, want 0", got)
	}
	if got := s.SlotIndex(at(t, 9, 22), domain.M5); got != 1 {
		t.Errorf("09:22 slot = %d, want 1", got)
	}
	// 09:15 to 15:30 is 375 minutes: 75 five-minute bars, 25 fifteens.
	if got := s.SlotCount(domain.M5); got != 75 {
		t.Errorf("SlotCount(5m) = %d, want 75", got)
	}
	if got := s.SlotCount(domain.M15); got != 25 {
		t.Errorf("SlotCount(15m) = %d, want 25", got)
	}
}

func TestTradingDayWithoutHolidaySource(t *testing.T) {
	c := New(NSE(), nil)
	ok, err := c.TradingDay(context.Background(), at(t, 10, 0))
	if err != nil || !ok {
		t.Errorf("weekday with no holiday source: got (%v, %v), want (true, nil)", ok, err)
	}

	saturday := time.Date(2026, 1, 17, 10, 0, 0, 0, ist(t))
	ok, err = c.TradingDay(context.Background(), saturday)
	if err != nil || ok {
		t.Errorf("Saturday: got (%v, %v), want (false, nil)", ok, err)
	}
}

func TestTradingDayHonoursHolidays(t *testing.T) {
	s := NSE()
	republicDay := time.Date(2026, 1, 26, 0, 0, 0, 0, ist(t)) // a Monday
	hs := &StaticHolidays{Days: map[string]map[string]string{
		"NSE": {"2026-01-26": "Republic Day"},
	}}
	c := New(s, hs)

	ok, err := c.TradingDay(context.Background(), republicDay.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("a listed holiday was reported as a trading day")
	}
}

func TestTradingDaysExcludesWeekendsAndHolidays(t *testing.T) {
	s := NSE()
	republicDay := time.Date(2026, 1, 26, 0, 0, 0, 0, ist(t))
	hs := &StaticHolidays{Days: map[string]map[string]string{
		"NSE": {"2026-01-26": "Republic Day"},
	}}
	c := New(s, hs)

	// Mon 2026-01-26 .. Fri 2026-01-30: five weekdays, one a holiday.
	from := time.Date(2026, 1, 26, 0, 0, 0, 0, ist(t))
	to := time.Date(2026, 1, 30, 0, 0, 0, 0, ist(t))
	days, err := c.TradingDays(context.Background(), from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(days) != 4 {
		t.Fatalf("got %d trading days, want 4: %v", len(days), days)
	}
	for _, d := range days {
		if d.Equal(republicDay) {
			t.Error("the holiday was included in TradingDays")
		}
	}
}

// failingHolidays stands in for an outage or an expired token.
type failingHolidays struct{}

func (failingHolidays) Closed(context.Context, string, time.Time, time.Time) (map[string]string, error) {
	return nil, context.DeadlineExceeded
}

func TestTradingDayFailsOpen(t *testing.T) {
	c := New(NSE(), failingHolidays{})
	ok, err := c.TradingDay(context.Background(), at(t, 10, 0))
	if err == nil {
		t.Error("expected the underlying failure to be reported")
	}
	if !ok {
		t.Error("a holiday-source failure must fail open: skipping a real trading day is the worse error")
	}
}

// TestHolidayLookupSurvivesDistinctLocationPointers pins the reason holiday
// maps are keyed by string. time.LoadLocation returns a fresh *Location on
// every call, so a time.Time-keyed map would compile and silently never match.
func TestHolidayLookupSurvivesDistinctLocationPointers(t *testing.T) {
	locA, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	locB, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	a := time.Date(2026, 1, 26, 0, 0, 0, 0, locA)
	b := time.Date(2026, 1, 26, 0, 0, 0, 0, locB)

	m := map[time.Time]string{a: "Republic Day"}
	if _, ok := m[b]; ok {
		t.Log("this Go build happens to intern locations; the string key is still the contract")
	}

	c := New(NSE(), &StaticHolidays{Days: map[string]map[string]string{
		"NSE": {"2026-01-26": "Republic Day"},
	}})
	ok, err := c.TradingDay(context.Background(), b.Add(10*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("holiday lookup missed across distinct Location pointers")
	}
}
