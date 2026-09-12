package risk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

var key = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func TestSizeFromStopDistance(t *testing.T) {
	// Capital 100_000.00, risking 1% = 1_000.00. Stop is 10.00 away,
	// so 100 shares.
	got := Size(SizeParams{
		Capital:      money.MustParse("100000.00"),
		RiskFraction: 0.01,
		Entry:        money.MustParse("500.00"),
		Stop:         money.MustParse("490.00"),
	})
	if got.Quantity != 100 {
		t.Errorf("Quantity = %d, want 100 (%s)", got.Quantity, got.Reason)
	}
}

func TestSizeIsSymmetricForShorts(t *testing.T) {
	long := Size(SizeParams{
		Capital: money.MustParse("100000.00"), RiskFraction: 0.01,
		Entry: money.MustParse("500.00"), Stop: money.MustParse("490.00"),
	})
	short := Size(SizeParams{
		Capital: money.MustParse("100000.00"), RiskFraction: 0.01,
		Entry: money.MustParse("500.00"), Stop: money.MustParse("510.00"),
	})
	if long.Quantity != short.Quantity {
		t.Errorf("a short with the same stop distance sized differently: %d vs %d", long.Quantity, short.Quantity)
	}
}

func TestSizeNotionalCap(t *testing.T) {
	// Risk budget alone would allow 100 shares at 500.00 = 50_000.00
	// notional. A 10_000.00 cap allows only 20.
	got := Size(SizeParams{
		Capital:      money.MustParse("100000.00"),
		RiskFraction: 0.01,
		Entry:        money.MustParse("500.00"),
		Stop:         money.MustParse("490.00"),
		MaxNotional:  money.MustParse("10000.00"),
	})
	if got.Quantity != 20 {
		t.Errorf("Quantity = %d, want 20", got.Quantity)
	}
	if got.Reason != "capped by notional limit" {
		t.Errorf("Reason = %q, want the notional cap to be named", got.Reason)
	}
}

func TestSizeLeverageWidensTheNotionalCap(t *testing.T) {
	base := SizeParams{
		Capital: money.MustParse("10000.00"), RiskFraction: 0.10,
		Entry: money.MustParse("500.00"), Stop: money.MustParse("495.00"),
	}
	// Risk budget 1_000.00 over a 5.00 stop = 200 shares, but 200 shares at
	// 500.00 is 100_000.00 of notional against 10_000.00 of cash.
	cash := Size(base)
	if cash.Quantity != 20 {
		t.Errorf("cash quantity = %d, want 20 (capped at 1x capital)", cash.Quantity)
	}
	base.Leverage = 5
	levered := Size(base)
	if levered.Quantity != 100 {
		t.Errorf("levered quantity = %d, want 100 (5x buying power)", levered.Quantity)
	}
}

func TestSizeFloorsToWholeLots(t *testing.T) {
	// Budget affords 137 units; a lot of 50 means 100.
	got := Size(SizeParams{
		Capital:      money.MustParse("13700.00"),
		RiskFraction: 0.10,
		Entry:        money.MustParse("500.00"),
		Stop:         money.MustParse("490.00"),
		LotSize:      50,
		Leverage:     100, // keep the notional cap out of the way
	})
	if got.Quantity != 100 {
		t.Errorf("Quantity = %d, want 100 (%s)", got.Quantity, got.Reason)
	}
}

func TestSizeReturnsZeroBelowOneLot(t *testing.T) {
	got := Size(SizeParams{
		Capital:      money.MustParse("1000.00"),
		RiskFraction: 0.01,
		Entry:        money.MustParse("500.00"),
		Stop:         money.MustParse("490.00"),
		LotSize:      50,
	})
	if got.Quantity != 0 {
		t.Errorf("Quantity = %d, want 0: a fraction of a lot cannot be traded", got.Quantity)
	}
	if got.Reason == "" {
		t.Error("a zero size must explain itself")
	}
}

