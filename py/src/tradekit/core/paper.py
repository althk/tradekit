"""Order execution, simulated.

This mirrors ``go/core/paper`` module for module. It replaces five separate
simulators, but more importantly it is what makes backtesting through the same
:class:`~tradekit.core.ports.Broker` interface as live trading possible: a
strategy cannot tell which it is running against, so there is no second code
path to keep in step and no class of bug that appears only in production.

That makes this module's fidelity a correctness requirement rather than a
convenience. A simulator that is optimistic about fills produces backtests that
are wrong in the one direction that costs money, so the rules below are
deliberately conservative wherever the real sequence is unknowable:

* A gap through a stop fills at the gap price, not at the stop. A stop is a
  trigger, not a guarantee; a bar that opens below a long's stop fills there.
* When a bar's range contains both the stop and the target, the stop is taken.
  Daily and even 5-minute bars do not say which came first, and assuming the
  target would flatter every result.
* Slippage is applied against the position, never for it.

Those three rules are pinned by ``contracts/testdata/parity.json``, which the Go
suite runs too, so an ambiguous bar cannot resolve one way in a Go backtest and
the other way in a Python one.

The simulator is driven by prices, not by wall-clock time: call
:meth:`Broker.on_bar` for a bar-based backtest or :meth:`Broker.on_tick` for a
tick stream. Both apply the same rules.
"""

from __future__ import annotations

import contextlib
import datetime as dt
import threading
from collections.abc import Callable
from dataclasses import dataclass, field, replace

from . import costs, money
from .domain import (
    Account,
    Candle,
    ExitReason,
    InstrumentKey,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Product,
    Protective,
    Quote,
    Side,
    Trade,
    gross_for,
)
from .money import Money

__all__ = [
    "ORDER_ID_PREFIX",
    "Broker",
    "Options",
    "RejectedError",
    "Slippage",
]

ORDER_ID_PREFIX = "PAPER-"
"""Tags every order id the simulator mints.

It is the last-resort mode marker: an order that reaches persistence without its
paper flag set is still recognisable by its id, so a live row can never be
created from a simulated fill through a path that forgot to propagate the flag.
"""

_UNKNOWN_TIME = dt.datetime.min.replace(tzinfo=dt.UTC)
"""Stamp for an order placed before any price has been seen.

It mirrors Go's zero ``time.Time``: far enough in the past that it sorts before
every real bar, so such an order can never be mistaken for a recent one.
"""


class RejectedError(Exception):
    """An order the simulator refused.

    The reasons are the ones a real venue would refuse for: no such price, not
    enough capital, nothing to close. The rejected order is still readable
    through :meth:`Broker.order_status`, which is where its message lives.
    """


@dataclass(frozen=True, slots=True)
class Slippage:
    """The gap between the price a fill is triggered at and the price it gets."""

    fraction: float = 0.0
    """Fraction of the price, applied against the position: 0.0005 is five basis
    points. A buy fills higher, a sell lower."""

    ticks: int = 0
    """An additional whole-tick adjustment, also against the position. Use it
    when the instrument's spread is better described in ticks than in basis
    points."""

    def apply(self, price: Money, side: Side, tick: Money) -> Money:
        """Move ``price`` against ``side`` by the configured amount."""
        adjusted = int(price)
        if self.fraction:
            delta = int(money.mul_fraction(price, self.fraction))
            adjusted += delta if side is Side.BUY else -delta
        if self.ticks and tick > 0:
            delta = int(money.mul(tick, self.ticks))
            adjusted += delta if side is Side.BUY else -delta
        return Money(max(adjusted, 0))


@dataclass(slots=True)
class Options:
    """How a simulator prices and constrains fills."""

    cash: Money = money.ZERO
    """The opening balance."""

    slippage: Slippage = field(default_factory=Slippage)
    """Applied to every fill."""

    tick_size: Callable[[InstrumentKey], Money] | None = None
    """Returns an instrument's tick, used to round fills to a price the exchange
    could actually print. ``None`` means no rounding."""

    charges: costs.Table | None = None
    """Prices closed round trips. ``None`` means trades close with zero charges,
    which is honest about not knowing them rather than inventing a figure."""

    broker: str = ""
    """Selects the schedule within :attr:`charges`."""

    segment: costs.Segment | None = None
    """Selects the schedule within :attr:`charges`. ``None`` prices nothing, for
    the same reason an absent table does: guessing the segment would price an
    intraday trade as delivery, and the two differ in which components apply at
    all, not merely in their rates."""

    leverage: float = 1.0
    """Multiplies buying power for the affordability check."""


