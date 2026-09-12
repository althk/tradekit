package paper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/ports"
)

var key = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func at(day, hour int) time.Time {
	return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC)
}

func rs(s string) money.Money { return money.MustParse(s) }

// sim returns a simulator with a round opening balance and no frictions, so a
// test that is not about slippage or charges reads as plain arithmetic.
func sim(t *testing.T) *Broker {
	t.Helper()
	return New(Options{Cash: rs("1000000.00")})
}

func dayBar(day int, o, h, l, c string) domain.Candle {
	return domain.Candle{
		Key: key, Timeframe: domain.D1, Start: at(day, 15),
		Open: rs(o), High: rs(h), Low: rs(l), Close: rs(c), Volume: 1000,
	}
}

func buy(qty int) domain.OrderRequest {
	return domain.OrderRequest{Key: key, Side: domain.Buy, Quantity: qty, Type: domain.Market, Product: domain.CNC}
}

func sell(qty int) domain.OrderRequest {
	return domain.OrderRequest{Key: key, Side: domain.Sell, Quantity: qty, Type: domain.Market, Product: domain.CNC}
}

func TestSatisfiesThePorts(t *testing.T) {
	var b any = New(Options{})
	if _, ok := b.(ports.Broker); !ok {
		t.Error("must implement ports.Broker: that is the whole point of the package")
	}
	for name, ok := range map[string]bool{
		"Quoter":           implements[ports.Quoter](b),
		"ProtectiveOrders": implements[ports.ProtectiveOrders](b),
		"PositionCloser":   implements[ports.PositionCloser](b),
		"TickObserver":     implements[ports.TickObserver](b),
	} {
		if !ok {
			t.Errorf("must implement ports.%s", name)
		}
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

func TestMarketOrderNeedsAPrice(t *testing.T) {
	b := sim(t)
	// Filling at a price the simulator invented is the error that makes a
	// backtest look better than the strategy is.
	_, err := b.PlaceOrder(context.Background(), buy(10))
	if !errors.Is(err, ErrRejected) {
		t.Errorf("err = %v, want ErrRejected before any price is seen", err)
	}
}

func TestMarketBuyThenSell(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(ctx, buy(10))
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != domain.StatusComplete || order.AveragePrice != rs("100.00") {
		t.Fatalf("entry = %+v", order)
	}

	positions, err := b.Positions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].Quantity != 10 {
		t.Fatalf("positions = %+v", positions)
	}

	b.OnTick(key, rs("110.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}

	trades := b.Trades()
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].GrossPnL != rs("100.00") || trades[0].NetPnL != rs("100.00") {
		t.Errorf("pnl = gross %s / net %s, want 100.00 each", trades[0].GrossPnL, trades[0].NetPnL)
	}
	if !trades[0].Paper {
		t.Error("every simulated trade must be marked paper")
	}
	if b.RealizedPnL() != rs("100.00") {
		t.Errorf("realized = %s, want 100.00", b.RealizedPnL())
	}
	if left, _ := b.Positions(ctx); len(left) != 0 {
		t.Errorf("position should be flat, got %+v", left)
	}
}

func TestShortRoundTrip(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}
	positions, _ := b.Positions(ctx)
	if len(positions) != 1 || positions[0].Quantity != -10 {
		t.Fatalf("a short must be a negative quantity, got %+v", positions)
	}

	b.OnTick(key, rs("90.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	trades := b.Trades()
	if len(trades) != 1 || trades[0].Side != domain.Sell {
		t.Fatalf("trades = %+v", trades)
	}
	if trades[0].GrossPnL != rs("100.00") {
		t.Errorf("a short that fell 10.00 on 10 units should make 100.00, got %s", trades[0].GrossPnL)
	}
}

func TestStopFillsAtGapPriceNotAtTheStop(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"),
	}); err != nil {
		t.Fatal(err)
	}

	// The next bar opens at 90, straight through the 95 stop. A stop is a
	// trigger, not a guarantee: the fill is at 90.
	b.OnBar(dayBar(2, "90.00", "92.00", "88.00", "91.00"))

	trades := b.Trades()
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].ExitPrice != rs("90.00") {
		t.Errorf("exit = %s, want 90.00: a gap through the stop fills at the open", trades[0].ExitPrice)
	}
	if trades[0].ExitReason != domain.ExitStop {
		t.Errorf("reason = %s, want stop", trades[0].ExitReason)
	}
	if trades[0].GrossPnL != rs("-100.00") {
		t.Errorf("pnl = %s, want -100.00", trades[0].GrossPnL)
	}
}

