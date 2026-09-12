// Package risk sizes positions and decides whether a signal may be acted on.
//
// It replaces six near-identical risk managers, two of which were a verbatim
// fork of each other. The differences between them were not disagreements about
// the formula — every one sized a position from the stop distance — but about
// which cap applied: one capped by leverage, one by a percentage of equity, one
// by a notional ceiling, one not at all. Here the cap is a parameter, so those
// four are one function.
//
// Gates are composed rather than hardcoded into a single Evaluate method, so a
// project adds a rule without editing shared code and the order of evaluation
// is visible at the call site.
package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// SizeParams is everything needed to size one position.
type SizeParams struct {
	// Capital is the account equity the risk fraction applies to.
	Capital money.Money
	// RiskFraction is the share of capital lost if the stop is hit,
	// expressed as a fraction: 0.01 is one percent.
	RiskFraction float64
	// Entry and Stop bracket the per-unit risk. Their order does not
	// matter; the distance is taken as an absolute value so a short sizes
	// exactly as a long does.
	Entry money.Money
	Stop  money.Money
	// LotSize rounds the result down to a whole number of lots. Use 0 or 1
	// for cash equity.
	LotSize int
	// MaxNotional caps one position's value. Zero means uncapped.
	MaxNotional money.Money
	// Leverage multiplies buying power for the notional check. Zero or 1
	// means cash.
	Leverage float64
}

// SizeResult is a quantity and the reason it came out as it did. The reason is
// always populated, including on success, because "why is this position
// smaller than I expected" is the question a sizing function is asked most.
type SizeResult struct {
	Quantity int
	Reason   string
}

// Size computes a position size from the stop distance.
//
// The result is the smaller of the volatility-derived quantity and the
// notional cap, floored to a whole lot. A budget too small for one lot returns
// zero rather than a fractional contract, because a fraction of a lot cannot
// be traded and rounding it up would silently exceed the risk budget.
func Size(p SizeParams) SizeResult {
	if p.Capital <= 0 {
		return SizeResult{0, "capital is not positive"}
	}
	if p.RiskFraction <= 0 {
		return SizeResult{0, "risk fraction is not positive"}
	}
	if p.Entry <= 0 {
		return SizeResult{0, "entry price is not positive"}
	}

	perUnitRisk := (p.Entry - p.Stop).Abs()
	if perUnitRisk == 0 {
		return SizeResult{0, "entry equals stop, so risk per unit is zero"}
	}

	budget := p.Capital.MulFraction(p.RiskFraction)
	qty := int(budget / perUnitRisk)
	reason := "sized by risk budget"

	cap, capReason := p.notionalCap()
	if byNotional := int(cap / p.Entry); byNotional < qty {
		qty, reason = byNotional, capReason
	}

	if lot := p.LotSize; lot > 1 {
		lots := qty / lot
		if lots < 1 {
			return SizeResult{0, fmt.Sprintf("risk budget affords less than one lot of %d", lot)}
		}
		qty = lots * lot
		reason += ", floored to whole lots"
	}

	if qty <= 0 {
		return SizeResult{0, "risk budget affords less than one unit"}
	}
	return SizeResult{qty, reason}
}

// notionalCap returns the position value ceiling and the reason for it.
//
// Buying power is always a ceiling, even with no MaxNotional set: a cash
// account cannot take a position larger than its capital, and a risk budget
// alone does not know that. A wide stop on a small account produces a large
// share count, and without this the sizer would happily return a position
// worth ten times the equity behind it.
func (p SizeParams) notionalCap() (money.Money, string) {
	leverage := p.Leverage
	if leverage < 1 {
		leverage = 1
	}
	cap, reason := p.Capital.MulFraction(leverage), "capped by buying power"
	if p.MaxNotional > 0 && p.MaxNotional < cap {
		cap, reason = p.MaxNotional, "capped by notional limit"
	}
	return cap, reason
}

// DailyState is the per-day counters the gates read and the executor updates.
// It is persisted through the store's key-value repository between restarts:
// a process that restarts at noon must not forget that it has already lost its
// daily budget.
type DailyState struct {
	// Date is the trading date these counters belong to, as "2006-01-02" in
	// the exchange's timezone. State from another date is discarded on load
	// rather than carried forward.
	Date           string         `json:"date"`
	TradesToday    int            `json:"trades_today"`
	TradesPerKey   map[string]int `json:"trades_per_key"`
	RealizedPnL    money.Money    `json:"realized_pnl"`
	OpenPositions  int            `json:"open_positions"`
	NotionalOpen   money.Money    `json:"notional_open"`
	KillSwitchName string         `json:"kill_switch,omitempty"`
}

