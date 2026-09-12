package backtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
	"github.com/althk/tradekit/go/store"
)

// openTest returns a migrated in-memory database, or skips.
//
// This module imports no SQLite driver — the consumer chooses one — so these
// tests need one registered by the build that runs them; driver_test.go does it
// behind the sqlitedriver tag:
//
//	go test -tags sqlitedriver ./...
func openTest(t *testing.T) *store.DB {
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
	db, err := store.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return db
}

// sampleTrades returns a few closed paper trades, exit-ordered.
func sampleTrades() []domain.Trade {
	entry := time.Date(2025, 4, 14, 9, 30, 0, 0, ist)
	mk := func(day int, entryPrice, exitPrice string, reason domain.ExitReason) domain.Trade {
		e, x := money.MustParse(entryPrice), money.MustParse(exitPrice)
		gross := domain.GrossFor(domain.Buy, 10, e, x)
		return domain.Trade{
			Key: reliance, Strategy: "test", Side: domain.Buy, Quantity: 10,
			EntryPrice: e, ExitPrice: x,
			EntryAt: entry.AddDate(0, 0, day), ExitAt: entry.AddDate(0, 0, day+1),
			GrossPnL: gross, Charges: money.MustParse("2.00"), NetPnL: gross - money.MustParse("2.00"),
			ExitReason: reason, Paper: true,
		}
	}
	return []domain.Trade{
		mk(0, "100.00", "104.00", domain.ExitTarget),
		mk(2, "104.00", "101.00", domain.ExitStop),
		mk(4, "101.00", "107.00", domain.ExitTarget),
		mk(6, "107.00", "105.50", domain.ExitTrail),
	}
}

func sampleCurve() []stats.Point {
	start := time.Date(2025, 4, 14, 0, 0, 0, 0, ist)
	equities := []string{"1000.00", "1040.00", "1005.00", "1010.00", "1070.00", "1050.00", "1055.00"}
	out := make([]stats.Point, 0, len(equities))
	for i, e := range equities {
		out = append(out, stats.Point{At: start.AddDate(0, 0, i), Equity: money.MustParse(e)})
	}
	return out
}

func sampleSpec() RunSpec {
	return RunSpec{
		Name: "sample", Strategy: "test",
		From: time.Date(2025, 4, 14, 0, 0, 0, 0, ist), To: time.Date(2025, 4, 21, 0, 0, 0, 0, ist),
		Params:  map[string]any{"atr_mult": 2.5, "lookback": 20},
		Opening: money.MustParse("1000.00"),
	}
}

func TestASavedRunReadsBack(t *testing.T) {
	db := openTest(t)
	rec := &Recorder{DB: db}
	trades, curve := sampleTrades(), sampleCurve()

	runID, err := rec.Save(t.Context(), sampleSpec(), trades, curve)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runID == 0 {
		t.Fatal("a saved run has an id")
	}

	gotTrades, err := db.Trades(t.Context(), time.Time{}, true)
	if err != nil {
		t.Fatalf("reading trades: %v", err)
	}
	if len(gotTrades) != len(trades) {
		t.Errorf("expected %d trades back, got %d", len(trades), len(gotTrades))
	}
	gotCurve, err := db.EquityCurve(t.Context(), time.Time{}, true)
	if err != nil {
		t.Fatalf("reading curve: %v", err)
	}
	if len(gotCurve) != len(curve) {
		t.Errorf("expected %d points back, got %d", len(curve), len(gotCurve))
	}

	var kind, status, params string
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT kind, status, params FROM runs WHERE id = ?`, runID).Scan(&kind, &status, &params); err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if kind != "backtest" || status != "ok" {
		t.Errorf("a backtest run is recorded as kind backtest, status ok; got %q/%q", kind, status)
	}
	if !strings.Contains(params, `"atr_mult":2.5`) || !strings.Contains(params, `"strategy":"test"`) {
		t.Errorf("params must carry the strategy's parameters as JSON, got %s", params)
	}

	live, err := db.Trades(t.Context(), time.Time{}, false)
	if err != nil {
		t.Fatalf("reading live trades: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a backtest must write nothing into the live scope, got %d rows", len(live))
	}
}

func TestALiveTradeIsRejectedByName(t *testing.T) {
	db := openTest(t)
	rec := &Recorder{DB: db}
	trades := sampleTrades()
	trades[2].Paper = false

	_, err := rec.Save(t.Context(), sampleSpec(), trades, sampleCurve())
	if err == nil {
		t.Fatal("a live trade written by a backtest recorder would poison the live statistics of whatever project shares the database")
	}
	if !strings.Contains(err.Error(), "trade 2") || !strings.Contains(err.Error(), "RELIANCE") {
		t.Errorf("the error must name the offending trade, got %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("nothing may be written when a trade is rejected, found %d runs", n)
	}
}

func TestAFailurePartwayThroughLeavesNoRunRow(t *testing.T) {
	db := openTest(t)
	rec := &Recorder{DB: db}
	// A trade whose exit_reason is NULL-equivalent cannot exist through the
	// domain type, so break the schema instead: drop the snapshots table so
	// the third statement of the transaction fails after the run and trades
	// have been written.
	if _, err := db.SQL().ExecContext(t.Context(), `DROP TABLE equity_snapshots`); err != nil {
		t.Fatal(err)
	}

	_, err := rec.Save(t.Context(), sampleSpec(), sampleTrades(), sampleCurve())
	if err == nil {
		t.Fatal("expected the snapshot insert to fail")
	}
	var runs, trades int
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT count(*) FROM trades`).Scan(&trades); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || trades != 0 {
		t.Errorf("a partially written run is worse than no run: it will be read back and compared as if complete. Found %d runs and %d trades", runs, trades)
	}
}

func TestRecorderNeedsAStore(t *testing.T) {
	if _, err := (&Recorder{}).Save(context.Background(), sampleSpec(), nil, nil); err == nil {
		t.Error("a recorder with no store must refuse")
	}
}