func TestBarContainingBothLevelsResolvesAsTheStop(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"), Target: rs("110.00"),
	}); err != nil {
		t.Fatal(err)
	}

	// The bar's range contains both levels. Nothing in a daily bar says
	// which came first, so the stop is taken.
	b.OnBar(dayBar(2, "100.00", "112.00", "94.00", "105.00"))

	trades := b.Trades()
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].ExitReason != domain.ExitStop {
		t.Errorf("reason = %s, want stop: an ambiguous bar must resolve against the position", trades[0].ExitReason)
	}
	if trades[0].ExitPrice != rs("95.00") {
		t.Errorf("exit = %s, want 95.00", trades[0].ExitPrice)
	}
}

func TestTargetFillsWhenTheStopIsUntouched(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"), Target: rs("110.00"),
	}); err != nil {
		t.Fatal(err)
	}

	b.OnBar(dayBar(2, "101.00", "112.00", "99.00", "108.00"))

	trades := b.Trades()
	if len(trades) != 1 || trades[0].ExitReason != domain.ExitTarget {
		t.Fatalf("trades = %+v, want one target exit", trades)
	}
	if trades[0].ExitPrice != rs("110.00") {
		t.Errorf("exit = %s, want 110.00", trades[0].ExitPrice)
	}
}

func TestOCOCancelsTheSiblingLeg(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"), Target: rs("110.00"),
	}); err != nil {
		t.Fatal(err)
	}

	b.OnBar(dayBar(2, "101.00", "112.00", "99.00", "108.00")) // target
	b.OnBar(dayBar(3, "100.00", "101.00", "90.00", "92.00"))  // would have hit the stop

	if trades := b.Trades(); len(trades) != 1 {
		t.Errorf("got %d trades, want 1: filling one leg must cancel the other", len(trades))
	}
	if legs, _ := b.ListProtective(ctx); len(legs) != 0 {
		t.Errorf("resting legs remain after the position closed: %+v", legs)
	}
}