// NewDailyState returns empty counters for a trading date.
func NewDailyState(date string) *DailyState {
	return &DailyState{Date: date, TradesPerKey: map[string]int{}}
}

// FreshFor returns the state if it belongs to date, and empty counters
// otherwise. Loading yesterday's counters into today is the failure this
// prevents, and every implementation tradekit replaces had to write it out.
func (s *DailyState) FreshFor(date string) *DailyState {
	if s == nil || s.Date != date {
		return NewDailyState(date)
	}
	if s.TradesPerKey == nil {
		s.TradesPerKey = map[string]int{}
	}
	return s
}

// Tracker guards a DailyState for concurrent use by a scanner goroutine and an
// order-update goroutine.
type Tracker struct {
	mu    sync.Mutex
	state *DailyState
}

// NewTracker wraps a state for concurrent access.
func NewTracker(s *DailyState) *Tracker { return &Tracker{state: s} }

// Snapshot returns a copy safe to read without holding the lock.
func (t *Tracker) Snapshot() DailyState {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := *t.state
	cp.TradesPerKey = make(map[string]int, len(t.state.TradesPerKey))
	for k, v := range t.state.TradesPerKey {
		cp.TradesPerKey[k] = v
	}
	return cp
}

// RecordTrade increments the counters after an entry is placed.
func (t *Tracker) RecordTrade(key domain.InstrumentKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.TradesToday++
	t.state.TradesPerKey[key.String()]++
}

// SetRealizedPnL replaces the day's realised profit with the broker's figure.
//
// It is a replace rather than an add because the daily-loss kill switch is only
// meaningful against a number the broker agrees with; accumulating locally
// drifts from the broker's view over a day of partial fills.
func (t *Tracker) SetRealizedPnL(v money.Money) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.RealizedPnL = v
}

// SetExposure records the current open position count and notional.
func (t *Tracker) SetExposure(open int, notional money.Money) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.OpenPositions, t.state.NotionalOpen = open, notional
}

// Trip latches a kill switch by name. Once tripped it stays tripped for the
// rest of the trading day: a kill switch that resets when the condition
// momentarily clears is not a kill switch.
func (t *Tracker) Trip(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.KillSwitchName == "" {
		t.state.KillSwitchName = name
	}
}

// ErrBlocked is the sentinel every gate rejection wraps, so a caller can tell
// a risk refusal from a transport failure with errors.Is.
var ErrBlocked = errors.New("risk: blocked")

// Blocked builds a rejection carrying the gate's name and reason.
func Blocked(gate, reason string) error {
	return fmt.Errorf("%w by %s: %s", ErrBlocked, gate, reason)
}

// Gate decides whether one signal may proceed.
type Gate interface {
	// Name identifies the gate in logs and rejections.
	Name() string
	// Check returns nil to allow, or an error wrapping ErrBlocked to refuse.
	Check(ctx context.Context, sig domain.Signal, st DailyState) error
}

// Chain evaluates gates in order and returns the first refusal.
//
// Order is the caller's choice and it matters: a hard stop such as the daily
// loss limit belongs first, so that a day which has blown its budget reports
// that rather than reporting whichever per-symbol limit happened to trip.
type Chain []Gate

// Check runs every gate until one refuses.
func (c Chain) Check(ctx context.Context, sig domain.Signal, st DailyState) error {
	for _, g := range c {
		if err := g.Check(ctx, sig, st); err != nil {
			return err
		}
	}
	return nil
}

// KillSwitch refuses everything once a switch has been latched.
type KillSwitch struct{}

// Name identifies the gate.
func (KillSwitch) Name() string { return "kill_switch" }

// Check refuses if any kill switch has tripped today.
func (k KillSwitch) Check(_ context.Context, _ domain.Signal, st DailyState) error {
	if st.KillSwitchName != "" {
		return Blocked(k.Name(), "tripped: "+st.KillSwitchName)
	}
	return nil
}

// DailyLossLimit refuses new entries once the day's realised loss reaches the
// limit. Limit is a positive amount: the loss the account may absorb.
type DailyLossLimit struct{ Limit money.Money }

// Name identifies the gate.
func (DailyLossLimit) Name() string { return "daily_loss_limit" }

// Check refuses when realised loss has reached the limit.
func (g DailyLossLimit) Check(_ context.Context, _ domain.Signal, st DailyState) error {
	if g.Limit <= 0 {
		return nil
	}
	if st.RealizedPnL < 0 && st.RealizedPnL.Abs() >= g.Limit {
		return Blocked(g.Name(), fmt.Sprintf("realised loss %s has reached the %s limit", st.RealizedPnL.Abs(), g.Limit))
	}
	return nil
}

// MaxTradesPerDay caps how many entries may be taken in one session.
type MaxTradesPerDay struct{ Max int }

