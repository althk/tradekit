// Package paper simulates order execution.
//
// It replaces five separate simulators. More importantly, it is what makes
// backtesting through the same ports.Broker interface as live trading possible:
// a strategy cannot tell which it is running against, so there is no second
// code path to keep in step and no class of bug that appears only in production.
//
// That makes this package's fidelity a correctness requirement rather than a
// convenience. A simulator that is optimistic about fills produces backtests
// that are wrong in the one direction that costs money, so the rules below are
// deliberately conservative wherever the real sequence is unknowable:
//
//   - A gap through a stop fills at the gap price, not at the stop. A stop is a
//     trigger, not a guarantee; a bar that opens below a long's stop fills there.
//   - When a bar's range contains both the stop and the target, the stop is
//     taken. Daily and even 5-minute bars do not say which came first, and
//     assuming the target would flatter every result.
//   - Slippage is applied against the position, never for it.
//
// The simulator is driven by prices, not by wall-clock time: call OnBar for a
// bar-based backtest or OnTick for a tick stream. Both apply the same rules.
package paper

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// OrderIDPrefix tags every order id the simulator mints.
//
// It is the last-resort mode marker: an order that reaches persistence without
// its paper flag set is still recognisable by its id, so a live row can never
// be created from a simulated fill through a path that forgot to propagate the
// flag.
const OrderIDPrefix = "PAPER-"

// ErrRejected reports an order the simulator refused, for the reasons a real
// venue would: no such price, not enough capital, nothing to close.
var ErrRejected = errors.New("paper: order rejected")

// Slippage models the gap between the price a fill is triggered at and the
// price it actually gets.
type Slippage struct {
	// Fraction of the price, applied against the position: 0.0005 is five
	// basis points. A buy fills higher, a sell lower.
	Fraction float64
	// Ticks is an additional whole-tick adjustment, also against the
	// position. Use it when the instrument's spread is better described in
	// ticks than in basis points.
	Ticks int
}

// apply moves price against the side by the configured amount.
func (s Slippage) apply(price money.Money, side domain.Side, tick money.Money) money.Money {
	adjusted := price
	if s.Fraction != 0 {
		delta := price.MulFraction(s.Fraction)
		if side == domain.Buy {
			adjusted += delta
		} else {
			adjusted -= delta
		}
	}
	if s.Ticks != 0 && tick > 0 {
		delta := tick.Mul(int64(s.Ticks))
		if side == domain.Buy {
			adjusted += delta
		} else {
			adjusted -= delta
		}
	}
	if adjusted < 0 {
		return 0
	}
	return adjusted
}

// Options configures a simulator.
type Options struct {
	// Cash is the opening balance.
	Cash money.Money
	// Slippage applied to every fill.
	Slippage Slippage
	// TickSize returns an instrument's tick, used to round fills to a price
	// the exchange could actually print. A nil function means no rounding.
	TickSize func(domain.InstrumentKey) money.Money
	// Charges prices closed round trips. A nil table means trades close
	// with zero charges, which is honest about not knowing them rather
	// than inventing a figure.
	Charges *costs.Table
	// Broker and Segment select the schedule within Charges.
	Broker  string
	Segment costs.Segment
	// Leverage multiplies buying power for the affordability check.
	Leverage float64
}

// position is an open holding and the bookkeeping the simulator needs to close
// it correctly.
type position struct {
	Key          domain.InstrumentKey `json:"key"`
	Quantity     int                  `json:"quantity"` // signed: negative is short
	AveragePrice money.Money          `json:"average_price"`
	Product      domain.Product       `json:"product"`
	OpenedAt     time.Time            `json:"opened_at"`
	Strategy     string               `json:"strategy"`
}

// resting is an order waiting for a price: a limit entry or a protective leg.
type resting struct {
	ID       string               `json:"id"`
	Key      domain.InstrumentKey `json:"key"`
	Side     domain.Side          `json:"side"`
	Quantity int                  `json:"quantity"`
	// Limit fills when price reaches it or better; zero for a protective.
	Limit money.Money `json:"limit"`
	// Stop and Target are the protective legs; zero when unset.
	Stop    money.Money    `json:"stop"`
	Target  money.Money    `json:"target"`
	Product domain.Product `json:"product"`
	Tag     string         `json:"tag"`
	// Protective marks a resting order that closes a position rather than
	// opening one.
	Protective bool `json:"protective"`
}

