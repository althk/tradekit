package backtest

import (
	"context"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/marketdata"
)

// Replay drives a paper broker through a bar feed.
//
// It is the loop every donor wrote and every donor got subtly wrong at least
// once. The ordering is the whole point: a bar reaches the broker BEFORE the
// strategy, so a stop that should have been hit on this bar has already filled
// when the strategy decides whether to add to the position. Getting this
// backwards is the single most common way a backtest becomes optimistic, and
// it is invisible in the output.
type Replay struct {
	Clock  *SimClock
	Feed   marketdata.BarFeed
	Broker *paper.Broker
}

// OnBar is called after the broker has seen the bar, so any fill the bar
// caused has already happened and the broker's positions reflect it.
type OnBar func(ctx context.Context, c domain.Candle) error

// Run replays the feed to exhaustion, or until fn returns an error or ctx is
// cancelled. fn's error is returned unwrapped so a caller can errors.Is it.
//
// An out-of-order bar is an error, not a sort and not a skip: an unordered
// feed means the caller's query or CSV is wrong, and silently repairing it
// hides a data bug that will also affect live sync.
//
// A single feed is one instrument. Multi-symbol replay is composed by the
// caller from several feeds; the k-way merge that needs is a marketdata
// concern and is built when a real caller needs it.
func (r *Replay) Run(ctx context.Context, fn OnBar) error {
	if r.Clock == nil || r.Feed == nil || r.Broker == nil {
		return fmt.Errorf("backtest: Replay needs a Clock, a Feed and a Broker")
	}
	var last time.Time
	seen := false
	for {
		c, ok, err := r.Feed.Next()
		if err != nil {
			return fmt.Errorf("backtest: reading feed: %w", err)
		}
		if !ok {
			return nil
		}
		if seen && c.Start.Before(last) {
			return fmt.Errorf("backtest: feed is out of order: bar at %v follows bar at %v", c.Start, last)
		}
		last, seen = c.Start, true

		r.Clock.Advance(c.Start)
		r.Broker.OnBar(c)
		if err := fn(ctx, c); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
