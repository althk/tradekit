"""Types that cross module and language boundaries.

Every type here is defined by ``contracts/domain.md`` and implemented
identically by ``go/core/domain``. Renaming a field in one language only is a
defect, not a refactor: the two libraries read each other's rows out of the same
SQLite file.
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from enum import StrEnum

from . import money
from .money import Money

__all__ = [
    "Account",
    "Candle",
    "ExitReason",
    "Fill",
    "Instrument",
    "InstrumentKey",
    "MarginLeg",
    "Order",
    "OrderRequest",
    "OrderStatus",
    "OrderType",
    "Position",
    "Product",
    "Protective",
    "Quote",
    "Side",
    "Signal",
    "SignalKind",
    "Tick",
    "TimeInForce",
    "Timeframe",
    "Trade",
    "gross_for",
]


class Side(StrEnum):
    """Direction of an order or fill.

    A short position is expressed by a sell entry, not by a negative quantity.
    """

    BUY = "buy"
    SELL = "sell"

    @property
    def opposite(self) -> Side:
        """The side that closes a position opened with this one."""
        return Side.SELL if self is Side.BUY else Side.BUY


class OrderType(StrEnum):
    """How an order is priced and triggered."""

    MARKET = "market"
    LIMIT = "limit"
    STOP = "stop"
    STOP_LIMIT = "stop_limit"


class Product(StrEnum):
    """Settlement and margin bucket the position sits in."""

    CNC = "cnc"
    MIS = "mis"
    NRML = "nrml"
    MARGIN = "margin"


class TimeInForce(StrEnum):
    """How long an unfilled order stays live."""

    DAY = "day"
    IOC = "ioc"
    GTT = "gtt"


class OrderStatus(StrEnum):
    """Lifecycle state of an order, normalised across brokers."""

    PENDING = "pending"
    OPEN = "open"
    COMPLETE = "complete"
    CANCELLED = "cancelled"
    REJECTED = "rejected"
    TRIGGERED = "triggered"

    @property
    def terminal(self) -> bool:
        """Whether no further state change is expected.

        This is what tells a reconciler to stop polling the order.
        """
        return self in (OrderStatus.COMPLETE, OrderStatus.CANCELLED, OrderStatus.REJECTED)


class SignalKind(StrEnum):
    """What a strategy is asking for."""

    LONG = "long"
    SHORT = "short"
    EXIT_LONG = "exit_long"
    EXIT_SHORT = "exit_short"


class ExitReason(StrEnum):
    """Why a position was closed.

    The set is deliberately closed: every backtest and every live journal must
    classify an exit into the same categories, or the statistics they produce
    cannot be compared with each other.
    """

    STOP = "stop"
    TARGET = "target"
    TRAIL = "trail"
    TIME_STOP = "time_stop"
    EOD = "eod"
    SIGNAL = "signal"
    KILL_SWITCH = "kill_switch"
    MANUAL = "manual"


class Timeframe(StrEnum):
    """A bar size."""

    M1 = "1m"
    M3 = "3m"
    M5 = "5m"
    M15 = "15m"
    M30 = "30m"
    M60 = "60m"
    D1 = "1d"
    W1 = "1w"

    @property
    def duration(self) -> dt.timedelta:
        """Wall-clock span of one bar.

        Daily and weekly bars return zero: their boundaries come from the
        exchange calendar rather than from arithmetic, so a caller must ask the
        calendar rather than add a duration.
        """
        return _TIMEFRAME_MINUTES.get(self, dt.timedelta(0))

    @property
    def intraday(self) -> bool:
        """Whether the bar is smaller than one session."""
        return self.duration > dt.timedelta(0)


_TIMEFRAME_MINUTES: dict[Timeframe, dt.timedelta] = {
    Timeframe.M1: dt.timedelta(minutes=1),
    Timeframe.M3: dt.timedelta(minutes=3),
    Timeframe.M5: dt.timedelta(minutes=5),
    Timeframe.M15: dt.timedelta(minutes=15),
    Timeframe.M30: dt.timedelta(minutes=30),
    Timeframe.M60: dt.timedelta(minutes=60),
}


@dataclass(frozen=True, slots=True)
class InstrumentKey:
    """Venue-neutral identity of a tradable instrument.

    A broker adapter maps it to and from that broker's own encoding.
    """

    exchange: str
    symbol: str

    def __str__(self) -> str:
        """Render as ``EXCHANGE:SYMBOL`` for logs and in-memory keys."""
        return f"{self.exchange}:{self.symbol}"


@dataclass(slots=True)
class Instrument:
    """A tradable contract and the reference data needed to size and price it."""

    key: InstrumentKey
    name: str = ""
    isin: str = ""
    segment: str = "equity"
    lot_size: int = 1
    tick_size: Money = money.ZERO
    expiry: dt.date | None = None
    strike: Money = money.ZERO
    option_type: str = ""
    active: bool = True


@dataclass(slots=True)
class Candle:
    """One completed OHLCV bar.

    ``start`` is the bar's opening timestamp and is always timezone-aware.
    """

    key: InstrumentKey
    timeframe: Timeframe
    start: dt.datetime
    open: Money
    high: Money
    low: Money
    close: Money
    volume: int = 0
    open_interest: int = 0


@dataclass(slots=True)
class Tick:
    """A single traded print."""

    key: InstrumentKey
    at: dt.datetime
    price: Money
    volume: int = 0


@dataclass(slots=True)
class Quote:
    """The still-forming session's state for an instrument."""

    key: InstrumentKey
    at: dt.datetime
    last: Money
    open: Money = money.ZERO
    high: Money = money.ZERO
    low: Money = money.ZERO
    close: Money = money.ZERO
    bid: Money = money.ZERO
    ask: Money = money.ZERO


