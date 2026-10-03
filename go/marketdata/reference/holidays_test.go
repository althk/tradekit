package reference

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSource is a holiday source whose answer and failure are controlled by the
// test.
type fakeSource struct {
	days  map[string]string
	err   error
	calls int
}

func (f *fakeSource) Closed(context.Context, string, time.Time, time.Time) (map[string]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.days, nil
}

func from() time.Time { return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) }
func to() time.Time   { return time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC) }

func TestHolidaysFetchesThenServesFromCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "holidays.json")
	source := &fakeSource{days: map[string]string{"2025-08-15": "Independence Day"}}

	h, err := NewHolidays(source, cache)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	closed, err := h.Closed(context.Background(), "NSE", from(), to())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed["2025-08-15"] != "Independence Day" {
		t.Fatalf("the live calendar must be served, got %v", closed)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("the calendar must be mirrored to disk so a backtest can run offline: %v", err)
	}

	if _, err := h.Closed(context.Background(), "NSE", from(), to()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source.calls != 1 {
		t.Errorf("a calendar within its TTL must not be refetched; the source was called %d times", source.calls)
	}
}

func TestHolidaysFallBackToTheCacheWhenTheFetchFails(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "holidays.json")
	source := &fakeSource{days: map[string]string{"2025-08-15": "Independence Day"}}

	h, _ := NewHolidays(source, cache)
	if _, err := h.Closed(context.Background(), "NSE", from(), to()); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}

	// A new client, so nothing is in memory, and a source that is now down.
	broken := &fakeSource{err: errors.New("holiday endpoint is down")}
	cold, _ := NewHolidays(broken, cache)
	cold.Now = func() time.Time { return time.Now().Add(48 * time.Hour) } // force a refetch

	closed, err := cold.Closed(context.Background(), "NSE", from(), to())
	if err != nil {
		t.Fatalf("a failed fetch must not surface as an error while a cache exists: %v", err)
	}
	if closed["2025-08-15"] != "Independence Day" {
		t.Errorf("exchanges publish the year in advance, so a stale calendar is almost certainly still right and is far better than none; got %v", closed)
	}
	if broken.calls == 0 {
		t.Error("the live source must still be tried; falling straight to cache would never pick up a correction")
	}
}

func TestACacheMissAndAFailedFetchFailOpen(t *testing.T) {
	h, _ := NewHolidays(&fakeSource{err: errors.New("down")}, filepath.Join(t.TempDir(), "none.json"))

	closed, err := h.Closed(context.Background(), "NSE", from(), to())
	if err != nil {
		t.Fatalf("failing open means no error reaches the caller, got %v", err)
	}
	if len(closed) != 0 {
		t.Errorf("with no calendar at all every day must read as a trading day; got %v", closed)
	}
	// Skipping a real trading day because a calendar could not be loaded is
	// worse than running on a holiday, where the exchange rejects the orders
	// and the cost is one wasted pass.
}

func TestHolidaysBoundTheRequestedRange(t *testing.T) {
	source := &fakeSource{days: map[string]string{
		"2024-12-25": "Christmas",
		"2025-08-15": "Independence Day",
		"2026-01-26": "Republic Day",
	}}
	h, _ := NewHolidays(source, "")

	closed, err := h.Closed(context.Background(), "NSE", from(), to())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(closed) != 1 || closed["2025-08-15"] == "" {
		t.Errorf("only dates inside [from, to] belong in the result; got %v", closed)
	}
}

// rangedSource answers only for the range it is asked about, as every real
// source does, and records the ranges it was asked for.
type rangedSource struct {
	days  map[string]string
	asked [][2]string
}

func (r *rangedSource) Closed(_ context.Context, _ string, from, to time.Time) (map[string]string, error) {
	lo, hi := from.Format(time.DateOnly), to.Format(time.DateOnly)
	r.asked = append(r.asked, [2]string{lo, hi})
	out := map[string]string{}
	for d, desc := range r.days {
		if d >= lo && d <= hi {
			out[d] = desc
		}
	}
	return out, nil
}

func TestANarrowFirstQueryDoesNotHideOtherHolidays(t *testing.T) {
	source := &rangedSource{days: map[string]string{
		"2025-08-15": "Independence Day",
		"2025-10-02": "Gandhi Jayanti",
		"2026-01-26": "Republic Day",
	}}
	h, _ := NewHolidays(source, filepath.Join(t.TempDir(), "holidays.json"))

	if _, err := h.Closed(context.Background(), "NSE", day(2025, 8, 15), day(2025, 8, 15)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	closed, _ := h.Closed(context.Background(), "NSE", day(2025, 10, 2), day(2025, 10, 2))
	if closed["2025-10-02"] == "" {
		t.Errorf("a calendar fetched for one day must not be taken as the whole calendar; every other holiday would read as a trading day; got %v", closed)
	}
	if len(source.asked) != 1 {
		t.Errorf("a day in a year already fetched must be served from memory; the source was asked %v", source.asked)
	}

	closed, _ = h.Closed(context.Background(), "NSE", day(2026, 1, 26), day(2026, 1, 26))
	if closed["2026-01-26"] == "" {
		t.Errorf("a day outside the fetched range must be fetched, not read from the cache as open; got %v", closed)
	}
	again, _ := h.Closed(context.Background(), "NSE", day(2025, 8, 15), day(2025, 8, 15))
	if again["2025-08-15"] == "" || len(source.asked) != 2 {
		t.Errorf("widening the range must keep what was already known; got %v after asking %v", again, source.asked)
	}
}

func TestACacheWithoutARecordedRangeIsRefetched(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "holidays.json")
	legacy := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","days":{"NSE":{"2025-08-15":"Independence Day"}}}`
	if err := os.WriteFile(cache, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	source := &rangedSource{days: map[string]string{"2025-10-02": "Gandhi Jayanti"}}
	h, _ := NewHolidays(source, cache)

	closed, _ := h.Closed(context.Background(), "NSE", day(2025, 10, 2), day(2025, 10, 2))
	if closed["2025-10-02"] == "" {
		t.Errorf("a cache of unknown extent, written before ranges were recorded, must be refetched; got %v", closed)
	}
}

func TestACacheOnlySourceNeedsNoNetwork(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "holidays.json")
	primed, _ := NewHolidays(&fakeSource{days: map[string]string{"2025-08-15": "Independence Day"}}, cache)
	if _, err := primed.Closed(context.Background(), "NSE", from(), to()); err != nil {
		t.Fatalf("priming: %v", err)
	}

	// This is the backtest's configuration: a cache and no live source.
	offline, err := NewHolidays(nil, cache)
	if err != nil {
		t.Fatalf("a cache-only source must be permitted: %v", err)
	}
	closed, err := offline.Closed(context.Background(), "NSE", from(), to())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if closed["2025-08-15"] == "" {
		t.Errorf("a backtest runs offline and must still see the calendar; got %v", closed)
	}
}

func TestNewHolidaysNeedsSomethingToReadFrom(t *testing.T) {
	if _, err := NewHolidays(nil, ""); err == nil {
		t.Error("a source with neither a live calendar nor a cache can never answer anything and must be refused at wiring time")
	}
}
