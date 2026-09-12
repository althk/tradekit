package stats

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func day(n int) time.Time {
	return time.Date(2026, 1, n, 15, 30, 0, 0, time.UTC)
}

// trade builds a closed trade with the given net P&L in rupees.
func trade(n int, netRupees string, paper bool) domain.Trade {
	net := money.MustParse(netRupees)
	return domain.Trade{
		Key:        domain.InstrumentKey{Exchange: "NSE", Symbol: "X"},
		Side:       domain.Buy,
		Quantity:   1,
		EntryPrice: money.MustParse("100.00"),
		ExitPrice:  money.MustParse("100.00") + net,
		EntryAt:    day(n).Add(-2 * time.Hour),
		ExitAt:     day(n),
		GrossPnL:   net,
		NetPnL:     net,
		ExitReason: domain.ExitTarget,
		Paper:      paper,
	}
}

func TestSummarizeBasics(t *testing.T) {
	trades := []domain.Trade{
		trade(1, "100.00", false),
		trade(2, "-50.00", false),
		trade(3, "200.00", false),
		trade(4, "-50.00", false),
	}
	m, err := Summarize(trades)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Trades != 4 || m.Wins != 2 || m.Losses != 2 {
		t.Errorf("counts = %d trades, %d wins, %d losses; want 4/2/2", m.Trades, m.Wins, m.Losses)
	}
	if m.NetPnL != money.MustParse("200.00") {
		t.Errorf("NetPnL = %s, want 200.00", m.NetPnL)
	}
	if m.GrossProfit != money.MustParse("300.00") || m.GrossLoss != money.MustParse("100.00") {
		t.Errorf("gross = +%s / -%s, want +300.00 / -100.00", m.GrossProfit, m.GrossLoss)
	}
	if m.WinRate != 0.5 {
		t.Errorf("WinRate = %v, want 0.5", m.WinRate)
	}
	if m.ProfitFactor != 3 {
		t.Errorf("ProfitFactor = %v, want 3", m.ProfitFactor)
	}
	if m.Expectancy != money.MustParse("50.00") {
		t.Errorf("Expectancy = %s, want 50.00", m.Expectancy)
	}
	if m.AvgWin != money.MustParse("150.00") || m.AvgLoss != money.MustParse("50.00") {
		t.Errorf("averages = win %s / loss %s, want 150.00 / 50.00", m.AvgWin, m.AvgLoss)
	}
	if m.AvgHolding != 2*time.Hour {
		t.Errorf("AvgHolding = %v, want 2h", m.AvgHolding)
	}
}

func TestSummarizeRefusesMixedModes(t *testing.T) {
	_, err := Summarize([]domain.Trade{
		trade(1, "100.00", false),
		trade(2, "100.00", true),
	})
	if !errors.Is(err, ErrMixedModes) {
		t.Errorf("err = %v, want ErrMixedModes: blending simulated and real fills produces a plausible, wrong number", err)
	}
}

