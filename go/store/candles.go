package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// UpsertCandles stores bars, ignoring ones already held, and reports how many
// rows were new.
//
// INSERT OR IGNORE rather than REPLACE: re-fetching a window is the normal way
// to deepen history, and a vendor occasionally returns a slightly different
// volume for an old bar. Keeping the first value read makes a backtest
// reproducible; overwriting would silently change yesterday's results.
func (d *DB) UpsertCandles(ctx context.Context, candles []domain.Candle) (int, error) {
	if len(candles) == 0 {
		return 0, nil
	}

	var inserted int
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT OR IGNORE INTO candles
				(exchange, symbol, timeframe, start, open, high, low, close, volume, open_interest)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("store: preparing candle insert: %w", err)
		}
		defer stmt.Close()

		for _, c := range candles {
			res, err := stmt.ExecContext(ctx,
				c.Key.Exchange, c.Key.Symbol, string(c.Timeframe), formatTime(c.Start),
				int64(c.Open), int64(c.High), int64(c.Low), int64(c.Close),
				c.Volume, c.OpenInterest)
			if err != nil {
				return fmt.Errorf("store: inserting candle %s %s %s: %w",
					c.Key, c.Timeframe, c.Start.Format(time.RFC3339), err)
			}
			if n, err := res.RowsAffected(); err == nil {
				inserted += int(n)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// Candles returns bars in [from, to] inclusive, oldest first.
func (d *DB) Candles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT start, open, high, low, close, volume, open_interest
		FROM candles
		WHERE exchange = ? AND symbol = ? AND timeframe = ? AND start >= ? AND start <= ?
		ORDER BY start`,
		key.Exchange, key.Symbol, string(tf), formatTime(from), formatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store: reading candles for %s: %w", key, err)
	}
	defer rows.Close()

	var out []domain.Candle
	for rows.Next() {
		c := domain.Candle{Key: key, Timeframe: tf}
		var start string
		var open, high, low, cl int64
		if err := rows.Scan(&start, &open, &high, &low, &cl, &c.Volume, &c.OpenInterest); err != nil {
			return nil, fmt.Errorf("store: scanning candle: %w", err)
		}
		if c.Start, err = parseTime(start); err != nil {
			return nil, err
		}
		c.Open, c.High, c.Low, c.Close = money.Money(open), money.Money(high), money.Money(low), money.Money(cl)
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastCandleStart returns the newest stored bar's start for an instrument and
// timeframe, reporting false when none is held.
//
// This is the query an incremental sync is built on: fetch only from here
// forward. Every project tradekit replaces wrote its own version of it, and it
// is the reason candles are keyed by start rather than by an arbitrary id.
func (d *DB) LastCandleStart(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe) (time.Time, bool, error) {
	var start sql.NullString
	err := d.db.QueryRowContext(ctx,
		`SELECT max(start) FROM candles WHERE exchange = ? AND symbol = ? AND timeframe = ?`,
		key.Exchange, key.Symbol, string(tf)).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) || !start.Valid {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: reading last candle for %s: %w", key, err)
	}
	t, err := parseTime(start.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// CandleSpan returns the oldest and newest stored bar starts, reporting false
// when none is held. It answers "how much history do I actually have", which a
// backtest must know before trusting its own warm-up.
func (d *DB) CandleSpan(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe) (first, last time.Time, ok bool, err error) {
	var lo, hi sql.NullString
	err = d.db.QueryRowContext(ctx,
		`SELECT min(start), max(start) FROM candles WHERE exchange = ? AND symbol = ? AND timeframe = ?`,
		key.Exchange, key.Symbol, string(tf)).Scan(&lo, &hi)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("store: reading candle span for %s: %w", key, err)
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	if first, err = parseTime(lo.String); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if last, err = parseTime(hi.String); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	return first, last, true, nil
}

// CandleCount reports how many bars are stored for an instrument and timeframe.
func (d *DB) CandleCount(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM candles WHERE exchange = ? AND symbol = ? AND timeframe = ?`,
		key.Exchange, key.Symbol, string(tf)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting candles for %s: %w", key, err)
	}
	return n, nil
}
