package backtest

import (
	"context"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/core/stats"
)

func TestSnapshotterEmitsOnePointPerDateAndTheLastOnlyOnClose(t *testing.T) {
	broker := paper.New(paper.Options{Cash: money.MustParse("1000.00")})
	s := &Snapshotter{Broker: broker, Loc: ist}

	day := time.Date(2025, 4, 14, 9, 15, 0, 0, ist)
	for d := 0; d < 3; d++ {
		for m := 0; m < 3; m++ {
			c := bars("100.00")[0]
			c.Start = day.AddDate(0, 0, d).Add(time.Duration(m) * 5 * time.Minute)
			broker.OnBar(c)
			s.Observe(c)
		}
	}
	if len(s.Points) != 2 {
		t.Fatalf("three dates of bars yield two points before Close, got %d", len(s.Points))
	}
	s.Close()
	if len(s.Points) != 3 {
		t.Fatalf("Close must emit the final date; a caller who omits it silently loses the last session, which is the one a walk-forward run cares about. Got %d points", len(s.Points))
	}
	for i, p := range s.Points {
		want := day.AddDate(0, 0, i).Add(10 * time.Minute)
		if !p.At.Equal(want) {
			t.Errorf("point %d is stamped %v, want the date's last bar %v", i, p.At, want)
		}
	}
	s.Close()
	if len(s.Points) != 3 {
		t.Error("a second Close must not emit a duplicate point")
	}
}

func TestAWeekendGapProducesTwoPointsNotThree(t *testing.T) {
	broker := paper.New(paper.Options{Cash: money.MustParse("1000.00")})
	s := &Snapshotter{Broker: broker, Loc: ist}

	friday := time.Date(2025, 4, 18, 0, 0, 0, 0, ist)
	monday := friday.AddDate(0, 0, 3)
	for _, c := range bars("100.00", "101.00") {
		c.Start = friday
		broker.OnBar(c)
		s.Observe(c)
		friday = monday
	}
	s.Close()
	if len(s.Points) != 2 {
		t.Errorf("a day changes when the date key changes, not when 24h elapse; a weekend is not three days of flat equity. Got %d points", len(s.Points))
	}
}

func TestMarkToMarketDrawdownExceedsClosedTradeDrawdown(t *testing.T) {
	// Buy at 100, watch the position fall to 70 for a day, recover, and exit
	// at 101 for a small profit. The closed-trade curve shows one small step
	// up; the mark-to-market curve shows the 30% hole in between.
	candles := bars("100.00", "70.00", "101.00", "101.00")
	r, broker := newReplay(candles)
	s := &Snapshotter{Broker: broker, Loc: ist}

	err := r.Run(t.Context(), func(ctx context.Context, c domain.Candle) error {
		switch {
		case c.Start.Equal(candles[0].Start):
			_, err := broker.PlaceOrder(ctx, domain.OrderRequest{
				Key: reliance, Side: domain.Buy, Type: domain.Market, Quantity: 100, Product: domain.CNC,
			})
			if err != nil {
				return err
			}
		case c.Start.Equal(candles[2].Start):
			_, err := broker.ClosePosition(ctx, reliance, domain.Buy, c.Close, c.Start, domain.ExitSignal)
			if err != nil {
				return err
			}
		}
		s.Observe(c)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s.Close()

	opening := money.MustParse("1000000.00")
	closedDD, _ := stats.MaxDrawdown(stats.EquityCurve(broker.Trades(), opening))
	markedDD, _ := stats.MaxDrawdown(s.Points)
	if closedDD != 0 {
		t.Fatalf("one profitable trade has no closed-trade drawdown, got %s", closedDD)
	}
	if markedDD < money.MustParse("3000.00") {
		t.Errorf("the mark-to-market curve must show the day the position was 30%% underwater; a curve that only moves when a trade closes cannot see it, which is why this file exists. Got %s", markedDD)
	}
}
