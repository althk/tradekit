package marketdata

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

// openTest returns a migrated in-memory database, or skips.
//
// This module imports no SQLite driver — the consumer chooses one — so these
// tests need one registered by the build that runs them. driver_test.go does it
// behind the sqlitedriver tag:
//
//	go test -tags sqlitedriver ./...
//
// Without it the driver-independent tests still run and these skip, so a
// checkout with no network is not a broken checkout.
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

// storeBars writes a few daily bars for the test instrument.
func storeBars(t *testing.T, db *store.DB, key domain.InstrumentKey, days int) []domain.Candle {
	t.Helper()
	start := time.Date(2025, 4, 14, 0, 0, 0, 0, IST)
	candles := make([]domain.Candle, 0, days)
	for i := 0; i < days; i++ {
		price := money.MustParse("100.00") + money.Money(i*100)
		candles = append(candles, domain.Candle{
			Key:       key,
			Timeframe: domain.D1,
			Start:     start.AddDate(0, 0, i),
			Open:      price,
			High:      price + 200,
			Low:       price - 200,
			Close:     price + 50,
			Volume:    int64(1000 * (i + 1)),
		})
	}
	if _, err := db.UpsertCandles(t.Context(), candles); err != nil {
		t.Fatalf("storing test bars: %v", err)
	}
	return candles
}

func TestFromStoreYieldsTheStoredBarsInOrder(t *testing.T) {
	db := openTest(t)
	want := storeBars(t, db, reliance, 5)

	feed, err := FromStore(t.Context(), db, reliance, domain.D1,
		time.Date(2000, 1, 1, 0, 0, 0, 0, IST), time.Date(2030, 1, 1, 0, 0, 0, 0, IST))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := drain(t, feed)

	if len(got) != len(want) {
		t.Fatalf("expected %d bars, got %d", len(want), len(got))
	}
	for i := range got {
		if !got[i].Start.Equal(want[i].Start) {
			t.Errorf("bar %d is out of order: got %v want %v", i, got[i].Start, want[i].Start)
		}
		if got[i].Close != want[i].Close {
			t.Errorf("bar %d close is %s, want %s", i, got[i].Close, want[i].Close)
		}
	}
}

func TestFromStoreBoundsTheRange(t *testing.T) {
	db := openTest(t)
	all := storeBars(t, db, reliance, 5)

	feed, err := FromStore(t.Context(), db, reliance, domain.D1, all[1].Start, all[3].Start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := drain(t, feed)
	if len(got) != 3 {
		t.Fatalf("the range is inclusive at both ends: expected 3 bars, got %d", len(got))
	}
}

func TestExportCSVMatchesTheHistdlLayout(t *testing.T) {
	db := openTest(t)
	want := storeBars(t, db, reliance, 3)

	dir := t.TempDir()
	if err := ExportCSV(t.Context(), db, dir, "day", []domain.InstrumentKey{reliance}, domain.D1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	path := CSVPath(dir, "day", "RELIANCE")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file must land where zerobha's backtester looks for it: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) != len(want)+1 {
		t.Fatalf("expected a header and %d rows, got %d lines", len(want), len(lines))
	}
	if header := strings.TrimRight(lines[0], "\r"); header != strings.Join(csvHeader, ",") {
		t.Errorf("the column order must match what cmd/histdl writes.\n got %q\nwant %q", header, strings.Join(csvHeader, ","))
	}

	first := strings.Split(strings.TrimRight(lines[1], "\r"), ",")
	if len(first) != 6 {
		t.Fatalf("expected six columns, got %d: %v", len(first), first)
	}
	if first[0] != want[0].Start.In(IST).Format(time.RFC3339) {
		t.Errorf("timestamps are RFC3339 with the exchange offset, got %q", first[0])
	}
	if first[1] != want[0].Open.String() {
		t.Errorf("prices are written with two decimals, got %q want %q", first[1], want[0].Open.String())
	}
}

func TestExportedCSVReadsBackIdentically(t *testing.T) {
	// The round trip is what makes the export safe to use as zerobha's input
	// during its migration: what the store holds is what the backtester sees.
	db := openTest(t)
	want := storeBars(t, db, reliance, 4)

	dir := t.TempDir()
	if err := ExportCSV(t.Context(), db, dir, "day", []domain.InstrumentKey{reliance}, domain.D1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	feed, err := FromCSV(CSVPath(dir, "day", "RELIANCE"), reliance, domain.D1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := drain(t, feed)

	if len(got) != len(want) {
		t.Fatalf("expected %d bars back, got %d", len(want), len(got))
	}
	for i := range got {
		if !got[i].Start.Equal(want[i].Start) {
			t.Errorf("bar %d timestamp did not survive the round trip: %v vs %v", i, got[i].Start, want[i].Start)
		}
		if got[i].Open != want[i].Open || got[i].High != want[i].High ||
			got[i].Low != want[i].Low || got[i].Close != want[i].Close {
			t.Errorf("bar %d prices did not survive the round trip:\n got %+v\nwant %+v", i, got[i], want[i])
		}
		if got[i].Volume != want[i].Volume {
			t.Errorf("bar %d volume did not survive: %d vs %d", i, got[i].Volume, want[i].Volume)
		}
	}
}

func TestExportCSVSkipsAnInstrumentWithNoBars(t *testing.T) {
	db := openTest(t)
	dir := t.TempDir()
	empty := domain.InstrumentKey{Exchange: "NSE", Symbol: "NOTHING"}

	if err := ExportCSV(t.Context(), db, dir, "day", []domain.InstrumentKey{empty}, domain.D1); err != nil {
		t.Fatalf("an instrument with no history is not an error: %v", err)
	}
	if _, err := os.Stat(CSVPath(dir, "day", "NOTHING")); !os.IsNotExist(err) {
		t.Error("a header-only file reads as an instrument with no history rather than one that was never exported; histdl skips it and so must this")
	}
	if _, err := os.Stat(filepath.Join(dir, "day")); err != nil {
		t.Errorf("the interval directory must still be created: %v", err)
	}
}
