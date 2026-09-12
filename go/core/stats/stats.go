// Package stats summarises a set of closed trades.
//
// Every dashboard and every backtest report in the projects tradekit replaces
// recomputed these same figures, and no two agreed on all of them. Sharing the
// queries rather than the templates is the point: a win rate should not depend
// on which project is displaying it.
package stats

import (
	"errors"
	"math"
	"sort"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// ErrMixedModes reports that paper and live trades were summarised together.
//
// It is an error rather than a silent merge because every aggregate below —
// win rate, profit factor, drawdown, the equity curve — is meaningless when
// simulated fills are blended with real ones, and the resulting number looks
// entirely plausible.
var ErrMixedModes = errors.New("stats: paper and live trades cannot be summarised together")

// Metrics is the summary of a set of closed trades. Monetary fields are in
// minor units; ratios are plain fractions, not percentages.
type Metrics struct {
	Trades    int
	Wins      int
	Losses    int
	Breakeven int

	GrossProfit money.Money // sum of winning trades' net P&L
	GrossLoss   money.Money // sum of losing trades' net P&L, as a positive amount
	NetPnL      money.Money
	Charges     money.Money

	WinRate      float64     // wins / trades
	ProfitFactor float64     // gross profit / gross loss; +Inf when there are no losses
	Expectancy   money.Money // average net P&L per trade

	AvgWin  money.Money
	AvgLoss money.Money // a positive amount

	MaxDrawdown    money.Money // largest peak-to-trough fall in the equity curve
	MaxDrawdownPct float64     // as a fraction of the peak it fell from

	AvgHolding time.Duration

	ByExitReason map[domain.ExitReason]int
}

// Summarize computes the metrics for a set of closed trades.
//
// Trades need not be sorted; they are ordered by exit time internally, because
// drawdown is meaningless over an arbitrary ordering.
func Summarize(trades []domain.Trade) (Metrics, error) {
	m := Metrics{ByExitReason: map[domain.ExitReason]int{}}
	if len(trades) == 0 {
		return m, nil
	}
	if mixedModes(trades) {
		return m, ErrMixedModes
	}

	ordered := make([]domain.Trade, len(trades))
	copy(ordered, trades)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ExitAt.Before(ordered[j].ExitAt) })

	var holding time.Duration
	for _, t := range ordered {
		m.Trades++
		m.NetPnL += t.NetPnL
		m.Charges += t.Charges
		m.ByExitReason[t.ExitReason]++
		holding += t.ExitAt.Sub(t.EntryAt)

		switch {
		case t.NetPnL > 0:
			m.Wins++
			m.GrossProfit += t.NetPnL
		case t.NetPnL < 0:
			m.Losses++
			m.GrossLoss += -t.NetPnL
		default:
			m.Breakeven++
		}
	}

	m.WinRate = float64(m.Wins) / float64(m.Trades)
	m.Expectancy = m.NetPnL / money.Money(m.Trades)
	m.AvgHolding = holding / time.Duration(m.Trades)

	if m.Wins > 0 {
		m.AvgWin = m.GrossProfit / money.Money(m.Wins)
	}
	if m.Losses > 0 {
		m.AvgLoss = m.GrossLoss / money.Money(m.Losses)
	}

	switch {
	case m.GrossLoss > 0:
		m.ProfitFactor = float64(m.GrossProfit) / float64(m.GrossLoss)
	case m.GrossProfit > 0:
		// No losing trades. Infinity is the honest answer; a caller
		// formatting it must decide how to display that rather than
		// being handed a fabricated finite number.
		m.ProfitFactor = math.Inf(1)
	}

	m.MaxDrawdown, m.MaxDrawdownPct = MaxDrawdown(EquityCurve(ordered, 0))
	return m, nil
}

func mixedModes(trades []domain.Trade) bool {
	first := trades[0].Paper
	for _, t := range trades[1:] {
		if t.Paper != first {
			return true
		}
	}
	return false
}