func TestSizeRejectsDegenerateInputs(t *testing.T) {
	cases := map[string]SizeParams{
		"entry equals stop": {Capital: 1000000, RiskFraction: 0.01, Entry: 50000, Stop: 50000},
		"zero capital":      {Capital: 0, RiskFraction: 0.01, Entry: 50000, Stop: 49000},
		"zero risk":         {Capital: 1000000, RiskFraction: 0, Entry: 50000, Stop: 49000},
		"zero entry":        {Capital: 1000000, RiskFraction: 0.01, Entry: 0, Stop: 49000},
	}
	for name, p := range cases {
		got := Size(p)
		if got.Quantity != 0 {
			t.Errorf("%s: Quantity = %d, want 0", name, got.Quantity)
		}
		if got.Reason == "" {
			t.Errorf("%s: a zero size must explain itself", name)
		}
	}
}

func sig(at time.Time) domain.Signal {
	return domain.Signal{Key: key, Kind: domain.Long, At: at}
}

func TestChainReturnsFirstRefusal(t *testing.T) {
	st := DailyState{
		Date:         "2026-01-15",
		TradesToday:  5,
		TradesPerKey: map[string]int{key.String(): 5},
		RealizedPnL:  money.MustParse("-5000.00"),
	}
	chain := Chain{
		DailyLossLimit{Limit: money.MustParse("2000.00")},
		MaxTradesPerDay{Max: 3},
	}
	err := chain.Check(context.Background(), sig(time.Now()), st)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("refusal must wrap ErrBlocked, got %v", err)
	}
	// The loss limit is first in the chain, so it must be the one reported:
	// a day that has blown its budget should say so, not report whichever
	// secondary limit also happens to be breached.
	if got := err.Error(); !contains(got, "daily_loss_limit") {
		t.Errorf("error = %q, want the first gate in the chain to be named", got)
	}
}

func TestDailyLossLimitAllowsBelowThreshold(t *testing.T) {
	g := DailyLossLimit{Limit: money.MustParse("2000.00")}
	st := DailyState{RealizedPnL: money.MustParse("-1999.99")}
	if err := g.Check(context.Background(), sig(time.Now()), st); err != nil {
		t.Errorf("a loss just under the limit must be allowed, got %v", err)
	}
	st.RealizedPnL = money.MustParse("-2000.00")
	if err := g.Check(context.Background(), sig(time.Now()), st); err == nil {
		t.Error("a loss exactly at the limit must be refused")
	}
}

func TestDailyLossLimitIgnoresProfit(t *testing.T) {
	g := DailyLossLimit{Limit: money.MustParse("2000.00")}
	st := DailyState{RealizedPnL: money.MustParse("9999.00")}
	if err := g.Check(context.Background(), sig(time.Now()), st); err != nil {
		t.Errorf("a profitable day must not trip the loss limit, got %v", err)
	}
}

func TestZeroLimitsDisableTheirGates(t *testing.T) {
	st := DailyState{
		TradesToday:   999,
		TradesPerKey:  map[string]int{key.String(): 999},
		OpenPositions: 999,
		NotionalOpen:  money.MustParse("9999999.00"),
		RealizedPnL:   money.MustParse("-999999.00"),
	}
	chain := Chain{
		DailyLossLimit{Limit: 0},
		MaxTradesPerDay{Max: 0},
		MaxTradesPerSymbol{Max: 0},
		MaxConcurrentPositions{Max: 0},
		MaxOpenNotional{Max: 0},
	}
	if err := chain.Check(context.Background(), sig(time.Now()), st); err != nil {
		t.Errorf("an unset limit must disable its gate rather than refuse everything, got %v", err)
	}
}

func TestKillSwitchLatches(t *testing.T) {
	tr := NewTracker(NewDailyState("2026-01-15"))
	tr.Trip("max_slippage")
	tr.Trip("something_else")

	st := tr.Snapshot()
	if st.KillSwitchName != "max_slippage" {
		t.Errorf("kill switch = %q, want the first trip to be retained", st.KillSwitchName)
	}
	if err := (KillSwitch{}).Check(context.Background(), sig(time.Now()), st); err == nil {
		t.Error("a tripped kill switch must refuse")
	}
}

func TestTradingWindow(t *testing.T) {
	loc := time.UTC
	g := TradingWindow{From: 9*60 + 30, Until: 15 * 60, Location: loc}
	inside := time.Date(2026, 1, 15, 10, 0, 0, 0, loc)
	early := time.Date(2026, 1, 15, 9, 20, 0, 0, loc)
	late := time.Date(2026, 1, 15, 15, 20, 0, 0, loc)

	if err := g.Check(context.Background(), sig(inside), DailyState{}); err != nil {
		t.Errorf("10:00 should be inside the window, got %v", err)
	}
	if err := g.Check(context.Background(), sig(early), DailyState{}); err == nil {
		t.Error("09:20 should be refused as too early")
	}
	if err := g.Check(context.Background(), sig(late), DailyState{}); err == nil {
		t.Error("15:20 should be refused as too late")
	}
}

