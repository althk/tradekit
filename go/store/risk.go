package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/althk/tradekit/go/core/risk"
)

// The risk gates read per-day counters that must survive a restart -- a
// process that comes back at noon must still know it has spent its daily
// budget -- and every project kept them in kv_state under its own name with
// its own version of the "is this still today?" check. These two are that
// pair, once.

// LoadDailyState returns the counters stored under key if they belong to
// date, and empty counters otherwise: on a cold start, on the first run of
// a new day, and when the key was written by another date. A read that
// fails for any other reason is an error, because starting the day with
// zeroed counters after a failed read is exactly the silent failure the
// counters exist to prevent.
func (d *DB) LoadDailyState(ctx context.Context, key, date string) (*risk.DailyState, error) {
	var st risk.DailyState
	err := d.GetState(ctx, key, &st)
	if errors.Is(err, ErrStateNotFound) {
		return risk.NewDailyState(date), nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: loading daily risk state: %w", err)
	}
	return st.FreshFor(date), nil
}

// SaveDailyState persists the counters under key, typically from
// Tracker.Snapshot after each trade.
func (d *DB) SaveDailyState(ctx context.Context, key string, st risk.DailyState) error {
	return d.SetState(ctx, key, st)
}
