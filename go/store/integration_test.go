package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// openTest returns a migrated in-memory database, or skips.
//
// This package deliberately imports no SQLite driver — the consumer chooses one
// — so these tests need a driver registered by the build that runs them. The
// sqlitedriver build tag in driver_test.go registers modernc.org/sqlite:
//
//	go test -tags sqlitedriver ./...
//
// Without it the driver-independent tests still run and these skip, so a
// checkout with no network is not a broken checkout.
func openTest(t *testing.T) *DB {
	t.Helper()

	var driverName string
	for _, name := range sql.Drivers() {
		if name == "sqlite" || name == "sqlite3" {
			driverName = name
			break
		}
	}
	if driverName == "" {
		t.Skip("no SQLite driver registered; run with -tags sqlitedriver")
	}

	db, err := Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return db
}

// openRawTest returns an open, unmigrated in-memory database, or skips.
func openRawTest(t *testing.T) *DB {
	t.Helper()
	var driverName string
	for _, name := range sql.Drivers() {
		if name == "sqlite" || name == "sqlite3" {
			driverName = name
			break
		}
	}
	if driverName == "" {
		t.Skip("no SQLite driver registered; run with -tags sqlitedriver")
	}
	db, err := Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var testKey = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	first, err := db.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("expected at least one applied migration")
	}

	// Calling Migrate on every startup must be safe.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	second, err := db.AppliedVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Errorf("re-running Migrate applied %d extra migrations", len(second)-len(first))
	}
}