func TestDailyStateFreshForDiscardsAnotherDay(t *testing.T) {
	yesterday := &DailyState{
		Date: "2026-01-14", TradesToday: 7,
		TradesPerKey: map[string]int{key.String(): 7},
		RealizedPnL:  money.MustParse("-5000.00"),
	}
	got := yesterday.FreshFor("2026-01-15")
	if got.TradesToday != 0 || got.RealizedPnL != 0 || len(got.TradesPerKey) != 0 {
		t.Errorf("yesterday's counters leaked into today: %+v", got)
	}

	same := (&DailyState{Date: "2026-01-15", TradesToday: 3}).FreshFor("2026-01-15")
	if same.TradesToday != 3 {
		t.Error("today's counters must be preserved")
	}
	if same.TradesPerKey == nil {
		t.Error("a nil per-key map must be initialised on load")
	}

	if got := (*DailyState)(nil).FreshFor("2026-01-15"); got == nil || got.Date != "2026-01-15" {
		t.Error("a nil state must yield fresh counters rather than panicking")
	}
}

func TestTrackerSnapshotIsACopy(t *testing.T) {
	tr := NewTracker(NewDailyState("2026-01-15"))
	tr.RecordTrade(key)

	snap := tr.Snapshot()
	snap.TradesPerKey[key.String()] = 99
	snap.TradesToday = 99

	if again := tr.Snapshot(); again.TradesToday != 1 || again.TradesPerKey[key.String()] != 1 {
		t.Error("mutating a snapshot must not affect the tracker's state")
	}
}

func TestTrailIsMonotonic(t *testing.T) {
	entry := money.MustParse("500.00")
	// Long: price runs to 520, ATR 2.00, multiple 2 -> stop 516.00.
	got := Trail(domain.Buy, entry, money.MustParse("520.00"), 200, 2)
	if got != money.MustParse("516.00") {
		t.Errorf("long trail = %s, want 516.00", got)
	}
	// The extreme falls back; the stop must not follow it down.
	got = Trail(domain.Buy, got, money.MustParse("505.00"), 200, 2)
	if got != money.MustParse("516.00") {
		t.Errorf("long trail moved against the position: %s", got)
	}

	// Short: price falls to 480, stop 484.00, and must not rise afterwards.
	s := Trail(domain.Sell, money.MustParse("500.00"), money.MustParse("480.00"), 200, 2)
	if s != money.MustParse("484.00") {
		t.Errorf("short trail = %s, want 484.00", s)
	}
	s2 := Trail(domain.Sell, s, money.MustParse("495.00"), 200, 2)
	if s2 != money.MustParse("484.00") {
		t.Errorf("short trail moved against the position: %s", s2)
	}
}

func TestBreakeven(t *testing.T) {
	entry, initial := money.MustParse("500.00"), money.MustParse("490.00")

	// Below 1R the stop is untouched.
	got := Breakeven(domain.Buy, initial, entry, initial, money.MustParse("505.00"), true)
	if got != initial {
		t.Errorf("stop moved before 1R: %s", got)
	}
	// At 1R (510.00) it moves to entry.
	got = Breakeven(domain.Buy, initial, entry, initial, money.MustParse("510.00"), true)
	if got != entry {
		t.Errorf("stop = %s, want breakeven at %s", got, entry)
	}
	// Disabled, it never moves.
	got = Breakeven(domain.Buy, initial, entry, initial, money.MustParse("510.00"), false)
	if got != initial {
		t.Errorf("stop moved while the rule was disabled: %s", got)
	}
	// Shorts too: 1R below entry is 490.00.
	sEntry, sInitial := money.MustParse("500.00"), money.MustParse("510.00")
	got = Breakeven(domain.Sell, sInitial, sEntry, sInitial, money.MustParse("490.00"), true)
	if got != sEntry {
		t.Errorf("short stop = %s, want breakeven at %s", got, sEntry)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
