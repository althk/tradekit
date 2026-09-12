// Package calendar answers when an exchange trades and where a timestamp falls
// within its session.
//
// It replaces four incompatible answers to "is the market open": one that
// hardcoded 09:15-15:30 with no holiday awareness at all, one that consulted a
// broker API on every call, one that cached a JSON file, and one that did not
// exist. Holidays arrive through a HolidaySource so that a backtest can run
// offline against a cached list while a live process refreshes from the venue.
package calendar

import (
	"context"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
)

// Session describes an exchange's regular trading day.
type Session struct {
	Exchange string
	Location *time.Location
	// Open and Close are minutes from midnight in Location, so that a
	// session is described without reference to any particular date.
	OpenMinute  int
	CloseMinute int
	// Weekdays that the exchange trades. Absent a holiday list this is the
	// only thing separating a trading day from a closed one.
	TradingDays map[time.Weekday]bool
}

// NSE returns the National Stock Exchange of India's equity session:
// Monday to Friday, 09:15 to 15:30 IST.
//
// It panics if the tzdata for Asia/Kolkata is unavailable, because a trading
// system that has silently fallen back to UTC will place orders at the wrong
// time, and failing at startup is the only safe response.
func NSE() Session {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		panic("calendar: Asia/Kolkata unavailable; install tzdata or import time/tzdata: " + err.Error())
	}
	return Session{
		Exchange:    "NSE",
		Location:    loc,
		OpenMinute:  9*60 + 15,
		CloseMinute: 15*60 + 30,
		TradingDays: weekdaysMonToFri(),
	}
}

// USEquity returns the NYSE/Nasdaq regular session: Monday to Friday,
// 09:30 to 16:00 America/New_York.
func USEquity() Session {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic("calendar: America/New_York unavailable; install tzdata or import time/tzdata: " + err.Error())
	}
	return Session{
		Exchange:    "NASDAQ",
		Location:    loc,
		OpenMinute:  9*60 + 30,
		CloseMinute: 16 * 60,
		TradingDays: weekdaysMonToFri(),
	}
}

func weekdaysMonToFri() map[time.Weekday]bool {
	return map[time.Weekday]bool{
		time.Monday:    true,
		time.Tuesday:   true,
		time.Wednesday: true,
		time.Thursday:  true,
		time.Friday:    true,
	}
}

// Date truncates t to midnight on its calendar date in the session's timezone.
func (s Session) Date(t time.Time) time.Time {
	local := t.In(s.Location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.Location)
}

// DateKey renders t's calendar date in the session's timezone as "2006-01-02".
//
// This, not a time.Time, is the key for every date-indexed map in tradekit.
// Two time.Time values for the same instant compare unequal as map keys when
// their *Location pointers differ, and time.LoadLocation returns a fresh
// pointer on every call, so a time-keyed lookup compiles, looks right and
// silently never matches.
func (s Session) DateKey(t time.Time) string {
	return t.In(s.Location).Format(time.DateOnly)
}

// Open returns the session's opening instant on t's calendar date.
func (s Session) Open(t time.Time) time.Time {
	return s.Date(t).Add(time.Duration(s.OpenMinute) * time.Minute)
}

// Close returns the session's closing instant on t's calendar date.
func (s Session) Close(t time.Time) time.Time {
	return s.Date(t).Add(time.Duration(s.CloseMinute) * time.Minute)
}

// IsWeekday reports whether the exchange trades on t's day of the week. It says
// nothing about holidays; use TradingDay for that.
func (s Session) IsWeekday(t time.Time) bool {
	return s.TradingDays[t.In(s.Location).Weekday()]
}

// InSession reports whether t falls inside the regular trading window,
// inclusive of both boundaries. It does not consult holidays: pair it with
// TradingDay when that matters.
func (s Session) InSession(t time.Time) bool {
	if !s.IsWeekday(t) {
		return false
	}
	local := t.In(s.Location)
	mins := local.Hour()*60 + local.Minute()
	return mins >= s.OpenMinute && mins <= s.CloseMinute
}

