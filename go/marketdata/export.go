package marketdata

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/store"
)

// csvHeader is the column order zerobha's cmd/histdl writes, verified against
// the files that project produced.
//
// The order matters for byte-comparability with the existing tree, and only for
// that: ReadCSV locates columns by name, so nothing here depends on it. Note
// that the older files in the same tree use a different order
// (timestamp,close,high,low,open,volume) — a positional reader would transpose
// open and close on half of them.
var csvHeader = []string{"timestamp", "open", "high", "low", "close", "volume"}

// ExportCSV writes each instrument's bars into zerobha's tree layout:
// <dir>/<interval>/<symbol>_real.csv, lowercased.
//
// It exists so zerobha's backtester keeps reading the files it already reads
// while its data moves into the store. Migrating the storage and rewriting the
// backtester at the same time makes a change nobody can review; this is what
// splits them.
//
// interval is the directory name rather than the timeframe's own string,
// because the existing tree spells the same timeframe two ways — "1d" and
// "day", "5m" and "5minute" — and the caller knows which convention its tree
// uses.
//
// An instrument with no stored bars is skipped rather than written as a
// header-only file: histdl skips it too, and an empty file reads as an
// instrument with no history rather than one that was never exported.
func ExportCSV(ctx context.Context, db *store.DB, dir, interval string, keys []domain.InstrumentKey, tf domain.Timeframe) error {
	if err := os.MkdirAll(filepath.Join(dir, interval), 0o755); err != nil {
		return fmt.Errorf("marketdata: creating %s: %w", filepath.Join(dir, interval), err)
	}

	// The whole stored range, which is what histdl writes: the file is the
	// instrument's history, not a window into it.
	from := time.Unix(0, 0)
	to := time.Now().AddDate(1, 0, 0)

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		candles, err := db.Candles(ctx, key, tf, from, to)
		if err != nil {
			return fmt.Errorf("marketdata: reading %s %s bars for export: %w", key, tf, err)
		}
		if len(candles) == 0 {
			continue
		}
		if err := writeCSV(CSVPath(dir, interval, key.Symbol), candles); err != nil {
			return err
		}
	}
	return nil
}

// writeCSV writes one instrument's bars.
//
// Prices are written with two decimals and timestamps in RFC3339 with the
// exchange's offset, matching histdl exactly. Two decimals is not a loss:
// prices are stored as paise, so the file carries the full stored precision.
func writeCSV(path string, candles []domain.Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("marketdata: creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	if err := w.Write(csvHeader); err != nil {
		return fmt.Errorf("marketdata: writing header to %s: %w", path, err)
	}
	for _, c := range candles {
		row := []string{
			c.Start.In(IST).Format(time.RFC3339),
			c.Open.String(),
			c.High.String(),
			c.Low.String(),
			c.Close.String(),
			strconv.FormatInt(c.Volume, 10),
		}
		if err := w.Write(row); err != nil {
			return fmt.Errorf("marketdata: writing %s: %w", path, err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("marketdata: flushing %s: %w", path, err)
	}
	return nil
}