// Point is one sample of the running equity curve.
type Point struct {
	At     time.Time
	Equity money.Money
}

// EquityCurve accumulates net P&L over trades in exit order, starting from
// opening.
func EquityCurve(trades []domain.Trade, opening money.Money) []Point {
	ordered := make([]domain.Trade, len(trades))
	copy(ordered, trades)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ExitAt.Before(ordered[j].ExitAt) })

	curve := make([]Point, 0, len(ordered))
	running := opening
	for _, t := range ordered {
		running += t.NetPnL
		curve = append(curve, Point{At: t.ExitAt, Equity: running})
	}
	return curve
}

// MaxDrawdown returns the largest peak-to-trough fall in a curve, both as an
// amount and as a fraction of the peak it fell from.
//
// The fraction is relative to each peak rather than to the starting equity, so
// a 10% fall late in a doubled account is reported as 10% and not as 5%.
func MaxDrawdown(curve []Point) (money.Money, float64) {
	if len(curve) == 0 {
		return 0, 0
	}
	peak := curve[0].Equity
	var worst money.Money
	var worstPct float64
	for _, p := range curve {
		if p.Equity > peak {
			peak = p.Equity
		}
		if fall := peak - p.Equity; fall > worst {
			worst = fall
			if peak > 0 {
				worstPct = float64(fall) / float64(peak)
			}
		}
	}
	return worst, worstPct
}

// RMultiple expresses a trade's result as a multiple of the risk taken on it.
//
// initialStop is the stop in force at entry, not wherever it was trailed to.
// A trade risking 1 and making 3 is a 3R trade regardless of the account size,
// which is what makes R comparable across instruments and across time.
// It reports false when the initial risk was zero, since no multiple exists.
func RMultiple(t domain.Trade, initialStop money.Money) (float64, bool) {
	risk := (t.EntryPrice - initialStop).Abs()
	if risk == 0 {
		return 0, false
	}
	move := t.ExitPrice - t.EntryPrice
	if t.Side == domain.Sell {
		move = -move
	}
	return float64(move) / float64(risk), true
}

// DailyReturns aggregates an equity curve into per-day fractional returns,
// which is the input Sharpe needs.
func DailyReturns(curve []Point, loc *time.Location) []float64 {
	if len(curve) < 2 {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}

	type dayEnd struct {
		day    string
		equity money.Money
	}
	var days []dayEnd
	for _, p := range curve {
		key := p.At.In(loc).Format(time.DateOnly)
		if n := len(days); n > 0 && days[n-1].day == key {
			days[n-1].equity = p.Equity
			continue
		}
		days = append(days, dayEnd{day: key, equity: p.Equity})
	}

	returns := make([]float64, 0, len(days)-1)
	for i := 1; i < len(days); i++ {
		prev := days[i-1].equity
		if prev == 0 {
			continue
		}
		returns = append(returns, float64(days[i].equity-prev)/math.Abs(float64(prev)))
	}
	return returns
}

// Sharpe is the annualised Sharpe ratio of a series of periodic returns.
//
// periodsPerYear scales the result: 252 for daily returns on an equity
// calendar. riskFree is the per-period risk-free rate, not the annual one.
// It reports false when the series has no variance, since the ratio is
// undefined rather than infinite in any useful sense.
func Sharpe(returns []float64, periodsPerYear float64, riskFree float64) (float64, bool) {
	if len(returns) < 2 || periodsPerYear <= 0 {
		return 0, false
	}

	var sum float64
	for _, r := range returns {
		sum += r - riskFree
	}
	mean := sum / float64(len(returns))

	var sq float64
	for _, r := range returns {
		d := (r - riskFree) - mean
		sq += d * d
	}
	// Sample standard deviation: the returns are a sample of the strategy's
	// behaviour, not the whole population of it.
	variance := sq / float64(len(returns)-1)
	if variance <= 0 {
		return 0, false
	}
	return mean / math.Sqrt(variance) * math.Sqrt(periodsPerYear), true
}