// Broker is the simulator. It is safe for concurrent use: a live paper session
// has a scanner goroutine placing orders while a feed goroutine delivers ticks.
type Broker struct {
	mu sync.Mutex

	opts Options

	cash      money.Money
	realized  money.Money
	seq       int
	lastPrice map[domain.InstrumentKey]money.Money
	lastAt    map[domain.InstrumentKey]time.Time
	lastBar   map[domain.InstrumentKey]domain.Candle
	positions map[domain.InstrumentKey]*position
	orders    map[string]*domain.Order
	resting   map[string]*resting
	trades    []domain.Trade
}

// New returns a simulator with the given opening balance and rules.
func New(opts Options) *Broker {
	if opts.Leverage < 1 {
		opts.Leverage = 1
	}
	return &Broker{
		opts:      opts,
		cash:      opts.Cash,
		lastPrice: map[domain.InstrumentKey]money.Money{},
		lastAt:    map[domain.InstrumentKey]time.Time{},
		lastBar:   map[domain.InstrumentKey]domain.Candle{},
		positions: map[domain.InstrumentKey]*position{},
		orders:    map[string]*domain.Order{},
		resting:   map[string]*resting{},
	}
}

// nextID mints a paper order id. The caller must hold the lock.
func (b *Broker) nextID(kind string) string {
	b.seq++
	return OrderIDPrefix + kind + "-" + strconv.Itoa(b.seq)
}

// tickOf returns an instrument's tick size, or zero when unknown.
func (b *Broker) tickOf(key domain.InstrumentKey) money.Money {
	if b.opts.TickSize == nil {
		return 0
	}
	return b.opts.TickSize(key)
}

// fillPrice applies slippage and rounds to a printable price.
//
// Rounding is directional: a buy rounds up and a sell rounds down, so the
// rounding itself can never improve the fill. Rounding to nearest would hand
// the simulation a fraction of a tick of free edge on half of all trades.
func (b *Broker) fillPrice(key domain.InstrumentKey, raw money.Money, side domain.Side) money.Money {
	tick := b.tickOf(key)
	slipped := b.opts.Slippage.apply(raw, side, tick)
	if tick <= 0 {
		return slipped
	}
	if side == domain.Buy {
		return slipped.CeilToTick(tick)
	}
	return slipped.FloorToTick(tick)
}

// --- ports.Broker ---

// Account returns the simulated funds view.
func (b *Broker) Account(context.Context) (domain.Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return domain.Account{
		Equity:    b.equityLocked(),
		Available: b.cash,
		Used:      b.exposureLocked(),
	}, nil
}

// equityLocked is cash plus the mark-to-market value of open positions.
func (b *Broker) equityLocked() money.Money {
	equity := b.cash
	for key, p := range b.positions {
		last, ok := b.lastPrice[key]
		if !ok {
			last = p.AveragePrice
		}
		equity += domain.GrossFor(sideOf(p.Quantity), abs(p.Quantity), p.AveragePrice, last)
	}
	return equity
}

// exposureLocked is the notional value of open positions at cost.
func (b *Broker) exposureLocked() money.Money {
	var used money.Money
	for _, p := range b.positions {
		used += p.AveragePrice.Mul(int64(abs(p.Quantity)))
	}
	return used
}

