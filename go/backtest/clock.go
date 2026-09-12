// Package backtest drives the paper broker forward in time, records what it
// did, and renders the result.
//
// It deliberately does not define a strategy interface. The design's central
// claim is that a backtest runs the live code path through core/paper: the
// strategy loop stays in the project, and this package hands it a clock, a
// feed, a ledger and a report. The fill model is not here either — it lives in
// core/paper, where live paper trading and every backtest see one set of rules.
package backtest

import (
	"fmt"
	"sync"
	"time"
)

// Clock reports the current time.
//
// Every backtest asks "what time is it" for three different reasons — whether
// the trading window is open, stamping a trade, and whether a time-stop has
// expired — and every donor answered it differently, one of them by calling the
// wall clock from inside a backtest, which is why its results were not
// reproducible across days. Live code holds a RealClock; a backtest holds a
// SimClock the replay driver advances. Strategy code holds the interface and
// cannot tell which.
type Clock interface {
	Now() time.Time
}

// RealClock is wall time in a fixed location.
//
// It carries a *time.Location because a strategy comparing a bar timestamp
// against a session window in the wrong zone silently trades the wrong hours.
// There is no zero-value default; construct it with the calendar's location.
type RealClock struct {
	Loc *time.Location
}

// Now returns the wall time in the clock's location.
func (c RealClock) Now() time.Time {
	if c.Loc == nil {
		panic("backtest: RealClock needs a location; a zone-less clock trades the wrong hours")
	}
	return time.Now().In(c.Loc)
}

// SimClock is time under the replay driver's control.
//
// Safe for concurrent reads; only the driver writes. The mutex matters because
// strategy code may read the clock from a goroutine the driver did not start.
type SimClock struct {
	mu  sync.RWMutex
	now time.Time
}

// NewSimClock returns a clock reading start.
func NewSimClock(start time.Time) *SimClock {
	return &SimClock{now: start}
}

// Now returns the simulated time.
func (c *SimClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

// Advance moves the clock to a later instant.
//
// It panics on going backwards rather than returning an error. A backtest that
// replays bars out of order produces plausible-looking results that are wrong
// in a way no assertion downstream can detect, so it must fail loudly at the
// moment it happens. This is a programming error in the driver, not a runtime
// condition, and it is the one place in the library where panicking is right.
func (c *SimClock) Advance(to time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if to.Before(c.now) {
		panic(fmt.Sprintf("backtest: SimClock asked to go backwards from %v to %v; the feed is out of order", c.now, to))
	}
	c.now = to
}

// Compile-time proof that both clocks satisfy the interface.
var (
	_ Clock = RealClock{}
	_ Clock = (*SimClock)(nil)
)
