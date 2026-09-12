package sync

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

var (
	reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	tcs      = domain.InstrumentKey{Exchange: "NSE", Symbol: "TCS"}
	infy     = domain.InstrumentKey{Exchange: "NSE", Symbol: "INFY"}
)

// openTest returns a migrated in-memory database, or skips. See
// go/store's equivalent: this module pins no SQLite driver, so the tests that
// need one run only under -tags sqlitedriver.
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

// fakeHistory serves bars for the instruments it knows and fails for the rest.
type fakeHistory struct {
	mu       sync.Mutex
	bars     map[domain.InstrumentKey][]domain.Candle
	fail     map[domain.InstrumentKey]bool
	requests []request
}

type request struct {
	key      domain.InstrumentKey
	from, to time.Time
}

func (f *fakeHistory) Candles(_ context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request{key: key, from: from, to: to})
	fail := f.fail[key]
	all := f.bars[key]
	f.mu.Unlock()

	if fail {
		return nil, errors.New("instrument is delisted")
	}
	var out []domain.Candle
	for _, c := range all {
		if !c.Start.Before(from) && !c.Start.After(to) {
			out = append(out, c)
		}
	}
	return out, nil
}

// dailyBars builds one bar per day ending at `end`.
func dailyBars(key domain.InstrumentKey, end time.Time, days int) []domain.Candle {
	out := make([]domain.Candle, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := end.AddDate(0, 0, -i)
		out = append(out, domain.Candle{
			Key:       key,
			Timeframe: domain.D1,
			Start:     time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
			Open:      money.MustParse("100.00"),
			High:      money.MustParse("101.00"),
			Low:       money.MustParse("99.00"),
			Close:     money.MustParse("100.50"),
			Volume:    1000,
		})
	}
	return out
}

func TestBackfillOnAnEmptyStoreFetchesTheWholeLookback(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 20, 12, 0, 0, 0, time.UTC)
	history := &fakeHistory{bars: map[domain.InstrumentKey][]domain.Candle{
		reliance: dailyBars(reliance, now, 10),
	}}

	report, err := Backfill(t.Context(), Params{
		DB: db, History: history, Keys: []domain.InstrumentKey{reliance},
		Timeframe: domain.D1, Lookback: 30 * 24 * time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Inserted != 10 {
		t.Errorf("an empty store must be filled with the whole lookback, got %d bars", report.Inserted)
	}
	if len(history.requests) != 1 {
		t.Fatalf("expected one request, got %d", len(history.requests))
	}
	if got := history.requests[0].from; !got.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Errorf("with nothing stored the request starts at the lookback boundary, got %v", got)
	}
}

func TestIncrementalBackfillResumesFromTheLastStoredBar(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 20, 12, 0, 0, 0, time.UTC)
	all := dailyBars(reliance, now, 10)
	history := &fakeHistory{bars: map[domain.InstrumentKey][]domain.Candle{reliance: all}}

	// Seed the first five bars, as a previous run would have.
	if _, err := db.UpsertCandles(t.Context(), all[:5]); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	report, err := Backfill(t.Context(), Params{
		DB: db, History: history, Keys: []domain.InstrumentKey{reliance},
		Timeframe: domain.D1, Lookback: 30 * 24 * time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Inserted != 5 {
		t.Errorf("only the five bars after the last stored one are new, got %d", report.Inserted)
	}
	from := history.requests[0].from
	if !from.After(all[4].Start) {
		t.Errorf("the request must start after the last stored bar, not at it — starting at it re-requests a stored bar on every run. got %v, last stored %v",
			from, all[4].Start)
	}
}

func TestFullBackfillRePullsAndInsertsNothingNew(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 20, 12, 0, 0, 0, time.UTC)
	all := dailyBars(reliance, now, 10)
	history := &fakeHistory{bars: map[domain.InstrumentKey][]domain.Candle{reliance: all}}

	params := Params{
		DB: db, History: history, Keys: []domain.InstrumentKey{reliance},
		Timeframe: domain.D1, Lookback: 30 * 24 * time.Hour, Now: now,
	}
	if _, err := Backfill(t.Context(), params); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	params.Full = true
	report, err := Backfill(t.Context(), params)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if report.Inserted != 0 {
		t.Errorf("candles are INSERT OR IGNORE, so a full re-pull is idempotent; got %d new bars", report.Inserted)
	}
	if got := history.requests[1].from; !got.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Errorf("full=true must re-request the whole window — the incremental default can only extend history forward, so deepening it is impossible without this. got %v", got)
	}
}

func TestOneDeadSymbolLeavesTheOthersStored(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 20, 12, 0, 0, 0, time.UTC)
	history := &fakeHistory{
		bars: map[domain.InstrumentKey][]domain.Candle{
			reliance: dailyBars(reliance, now, 3),
			infy:     dailyBars(infy, now, 3),
		},
		fail: map[domain.InstrumentKey]bool{tcs: true},
	}

	report, err := Backfill(t.Context(), Params{
		DB: db, History: history,
		Keys:      []domain.InstrumentKey{reliance, tcs, infy},
		Timeframe: domain.D1, Lookback: 30 * 24 * time.Hour, Now: now,
	})
	if err != nil {
		t.Fatalf("one failing symbol must not fail the pass: %v", err)
	}
	if report.Requested != 3 {
		t.Errorf("Requested counts every instrument attempted, got %d", report.Requested)
	}
	if report.Inserted != 6 {
		t.Errorf("the two working symbols must still be stored; got %d bars", report.Inserted)
	}
	if len(report.Failed) != 1 || report.Failed[0] != tcs {
		t.Fatalf("the dead symbol must be named in the report, got %v", report.Failed)
	}
	if len(report.Errors) != 1 {
		t.Errorf("an operator needs to see whether 400 symbols failed for 400 reasons or one; got %d errors", len(report.Errors))
	}

	// And the survivors really are in the store, not merely counted.
	for _, key := range []domain.InstrumentKey{reliance, infy} {
		n, err := db.CandleCount(t.Context(), key, domain.D1)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("%s should hold 3 bars, holds %d", key, n)
		}
	}
}

func TestBackfillStopsOnACancelledContext(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 20, 12, 0, 0, 0, time.UTC)
	history := &fakeHistory{bars: map[domain.InstrumentKey][]domain.Candle{}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Backfill(ctx, Params{
		DB: db, History: history, Keys: []domain.InstrumentKey{reliance, tcs},
		Timeframe: domain.D1, Lookback: 30 * 24 * time.Hour, Now: now,
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context means the operator asked the pass to stop; got %v", err)
	}
	if len(history.requests) != 0 {
		t.Error("no instrument may be fetched after cancellation")
	}
}

func TestBackfillRequiresItsCollaborators(t *testing.T) {
	db := openTest(t)
	if _, err := Backfill(t.Context(), Params{History: &fakeHistory{}, Lookback: time.Hour}); err == nil {
		t.Error("a nil store must fail at once, not on the first write")
	}
	if _, err := Backfill(t.Context(), Params{DB: db, Lookback: time.Hour}); err == nil {
		t.Error("a nil history provider must fail at once")
	}
	if _, err := Backfill(t.Context(), Params{DB: db, History: &fakeHistory{}}); err == nil {
		t.Error("a zero lookback would fetch nothing and silently report success")
	}
}
