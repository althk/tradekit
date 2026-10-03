package paper

import (
	"context"
	"errors"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func stopEntry(side domain.Side, qty int, trigger string) domain.OrderRequest {
	return domain.OrderRequest{
		Key: key, Side: side, Quantity: qty, Type: domain.Stop,
		TriggerPrice: rs(trigger), Product: domain.CNC,
	}
}

func TestBuyStopWaitsForPriceToRiseToTheTrigger(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(ctx, stopEntry(domain.Buy, 10, "110.00"))
	if err != nil {
		t.Fatal(err)
	}

	// A bar entirely below the trigger: a breakout that never happened must
	// not fill, least of all at a price better than the trigger.
	b.OnBar(dayBar(2, "100.00", "105.00", "95.00", "102.00"))
	if got, _ := b.OrderStatus(ctx, order.ID); got.Status != domain.StatusOpen {
		t.Fatalf("status = %s after a bar below the trigger, want open: a buy stop was matched as a limit", got.Status)
	}

	b.OnBar(dayBar(3, "104.00", "112.00", "103.00", "111.00"))
	got, _ := b.OrderStatus(ctx, order.ID)
	if got.Status != domain.StatusComplete || got.AveragePrice != rs("110.00") {
		t.Errorf("got %s at %s, want complete at the 110.00 trigger", got.Status, got.AveragePrice)
	}
}

func TestBuyStopGappingThroughFillsAtTheOpen(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	order, _ := b.PlaceOrder(ctx, stopEntry(domain.Buy, 10, "110.00"))

	b.OnBar(dayBar(2, "115.00", "118.00", "114.00", "116.00"))
	got, _ := b.OrderStatus(ctx, order.ID)
	if got.AveragePrice != rs("115.00") {
		t.Errorf("fill = %s, want the 115.00 gap open: a stop gets the gap, not the level", got.AveragePrice)
	}
}

func TestSellStopWaitsForPriceToFallToTheTrigger(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	order, _ := b.PlaceOrder(ctx, stopEntry(domain.Sell, 10, "95.00"))

	b.OnBar(dayBar(2, "100.00", "104.00", "96.00", "101.00"))
	if got, _ := b.OrderStatus(ctx, order.ID); got.Status != domain.StatusOpen {
		t.Fatalf("status = %s after a bar above the trigger, want open", got.Status)
	}
	b.OnBar(dayBar(3, "97.00", "98.00", "93.00", "94.00"))
	got, _ := b.OrderStatus(ctx, order.ID)
	if got.AveragePrice != rs("95.00") {
		t.Errorf("fill = %s, want the 95.00 trigger", got.AveragePrice)
	}
}

func TestStopLimitHonoursItsLimitAfterAGap(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	req := stopEntry(domain.Buy, 10, "110.00")
	req.Type, req.LimitPrice = domain.StopLimit, rs("111.00")
	order, err := b.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	// Gaps above the limit and never comes back: triggered but unfilled.
	b.OnBar(dayBar(2, "115.00", "118.00", "113.00", "116.00"))
	if got, _ := b.OrderStatus(ctx, order.ID); got.Status != domain.StatusOpen {
		t.Fatalf("status = %s, want open: a stop-limit must not fill above its limit", got.Status)
	}
	// Later trades back through the limit: fills as a limit would.
	b.OnBar(dayBar(3, "113.00", "114.00", "110.50", "111.50"))
	got, _ := b.OrderStatus(ctx, order.ID)
	if got.Status != domain.StatusComplete || got.AveragePrice != rs("111.00") {
		t.Errorf("got %s at %s, want complete at the 111.00 limit", got.Status, got.AveragePrice)
	}
}

func TestStopLimitNeedsBothPrices(t *testing.T) {
	b := sim(t)
	b.OnTick(key, rs("100.00"), at(1, 10))
	req := stopEntry(domain.Buy, 10, "110.00")
	req.Type = domain.StopLimit
	if _, err := b.PlaceOrder(context.Background(), req); !errors.Is(err, ErrRejected) {
		t.Errorf("err = %v, want ErrRejected for a stop-limit with no limit price", err)
	}
}

func TestOrderIDsAreUniqueAcrossSimulators(t *testing.T) {
	ctx := context.Background()
	first, second := sim(t), sim(t)
	first.OnTick(key, rs("100.00"), at(1, 10))
	second.OnTick(key, rs("100.00"), at(1, 10))
	a, _ := first.PlaceOrder(ctx, buy(1))
	b, _ := second.PlaceOrder(ctx, buy(1))
	if a.ID == b.ID {
		t.Errorf("both simulators minted %q: a restarted paper session would overwrite yesterday's stored order", a.ID)
	}
}

func TestBracketStopSurvivesUntilItsEntryFills(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: 10, Type: domain.Limit, LimitPrice: rs("98.00"), Product: domain.CNC,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"), Product: domain.CNC,
	}); err != nil {
		t.Fatal(err)
	}

	// No fill yet: the stop must still be resting.
	b.OnBar(dayBar(2, "100.00", "101.00", "99.00", "100.50"))
	if legs, _ := b.ListProtective(ctx); len(legs) != 1 {
		t.Fatalf("%d protectives resting, want 1: the stop was dropped before its entry filled", len(legs))
	}

	// The entry fills and the same bar runs through the stop: the stop must
	// be tested against the bar that opened the position.
	b.OnBar(dayBar(3, "99.00", "99.50", "94.00", "96.00"))
	trades := b.Trades()
	if len(trades) != 1 || trades[0].ExitReason != domain.ExitStop {
		t.Fatalf("trades = %+v, want one stopped-out trade in the fill bar", trades)
	}
	if trades[0].EntryPrice != rs("98.00") || trades[0].ExitPrice != rs("95.00") {
		t.Errorf("entry %s exit %s, want 98.00 and 95.00", trades[0].EntryPrice, trades[0].ExitPrice)
	}
}