func TestProtectiveIsDroppedWhenThePositionClosesElsewhere(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"),
	}); err != nil {
		t.Fatal(err)
	}

	// Closed by an explicit exit rather than by the stop.
	b.OnTick(key, rs("105.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}
	if legs, _ := b.ListProtective(ctx); len(legs) != 0 {
		t.Fatalf("a stop outlived its position: %+v", legs)
	}

	// The old stop level must not manufacture a second trade.
	b.OnBar(dayBar(3, "94.00", "94.00", "90.00", "91.00"))
	if trades := b.Trades(); len(trades) != 1 {
		t.Errorf("got %d trades, want 1", len(trades))
	}
}

func TestModifyStopMovesTheRestingLeg(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	id, err := b.PlaceProtective(ctx, domain.Protective{
		Key: key, Side: domain.Sell, Quantity: 10, Stop: rs("95.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Trail the stop up to breakeven.
	if err := b.ModifyStop(ctx, id, rs("100.00")); err != nil {
		t.Fatal(err)
	}

	b.OnBar(dayBar(2, "104.00", "105.00", "99.00", "101.00"))

	trades := b.Trades()
	if len(trades) != 1 || trades[0].ExitPrice != rs("100.00") {
		t.Fatalf("trades = %+v, want an exit at the moved stop", trades)
	}
	if trades[0].GrossPnL != 0 {
		t.Errorf("a breakeven stop should close flat, got %s", trades[0].GrossPnL)
	}
}

func TestLimitOrderRestsThenFills(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: 10, Type: domain.Limit,
		Product: domain.CNC, LimitPrice: rs("95.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != domain.StatusOpen {
		t.Fatalf("a limit away from the market must rest, got %s", order.Status)
	}
	if open, _ := b.OpenOrders(ctx); len(open) != 1 {
		t.Fatalf("expected one open order, got %d", len(open))
	}

	// A bar that never reaches 95 leaves it resting.
	b.OnBar(dayBar(2, "99.00", "101.00", "96.00", "100.00"))
	if open, _ := b.OpenOrders(ctx); len(open) != 1 {
		t.Error("the limit filled without the price reaching it")
	}

	b.OnBar(dayBar(3, "97.00", "98.00", "94.00", "96.00"))
	got, err := b.OrderStatus(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusComplete || got.AveragePrice != rs("95.00") {
		t.Errorf("limit fill = %+v, want complete at 95.00", got)
	}
}

func TestLimitOrderGappingThroughFillsAtTheOpen(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: 10, Type: domain.Limit,
		Product: domain.CNC, LimitPrice: rs("95.00"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The bar opens below the limit: the price was already better than
	// asked for, so the fill is at the open.
	b.OnBar(dayBar(2, "90.00", "93.00", "89.00", "92.00"))

	got, _ := b.OrderStatus(ctx, order.ID)
	if got.AveragePrice != rs("90.00") {
		t.Errorf("fill = %s, want 90.00 (the gap open, not the limit)", got.AveragePrice)
	}
}

func TestCancelOrder(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(ctx, domain.OrderRequest{
		Key: key, Side: domain.Buy, Quantity: 10, Type: domain.Limit,
		Product: domain.CNC, LimitPrice: rs("95.00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CancelOrder(ctx, order.ID); err != nil {
		t.Fatal(err)
	}

	b.OnBar(dayBar(2, "97.00", "98.00", "90.00", "96.00"))
	got, _ := b.OrderStatus(ctx, order.ID)
	if got.Status != domain.StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	if len(b.Trades()) != 0 {
		t.Error("a cancelled order filled anyway")
	}
	if err := b.CancelOrder(ctx, order.ID); !errors.Is(err, ErrRejected) {
		t.Errorf("cancelling twice: err = %v, want ErrRejected", err)
	}
}

func TestSlippageAlwaysWorksAgainstThePosition(t *testing.T) {
	b := New(Options{
		Cash:     rs("1000000.00"),
		Slippage: Slippage{Fraction: 0.001}, // 10 bps
	})
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	entry, err := b.PlaceOrder(ctx, buy(10))
	if err != nil {
		t.Fatal(err)
	}
	// A buy pays up.
	if entry.AveragePrice != rs("100.10") {
		t.Errorf("buy fill = %s, want 100.10", entry.AveragePrice)
	}

	b.OnTick(key, rs("110.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}
	// A sell receives less. Both legs erode the result; neither improves it.
	trade := b.Trades()[0]
	if trade.ExitPrice != rs("109.89") {
		t.Errorf("sell fill = %s, want 109.89", trade.ExitPrice)
	}
	if trade.GrossPnL >= rs("100.00") {
		t.Errorf("slippage must reduce the result, got %s", trade.GrossPnL)
	}
}

func TestTickRoundingNeverImprovesAFill(t *testing.T) {
	b := New(Options{
		Cash:     rs("1000000.00"),
		TickSize: func(domain.InstrumentKey) money.Money { return rs("0.05") },
	})
	ctx := context.Background()

	// 100.02 is not a printable price on a 5-paise tick.
	b.OnTick(key, rs("100.02"), at(1, 10))
	entry, err := b.PlaceOrder(ctx, buy(10))
	if err != nil {
		t.Fatal(err)
	}
	// A buy rounds up, never down: rounding to nearest would hand the
	// simulation free edge on half of all fills.
	if entry.AveragePrice != rs("100.05") {
		t.Errorf("buy fill = %s, want 100.05", entry.AveragePrice)
	}

	b.OnTick(key, rs("110.02"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}
	if got := b.Trades()[0].ExitPrice; got != rs("110.00") {
		t.Errorf("sell fill = %s, want 110.00 (rounded down)", got)
	}
}

func TestChargesAreDeductedFromNetPnL(t *testing.T) {
	table := costs.NewTable()
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	table.Set("testbroker", costs.EquityDelivery, costs.Brokerage, costs.Rate{Value: 0.001, EffectiveFrom: epoch})

	b := New(Options{
		Cash: rs("1000000.00"), Charges: table,
		Broker: "testbroker", Segment: costs.EquityDelivery,
	})
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	b.OnTick(key, rs("110.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}

	trade := b.Trades()[0]
	// 0.1% of each leg: 1.00 on the buy, 1.10 on the sell.
	if trade.Charges != rs("2.10") {
		t.Errorf("charges = %s, want 2.10", trade.Charges)
	}
	if trade.NetPnL != trade.GrossPnL-trade.Charges {
		t.Errorf("net (%s) must equal gross (%s) minus charges (%s)", trade.NetPnL, trade.GrossPnL, trade.Charges)
	}
	if b.RealizedPnL() != rs("97.90") {
		t.Errorf("realized = %s, want 97.90", b.RealizedPnL())
	}
}

func TestChargesAreZeroWithNoRateTable(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	b.OnTick(key, rs("110.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(10)); err != nil {
		t.Fatal(err)
	}
	// Zero means "not computed", never an invented estimate.
	if got := b.Trades()[0].Charges; got != 0 {
		t.Errorf("charges = %s, want 0 when no rate table is configured", got)
	}
}

func TestBuyingPowerIsEnforced(t *testing.T) {
	b := New(Options{Cash: rs("1000.00")})
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	// 20 units at 100.00 is 2000.00 against 1000.00 of cash.
	if _, err := b.PlaceOrder(ctx, buy(20)); !errors.Is(err, ErrRejected) {
		t.Errorf("err = %v, want ErrRejected for an unaffordable order", err)
	}
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Errorf("an affordable order was rejected: %v", err)
	}
}

func TestLeverageWidensBuyingPower(t *testing.T) {
	b := New(Options{Cash: rs("1000.00"), Leverage: 5})
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(40)); err != nil {
		t.Errorf("5x leverage should afford 4000.00 of notional: %v", err)
	}
}

func TestAveragingIntoAPosition(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	b.OnTick(key, rs("110.00"), at(1, 11))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}

	positions, _ := b.Positions(ctx)
	if len(positions) != 1 || positions[0].Quantity != 20 {
		t.Fatalf("positions = %+v", positions)
	}
	if positions[0].AveragePrice != rs("105.00") {
		t.Errorf("average = %s, want the quantity-weighted 105.00", positions[0].AveragePrice)
	}
}

func TestPartialClose(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	b.OnTick(key, rs("110.00"), at(2, 10))
	if _, err := b.PlaceOrder(ctx, sell(4)); err != nil {
		t.Fatal(err)
	}

	positions, _ := b.Positions(ctx)
	if len(positions) != 1 || positions[0].Quantity != 6 {
		t.Fatalf("positions = %+v, want 6 remaining", positions)
	}
	trades := b.Trades()
	if len(trades) != 1 || trades[0].Quantity != 4 {
		t.Fatalf("trades = %+v", trades)
	}
	if trades[0].GrossPnL != rs("40.00") {
		t.Errorf("pnl = %s, want 40.00 on the closed portion", trades[0].GrossPnL)
	}
}

func TestClosePositionReportsWhetherItFoundOne(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	closed, err := b.ClosePosition(ctx, key, domain.Buy, rs("100.00"), at(1, 10), domain.ExitEOD)
	if err != nil || closed {
		t.Errorf("closing nothing: (%v, %v), want (false, nil)", closed, err)
	}

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	// A long-exit must not close a short, and vice versa.
	if closed, _ := b.ClosePosition(ctx, key, domain.Sell, rs("110.00"), at(2, 10), domain.ExitEOD); closed {
		t.Error("a short-side close matched a long position")
	}
	closed, err = b.ClosePosition(ctx, key, domain.Buy, rs("110.00"), at(2, 10), domain.ExitEOD)
	if err != nil || !closed {
		t.Fatalf("closing a held position: (%v, %v), want (true, nil)", closed, err)
	}
	if got := b.Trades()[0].ExitReason; got != domain.ExitEOD {
		t.Errorf("reason = %s, want eod", got)
	}
}

func TestEquityTracksUnrealisedProfit(t *testing.T) {
	b := sim(t)
	ctx := context.Background()

	b.OnTick(key, rs("100.00"), at(1, 10))
	if _, err := b.PlaceOrder(ctx, buy(10)); err != nil {
		t.Fatal(err)
	}
	// Cash is untouched until the position closes; equity marks to market.
	if b.Cash() != rs("1000000.00") {
		t.Errorf("cash = %s, want the opening balance until a trade closes", b.Cash())
	}
	b.OnTick(key, rs("110.00"), at(1, 11))
	if b.Equity() != rs("1000100.00") {
		t.Errorf("equity = %s, want 1000100.00", b.Equity())
	}

	account, err := b.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if account.Used != rs("1000.00") {
		t.Errorf("used = %s, want the 1000.00 at cost", account.Used)
	}
}

func TestOrderIDsCarryThePaperPrefix(t *testing.T) {
	b := sim(t)
	b.OnTick(key, rs("100.00"), at(1, 10))
	order, err := b.PlaceOrder(context.Background(), buy(10))
	if err != nil {
		t.Fatal(err)
	}
	// The id is the last-resort mode marker for a persistence path that
	// forgot to propagate the paper flag.
	if len(order.ID) < len(OrderIDPrefix) || order.ID[:len(OrderIDPrefix)] != OrderIDPrefix {
		t.Errorf("id = %q, want the %q prefix", order.ID, OrderIDPrefix)
	}
}

func TestLTPOmitsUnseenInstruments(t *testing.T) {
	b := sim(t)
	other := domain.InstrumentKey{Exchange: "NSE", Symbol: "TCS"}
	b.OnTick(key, rs("100.00"), at(1, 10))

	got, err := b.LTP(context.Background(), []domain.InstrumentKey{key, other})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[key] != rs("100.00") {
		t.Errorf("ltp = %v, want only the instrument with a price", got)
	}
}

func TestRejectsDegenerateOrders(t *testing.T) {
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))

	if _, err := b.PlaceOrder(ctx, domain.OrderRequest{Key: key, Side: domain.Buy, Quantity: 0, Type: domain.Market}); !errors.Is(err, ErrRejected) {
		t.Error("a zero-quantity order must be rejected")
	}
	if _, err := b.PlaceOrder(ctx, domain.OrderRequest{Key: key, Side: domain.Buy, Quantity: 1, Type: domain.Limit}); !errors.Is(err, ErrRejected) {
		t.Error("a limit order with no price must be rejected")
	}
	if _, err := b.PlaceProtective(ctx, domain.Protective{Key: key, Side: domain.Sell, Quantity: 10}); !errors.Is(err, ErrRejected) {
		t.Error("a protective with neither leg must be rejected")
	}
}

func TestConcurrentTicksAndOrders(t *testing.T) {
	// A live paper session has a scanner placing orders while a feed
	// delivers ticks; this must not race.
	b := sim(t)
	ctx := context.Background()
	b.OnTick(key, rs("100.00"), at(1, 10))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			b.OnTick(key, rs("100.00")+money.Money(i), at(1, 10))
		}
	}()
	for range 50 {
		_, _ = b.PlaceOrder(ctx, buy(1))
		_, _ = b.Positions(ctx)
		_ = b.Equity()
	}
	<-done
}