func TestSummarizeAllPaperIsFine(t *testing.T) {
	if _, err := Summarize([]domain.Trade{trade(1, "10.00", true), trade(2, "20.00", true)}); err != nil {
		t.Errorf("a consistently paper set must summarise cleanly, got %v", err)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	m, err := Summarize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Trades != 0 || m.ProfitFactor != 0 {
		t.Errorf("an empty set must summarise to zeroes, got %+v", m)
	}
}

func TestProfitFactorWithNoLosses(t *testing.T) {
	m, err := Summarize([]domain.Trade{trade(1, "100.00", false), trade(2, "50.00", false)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !math.IsInf(m.ProfitFactor, 1) {
		t.Errorf("ProfitFactor = %v, want +Inf when there are no losing trades", m.ProfitFactor)
	}
}

func TestSummarizeIsOrderIndependent(t *testing.T) {
	ordered := []domain.Trade{trade(1, "100.00", false), trade(2, "-300.00", false), trade(3, "50.00", false)}
	shuffled := []domain.Trade{ordered[2], ordered[0], ordered[1]}

	a, err := Summarize(ordered)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Summarize(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if a.MaxDrawdown != b.MaxDrawdown {
		t.Errorf("drawdown depended on input order: %s vs %s", a.MaxDrawdown, b.MaxDrawdown)
	}
}

func TestMaxDrawdown(t *testing.T) {
	curve := []Point{
		{Equity: money.MustParse("1000.00")},
		{Equity: money.MustParse("1200.00")}, // peak
		{Equity: money.MustParse("900.00")},  // -300 from the peak
		{Equity: money.MustParse("1100.00")},
		{Equity: money.MustParse("1050.00")},
	}
	amt, pct := MaxDrawdown(curve)
	if amt != money.MustParse("300.00") {
		t.Errorf("MaxDrawdown = %s, want 300.00", amt)
	}
	if math.Abs(pct-0.25) > 1e-9 {
		t.Errorf("MaxDrawdownPct = %v, want 0.25 (relative to the peak it fell from)", pct)
	}
}

func TestMaxDrawdownOfARisingCurveIsZero(t *testing.T) {
	curve := []Point{{Equity: 100}, {Equity: 200}, {Equity: 300}}
	if amt, pct := MaxDrawdown(curve); amt != 0 || pct != 0 {
		t.Errorf("a monotonically rising curve has no drawdown, got %s / %v", amt, pct)
	}
}

func TestEquityCurveStartsFromOpening(t *testing.T) {
	curve := EquityCurve([]domain.Trade{trade(1, "100.00", false), trade(2, "-40.00", false)}, money.MustParse("1000.00"))
	if len(curve) != 2 {
		t.Fatalf("got %d points, want 2", len(curve))
	}
	if curve[0].Equity != money.MustParse("1100.00") || curve[1].Equity != money.MustParse("1060.00") {
		t.Errorf("curve = %s, %s; want 1100.00, 1060.00", curve[0].Equity, curve[1].Equity)
	}
}

func TestRMultiple(t *testing.T) {
	long := domain.Trade{
		Side:       domain.Buy,
		EntryPrice: money.MustParse("100.00"),
		ExitPrice:  money.MustParse("130.00"),
	}
	got, ok := RMultiple(long, money.MustParse("90.00"))
	if !ok || math.Abs(got-3) > 1e-9 {
		t.Errorf("RMultiple = %v (ok=%v), want 3", got, ok)
	}

	short := domain.Trade{
		Side:       domain.Sell,
		EntryPrice: money.MustParse("100.00"),
		ExitPrice:  money.MustParse("90.00"),
	}
	got, ok = RMultiple(short, money.MustParse("105.00"))
	if !ok || math.Abs(got-2) > 1e-9 {
		t.Errorf("short RMultiple = %v (ok=%v), want 2", got, ok)
	}

	if _, ok := RMultiple(long, money.MustParse("100.00")); ok {
		t.Error("a zero initial risk has no R multiple and must report false")
	}
}

func TestDailyReturnsUsesTheLastPointOfEachDay(t *testing.T) {
	curve := []Point{
		{At: day(1).Add(-time.Hour), Equity: money.MustParse("1000.00")},
		{At: day(1), Equity: money.MustParse("1100.00")}, // same day, later
		{At: day(2), Equity: money.MustParse("1210.00")},
	}
	got := DailyReturns(curve, time.UTC)
	if len(got) != 1 {
		t.Fatalf("got %d returns, want 1 (two distinct days)", len(got))
	}
	if math.Abs(got[0]-0.1) > 1e-9 {
		t.Errorf("return = %v, want 0.1 from the day's closing equity", got[0])
	}
}

func TestSharpe(t *testing.T) {
	// A constant return series has no variance, so the ratio is undefined.
	if _, ok := Sharpe([]float64{0.01, 0.01, 0.01}, 252, 0); ok {
		t.Error("a zero-variance series must report false rather than an infinite ratio")
	}
	if _, ok := Sharpe([]float64{0.01}, 252, 0); ok {
		t.Error("a single return is not enough to compute a sample deviation")
	}

	got, ok := Sharpe([]float64{0.01, -0.005, 0.02, 0.0, 0.015}, 252, 0)
	if !ok {
		t.Fatal("expected a Sharpe ratio")
	}
	if got <= 0 {
		t.Errorf("a net-positive return series should give a positive Sharpe, got %v", got)
	}
	// The same series with the risk-free rate above its mean must invert.
	neg, ok := Sharpe([]float64{0.01, -0.005, 0.02, 0.0, 0.015}, 252, 0.05)
	if !ok || neg >= 0 {
		t.Errorf("a risk-free rate above the mean return should give a negative Sharpe, got %v", neg)
	}
}

func TestByExitReason(t *testing.T) {
	a := trade(1, "10.00", false)
	a.ExitReason = domain.ExitStop
	b := trade(2, "10.00", false)
	b.ExitReason = domain.ExitTarget
	c := trade(3, "10.00", false)
	c.ExitReason = domain.ExitTarget

	m, err := Summarize([]domain.Trade{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if m.ByExitReason[domain.ExitTarget] != 2 || m.ByExitReason[domain.ExitStop] != 1 {
		t.Errorf("ByExitReason = %v, want 2 targets and 1 stop", m.ByExitReason)
	}
}
