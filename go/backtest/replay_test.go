package backtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/marketdata"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

var ist = time.FixedZone("IST", 5*3600+1800)

// bars builds one flat daily bar per close, a day apart, starting 2025-04-14.
func bars(closes ...string) []domain.Candle {
	start := time.Date(2025, 4, 14, 0, 0, 0, 0, ist)
	out := make([]domain.Candle, 0, len(closes))
	for i, c := range closes {
		p := money.MustParse(c)
		out = append(out, domain.Candle{
			Key: reliance, Timeframe: domain.D1, Start: start.AddDate(0, 0, i),
			Open: p, High: p + 100, Low: p - 100, Close: p, Volume: 1000,
		})
	}
	return out
}

func newReplay(candles []domain.Candle) (*Replay, *paper.Broker) {
	broker := paper.New(paper.Options{Cash: money.MustParse("1000000.00")})
	return &Replay{
		Clock:  NewSimClock(candles[0].Start),
		Feed:   marketdata.FromSlice(candles),
		Broker: broker,
	}, broker
}

func TestBrokerSeesTheBarBeforeTheStrategy(t *testing.T) {
	// Buy on the first bar with a stop at 95; the third bar trades down
	// through it. On that bar the strategy must already see a flat book.
	candles := bars("100.00", "101.00", "94.00", "96.00", "97.00")
	r, broker := newReplay(candles)
	ctx := t.Context()

	var flatOnStopBar bool
	var stopBarSeen bool
	err := r.Run(ctx, func(ctx context.Context, c domain.Candle) error {
		switch {
		case c.Start.Equal(candles[0].Start):
			if _, err := broker.PlaceOrder(ctx, domain.OrderRequest{
				Key: reliance, Side: domain.Buy, Type: domain.Market, Quantity: 10, Product: domain.CNC,
			}); err != nil {
				return err
			}
			if _, err := broker.PlaceProtective(ctx, domain.Protective{
				Key: reliance, Side: domain.Sell, Quantity: 10, Stop: money.MustParse("95.00"), Product: domain.CNC,
			}); err != nil {
				return err
			}
		case c.Start.Equal(candles[2].Start):
			stopBarSeen = true
			positions, err := broker.Positions(ctx)
			if err != nil {
				return err
			}
			flatOnStopBar = len(positions) == 0
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stopBarSeen {
		t.Fatal("the strategy never saw the stop bar")
	}
	if !flatOnStopBar {
		t.Error("the strategy saw an open position on the bar that hit its stop; the broker must see a bar BEFORE the strategy, or the strategy adds to a position that is already stopped out and every backtest is optimistic in a way invisible in its output")
	}
	trades := broker.Trades()
	if len(trades) != 1 || trades[0].ExitReason != domain.ExitStop {
		t.Fatalf("expected one stop exit, got %+v", trades)
	}
	if !trades[0].ExitAt.Equal(candles[2].Start) {
		t.Errorf("the stop must fill on the bar that touched it (%v), got %v", candles[2].Start, trades[0].ExitAt)
	}
	if !r.Clock.Now().Equal(candles[4].Start) {
		t.Errorf("the clock must end at the last bar, got %v", r.Clock.Now())
	}
}

func TestAnOutOfOrderFeedIsAnErrorNotASortedReplay(t *testing.T) {
	candles := bars("100.00", "101.00", "102.00")
	candles[1], candles[2] = candles[2], candles[1]
	r, _ := newReplay(candles)

	var seen int
	err := r.Run(t.Context(), func(context.Context, domain.Candle) error { seen++; return nil })
	if err == nil {
		t.Fatal("an unordered feed means the caller's query or CSV is wrong; silently repairing it hides a data bug that will also affect live sync")
	}
	if seen != 2 {
		t.Errorf("the replay stops at the offending bar, got %d bars through", seen)
	}
}

func TestACancelledContextStopsTheLoop(t *testing.T) {
	r, _ := newReplay(bars("100.00", "101.00", "102.00", "103.00"))
	ctx, cancel := context.WithCancel(t.Context())
	var seen int
	err := r.Run(ctx, func(context.Context, domain.Candle) error {
		seen++
		if seen == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must be returned as ctx.Err(), got %v", err)
	}
	if seen != 2 {
		t.Errorf("the loop must stop at the cancelling bar, got %d", seen)
	}
}

func TestTheStrategysErrorPropagatesUnwrapped(t *testing.T) {
	r, _ := newReplay(bars("100.00", "101.00"))
	boom := errors.New("strategy refused")
	err := r.Run(t.Context(), func(context.Context, domain.Candle) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("fn's error must propagate so a caller can errors.Is it, got %v", err)
	}
}

func TestReplayNeedsItsCollaborators(t *testing.T) {
	if err := (&Replay{}).Run(t.Context(), func(context.Context, domain.Candle) error { return nil }); err == nil {
		t.Error("a replay with no clock, feed or broker must refuse to run")
	}
}
