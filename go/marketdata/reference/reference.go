// Package reference serves the per-instrument facts that sizing and order
// placement need: tick sizes, lot sizes and the exchange holiday calendar.
//
// Each of the three had a donor and each had a wrong version in use somewhere.
// Tick sizes were hardcoded as a slab table that silently rots at the next NSE
// revision; the holiday calendar was fetched with no cache, so a backtest could
// not run offline. The data-driven and the cached versions are the ones carried
// over.
package reference

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/store"
)

// FallbackTick is the tick used for an instrument the master does not name.
//
// It is ₹0.10, which is neither the most common tick (₹0.01) nor the one most
// people would name (₹0.05). The reasoning, measured by breakout500 against the
// live NSE master, is that a fallback is safe only when it is a *multiple* of
// the instrument's real tick: a multiple of ₹0.10 is also a multiple of ₹0.05
// and of ₹0.01, so rounding coarser than the truth still lands on the grid
// while rounding finer does not. ₹0.10 satisfies 9644 of 9724 NSE equities;
// ₹0.05 satisfies 9257, missing every ₹0.10-tick name including Reliance.
//
// It is deliberately not zero. A zero tick disables rounding silently, and an
// unrounded stop is rejected outright — which leaves a live position with no
// protection at all, the failure this whole package exists to prevent.
const FallbackTick money.Money = 10

// FallbackLot is the lot size for an instrument the master does not name. Cash
// equity trades in single shares, and a zero lot would size every position to
// nothing.
const FallbackLot = 1

// TickSizes returns a lookup from instrument to tick size, built from the
// instruments table.
//
// The shape is exactly what core/paper.Options.TickSize wants, so it wires
// straight in. The lookup is a snapshot: the master changes daily and a process
// that runs for one session wants one consistent answer, not a value that
// changes under it mid-run.
func TickSizes(ctx context.Context, db *store.DB) (func(domain.InstrumentKey) money.Money, error) {
	instruments, err := db.ActiveInstruments(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("reference: reading instruments for tick sizes: %w", err)
	}
	ticks := make(map[domain.InstrumentKey]money.Money, len(instruments))
	for _, in := range instruments {
		if in.TickSize > 0 {
			ticks[in.Key] = in.TickSize
		}
	}
	return func(key domain.InstrumentKey) money.Money {
		if tick, ok := ticks[key]; ok {
			return tick
		}
		return FallbackTick
	}, nil
}

// LotSizes returns a lookup from instrument to lot size, built from the
// instruments table.
//
// The shape is what risk.SizeParams.LotSize wants. A lot size of 0 in the
// master — which is how the vendors spell "cash equity" — becomes 1, so sizing
// can multiply by it unconditionally.
func LotSizes(ctx context.Context, db *store.DB) (func(domain.InstrumentKey) int, error) {
	instruments, err := db.ActiveInstruments(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("reference: reading instruments for lot sizes: %w", err)
	}
	lots := make(map[domain.InstrumentKey]int, len(instruments))
	for _, in := range instruments {
		if in.LotSize > 0 {
			lots[in.Key] = in.LotSize
		}
	}
	return func(key domain.InstrumentKey) int {
		if lot, ok := lots[key]; ok {
			return lot
		}
		return FallbackLot
	}, nil
}

// Holidays is a ports.HolidaySource with a disk cache in front of a live one.
//
// A backtest runs offline and must not need the network, and a live process
// must not lose its calendar because the broker's holiday endpoint is down. The
// cache satisfies both: a fetch that fails falls back to it, and a fetch that
// succeeds refreshes it.
type Holidays struct {
	// Source is the live calendar. Nil means cache-only, which is what a
	// backtest uses.
	Source ports.HolidaySource
	// CachePath is the JSON file the calendar is mirrored into.
	CachePath string
	// TTL is how long a cached calendar is used without refetching. Zero
	// means one day: exchanges publish the year's calendar in advance, so a
	// daily refresh is already far more often than it changes.
	TTL time.Duration
	// Now is injectable for tests.
	Now func() time.Time

	mu     sync.Mutex
	loaded map[string]map[string]string // exchange -> "YYYY-MM-DD" -> description
	at     time.Time
}

