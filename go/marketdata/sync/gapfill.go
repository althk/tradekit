package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/store"
)

// Params configures one Backfill pass.
type Params struct {
	// DB receives the bars. Required.
	DB *store.DB
	// History serves them. Required.
	History ports.HistoryProvider
	// Keys are the instruments to fill.
	Keys []domain.InstrumentKey
	// Timeframe of the bars.
	Timeframe domain.Timeframe

	// Lookback is how far back a full pull reaches, and how far back an
	// instrument with no stored bars is filled.
	Lookback time.Duration

	// Full re-requests the whole lookback window rather than only the gap
	// since the last stored bar.
	//
	// The incremental default can only extend history *forward*. Going from
	// one year of stored data to five is impossible without this, because
	// the last stored bar is already recent and the gap after it is empty —
	// the trap breakout500's own docstring names. Re-fetched bars are
	// INSERT OR IGNORE, so a full pull is safe and idempotent, and the only
	// cost is bandwidth.
	Full bool

	// Now is the end of the range, injectable for tests. Zero means
	// time.Now().
	Now time.Time
}

// Report is what one pass did.
type Report struct {
	// Requested counts the instruments attempted.
	Requested int
	// Inserted counts the bars the store accepted as new.
	Inserted int
	// Failed lists the instruments whose fetch or store failed. They are
	// collected rather than returned as an error so the pass completes.
	Failed []domain.InstrumentKey
	// Errors holds one error per failed instrument, in the same order, so an
	// operator can see whether 400 symbols failed for 400 reasons or for one.
	Errors []error
}

// Backfill fetches and stores the bars missing for each instrument.
//
// One instrument's failure does not stop the pass: it is collected in the
// report and the next is attempted. A 500-symbol sync that aborts on the first
// delisted ticker leaves 499 instruments stale, and the operator finds out from
// a backtest weeks later.
//
// A cancelled context does stop the pass, because it means the operator or the
// scheduler asked it to.
func Backfill(ctx context.Context, p Params) (Report, error) {
	if p.DB == nil {
		return Report{}, fmt.Errorf("sync: Params.DB is required")
	}
	if p.History == nil {
		return Report{}, fmt.Errorf("sync: Params.History is required")
	}
	if p.Lookback <= 0 {
		return Report{}, fmt.Errorf("sync: Params.Lookback must be positive")
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	report := Report{Requested: len(p.Keys)}
	for _, key := range p.Keys {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		inserted, err := backfillOne(ctx, p, key, now)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return report, err
			}
			report.Failed = append(report.Failed, key)
			report.Errors = append(report.Errors, fmt.Errorf("sync: %s: %w", key, err))
			continue
		}
		report.Inserted += inserted
	}
	return report, nil
}

// backfillOne fills a single instrument, reporting how many bars were new.
func backfillOne(ctx context.Context, p Params, key domain.InstrumentKey, now time.Time) (int, error) {
	from := now.Add(-p.Lookback)
	if !p.Full {
		last, ok, err := p.DB.LastCandleStart(ctx, key, p.Timeframe)
		if err != nil {
			return 0, err
		}
		if ok {
			// Resume from the bar after the last one held. Starting at
			// the last bar itself would re-request a bar that is already
			// stored on every single run.
			from = last.Add(time.Second)
		}
	}
	if from.After(now) {
		// Already current. This is the normal state of an intraday sync
		// run twice in one minute, not a failure.
		return 0, nil
	}

	candles, err := p.History.Candles(ctx, key, p.Timeframe, from, now)
	if err != nil {
		return 0, err
	}
	if len(candles) == 0 {
		return 0, nil
	}
	return p.DB.UpsertCandles(ctx, candles)
}