func TestApplyRejectsUnknownAppliedVersion(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	// A database written by a newer binary carries a version this build
	// has never seen. Continuing would query a shape it does not know.
	_, err := db.SQL().ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (499, 'from_the_future', ?)`,
		formatTime(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}

	err = db.Migrate(ctx)
	if !errors.Is(err, ErrUnknownMigration) {
		t.Errorf("err = %v, want ErrUnknownMigration", err)
	}
}

func TestInstrumentRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	expiry := time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC)
	in := domain.Instrument{
		Key: testKey, Name: "Reliance Industries", ISIN: "INE002A01018",
		Segment: "equity", LotSize: 1, TickSize: money.MustParse("0.05"), Active: true,
	}
	opt := domain.Instrument{
		Key:     domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY26JAN23000CE"},
		Segment: "options", LotSize: 50, TickSize: money.MustParse("0.05"),
		Expiry: &expiry, Strike: money.MustParse("23000.00"), OptionType: "ce", Active: true,
	}
	if err := db.UpsertInstruments(ctx, []domain.Instrument{in, opt}); err != nil {
		t.Fatal(err)
	}

	got, err := db.Instrument(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != in.Name || got.ISIN != in.ISIN || got.TickSize != in.TickSize {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.Expiry != nil {
		t.Error("a cash equity must round-trip with a nil expiry, not the zero date")
	}

	gotOpt, err := db.Instrument(ctx, opt.Key)
	if err != nil {
		t.Fatal(err)
	}
	if gotOpt.Expiry == nil || !gotOpt.Expiry.Equal(expiry) {
		t.Errorf("option expiry = %v, want %v", gotOpt.Expiry, expiry)
	}
	if gotOpt.Strike != opt.Strike || gotOpt.LotSize != 50 {
		t.Errorf("option round trip lost fields: %+v", gotOpt)
	}

	// Upsert must update rather than duplicate.
	in.Name = "Reliance Industries Ltd"
	if err := db.UpsertInstruments(ctx, []domain.Instrument{in}); err != nil {
		t.Fatal(err)
	}
	active, err := db.ActiveInstruments(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d active instruments, want 2", len(active))
	}

	equities, err := db.ActiveInstruments(ctx, "equity")
	if err != nil {
		t.Fatal(err)
	}
	if len(equities) != 1 {
		t.Errorf("segment filter returned %d, want 1", len(equities))
	}

	if _, err := db.Instrument(ctx, domain.InstrumentKey{Exchange: "NSE", Symbol: "NOPE"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing instrument: err = %v, want ErrNotFound", err)
	}
}

func TestBrokerIDMapping(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	if err := db.UpsertInstruments(ctx, []domain.Instrument{{Key: testKey, Active: true}}); err != nil {
		t.Fatal(err)
	}
	// One instrument carries identifiers for several brokers at once.
	if err := db.SetBrokerID(ctx, testKey, "zerodha", "738561"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBrokerID(ctx, testKey, "upstox", "NSE_EQ|INE002A01018"); err != nil {
		t.Fatal(err)
	}

	if got, err := db.BrokerID(ctx, testKey, "zerodha"); err != nil || got != "738561" {
		t.Errorf("BrokerID = %q, %v; want 738561", got, err)
	}
	// The reverse lookup is what a tick stream needs.
	got, err := db.KeyForBrokerID(ctx, "upstox", "NSE_EQ|INE002A01018")
	if err != nil || got != testKey {
		t.Errorf("KeyForBrokerID = %v, %v; want %v", got, err, testKey)
	}
	if _, err := db.KeyForBrokerID(ctx, "zerodha", "999999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown broker id: err = %v, want ErrNotFound", err)
	}
}

func TestDeactivateMissing(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	a := domain.InstrumentKey{Exchange: "NSE", Symbol: "AAA"}
	b := domain.InstrumentKey{Exchange: "NSE", Symbol: "BBB"}
	if err := db.UpsertInstruments(ctx, []domain.Instrument{{Key: a, Active: true}, {Key: b, Active: true}}); err != nil {
		t.Fatal(err)
	}

	n, err := db.DeactivateMissing(ctx, "NSE", []domain.InstrumentKey{a})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deactivated %d, want 1", n)
	}
	active, err := db.ActiveInstruments(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Key != a {
		t.Errorf("active set = %v, want only %v", active, a)
	}
	// The dropped instrument's row survives so its history stays readable.
	if _, err := db.Instrument(ctx, b); err != nil {
		t.Errorf("a deactivated instrument must still be readable, got %v", err)
	}
}

func candle(day int, close money.Money) domain.Candle {
	return domain.Candle{
		Key: testKey, Timeframe: domain.D1,
		Start: time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC),
		Open:  close, High: close, Low: close, Close: close, Volume: 1000,
	}
}

func TestCandleUpsertIsIdempotent(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	batch := []domain.Candle{candle(1, 10000), candle(2, 10100), candle(3, 10200)}
	n, err := db.UpsertCandles(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("inserted %d, want 3", n)
	}

	// Re-fetching a window is the normal way to deepen history; it must not
	// duplicate rows, and it must not rewrite the value already held.
	changed := candle(2, 99999)
	n, err = db.UpsertCandles(ctx, []domain.Candle{changed})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("re-inserting an existing bar reported %d new rows, want 0", n)
	}

	got, err := db.Candles(ctx, testKey, domain.D1,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d candles, want 3", len(got))
	}
	if got[1].Close != 10100 {
		t.Errorf("an existing bar was overwritten: close = %s, want 101.00", got[1].Close)
	}
	if !got[0].Start.Before(got[1].Start) {
		t.Error("candles must come back oldest first")
	}
}

func TestLastCandleStartDrivesIncrementalSync(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	if _, ok, err := db.LastCandleStart(ctx, testKey, domain.D1); err != nil || ok {
		t.Errorf("an empty store must report no last candle, got ok=%v err=%v", ok, err)
	}

	if _, err := db.UpsertCandles(ctx, []domain.Candle{candle(1, 100), candle(5, 100), candle(3, 100)}); err != nil {
		t.Fatal(err)
	}
	last, ok, err := db.LastCandleStart(ctx, testKey, domain.D1)
	if err != nil || !ok {
		t.Fatalf("LastCandleStart: ok=%v err=%v", ok, err)
	}
	if last.Day() != 5 {
		t.Errorf("last candle start = %v, want the 5th regardless of insertion order", last)
	}

	first, lastSpan, ok, err := db.CandleSpan(ctx, testKey, domain.D1)
	if err != nil || !ok {
		t.Fatalf("CandleSpan: ok=%v err=%v", ok, err)
	}
	if first.Day() != 1 || lastSpan.Day() != 5 {
		t.Errorf("span = %v..%v, want the 1st..5th", first, lastSpan)
	}

	// A different timeframe is a different series.
	if _, ok, _ := db.LastCandleStart(ctx, testKey, domain.M5); ok {
		t.Error("a 5-minute series must not see daily bars")
	}
}

func TestTradeInsertIsIdempotent(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	tr := domain.Trade{
		Key: testKey, Strategy: "donchian", Side: domain.Buy, Quantity: 10,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("110.00"),
		EntryAt:  time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC),
		ExitAt:   time.Date(2026, 1, 6, 10, 0, 0, 0, time.UTC),
		GrossPnL: money.MustParse("100.00"), Charges: money.MustParse("6.94"),
		NetPnL: money.MustParse("93.06"), ExitReason: domain.ExitTarget,
	}
	// Re-running a reconciler over the same broker fills must not duplicate.
	for range 3 {
		if err := db.InsertTrade(ctx, "ORDER-1:0", tr, 0); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.Trades(ctx, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d trades, want 1", len(got))
	}
	if got[0].NetPnL != tr.NetPnL || got[0].Charges != tr.Charges || got[0].ExitReason != domain.ExitTarget {
		t.Errorf("trade round trip lost fields: %+v", got[0])
	}
}

func TestTradesAreScopedByMode(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	base := domain.Trade{
		Key: testKey, Side: domain.Buy, Quantity: 1,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("110.00"),
		EntryAt: time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC),
		ExitAt:  time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC),
		NetPnL:  money.MustParse("10.00"), ExitReason: domain.ExitTarget,
	}
	live, paper := base, base
	paper.Paper = true
	if err := db.InsertTrade(ctx, "LIVE-1", live, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTrade(ctx, "PAPER-1", paper, 0); err != nil {
		t.Fatal(err)
	}

	liveTrades, err := db.Trades(ctx, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	paperTrades, err := db.Trades(ctx, time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(liveTrades) != 1 || liveTrades[0].Paper {
		t.Errorf("live query returned %d rows, paper=%v", len(liveTrades), liveTrades[0].Paper)
	}
	if len(paperTrades) != 1 || !paperTrades[0].Paper {
		t.Errorf("paper query returned %d rows", len(paperTrades))
	}
}

func TestOrderLifecycle(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	req := domain.OrderRequest{
		Key: testKey, Side: domain.Buy, Quantity: 10, Type: domain.Limit,
		Product: domain.CNC, LimitPrice: money.MustParse("100.00"),
		TimeInForce: domain.Day, Tag: "donchian",
	}
	o := domain.Order{
		ID: "ORDER-1", Request: req, Status: domain.StatusOpen,
		PlacedAt:  time.Date(2026, 1, 5, 9, 20, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 5, 9, 20, 0, 0, time.UTC),
	}
	if err := db.UpsertOrder(ctx, o, false); err != nil {
		t.Fatal(err)
	}

	pending, err := db.PendingOrders(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending orders, want 1", len(pending))
	}
	if pending[0].Request.LimitPrice != req.LimitPrice || pending[0].Request.Tag != "donchian" {
		t.Errorf("order round trip lost fields: %+v", pending[0].Request)
	}

	// Reconciling the same order id updates rather than duplicating.
	o.Status = domain.StatusComplete
	o.FilledQuantity = 10
	o.AveragePrice = money.MustParse("100.25")
	o.UpdatedAt = o.PlacedAt.Add(time.Minute)
	if err := db.UpsertOrder(ctx, o, false); err != nil {
		t.Fatal(err)
	}

	if pending, err = db.PendingOrders(ctx, false); err != nil || len(pending) != 0 {
		t.Errorf("a completed order must leave the pending set, got %d (%v)", len(pending), err)
	}

	awaiting, err := db.OrdersAwaitingProtective(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(awaiting) != 1 {
		t.Fatalf("a filled order with no stop must be listed, got %d", len(awaiting))
	}

	if err := db.SetOrderProtective(ctx, "ORDER-1", "GTT-99"); err != nil {
		t.Fatal(err)
	}
	if awaiting, err = db.OrdersAwaitingProtective(ctx, false); err != nil || len(awaiting) != 0 {
		t.Errorf("once a stop is recorded the order must drop out, got %d (%v)", len(awaiting), err)
	}

	got, err := db.Order(ctx, "ORDER-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AveragePrice != money.MustParse("100.25") || got.FilledQuantity != 10 {
		t.Errorf("execution details lost: %+v", got)
	}
	// A dashboard reads the covering stop off the order; a reconciler that
	// re-upserts the order must not wipe it.
	if got.ProtectiveID != "GTT-99" {
		t.Errorf("ProtectiveID = %q, want GTT-99 to survive the read", got.ProtectiveID)
	}
	if err := db.UpsertOrder(ctx, o, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.Order(ctx, "ORDER-1"); got.ProtectiveID != "GTT-99" {
		t.Errorf("ProtectiveID = %q after re-upsert, want GTT-99", got.ProtectiveID)
	}
	if _, err := db.Order(ctx, "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing order: err = %v, want ErrNotFound", err)
	}
}

func TestSignalAndEquity(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	runID, err := db.StartRun(ctx, "backtest", "donchian-2026", map[string]any{"period": 20})
	if err != nil {
		t.Fatal(err)
	}

	sig := domain.Signal{
		Key: testKey, Kind: domain.Long, At: time.Date(2026, 1, 5, 9, 20, 0, 0, time.UTC),
		Price: money.MustParse("100.00"), Stop: money.MustParse("95.00"),
		Strategy: "donchian", Metadata: map[string]string{"band": "upper"},
	}
	if _, err := db.InsertSignal(ctx, sig, runID, true); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 1, 5, 15, 30, 0, 0, time.UTC)
	if err := db.InsertEquitySnapshot(ctx, at, money.MustParse("100000.00"), money.MustParse("500.00"), 0, 1, true, runID); err != nil {
		t.Fatal(err)
	}
	curve, err := db.EquityCurve(ctx, time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(curve) != 1 || curve[0].Equity != money.MustParse("100000.00") {
		t.Errorf("equity curve = %+v", curve)
	}
	// The live curve must not see the paper snapshot.
	if live, err := db.EquityCurve(ctx, time.Time{}, false); err != nil || len(live) != 0 {
		t.Errorf("live curve saw %d paper points (%v)", len(live), err)
	}

	if err := db.FinishRun(ctx, runID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
}

func TestKVState(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	type riskState struct {
		Date        string `json:"date"`
		TradesToday int    `json:"trades_today"`
	}

	var out riskState
	err := db.GetState(ctx, "risk_state", &out)
	if !errors.Is(err, ErrStateNotFound) {
		t.Errorf("a cold start must report ErrStateNotFound, got %v", err)
	}

	want := riskState{Date: "2026-01-15", TradesToday: 3}
	if err := db.SetState(ctx, "risk_state", want); err != nil {
		t.Fatal(err)
	}
	if err := db.GetState(ctx, "risk_state", &out); err != nil {
		t.Fatal(err)
	}
	if out != want {
		t.Errorf("state round trip = %+v, want %+v", out, want)
	}

	want.TradesToday = 7
	if err := db.SetState(ctx, "risk_state", want); err != nil {
		t.Fatal(err)
	}
	if err := db.GetState(ctx, "risk_state", &out); err != nil {
		t.Fatal(err)
	}
	if out.TradesToday != 7 {
		t.Errorf("overwrite failed: %+v", out)
	}

	if err := db.DeleteState(ctx, "risk_state"); err != nil {
		t.Fatal(err)
	}
	if err := db.GetState(ctx, "risk_state", &out); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("after delete: err = %v, want ErrStateNotFound", err)
	}
}

func TestChargeTableRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	rates := []struct {
		kind costs.Kind
		rate costs.Rate
	}{
		{costs.STTSell, costs.Rate{Value: 0.001, EffectiveFrom: epoch}},
		{costs.STTSell, costs.Rate{Value: 0.002, EffectiveFrom: later}},
		{costs.DP, costs.Rate{Value: 1534, Flat: true, EffectiveFrom: epoch}},
	}
	for _, r := range rates {
		if err := db.UpsertChargeRate(ctx, "upstox", costs.EquityDelivery, r.kind, r.rate); err != nil {
			t.Fatal(err)
		}
	}

	table, err := db.ChargeTable(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Effective dating must survive the round trip: a 2026-01 trade prices
	// at the old rate, a 2026-03 trade at the new one.
	early, ok := table.Lookup("upstox", costs.EquityDelivery, costs.STTSell, time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC))
	if !ok || early.Value != 0.001 {
		t.Errorf("early STT = %+v (ok=%v), want 0.001", early, ok)
	}
	late, ok := table.Lookup("upstox", costs.EquityDelivery, costs.STTSell, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if !ok || late.Value != 0.002 {
		t.Errorf("late STT = %+v (ok=%v), want 0.002", late, ok)
	}
	dp, ok := table.Lookup("upstox", costs.EquityDelivery, costs.DP, later)
	if !ok || !dp.Flat || dp.Value != 1534 {
		t.Errorf("DP = %+v (ok=%v), want a flat 1534", dp, ok)
	}
	if _, ok := table.Lookup("zerodha", costs.EquityDelivery, costs.DP, later); ok {
		t.Error("a rate for another broker must not be found")
	}
}

// The dashboard reads newest-first and must not see the other mode's rows: a
// paper order on a live dashboard looks like a fill that never happened.
func TestRecentOrdersAndSignals(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()

	base := time.Date(2026, 1, 5, 9, 20, 0, 0, time.UTC)
	for i, id := range []string{"O-1", "O-2", "O-3"} {
		o := domain.Order{
			ID: id, Status: domain.StatusOpen, PlacedAt: base.Add(time.Duration(i) * time.Minute),
			Request: domain.OrderRequest{Key: testKey, Side: domain.Buy, Quantity: 1, Type: domain.Market, Product: domain.CNC},
		}
		if err := db.UpsertOrder(ctx, o, i == 2); err != nil {
			t.Fatal(err)
		}
		sig := domain.Signal{Key: testKey, Kind: domain.Long, At: o.PlacedAt, Price: 100, Metadata: map[string]string{"n": id}}
		if _, err := db.InsertSignal(ctx, sig, 0, i == 2); err != nil {
			t.Fatal(err)
		}
	}

	orders, err := db.RecentOrders(ctx, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || orders[0].ID != "O-2" || orders[1].ID != "O-1" {
		t.Errorf("RecentOrders = %+v, want O-2 then O-1 and no paper row", orders)
	}
	if orders, err = db.RecentOrders(ctx, 1, false); err != nil || len(orders) != 1 {
		t.Errorf("limit not honoured: %d orders (%v)", len(orders), err)
	}

	signals, err := db.RecentSignals(ctx, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0].Metadata["n"] != "O-2" || signals[1].Metadata["n"] != "O-1" {
		t.Errorf("RecentSignals = %+v, want newest first and no paper row", signals)
	}
}

// A pre-tradekit database has tables named like the contract's but shaped
// differently. They must be moved aside -- indexes included, or the library's
// same-named index would never be built -- so Migrate can create its own.
func TestAdoptLegacyMovesCollidingTablesAside(t *testing.T) {
	db := openRawTest(t)
	ctx := t.Context()
	sqlDB := db.SQL()

	for _, q := range []string{
		`CREATE TABLE orders (order_id TEXT PRIMARY KEY, symbol TEXT, price REAL)`,
		`CREATE INDEX idx_orders_status ON orders(symbol)`,
		`INSERT INTO orders VALUES ('o1', 'INFY', 1500.5)`,
		`CREATE TABLE paper_state (trade_date TEXT PRIMARY KEY)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	if err := AdoptLegacy(ctx, sqlDB, "orders", "signals"); err != nil {
		t.Fatalf("AdoptLegacy returned %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after adoption returned %v: the legacy index must not shadow the library's", err)
	}

	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders_legacy`).Scan(&n); err != nil || n != 1 {
		t.Errorf("legacy rows must survive the rename: n=%d err=%v", n, err)
	}
	cols, err := Columns(ctx, sqlDB, "orders")
	if err != nil || !cols["placed_at"] {
		t.Errorf("the library's orders table must exist after adoption: cols=%v err=%v", cols, err)
	}
	if ok, _ := TableExists(ctx, sqlDB, "paper_state"); !ok {
		t.Error("a project table not named in the call must be left alone")
	}

	// Second run: schema_migrations exists, so nothing moves.
	if err := AdoptLegacy(ctx, sqlDB, "orders"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := TableExists(ctx, sqlDB, "orders_legacy_legacy"); ok {
		t.Error("adoption must be a no-op once the database is migrated")
	}
}