// PlaceOrder accepts a market or limit order.
//
// A market order fills immediately at the last seen price; without one the
// order is rejected rather than filled at a guess, because a fill price
// invented by the simulator is the kind of error that makes a backtest look
// better than the strategy is.
func (b *Broker) PlaceOrder(_ context.Context, req domain.OrderRequest) (domain.Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if req.Quantity <= 0 {
		return domain.Order{}, fmt.Errorf("%w: quantity must be positive, got %d", ErrRejected, req.Quantity)
	}

	id := b.nextID("ORD")
	now := b.lastAt[req.Key]
	order := &domain.Order{ID: id, Request: req, Status: domain.StatusOpen, PlacedAt: now, UpdatedAt: now}
	b.orders[id] = order

	switch req.Type {
	case domain.Market:
		last, ok := b.lastPrice[req.Key]
		if !ok {
			order.Status = domain.StatusRejected
			order.Message = "no price seen for " + req.Key.String()
			return *order, fmt.Errorf("%w: no price seen for %s", ErrRejected, req.Key)
		}
		if err := b.fillLocked(order, last, now); err != nil {
			return *order, err
		}
	case domain.Limit, domain.Stop, domain.StopLimit:
		trigger := req.LimitPrice
		if req.Type != domain.Limit {
			trigger = req.TriggerPrice
		}
		if trigger <= 0 {
			order.Status = domain.StatusRejected
			order.Message = "a resting order needs a price"
			return *order, fmt.Errorf("%w: a resting order needs a price", ErrRejected)
		}
		b.resting[id] = &resting{
			ID: id, Key: req.Key, Side: req.Side, Quantity: req.Quantity,
			Limit: trigger, Product: req.Product, Tag: req.Tag,
		}
	default:
		order.Status = domain.StatusRejected
		order.Message = "unsupported order type " + string(req.Type)
		return *order, fmt.Errorf("%w: unsupported order type %q", ErrRejected, req.Type)
	}
	return *order, nil
}

// CancelOrder withdraws a resting order.
func (b *Broker) CancelOrder(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	order, ok := b.orders[id]
	if !ok {
		return fmt.Errorf("%w: unknown order %s", ErrRejected, id)
	}
	if order.Status.Terminal() {
		return fmt.Errorf("%w: order %s is already %s", ErrRejected, id, order.Status)
	}
	delete(b.resting, id)
	order.Status = domain.StatusCancelled
	order.UpdatedAt = b.lastAt[order.Request.Key]
	return nil
}

// OrderStatus returns one order's state.
func (b *Broker) OrderStatus(_ context.Context, id string) (domain.Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	order, ok := b.orders[id]
	if !ok {
		return domain.Order{}, fmt.Errorf("%w: unknown order %s", ErrRejected, id)
	}
	return *order, nil
}