// cacheFile is the on-disk shape. Dates are "YYYY-MM-DD" strings, per
// invariant 4: a time-keyed map compiles, looks right and never matches,
// because two time.Time values for the same instant compare unequal when their
// *Location pointers differ.
type cacheFile struct {
	FetchedAt time.Time                    `json:"fetched_at"`
	Days      map[string]map[string]string `json:"days"`
}

// NewHolidays returns a cached holiday source. A nil live source is permitted
// and means cache-only.
func NewHolidays(source ports.HolidaySource, cachePath string) (*Holidays, error) {
	if source == nil && cachePath == "" {
		return nil, fmt.Errorf("reference: a holiday source needs either a live source or a cache path")
	}
	return &Holidays{Source: source, CachePath: cachePath}, nil
}

// Closed returns the closed dates for an exchange within [from, to].
//
// It fails **open**: when neither the live source nor the cache can answer, the
// result is an empty map and a nil error, so every day in the range reads as a
// trading day. Skipping a real trading day because a calendar could not be
// loaded is a worse outcome than running on a holiday, where the exchange
// rejects the orders anyway and the cost is one wasted pass.
func (h *Holidays) Closed(ctx context.Context, exchange string, from, to time.Time) (map[string]string, error) {
	days, err := h.calendar(ctx, exchange, from, to)
	if err != nil {
		return map[string]string{}, nil
	}

	lo, hi := from.Format(time.DateOnly), to.Format(time.DateOnly)
	out := map[string]string{}
	// The format is fixed-width and zero-padded, so lexical order is
	// chronological order and a string comparison bounds the range correctly.
	for day, desc := range days {
		if day >= lo && day <= hi {
			out[day] = desc
		}
	}
	return out, nil
}

// calendar returns the exchange's closed days, from memory, the cache, or the
// live source in that order.
func (h *Holidays) calendar(ctx context.Context, exchange string, from, to time.Time) (map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.loaded == nil {
		h.loadCache()
	}
	if !h.stale() {
		if days, ok := h.loaded[exchange]; ok {
			return days, nil
		}
	}

	if h.Source == nil {
		if days, ok := h.loaded[exchange]; ok {
			return days, nil
		}
		return nil, fmt.Errorf("reference: no cached calendar for %s and no live source", exchange)
	}

	fresh, err := h.Source.Closed(ctx, exchange, from, to)
	if err != nil {
		// The live fetch failed. A stale calendar is far better than none:
		// exchanges publish the year in advance, so last week's copy is
		// almost certainly still correct.
		if days, ok := h.loaded[exchange]; ok {
			return days, nil
		}
		return nil, err
	}

	if h.loaded == nil {
		h.loaded = map[string]map[string]string{}
	}
	h.loaded[exchange] = fresh
	h.at = h.now()
	h.saveCache()
	return fresh, nil
}

// stale reports whether the loaded calendar is old enough to refetch.
func (h *Holidays) stale() bool {
	if h.at.IsZero() {
		return true
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return h.now().Sub(h.at) >= ttl
}

// loadCache reads the calendar from disk, tolerating its absence.
//
// A missing or unreadable cache is not an error: it is what a first run looks
// like, and the live source is about to be consulted anyway.
func (h *Holidays) loadCache() {
	h.loaded = map[string]map[string]string{}
	if h.CachePath == "" {
		return
	}
	raw, err := os.ReadFile(h.CachePath)
	if err != nil {
		return
	}
	var file cacheFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return
	}
	if file.Days != nil {
		h.loaded = file.Days
		h.at = file.FetchedAt
	}
}

// saveCache mirrors the calendar to disk.
//
// A write failure is deliberately ignored: the calendar is already in memory
// and the process can trade. Failing a sync because a cache file could not be
// written would turn a convenience into a dependency.
func (h *Holidays) saveCache() {
	if h.CachePath == "" {
		return
	}
	raw, err := json.MarshalIndent(cacheFile{FetchedAt: h.at, Days: h.loaded}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.CachePath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(h.CachePath, raw, 0o644)
}

func (h *Holidays) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}