// Name identifies the gate.
func (MaxTradesPerDay) Name() string { return "max_trades_per_day" }

// Check refuses once the day's entry count reaches the cap.
func (g MaxTradesPerDay) Check(_ context.Context, _ domain.Signal, st DailyState) error {
	if g.Max <= 0 {
		return nil
	}
	if st.TradesToday >= g.Max {
		return Blocked(g.Name(), fmt.Sprintf("%d trades already taken today", st.TradesToday))
	}
	return nil
}

// MaxTradesPerSymbol caps re-entries into the same instrument in one session,
// which is what stops a chopping symbol from consuming the whole day's budget.
type MaxTradesPerSymbol struct{ Max int }

// Name identifies the gate.
func (MaxTradesPerSymbol) Name() string { return "max_trades_per_symbol" }

// Check refuses once this instrument's entry count reaches the cap.
func (g MaxTradesPerSymbol) Check(_ context.Context, sig domain.Signal, st DailyState) error {
	if g.Max <= 0 {
		return nil
	}
	if n := st.TradesPerKey[sig.Key.String()]; n >= g.Max {
		return Blocked(g.Name(), fmt.Sprintf("%s already traded %d times today", sig.Key, n))
	}
	return nil
}

// MaxConcurrentPositions caps how many positions may be open at once.
type MaxConcurrentPositions struct{ Max int }

// Name identifies the gate.
func (MaxConcurrentPositions) Name() string { return "max_concurrent_positions" }

// Check refuses once the open position count reaches the cap.
func (g MaxConcurrentPositions) Check(_ context.Context, _ domain.Signal, st DailyState) error {
	if g.Max <= 0 {
		return nil
	}
	if st.OpenPositions >= g.Max {
		return Blocked(g.Name(), fmt.Sprintf("%d positions already open", st.OpenPositions))
	}
	return nil
}

// MaxOpenNotional caps the total value of open positions.
type MaxOpenNotional struct{ Max money.Money }

// Name identifies the gate.
func (MaxOpenNotional) Name() string { return "max_open_notional" }

// Check refuses once open exposure reaches the cap.
func (g MaxOpenNotional) Check(_ context.Context, _ domain.Signal, st DailyState) error {
	if g.Max <= 0 {
		return nil
	}
	if st.NotionalOpen >= g.Max {
		return Blocked(g.Name(), fmt.Sprintf("open notional %s has reached the %s cap", st.NotionalOpen, g.Max))
	}
	return nil
}

// TradingWindow refuses entries outside a time-of-day window.
//
// It exists because the two most common intraday mistakes are entering in the
// first minutes before a range has formed and entering close enough to the bell
// that the position is squared off before the thesis can play out.
type TradingWindow struct {
	// From and Until are minutes from midnight in Location.
	From, Until int
	Location    *time.Location
}

// Name identifies the gate.
func (TradingWindow) Name() string { return "trading_window" }

// Check refuses a signal timestamped outside the window.
func (g TradingWindow) Check(_ context.Context, sig domain.Signal, _ DailyState) error {
	loc := g.Location
	if loc == nil {
		loc = time.UTC
	}
	local := sig.At.In(loc)
	mins := local.Hour()*60 + local.Minute()
	if mins < g.From || mins > g.Until {
		return Blocked(g.Name(), fmt.Sprintf("%s is outside the %02d:%02d-%02d:%02d window",
			local.Format("15:04"), g.From/60, g.From%60, g.Until/60, g.Until%60))
	}
	return nil
}

// Trail returns a chandelier trailing stop: the extreme price reached since
// entry, pulled back by a multiple of ATR.
//
// The stop is monotonic — it never moves against the position. Every trailing
// implementation this replaces had to state that separately, and one of them
// only enforced it for longs.
func Trail(side domain.Side, current, extreme money.Money, atr float64, mult float64) money.Money {
	offset := money.Money(math.Round(atr * mult))
	if side == domain.Buy {
		if candidate := extreme - offset; candidate > current {
			return candidate
		}
		return current
	}
	if candidate := extreme + offset; candidate < current || current == 0 {
		return candidate
	}
	return current
}

// Breakeven raises the stop to the entry price once the position has gained
// one unit of initial risk, when enabled.
func Breakeven(side domain.Side, current, entry, initialStop, last money.Money, enabled bool) money.Money {
	if !enabled {
		return current
	}
	oneR := (entry - initialStop).Abs()
	if oneR == 0 {
		return current
	}
	if side == domain.Buy {
		if last >= entry+oneR && entry > current {
			return entry
		}
		return current
	}
	if last <= entry-oneR && (entry < current || current == 0) {
		return entry
	}
	return current
}