// Positions returns the open positions.
func (b *Broker) Positions(context.Context) ([]domain.Position, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]domain.Position, 0, len(b.positions))
	for key, p := range b.positions {
		last, ok := b.lastPrice[key]
		if !ok {
			last = p.AveragePrice
		}
		out = append(out, domain.Position{
			Key:           key,
			Quantity:      p.Quantity,
			AveragePrice:  p.AveragePrice,
			Product:       p.Product,
			UnrealizedPnL: domain.GrossFor(sideOf(p.Quantity), abs(p.Quantity), p.AveragePrice, last),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out, nil
}

// OpenOrders returns orders that have not reached a terminal state.
func (b *Broker) OpenOrders(context.Context) ([]domain.Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []domain.Order
	for _, o := range b.orders {
		if !o.Status.Terminal() {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- ports.Quoter ---

// LTP returns the last price seen for each instrument. Instruments with no
// price yet are omitted rather than reported as zero.
func (b *Broker) LTP(_ context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]money.Money, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make(map[domain.InstrumentKey]money.Money, len(keys))
	for _, k := range keys {
		if p, ok := b.lastPrice[k]; ok {
			out[k] = p
		}
	}
	return out, nil
}

// Quote returns the last price seen, with the open, high, low and close of the
// most recent completed bar when one has been delivered.
//
// A tick-driven simulation has no bar to report, so those fields stay zero
// rather than being synthesised from the tick stream: a strategy reading a
// session high the simulator invented would behave differently here than live.
func (b *Broker) Quote(_ context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]domain.Quote, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make(map[domain.InstrumentKey]domain.Quote, len(keys))
	for _, k := range keys {
		last, ok := b.lastPrice[k]
		if !ok {
			continue
		}
		q := domain.Quote{Key: k, At: b.lastAt[k], Last: last}
		if c, ok := b.lastBar[k]; ok {
			q.Open, q.High, q.Low, q.Close = c.Open, c.High, c.Low, c.Close
		}
		out[k] = q
	}
	return out, nil
}

// --- ports.ProtectiveOrders ---

// PlaceProtective rests a stop, or a stop and target as a one-cancels-other
// pair, against an open position.
func (b *Broker) PlaceProtective(_ context.Context, p domain.Protective) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if p.Quantity <= 0 {
		return "", fmt.Errorf("%w: protective quantity must be positive", ErrRejected)
	}
	if p.Stop <= 0 && p.Target <= 0 {
		return "", fmt.Errorf("%w: a protective order needs a stop or a target", ErrRejected)
	}
	id := b.nextID("GTT")
	b.resting[id] = &resting{
		ID: id, Key: p.Key, Side: p.Side, Quantity: p.Quantity,
		Stop: p.Stop, Target: p.Target, Product: p.Product, Protective: true,
	}
	return id, nil
}

// ModifyStop moves a resting protective's stop, as a breakeven or trailing
// adjustment does.
func (b *Broker) ModifyStop(_ context.Context, id string, stop money.Money) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	r, ok := b.resting[id]
	if !ok || !r.Protective {
		return fmt.Errorf("%w: unknown protective %s", ErrRejected, id)
	}
	r.Stop = stop
	return nil
}

// CancelProtective removes a resting protective order.
func (b *Broker) CancelProtective(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if r, ok := b.resting[id]; !ok || !r.Protective {
		return fmt.Errorf("%w: unknown protective %s", ErrRejected, id)
	}
	delete(b.resting, id)
	return nil
}

// ListProtective returns the resting protective orders.
func (b *Broker) ListProtective(context.Context) ([]domain.Protective, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []domain.Protective
	for _, r := range b.resting {
		if !r.Protective {
			continue
		}
		out = append(out, domain.Protective{
			ID: r.ID, Key: r.Key, Side: r.Side, Quantity: r.Quantity,
			Stop: r.Stop, Target: r.Target, Product: r.Product,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- ports.PositionCloser ---

// ClosePosition closes any open position in key on the given side at price.
//
// The simulator implements this because its positions live inside its own book
// with the P&L bookkeeping attached: a generic counter order would open a
// second position rather than close the first.
func (b *Broker) ClosePosition(_ context.Context, key domain.InstrumentKey, side domain.Side, price money.Money, at time.Time, reason domain.ExitReason) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	p, ok := b.positions[key]
	if !ok || sideOf(p.Quantity) != side {
		return false, nil
	}
	b.closeLocked(p, abs(p.Quantity), b.fillPrice(key, price, side.Opposite()), at, reason)
	return true, nil
}

// --- ports.TickObserver ---

// OnTick delivers a traded price, filling anything it triggers.
//
// A simulator must see prices: without them a simulated position has no stop at
// all, and a backtest that silently loses its stops reports fictional results.
func (b *Broker) OnTick(key domain.InstrumentKey, price money.Money, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observeLocked(key, price, at)
	b.matchLocked(key, bar{open: price, high: price, low: price, close: price, at: at})
}

// OnBar delivers a completed bar, filling anything its range triggers.
//
// This is how a bar-based backtest drives the simulator. The bar is replayed
// conservatively: the open first, then the stop, then the target, so an
// ambiguous bar always resolves against the position.
func (b *Broker) OnBar(c domain.Candle) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observeLocked(c.Key, c.Close, c.Start)
	b.lastBar[c.Key] = c
	b.matchLocked(c.Key, bar{open: c.Open, high: c.High, low: c.Low, close: c.Close, at: c.Start})
}

func (b *Broker) observeLocked(key domain.InstrumentKey, price money.Money, at time.Time) {
	b.lastPrice[key] = price
	b.lastAt[key] = at
}

// bar is the price range a match runs against. A tick is a bar of zero width.
type bar struct {
	open, high, low, close money.Money
	at                     time.Time
}

// touched reports whether a price level falls inside the bar's range.
func (bb bar) touched(level money.Money) bool {
	return level >= bb.low && level <= bb.high
}

// matchLocked fills every resting order this bar triggers.
func (b *Broker) matchLocked(key domain.InstrumentKey, bb bar) {
	for _, r := range b.restingFor(key) {
		if r.Protective {
			b.matchProtectiveLocked(r, bb)
			continue
		}
		b.matchEntryLocked(r, bb)
	}
}

// restingFor returns this instrument's resting orders in a stable order, so a
// bar that triggers several of them behaves identically on every run.
func (b *Broker) restingFor(key domain.InstrumentKey) []*resting {
	var out []*resting
	for _, r := range b.resting {
		if r.Key == key {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// matchEntryLocked fills a resting entry when the bar reaches its price.
//
// A bar that opens through the limit fills at the open, not at the limit: the
// price was already better than asked for, and pretending otherwise would
// under-report the entry.
func (b *Broker) matchEntryLocked(r *resting, bb bar) {
	crossed := false
	trigger := r.Limit
	if r.Side == domain.Buy {
		if bb.open <= r.Limit {
			crossed, trigger = true, bb.open
		} else if bb.low <= r.Limit {
			crossed = true
		}
	} else {
		if bb.open >= r.Limit {
			crossed, trigger = true, bb.open
		} else if bb.high >= r.Limit {
			crossed = true
		}
	}
	if !crossed {
		return
	}
	order := b.orders[r.ID]
	delete(b.resting, r.ID)
	if order == nil {
		return
	}
	_ = b.fillLocked(order, trigger, bb.at)
}

// matchProtectiveLocked resolves a resting stop and target against the bar.
//
// The order of checks is the conservative rule every implementation this
// replaces arrived at independently: a gap through the stop fills at the gap
// price, and a bar containing both levels resolves as the stop.
func (b *Broker) matchProtectiveLocked(r *resting, bb bar) {
	p, ok := b.positions[r.Key]
	if !ok {
		// The position is already gone; the protective is stale.
		delete(b.resting, r.ID)
		return
	}
	long := p.Quantity > 0

	// A gap straight through the stop fills at the open. A stop is a
	// trigger, not a guarantee, and this is where a naive simulator quietly
	// awards a fill that never existed.
	if r.Stop > 0 {
		gapped := (long && bb.open <= r.Stop) || (!long && bb.open >= r.Stop)
		if gapped {
			b.resolveProtectiveLocked(r, p, bb.open, bb.at, domain.ExitStop)
			return
		}
	}
	if r.Target > 0 {
		gapped := (long && bb.open >= r.Target) || (!long && bb.open <= r.Target)
		if gapped {
			b.resolveProtectiveLocked(r, p, bb.open, bb.at, domain.ExitTarget)
			return
		}
	}

	// Within the bar, the stop is checked first: a daily or 5-minute bar
	// does not say which level was reached first, and assuming the target
	// would flatter every ambiguous trade.
	if r.Stop > 0 && bb.touched(r.Stop) {
		b.resolveProtectiveLocked(r, p, r.Stop, bb.at, domain.ExitStop)
		return
	}
	if r.Target > 0 && bb.touched(r.Target) {
		b.resolveProtectiveLocked(r, p, r.Target, bb.at, domain.ExitTarget)
	}
}

// resolveProtectiveLocked closes against a triggered protective and removes it
// along with its sibling leg, which is what one-cancels-other means.
func (b *Broker) resolveProtectiveLocked(r *resting, p *position, price money.Money, at time.Time, reason domain.ExitReason) {
	delete(b.resting, r.ID)
	qty := r.Quantity
	if held := abs(p.Quantity); qty > held {
		qty = held
	}
	if qty <= 0 {
		return
	}
	exitSide := sideOf(p.Quantity).Opposite()
	b.closeLocked(p, qty, b.fillPrice(p.Key, price, exitSide), at, reason)
}

// fillLocked executes an order against the book and updates the position.
func (b *Broker) fillLocked(order *domain.Order, raw money.Money, at time.Time) error {
	req := order.Request
	price := b.fillPrice(req.Key, raw, req.Side)

	existing := b.positions[req.Key]
	closing := existing != nil && sideOf(existing.Quantity) != req.Side

	if !closing && !b.affordableLocked(price, req.Quantity) {
		order.Status = domain.StatusRejected
		order.Message = "insufficient buying power"
		return fmt.Errorf("%w: insufficient buying power for %d of %s at %s",
			ErrRejected, req.Quantity, req.Key, price)
	}

	order.Status = domain.StatusComplete
	order.FilledQuantity = req.Quantity
	order.AveragePrice = price
	order.UpdatedAt = at

	if closing {
		qty := req.Quantity
		if held := abs(existing.Quantity); qty > held {
			qty = held
		}
		b.closeLocked(existing, qty, price, at, domain.ExitSignal)
		// Any remainder opens a position on the other side.
		if rest := req.Quantity - qty; rest > 0 {
			b.openLocked(req, rest, price, at)
		}
		return nil
	}
	b.openLocked(req, req.Quantity, price, at)
	return nil
}

// affordableLocked reports whether the account can carry a new position.
func (b *Broker) affordableLocked(price money.Money, qty int) bool {
	notional := price.Mul(int64(qty))
	buyingPower := b.equityLocked().MulFraction(b.opts.Leverage) - b.exposureLocked()
	return notional <= buyingPower
}

// openLocked adds to or creates a position.
func (b *Broker) openLocked(req domain.OrderRequest, qty int, price money.Money, at time.Time) {
	signed := qty
	if req.Side == domain.Sell {
		signed = -qty
	}

	p, ok := b.positions[req.Key]
	if !ok {
		b.positions[req.Key] = &position{
			Key: req.Key, Quantity: signed, AveragePrice: price,
			Product: req.Product, OpenedAt: at, Strategy: req.Tag,
		}
		return
	}
	// Averaging into an existing position: the new average is the
	// quantity-weighted mean, which is what the broker would report.
	held := abs(p.Quantity)
	total := held + qty
	p.AveragePrice = money.Money((int64(p.AveragePrice)*int64(held) + int64(price)*int64(qty)) / int64(total))
	p.Quantity += signed
}

// closeLocked closes qty of a position at price and records the round trip.
//
// price is the final fill price: slippage and tick rounding are the caller's
// responsibility, applied exactly once. Adjusting here as well would charge
// every closing fill twice, which is invisible in a single trade and
// compounds into a materially pessimistic backtest over thousands.
func (b *Broker) closeLocked(p *position, qty int, price money.Money, at time.Time, reason domain.ExitReason) {
	entrySide := sideOf(p.Quantity)

	gross := domain.GrossFor(entrySide, qty, p.AveragePrice, price)
	charges := b.chargesFor(p, qty, price, at, entrySide)

	trade := domain.Trade{
		Key:        p.Key,
		Strategy:   p.Strategy,
		Side:       entrySide,
		Quantity:   qty,
		EntryPrice: p.AveragePrice,
		ExitPrice:  price,
		EntryAt:    p.OpenedAt,
		ExitAt:     at,
		GrossPnL:   gross,
		Charges:    charges,
		NetPnL:     gross - charges,
		ExitReason: reason,
		Paper:      true,
	}
	b.trades = append(b.trades, trade)
	b.realized += trade.NetPnL
	b.cash += trade.NetPnL

	if entrySide == domain.Buy {
		p.Quantity -= qty
	} else {
		p.Quantity += qty
	}
	if p.Quantity == 0 {
		delete(b.positions, p.Key)
		// A flat position leaves nothing for its protective legs to
		// close; keeping them would fill against a position that no
		// longer exists.
		for id, r := range b.resting {
			if r.Protective && r.Key == p.Key {
				delete(b.resting, id)
			}
		}
	}
}

// chargesFor prices a round trip, returning zero when no rate table is set.
func (b *Broker) chargesFor(p *position, qty int, exit money.Money, at time.Time, entrySide domain.Side) money.Money {
	if b.opts.Charges == nil {
		return 0
	}
	c, err := costs.Compute(b.opts.Charges, costs.Trade{
		Broker:     b.opts.Broker,
		Segment:    b.opts.Segment,
		Quantity:   qty,
		EntryPrice: p.AveragePrice,
		ExitPrice:  exit,
		EntryAt:    p.OpenedAt,
		ExitAt:     at,
		Buying:     entrySide == domain.Buy,
	})
	if err != nil {
		// A charge that cannot be computed is reported as zero rather
		// than estimated; domain.Trade documents zero as "not computed".
		return 0
	}
	return c.Total
}

// Trades returns the round trips closed so far, oldest first.
func (b *Broker) Trades() []domain.Trade {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]domain.Trade, len(b.trades))
	copy(out, b.trades)
	return out
}

// Cash returns the free balance.
func (b *Broker) Cash() money.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cash
}

// Equity returns cash plus the mark-to-market value of open positions.
func (b *Broker) Equity() money.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.equityLocked()
}

// RealizedPnL returns the net profit of closed trades.
func (b *Broker) RealizedPnL() money.Money {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.realized
}

func sideOf(quantity int) domain.Side {
	if quantity < 0 {
		return domain.Sell
	}
	return domain.Buy
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