@dataclass(slots=True)
class Account:
    """The funds view a risk check needs."""

    equity: Money
    available: Money = money.ZERO
    used: Money = money.ZERO


@dataclass(slots=True)
class Position:
    """An open holding.

    ``quantity`` is signed -- negative is short. It is the only place in the
    domain where a signed quantity appears.
    """

    key: InstrumentKey
    quantity: int
    average_price: Money
    product: Product = Product.CNC
    realized_pnl: Money = money.ZERO
    unrealized_pnl: Money = money.ZERO


@dataclass(slots=True)
class OrderRequest:
    """An instruction to the broker.

    ``quantity`` is always positive; direction is carried by ``side``.
    """

    key: InstrumentKey
    side: Side
    quantity: int
    type: OrderType = OrderType.MARKET
    product: Product = Product.CNC
    limit_price: Money = money.ZERO
    trigger_price: Money = money.ZERO
    time_in_force: TimeInForce = TimeInForce.DAY
    tag: str = ""


@dataclass(slots=True)
class Order:
    """A request plus the broker's view of what happened to it."""

    id: str
    request: OrderRequest
    status: OrderStatus
    filled_quantity: int = 0
    average_price: Money = money.ZERO
    placed_at: dt.datetime | None = None
    updated_at: dt.datetime | None = None
    message: str = ""
    # Id of the resting stop covering this order's fill, or "" when none has
    # been placed: what a reconciler reads to decide whether a filled entry
    # still needs protecting, and what a dashboard shows.
    protective_id: str = ""


@dataclass(slots=True)
class Fill:
    """One execution against an order."""

    order_id: str
    key: InstrumentKey
    side: Side
    quantity: int
    price: Money
    at: dt.datetime


@dataclass(slots=True)
class Protective:
    """A resting stop, or a stop and target as a one-cancels-other pair.

    This is the single representation for Kite's GTT, Upstox's GTT and Alpaca's
    bracket legs.
    """

    key: InstrumentKey
    side: Side
    quantity: int
    stop: Money
    target: Money = money.ZERO
    product: Product = Product.CNC
    id: str = ""
    # Limit of the order the stop leg fires, for venues whose triggers place
    # limit orders; 0 means at ``stop``. A long's protective sell limit sits a
    # little below its trigger so the exit still fills through a fast move.
    stop_limit: Money = money.ZERO

    @property
    def oco(self) -> bool:
        """Whether both legs are set, which decides the trigger type used."""
        return self.stop > 0 and self.target > 0


@dataclass(slots=True)
class MarginLeg:
    """One leg of a basket for a margin estimate."""

    key: InstrumentKey
    side: Side
    quantity: int
    product: Product = Product.NRML
    price: Money = money.ZERO


@dataclass(slots=True)
class Signal:
    """A strategy's request to enter or exit."""

    key: InstrumentKey
    kind: SignalKind
    at: dt.datetime
    price: Money = money.ZERO
    stop: Money = money.ZERO
    target: Money = money.ZERO
    strategy: str = ""
    metadata: dict[str, str] = field(default_factory=dict)

    @property
    def side(self) -> Side:
        """The order side that opens the position this signal describes."""
        return Side.BUY if self.kind in (SignalKind.LONG, SignalKind.EXIT_SHORT) else Side.SELL


@dataclass(slots=True)
class Trade:
    """A closed round trip.

    ``net_pnl`` is always ``gross_pnl - charges``. A trade whose charges were
    never computed carries ``charges == 0`` rather than an estimate.
    """

    key: InstrumentKey
    side: Side
    quantity: int
    entry_price: Money
    exit_price: Money
    entry_at: dt.datetime
    exit_at: dt.datetime
    gross_pnl: Money = money.ZERO
    charges: Money = money.ZERO
    net_pnl: Money = money.ZERO
    exit_reason: ExitReason = ExitReason.SIGNAL
    strategy: str = ""
    paper: bool = False


def gross_for(side: Side, quantity: int, entry: Money, exit_: Money) -> Money:
    """Compute the gross profit of a round trip.

    This is the one place the long/short sign convention is written down.
    """
    per = int(exit_) - int(entry)
    if side is Side.SELL:
        per = -per
    return Money(per * quantity)