// BucketStart aligns a timestamp to the start of the bar containing it,
// anchored to the session open rather than to the hour.
//
// Anchoring matters: with a 15-minute bar and a 09:15 open, wall-clock
// bucketing would produce a 09:15-09:30 bar starting at 09:15 by coincidence
// but a 5-minute chain that disagrees with the exchange's own bars for any
// open that is not on a clean boundary. A timestamp before the open clamps to
// the open, so a pre-market print lands in the first bar rather than in a
// phantom bar of its own.
//
// A non-intraday timeframe returns the session date, which is the correct
// bucket for a daily bar.
func (s Session) BucketStart(t time.Time, tf domain.Timeframe) time.Time {
	d := tf.Duration()
	if d <= 0 {
		return s.Date(t)
	}
	open := s.Open(t)
	local := t.In(s.Location)
	if local.Before(open) {
		return open
	}
	since := local.Sub(open)
	return open.Add(since - since%d)
}

// SlotIndex returns the zero-based position of t's bar within its session,
// which is what a volume profile and a relative-volume calculation index on.
// It returns -1 for a timestamp after the close.
func (s Session) SlotIndex(t time.Time, tf domain.Timeframe) int {
	d := tf.Duration()
	if d <= 0 {
		return 0
	}
	start := s.BucketStart(t, tf)
	if start.After(s.Close(t)) {
		return -1
	}
	return int(start.Sub(s.Open(t)) / d)
}

// SlotCount returns how many whole bars of the given timeframe fit in one
// regular session.
func (s Session) SlotCount(tf domain.Timeframe) int {
	d := tf.Duration()
	if d <= 0 {
		return 1
	}
	span := time.Duration(s.CloseMinute-s.OpenMinute) * time.Minute
	return int(span / d)
}

// Calendar combines a session with a holiday source.
type Calendar struct {
	Session  Session
	Holidays ports.HolidaySource
}

// New returns a Calendar. A nil holidays source is permitted and means
// weekends are the only closures known — which is what the code this replaces
// assumed silently. Here it is at least explicit.
func New(s Session, h ports.HolidaySource) *Calendar {
	return &Calendar{Session: s, Holidays: h}
}

// TradingDay reports whether the exchange trades on t's calendar date.
//
// It fails open: if the holiday source errors, the day is reported as trading
// and the error is returned alongside. Skipping a real trading day is a worse
// outcome than one wasted run, so the caller decides whether to care, and the
// boolean is always usable.
func (c *Calendar) TradingDay(ctx context.Context, t time.Time) (bool, error) {
	if !c.Session.IsWeekday(t) {
		return false, nil
	}
	if c.Holidays == nil {
		return true, nil
	}
	day := c.Session.Date(t)
	closed, err := c.Holidays.Closed(ctx, c.Session.Exchange, day, day)
	if err != nil {
		return true, fmt.Errorf("calendar: holiday lookup for %s: %w", c.Session.DateKey(day), err)
	}
	_, isHoliday := closed[c.Session.DateKey(day)]
	return !isHoliday, nil
}

// TradingDays returns the exchange's trading dates in [from, to] inclusive.
// It makes one holiday lookup for the whole range rather than one per day.
func (c *Calendar) TradingDays(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	from, to = c.Session.Date(from), c.Session.Date(to)
	if to.Before(from) {
		return nil, nil
	}

	closed := map[string]string{}
	if c.Holidays != nil {
		var err error
		closed, err = c.Holidays.Closed(ctx, c.Session.Exchange, from, to)
		if err != nil {
			// Fail open, as TradingDay does: return the weekday-only
			// answer together with the error.
			return c.weekdaysIn(from, to, nil), fmt.Errorf("calendar: holiday lookup: %w", err)
		}
	}
	return c.weekdaysIn(from, to, closed), nil
}

func (c *Calendar) weekdaysIn(from, to time.Time, closed map[string]string) []time.Time {
	var days []time.Time
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if !c.Session.IsWeekday(d) {
			continue
		}
		if _, isHoliday := closed[c.Session.DateKey(d)]; isHoliday {
			continue
		}
		days = append(days, d)
	}
	return days
}

// StaticHolidays is a HolidaySource backed by an in-memory set, for backtests,
// tests, and as the cache behind a live source.
type StaticHolidays struct {
	// Days maps exchange to "2006-01-02" date to the holiday description.
	Days map[string]map[string]string
}

// Closed returns the closed dates for the exchange within [from, to].
//
// The range is compared as date strings, which is sound because the format is
// fixed-width and zero-padded: lexical order is chronological order.
func (s *StaticHolidays) Closed(_ context.Context, exchange string, from, to time.Time) (map[string]string, error) {
	lo, hi := from.Format(time.DateOnly), to.Format(time.DateOnly)
	out := map[string]string{}
	for day, desc := range s.Days[exchange] {
		if day >= lo && day <= hi {
			out[day] = desc
		}
	}
	return out, nil
}
