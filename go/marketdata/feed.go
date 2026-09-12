// Package marketdata is the bar, universe and reference-data layer every
// backtest and every sync reads through.
//
// It replaces four incompatible ways of reading the same bars: zerobha from a
// CSV tree, neev and breakout500 from SQLite, conflux from pandas pickles. One
// port over three sources ends that.
//
// # SQLite is the canonical store; CSV is a view
//
// A CSV tree cannot answer "what is the last bar I have for RELIANCE" without
// reading the whole file, and that is the query the entire sync layer is built
// on. FromCSV exists so zerobha's backtester keeps working unchanged through
// its migration, not as a source of truth. Export writes the view; the store
// holds the data.
package marketdata

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

// BarFeed yields bars in ascending time order.
//
// It is an iterator rather than a slice because a five-year minute series is
// millions of bars, and a backtest consumes them one at a time. A feed backed by
// a slice is free; a feed backed by a query need not materialise the whole
// result.
type BarFeed interface {
	// Next returns the following bar, reporting false when the feed is
	// exhausted. An error ends the feed: a caller must not treat it as a gap
	// and carry on, because a truncated series produces a complete-looking
	// backtest over the wrong data.
	Next() (domain.Candle, bool, error)
}

// sliceFeed serves bars already in memory.
type sliceFeed struct {
	candles []domain.Candle
	i       int
}

// FromSlice returns a feed over bars already in memory. It is what a test and a
// resampled series use, and it is the reference implementation of the interface.
func FromSlice(candles []domain.Candle) BarFeed {
	return &sliceFeed{candles: candles}
}

// Next yields the next bar.
func (f *sliceFeed) Next() (domain.Candle, bool, error) {
	if f.i >= len(f.candles) {
		return domain.Candle{}, false, nil
	}
	c := f.candles[f.i]
	f.i++
	return c, true, nil
}

// FromStore returns a feed over the bars held for one instrument and timeframe
// in [from, to].
//
// The rows are read eagerly. A window a backtest actually replays is bounded by
// its own date range, and a streaming cursor would hold a SQLite read
// transaction open for the length of the run — which, with the store's
// single-connection default, blocks the sync that is trying to write into it.
func FromStore(ctx context.Context, db *store.DB, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) (BarFeed, error) {
	candles, err := db.Candles(ctx, key, tf, from, to)
	if err != nil {
		return nil, fmt.Errorf("marketdata: reading %s %s bars: %w", key, tf, err)
	}
	return FromSlice(candles), nil
}

// FromCSV returns a feed over one of zerobha's bar files.
//
// path is the file itself, so a caller with a different layout is not forced
// into this one; CSVPath builds the conventional
// test/data/<interval>/<symbol>_real.csv location.
func FromCSV(path string, key domain.InstrumentKey, tf domain.Timeframe) (BarFeed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("marketdata: opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	candles, err := ReadCSV(f, key, tf)
	if err != nil {
		return nil, fmt.Errorf("marketdata: reading %s: %w", path, err)
	}
	return FromSlice(candles), nil
}

// CSVPath is the conventional location of one instrument's bar file in
// zerobha's tree: <dir>/<interval>/<symbol>_real.csv, lowercased.
//
// interval is the directory name, which is the vendor's interval spelling
// rather than a domain.Timeframe — the existing tree carries both "1d" and
// "day", and both "5m" and "5minute", written by different tools at different
// times. Making the caller name the directory keeps this function from having
// to guess which convention a given tree uses.
func CSVPath(dir, interval, symbol string) string {
	return filepath.Join(dir, interval, strings.ToLower(symbol)+"_real.csv")
}

// ReadCSV parses a bar file.
//
// Columns are located by header name, not by position. The tree zerobha
// carries today holds both orderings — cmd/histdl writes
// timestamp,open,high,low,close,volume while the older files are
// timestamp,close,high,low,open,volume — so a positional reader silently
// transposes open and close on half the tree, which is exactly the kind of
// error a backtest cannot detect from its own results.
func ReadCSV(r io.Reader, key domain.InstrumentKey, tf domain.Timeframe) ([]domain.Candle, error) {
	reader := csv.NewReader(r)
	header, err := reader.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("marketdata: file is empty")
	}
	if err != nil {
		return nil, fmt.Errorf("marketdata: reading header: %w", err)
	}

	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, required := range []string{"timestamp", "open", "high", "low", "close"} {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("marketdata: file has no %q column; header was %v", required, header)
		}
	}

	var out []domain.Candle
	for line := 2; ; line++ {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("marketdata: line %d: %w", line, err)
		}
		candle, err := rowToCandle(row, col, key, tf)
		if err != nil {
			return nil, fmt.Errorf("marketdata: line %d: %w", line, err)
		}
		out = append(out, candle)
	}
	return out, nil
}

// rowToCandle converts one CSV row.
//
// A malformed row is an error rather than a skip, unlike the instrument master:
// a bar file is this series and nothing else, and quietly dropping a bar from
// it changes every indicator computed over the series without saying so.
func rowToCandle(row []string, col map[string]int, key domain.InstrumentKey, tf domain.Timeframe) (domain.Candle, error) {
	field := func(name string) string {
		i, ok := col[name]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}

	start, err := ParseCSVTime(field("timestamp"))
	if err != nil {
		return domain.Candle{}, err
	}
	candle := domain.Candle{Key: key, Timeframe: tf, Start: start}
	for _, f := range []struct {
		name string
		dest *money.Money
	}{
		{"open", &candle.Open},
		{"high", &candle.High},
		{"low", &candle.Low},
		{"close", &candle.Close},
	} {
		v, err := strconv.ParseFloat(field(f.name), 64)
		if err != nil {
			return domain.Candle{}, fmt.Errorf("%s %q: %w", f.name, field(f.name), err)
		}
		*f.dest = money.FromMajor(v)
	}
	if raw := field("volume"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return domain.Candle{}, fmt.Errorf("volume %q: %w", raw, err)
		}
		candle.Volume = int64(v)
	}
	if raw := field("open_interest"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return domain.Candle{}, fmt.Errorf("open_interest %q: %w", raw, err)
		}
		candle.OpenInterest = int64(v)
	}
	return candle, nil
}

// csvTimeLayouts are the timestamp forms the existing tree carries, most
// specific first.
//
// There are four because the files were written by three different tools over
// two years: histdl emits RFC3339 with an IST offset, an older exporter emitted
// a UTC offset with no colon, and an older one still emitted a naive local
// datetime. Rejecting any of them would make the migration a rewrite of the
// data as well as of the code.
var csvTimeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05",
	time.DateOnly,
}

// ParseCSVTime reads a bar file's timestamp.
//
// A naive timestamp is interpreted as IST, which is what it meant when it was
// written: these are NSE bars, and reading them as UTC would shift every bar
// five and a half hours and put the whole session in the wrong day.
func ParseCSVTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	// ParseInLocation uses the location only when the input carries no zone
	// of its own, so one call covers both the zoned and the naive layouts.
	for _, layout := range csvTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, IST); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q matches none of the known layouts %v", s, csvTimeLayouts)
}

// IST is the exchange's own clock, used for the timestamps that carry no zone.
//
// It is a fixed zone rather than a loaded location deliberately: India has no
// daylight saving, so the offset is constant, and a fixed zone cannot fail at
// startup on a machine with no tzdata. Anything that needs holiday-aware
// session logic uses core/calendar, which does load the real location.
var IST = time.FixedZone("IST", 5*3600+1800)
