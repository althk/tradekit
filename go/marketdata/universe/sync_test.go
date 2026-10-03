package universe

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

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

func TestSyncKeepsTheMastersTickAndLotSizes(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	reliance := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	if err := db.UpsertInstruments(ctx, []domain.Instrument{{
		Key: reliance, Name: "RELIANCE", ISIN: "INE002A01018", Segment: "equity",
		LotSize: 1, TickSize: money.MustParse("0.10"), Active: true,
	}}); err != nil {
		t.Fatal(err)
	}

	c, _, _ := nseServer(t, map[string]string{"/content/indices/ind_nifty500list.csv": nifty500CSV})
	if _, err := c.Sync(ctx, db, "ind_nifty500list"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Instrument(ctx, reliance)
	if err != nil {
		t.Fatal(err)
	}
	if got.TickSize != money.MustParse("0.10") {
		t.Errorf("tick = %s, want the master's 0.10: a default tick puts every stop off the exchange's grid", got.TickSize)
	}
	if got.Name != "Reliance Industries Ltd." {
		t.Errorf("name = %q, want the index list's name", got.Name)
	}
}

func TestSyncDeactivatesOnlyLeaversOfThisIndex(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	other := domain.InstrumentKey{Exchange: "NSE", Symbol: "ZOMATO"}
	if err := db.UpsertInstruments(ctx, []domain.Instrument{{Key: other, Segment: "equity", Active: true}}); err != nil {
		t.Fatal(err)
	}

	lists := map[string]string{"/content/indices/ind_nifty50list.csv": nifty500CSV}
	c, _, _ := nseServer(t, lists)
	c.CacheDir = ""
	report, err := c.Sync(ctx, db, "ind_nifty50list")
	if err != nil {
		t.Fatal(err)
	}
	if report.Deactivated != 0 {
		t.Errorf("first sync deactivated %d; an instrument this index never listed is not a leaver", report.Deactivated)
	}

	// TCS leaves the index.
	lists["/content/indices/ind_nifty50list.csv"] = `Company Name,Industry,Symbol,Series,ISIN Code
Reliance Industries Ltd.,Oil Gas & Consumable Fuels,RELIANCE,EQ,INE002A01018
Infosys Ltd.,Information Technology,INFY,EQ,INE009A01021
`
	report, err = c.Sync(ctx, db, "ind_nifty50list")
	if err != nil {
		t.Fatal(err)
	}
	if report.Deactivated != 1 {
		t.Errorf("deactivated %d, want exactly the one leaver", report.Deactivated)
	}
	active, err := db.ActiveInstruments(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, in := range active {
		names[in.Key.Symbol] = true
	}
	if !names["ZOMATO"] || names["TCS"] {
		t.Errorf("active = %v, want ZOMATO kept (never in this index) and TCS deactivated", names)
	}
}

func TestAStaleIndexListIsRefetched(t *testing.T) {
	c, requests, _ := nseServer(t, map[string]string{"/content/indices/ind_nifty500list.csv": nifty500CSV})
	path := filepath.Join(c.CacheDir, "ind_nifty500list.csv")
	if err := os.WriteFile(path, []byte(nifty500CSV), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.IndexConstituents(context.Background(), "ind_nifty500list"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("a month-old list made %d requests, want 1: constituents change and the cache must expire", got)
	}
}