@dataclass(slots=True)
class _Position:
    """An open holding and the bookkeeping needed to close it correctly."""

    key: InstrumentKey
    quantity: int  # signed: negative is short
    average_price: Money
    product: Product
    opened_at: dt.datetime
    strategy: str = ""


@dataclass(slots=True)
class _Resting:
    """An order waiting for a price: a limit entry or a protective leg."""

    id: str
    key: InstrumentKey
    side: Side
    quantity: int

    limit: Money = money.ZERO
    """Fills when the price reaches it or better; zero for a protective."""

    stop: Money = money.ZERO
    target: Money = money.ZERO
    product: Product = Product.CNC
    tag: str = ""

    protective: bool = False
    """Marks a resting order that closes a position rather than opening one."""


@dataclass(frozen=True, slots=True)
class _Bar:
    """The price range a match runs against. A tick is a bar of zero width."""

    open: Money
    high: Money
    low: Money
    close: Money
    at: dt.datetime

    def touched(self, level: Money) -> bool:
        """Whether a price level falls inside the bar's range."""
        return self.low <= level <= self.high


class Broker:
    """The simulator.

    It is safe for concurrent use: a live paper session has a scanner thread
    placing orders while a feed thread delivers ticks.
    """

    def __init__(self, opts: Options | None = None) -> None:
        """Create a simulator with the given opening balance and rules."""
        self._opts = opts if opts is not None else Options()
        if self._opts.leverage < 1:
            self._opts.leverage = 1.0

        self._lock = threading.Lock()
        self._cash: Money = self._opts.cash
        self._realized: Money = money.ZERO
        self._seq = 0
        self._last_price: dict[InstrumentKey, Money] = {}
        self._last_at: dict[InstrumentKey, dt.datetime] = {}
        self._last_bar: dict[InstrumentKey, Candle] = {}
        self._positions: dict[InstrumentKey, _Position] = {}
        self._orders: dict[str, Order] = {}
        self._resting: dict[str, _Resting] = {}
        self._trades: list[Trade] = []

    # --- ports.Broker ---

    def account(self) -> Account:
        """Return the simulated funds view."""
        with self._lock:
            return Account(equity=self._equity_locked(), available=self._cash, used=self._exposure_locked())

    def place_order(self, req: OrderRequest) -> Order:
        """Accept a market or limit order.

        A market order fills immediately at the last seen price; without one the
        order is rejected rather than filled at a guess, because a fill price
        invented by the simulator is the kind of error that makes a backtest look
        better than the strategy is.

        Raises:
            RejectedError: If the order is refused. The order is still recorded,
                so :meth:`order_status` reports why.

        """
        with self._lock:
            if req.quantity <= 0:
                raise RejectedError(f"paper: quantity must be positive, got {req.quantity}")

            order_id = self._next_id("ORD")
            now = self._last_at.get(req.key, _UNKNOWN_TIME)
            order = Order(id=order_id, request=req, status=OrderStatus.OPEN, placed_at=now, updated_at=now)
            self._orders[order_id] = order

            if req.type is OrderType.MARKET:
                last = self._last_price.get(req.key)
                if last is None:
                    order.status = OrderStatus.REJECTED
                    order.message = f"no price seen for {req.key}"
                    raise RejectedError(f"paper: no price seen for {req.key}")
                self._fill_locked(order, last, now)
            elif req.type in (OrderType.LIMIT, OrderType.STOP, OrderType.STOP_LIMIT):
                trigger = req.limit_price if req.type is OrderType.LIMIT else req.trigger_price
                if trigger <= 0:
                    order.status = OrderStatus.REJECTED
                    order.message = "a resting order needs a price"
                    raise RejectedError("paper: a resting order needs a price")
                self._resting[order_id] = _Resting(
                    id=order_id,
                    key=req.key,
                    side=req.side,
                    quantity=req.quantity,
                    limit=trigger,
                    product=req.product,
                    tag=req.tag,
                )
            else:
                order.status = OrderStatus.REJECTED
                order.message = f"unsupported order type {req.type}"
                raise RejectedError(f"paper: unsupported order type {req.type!r}")

            return replace(order)

    def cancel_order(self, order_id: str) -> None:
        """Withdraw a resting order.

        Raises:
            RejectedError: If the order is unknown or already terminal.

        """
        with self._lock:
            order = self._orders.get(order_id)
            if order is None:
                raise RejectedError(f"paper: unknown order {order_id}")
            if order.status.terminal:
                raise RejectedError(f"paper: order {order_id} is already {order.status}")
            self._resting.pop(order_id, None)
            order.status = OrderStatus.CANCELLED
            order.updated_at = self._last_at.get(order.request.key, _UNKNOWN_TIME)

    def order_status(self, order_id: str) -> Order:
        """Return one order's state.

        Raises:
            RejectedError: If the order is unknown.

        """
        with self._lock:
            order = self._orders.get(order_id)
            if order is None:
                raise RejectedError(f"paper: unknown order {order_id}")
            return replace(order)

    def positions(self) -> list[Position]:
        """Return the open positions, ordered by instrument."""
        with self._lock:
            out = [
                Position(
                    key=key,
                    quantity=p.quantity,
                    average_price=p.average_price,
                    product=p.product,
                    unrealized_pnl=gross_for(
                        _side_of(p.quantity),
                        abs(p.quantity),
                        p.average_price,
                        self._last_price.get(key, p.average_price),
                    ),
                )
                for key, p in self._positions.items()
            ]
            out.sort(key=lambda p: str(p.key))
            return out

    def open_orders(self) -> list[Order]:
        """Return orders that have not reached a terminal state."""
        with self._lock:
            out = [replace(o) for o in self._orders.values() if not o.status.terminal]
            out.sort(key=lambda o: o.id)
            return out

    # --- ports.Quoter ---

    def ltp(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Money]:
        """Return the last price seen for each instrument.

        Instruments with no price yet are omitted rather than reported as zero.
        """
        with self._lock:
            return {k: self._last_price[k] for k in keys if k in self._last_price}

    def quote(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Quote]:
        """Return the last price seen, with the most recent completed bar's OHLC.

        A tick-driven simulation has no bar to report, so those fields stay zero
        rather than being synthesised from the tick stream: a strategy reading a
        session high the simulator invented would behave differently here than
        live.
        """
        with self._lock:
            out: dict[InstrumentKey, Quote] = {}
            for k in keys:
                last = self._last_price.get(k)
                if last is None:
                    continue
                q = Quote(key=k, at=self._last_at.get(k, _UNKNOWN_TIME), last=last)
                bar = self._last_bar.get(k)
                if bar is not None:
                    q.open, q.high, q.low, q.close = bar.open, bar.high, bar.low, bar.close
                out[k] = q
            return out

    # --- ports.ProtectiveOrders ---

    def place_protective(self, p: Protective) -> str:
        """Rest a stop, or a stop and target as a one-cancels-other pair.

        Raises:
            RejectedError: If the quantity is not positive, or neither leg is set.

        """
        with self._lock:
            if p.quantity <= 0:
                raise RejectedError("paper: protective quantity must be positive")
            if p.stop <= 0 and p.target <= 0:
                raise RejectedError("paper: a protective order needs a stop or a target")
            protective_id = self._next_id("GTT")
            self._resting[protective_id] = _Resting(
                id=protective_id,
                key=p.key,
                side=p.side,
                quantity=p.quantity,
                stop=p.stop,
                target=p.target,
                product=p.product,
                protective=True,
            )
            return protective_id

    def modify_stop(self, protective_id: str, stop: Money) -> None:
        """Move a resting protective's stop, as a breakeven or trail does.

        Raises:
            RejectedError: If no such protective is resting.

        """
        with self._lock:
            r = self._resting.get(protective_id)
            if r is None or not r.protective:
                raise RejectedError(f"paper: unknown protective {protective_id}")
            r.stop = stop

    def cancel_protective(self, protective_id: str) -> None:
        """Remove a resting protective order.

        Raises:
            RejectedError: If no such protective is resting.

        """
        with self._lock:
            r = self._resting.get(protective_id)
            if r is None or not r.protective:
                raise RejectedError(f"paper: unknown protective {protective_id}")
            del self._resting[protective_id]

    def list_protective(self) -> list[Protective]:
        """Return the resting protective orders, ordered by id."""
        with self._lock:
            out = [
                Protective(
                    id=r.id,
                    key=r.key,
                    side=r.side,
                    quantity=r.quantity,
                    stop=r.stop,
                    target=r.target,
                    product=r.product,
                )
                for r in self._resting.values()
                if r.protective
            ]
            out.sort(key=lambda p: p.id)
            return out

    # --- ports.PositionCloser ---

    def close_position(
        self,
        key: InstrumentKey,
        side: Side,
        price: Money,
        at: dt.datetime,
        reason: ExitReason,
    ) -> bool:
        """Close any open position in ``key`` on ``side`` at ``price``.

        The simulator implements this because its positions live inside its own
        book with the P&L bookkeeping attached: a generic counter order would
        open a second position rather than close the first.

        Returns:
            Whether a position was found and closed.

        """
        with self._lock:
            p = self._positions.get(key)
            if p is None or _side_of(p.quantity) is not side:
                return False
            self._close_locked(p, abs(p.quantity), self._fill_price(key, price, side.opposite), at, reason)
            return True

    # --- ports.TickObserver ---

    def on_tick(self, key: InstrumentKey, price: Money, at: dt.datetime) -> None:
        """Deliver a traded price, filling anything it triggers.

        A simulator must see prices: without them a simulated position has no
        stop at all, and a backtest that silently loses its stops reports
        fictional results.
        """
        with self._lock:
            self._observe_locked(key, price, at)
            self._match_locked(key, _Bar(open=price, high=price, low=price, close=price, at=at))

    # --- driving ---

    def on_bar(self, candle: Candle) -> None:
        """Deliver a completed bar, filling anything its range triggers.

        This is how a bar-based backtest drives the simulator. The bar is
        replayed conservatively: the open first, then the stop, then the target,
        so an ambiguous bar always resolves against the position.
        """
        with self._lock:
            self._observe_locked(candle.key, candle.close, candle.start)
            self._last_bar[candle.key] = candle
            self._match_locked(
                candle.key,
                _Bar(
                    open=candle.open,
                    high=candle.high,
                    low=candle.low,
                    close=candle.close,
                    at=candle.start,
                ),
            )

    # --- inspection ---

    def trades(self) -> list[Trade]:
        """Return the round trips closed so far, oldest first."""
        with self._lock:
            return list(self._trades)

    def cash(self) -> Money:
        """Return the free balance."""
        with self._lock:
            return self._cash

    def equity(self) -> Money:
        """Return cash plus the mark-to-market value of open positions."""
        with self._lock:
            return self._equity_locked()

    def realized_pnl(self) -> Money:
        """Return the net profit of closed trades."""
        with self._lock:
            return self._realized

    # --- internals; every _locked helper assumes the lock is already held ---

    def _next_id(self, kind: str) -> str:
        """Mint a paper order id."""
        self._seq += 1
        return f"{ORDER_ID_PREFIX}{kind}-{self._seq}"

    def _tick_of(self, key: InstrumentKey) -> Money:
        """Return an instrument's tick size, or zero when unknown."""
        if self._opts.tick_size is None:
            return money.ZERO
        return self._opts.tick_size(key)

    def _fill_price(self, key: InstrumentKey, raw: Money, side: Side) -> Money:
        """Apply slippage and round to a printable price.

        Rounding is directional: a buy rounds up and a sell rounds down, so the
        rounding itself can never improve the fill. Rounding to nearest would
        hand the simulation a fraction of a tick of free edge on half of all
        trades.
        """
        tick = self._tick_of(key)
        slipped = self._opts.slippage.apply(raw, side, tick)
        if tick <= 0:
            return slipped
        return money.ceil_to_tick(slipped, tick) if side is Side.BUY else money.floor_to_tick(slipped, tick)

    def _equity_locked(self) -> Money:
        """Cash plus the mark-to-market value of open positions."""
        equity = int(self._cash)
        for key, p in self._positions.items():
            last = self._last_price.get(key, p.average_price)
            equity += int(gross_for(_side_of(p.quantity), abs(p.quantity), p.average_price, last))
        return Money(equity)

    def _exposure_locked(self) -> Money:
        """The notional value of open positions at cost."""
        return Money(sum(int(money.mul(p.average_price, abs(p.quantity))) for p in self._positions.values()))

    def _observe_locked(self, key: InstrumentKey, price: Money, at: dt.datetime) -> None:
        """Record the latest price for an instrument."""
        self._last_price[key] = price
        self._last_at[key] = at

    def _match_locked(self, key: InstrumentKey, bar: _Bar) -> None:
        """Fill every resting order this bar triggers."""
        for r in self._resting_for(key):
            if r.protective:
                self._match_protective_locked(r, bar)
            else:
                self._match_entry_locked(r, bar)

    def _resting_for(self, key: InstrumentKey) -> list[_Resting]:
        """Return this instrument's resting orders in a stable order.

        A bar that triggers several of them must behave identically on every
        run, or a backtest stops being reproducible.
        """
        return sorted((r for r in self._resting.values() if r.key == key), key=lambda r: r.id)

    def _match_entry_locked(self, r: _Resting, bar: _Bar) -> None:
        """Fill a resting entry when the bar reaches its price.

        A bar that opens through the limit fills at the open, not at the limit:
        the price was already better than asked for, and pretending otherwise
        would under-report the entry.
        """
        crossed = False
        trigger = r.limit
        if r.side is Side.BUY:
            if bar.open <= r.limit:
                crossed, trigger = True, bar.open
            elif bar.low <= r.limit:
                crossed = True
        elif bar.open >= r.limit:
            crossed, trigger = True, bar.open
        elif bar.high >= r.limit:
            crossed = True
        if not crossed:
            return

        order = self._orders.get(r.id)
        del self._resting[r.id]
        if order is None:
            return
        # A resting order that has become unaffordable by the time it triggers
        # is rejected, exactly as a market order would be. There is no caller on
        # this path to raise to; the order's status carries the outcome.
        with contextlib.suppress(RejectedError):
            self._fill_locked(order, trigger, bar.at)

    def _match_protective_locked(self, r: _Resting, bar: _Bar) -> None:
        """Resolve a resting stop and target against the bar.

        The order of checks is the conservative rule every implementation this
        replaces arrived at independently: a gap through the stop fills at the
        gap price, and a bar containing both levels resolves as the stop.
        """
        p = self._positions.get(r.key)
        if p is None:
            # The position is already gone; the protective is stale.
            del self._resting[r.id]
            return
        long = p.quantity > 0

        # A gap straight through the stop fills at the open. A stop is a
        # trigger, not a guarantee, and this is where a naive simulator quietly
        # awards a fill that never existed.
        if r.stop > 0 and ((long and bar.open <= r.stop) or (not long and bar.open >= r.stop)):
            self._resolve_protective_locked(r, p, bar.open, bar.at, ExitReason.STOP)
            return
        if r.target > 0 and ((long and bar.open >= r.target) or (not long and bar.open <= r.target)):
            self._resolve_protective_locked(r, p, bar.open, bar.at, ExitReason.TARGET)
            return

        # Within the bar, the stop is checked first: a daily or 5-minute bar
        # does not say which level was reached first, and assuming the target
        # would flatter every ambiguous trade.
        if r.stop > 0 and bar.touched(r.stop):
            self._resolve_protective_locked(r, p, r.stop, bar.at, ExitReason.STOP)
            return
        if r.target > 0 and bar.touched(r.target):
            self._resolve_protective_locked(r, p, r.target, bar.at, ExitReason.TARGET)

    def _resolve_protective_locked(
        self,
        r: _Resting,
        p: _Position,
        price: Money,
        at: dt.datetime,
        reason: ExitReason,
    ) -> None:
        """Close against a triggered protective and remove it.

        The sibling leg goes when the position does, in :meth:`_close_locked`,
        which is what one-cancels-other means.
        """
        del self._resting[r.id]
        qty = min(r.quantity, abs(p.quantity))
        if qty <= 0:
            return
        exit_side = _side_of(p.quantity).opposite
        self._close_locked(p, qty, self._fill_price(p.key, price, exit_side), at, reason)

    def _fill_locked(self, order: Order, raw: Money, at: dt.datetime) -> None:
        """Execute an order against the book and update the position.

        Raises:
            RejectedError: If the account cannot carry the new position.

        """
        req = order.request
        price = self._fill_price(req.key, raw, req.side)

        existing = self._positions.get(req.key)
        closing = existing is not None and _side_of(existing.quantity) is not req.side

        if not closing and not self._affordable_locked(price, req.quantity):
            order.status = OrderStatus.REJECTED
            order.message = "insufficient buying power"
            raise RejectedError(
                f"paper: insufficient buying power for {req.quantity} of {req.key} at {money.format(price)}"
            )

        order.status = OrderStatus.COMPLETE
        order.filled_quantity = req.quantity
        order.average_price = price
        order.updated_at = at

        if closing and existing is not None:
            qty = min(req.quantity, abs(existing.quantity))
            self._close_locked(existing, qty, price, at, ExitReason.SIGNAL)
            # Any remainder opens a position on the other side.
            rest = req.quantity - qty
            if rest > 0:
                self._open_locked(req, rest, price, at)
            return
        self._open_locked(req, req.quantity, price, at)

    def _affordable_locked(self, price: Money, qty: int) -> bool:
        """Whether the account can carry a new position."""
        notional = int(money.mul(price, qty))
        buying_power = int(money.mul_fraction(self._equity_locked(), self._opts.leverage))
        return notional <= buying_power - int(self._exposure_locked())

    def _open_locked(self, req: OrderRequest, qty: int, price: Money, at: dt.datetime) -> None:
        """Add to or create a position."""
        signed = qty if req.side is Side.BUY else -qty

        p = self._positions.get(req.key)
        if p is None:
            self._positions[req.key] = _Position(
                key=req.key,
                quantity=signed,
                average_price=price,
                product=req.product,
                opened_at=at,
                strategy=req.tag,
            )
            return
        # Averaging into an existing position: the new average is the
        # quantity-weighted mean, which is what the broker would report. Prices
        # are non-negative, so flooring here matches Go's truncating division.
        held = abs(p.quantity)
        total = held + qty
        p.average_price = Money((int(p.average_price) * held + int(price) * qty) // total)
        p.quantity += signed

    def _close_locked(
        self,
        p: _Position,
        qty: int,
        price: Money,
        at: dt.datetime,
        reason: ExitReason,
    ) -> None:
        """Close ``qty`` of a position at ``price`` and record the round trip.

        ``price`` is the final fill price: slippage and tick rounding are the
        caller's responsibility, applied exactly once. Adjusting here as well
        would charge every closing fill twice, which is invisible in a single
        trade and compounds into a materially pessimistic backtest over
        thousands.
        """
        entry_side = _side_of(p.quantity)
        gross = gross_for(entry_side, qty, p.average_price, price)
        charges = self._charges_for(p, qty, price, at, entry_side)

        trade = Trade(
            key=p.key,
            strategy=p.strategy,
            side=entry_side,
            quantity=qty,
            entry_price=p.average_price,
            exit_price=price,
            entry_at=p.opened_at,
            exit_at=at,
            gross_pnl=gross,
            charges=charges,
            net_pnl=Money(int(gross) - int(charges)),
            exit_reason=reason,
            paper=True,
        )
        self._trades.append(trade)
        self._realized = Money(int(self._realized) + int(trade.net_pnl))
        self._cash = Money(int(self._cash) + int(trade.net_pnl))

        p.quantity += -qty if entry_side is Side.BUY else qty
        if p.quantity == 0:
            del self._positions[p.key]
            # A flat position leaves nothing for its protective legs to close;
            # keeping them would fill against a position that no longer exists.
            for rid in [rid for rid, r in self._resting.items() if r.protective and r.key == p.key]:
                del self._resting[rid]

    def _charges_for(
        self,
        p: _Position,
        qty: int,
        exit_price: Money,
        at: dt.datetime,
        entry_side: Side,
    ) -> Money:
        """Price a round trip, returning zero when no rate table is set."""
        table, segment = self._opts.charges, self._opts.segment
        if table is None or segment is None:
            return money.ZERO
        try:
            return costs.compute(
                table,
                costs.Trade(
                    broker=self._opts.broker,
                    segment=segment,
                    quantity=qty,
                    entry_price=p.average_price,
                    exit_price=exit_price,
                    entry_at=p.opened_at.date(),
                    exit_at=at.date(),
                    buying=entry_side is Side.BUY,
                ),
            ).total
        except ValueError:
            # A charge that cannot be computed is reported as zero rather than
            # estimated; domain.Trade documents zero as "not computed".
            return money.ZERO


def _side_of(quantity: int) -> Side:
    """The side a signed position quantity was opened on."""
    return Side.SELL if quantity < 0 else Side.BUY
