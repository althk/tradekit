package backtest

import (
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/core/stats"
)

// Snapshotter records mark-to-market equity once per trading day.
//
// stats.EquityCurve builds a curve from closed trades, which is the right input
// for trade statistics and the wrong one for drawdown: a curve that only moves
// when a trade closes cannot see the day a position was 30% underwater and
// recovered, so the drawdown it reports understates the real one. This marks
// the book to market once per session instead. Both curves are legitimate; the
// distinction has to be stated at the call site.
//
// It is a date-boundary detector, not an accounting engine: paper.Broker.Equity
// already values cash plus open exposure, and this does not reimplement it.
type Snapshotter struct {
	Broker *paper.Broker
	// Loc is the calendar's location, which decides where one date ends and
	// the next begins. Nil uses each bar's own zone, which is right when the
	// feed carries the exchange's offset and wrong when it carries UTC.
	Loc *time.Location
	// Points is the curve so far, in ascending date order.
	Points []stats.Point

	day  string // "YYYY-MM-DD" of the date being accumulated; empty before the first bar
	at   time.Time
	mark money.Money
	seen bool
}

// Observe is called with each bar, after the broker has seen it.
//
// It emits one point per calendar date, on the first bar of the next date,
// using the previous date's last mark. A day changes when the "YYYY-MM-DD" key
// changes, not when the gap exceeds 24 hours: sessions are not 24 hours apart
// and a weekend is not three days of flat equity.
func (s *Snapshotter) Observe(c domain.Candle) {
	key := s.dateKey(c.Start)
	if s.seen && key != s.day {
		s.emit()
	}
	s.day, s.at, s.seen = key, c.Start, true
	s.mark = s.Broker.Equity()
}

// Close emits the final date's point. It must be called after Replay.Run
// returns: the last day never has a "next date" to trigger its emission, so a
// caller who omits Close silently loses the final session — which is exactly
// the session a walk-forward run cares about.
func (s *Snapshotter) Close() {
	if s.seen {
		s.emit()
		s.seen = false
	}
}

func (s *Snapshotter) emit() {
	s.Points = append(s.Points, stats.Point{At: s.at, Equity: s.mark})
}

func (s *Snapshotter) dateKey(t time.Time) string {
	if s.Loc != nil {
		t = t.In(s.Loc)
	}
	return t.Format(time.DateOnly)
}