func TestAveragingRoundsAgainstTheLong(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("1.00"), at(1, 10))
	_, _ = b.PlaceOrder(ctx, buy(1))
	b.OnTick(key, rs("1.01"), at(1, 11))
	_, _ = b.PlaceOrder(ctx, buy(2))

	// (100 + 202) / 3 = 100.67 paise; truncating to 100 would understate
	// the cost and flatter every later exit.
	pos, _ := b.Positions(ctx)
	if pos[0].AveragePrice != 101 {
		t.Errorf("average = %d paise, want 101: rounding must work against the position", pos[0].AveragePrice)
	}
}

func TestSubPaisaSlippageIsNotRoundedAway(t *testing.T) {
	b := New(Options{Cash: rs("1000000.00"), Slippage: Slippage{Fraction: 0.0005}})
	b.OnTick(key, rs("9.00"), at(1, 10))
	order, _ := b.PlaceOrder(context.Background(), buy(1))
	// 900 paise at 5 bps is 0.45 paise: a buy must still pay a paisa more.
	if order.AveragePrice != 901 {
		t.Errorf("fill = %d paise, want 901", order.AveragePrice)
	}
}

func TestWholePaisaSlippageIsExact(t *testing.T) {
	b := New(Options{Cash: rs("1000000.00"), Slippage: Slippage{Fraction: 0.0005}})
	b.OnTick(key, rs("100.00"), at(1, 10))
	order, _ := b.PlaceOrder(context.Background(), buy(1))
	if order.AveragePrice != rs("100.05") {
		t.Errorf("fill = %s, want 100.05: float error must not add a paisa", order.AveragePrice)
	}
}

func TestReversalRemainderNeedsBuyingPower(t *testing.T) {
	b := New(Options{Cash: rs("1000.00")})
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(5)); err != nil {
		t.Fatal(err)
	}
	// Selling 1000 closes 5 and would open a 995-share short worth 99,500.
	if _, err := b.PlaceOrder(ctx, sell(1000)); !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected: the reversal's new short is unaffordable", err)
	}
	if pos, _ := b.Positions(ctx); len(pos) != 1 || pos[0].Quantity != 5 {
		t.Errorf("positions = %+v, want the original long untouched by a rejected order", pos)
	}
	// A reversal the account can carry still goes through.
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Errorf("err = %v, want a small reversal to fill", err)
	}
}

func TestAvailableExcludesOpenExposure(t *testing.T) {
	b := New(Options{Cash: rs("1000.00")})
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	_, _ = b.PlaceOrder(ctx, buy(6))
	acct, _ := b.Account(ctx)
	if want := rs("400.00"); acct.Available != want {
		t.Errorf("available = %s, want %s: capital tied up in a position is not available", acct.Available, want)
	}
	if acct.Available < money.Zero {
		t.Error("available must not go negative")
	}
}
