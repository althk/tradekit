"""Protocols separating strategy code from any particular broker, feed or clock.

The design rule mirrors ``go/core/ports``: :class:`Broker` is small and every
other capability is optional, checked with :func:`isinstance` against a
runtime-checkable protocol. The five broker interfaces tradekit replaces ranged
from 8 to 14 methods with almost no overlap beyond placing an order, and merging
them would have forced every adapter to stub methods its venue cannot perform.

A consumer therefore asks for what it needs::

    if isinstance(brk, Quoter):
        prices = brk.ltp(keys)

so a strategy needing a capability the configured broker lacks fails at wiring
time with a clear message, rather than at 09:15 with an empty result.
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from typing import Protocol, runtime_checkable

from .domain import (
    Account,
    Candle,
    ExitReason,
    Instrument,
    InstrumentKey,
    MarginLeg,
    Order,
    OrderRequest,
    Position,
    Protective,
    Quote,
    Side,
    Timeframe,
)
from .money import Money

__all__ = [
    "Broker",
    "Clock",
    "GateVerdict",
    "HistoryProvider",
    "HolidaySource",
    "InstrumentSource",
    "MarginEstimator",
    "NewsGate",
    "Notifier",
    "PositionCloser",
    "ProtectiveOrders",
    "Quoter",
    "Streamer",
    "SystemClock",
    "TickObserver",
    "TokenState",
]


@runtime_checkable
class Broker(Protocol):
    """The minimum every execution venue must provide."""

    def account(self) -> Account: ...
    def place_order(self, req: OrderRequest) -> Order: ...
    def cancel_order(self, order_id: str) -> None: ...
    def order_status(self, order_id: str) -> Order: ...
    def positions(self) -> list[Position]: ...
    def open_orders(self) -> list[Order]: ...


@runtime_checkable
class Quoter(Protocol):
    """Current prices.

    Instruments the venue does not know are omitted from the result rather than
    raising, so one bad symbol cannot fail a 500-symbol scan.
    """

    def ltp(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Money]: ...
    def quote(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Quote]: ...


@runtime_checkable
class HistoryProvider(Protocol):
    """Completed bars, in ascending time order over ``[start, end]``.

    Implementations handle the venue's per-request span limit internally: a
    caller asking for ten years must not have to know about chunking.
    """

    def candles(
        self,
        key: InstrumentKey,
        timeframe: Timeframe,
        start: dt.datetime,
        end: dt.datetime,
    ) -> list[Candle]: ...


@runtime_checkable
class ProtectiveOrders(Protocol):
    """Resting stops, singly or as one-cancels-other pairs."""

    def place_protective(self, p: Protective) -> str: ...
    def modify_stop(self, protective_id: str, stop: Money) -> None: ...
    def cancel_protective(self, protective_id: str) -> None: ...
    def list_protective(self) -> list[Protective]: ...


@runtime_checkable
class MarginEstimator(Protocol):
    """What a basket would cost in margin before it is placed."""

    def basket_margin(self, legs: list[MarginLeg]) -> Money: ...


@runtime_checkable
class Streamer(Protocol):
    """Live updates, delivered to a handler on a background thread."""

    def subscribe_ticks(self, keys: list[InstrumentKey], handle) -> None: ...
    def subscribe_orders(self, handle) -> None: ...


@runtime_checkable
class PositionCloser(Protocol):
    """Closing one position in a single call, at a known price.

    A simulator must implement it: its positions live inside its own book with
    the P&L bookkeeping attached, and a generic counter order would open a
    second position rather than close the first.
    """

    def close_position(
        self,
        key: InstrumentKey,
        side: Side,
        price: Money,
        at: dt.datetime,
        reason: ExitReason,
    ) -> bool: ...


@runtime_checkable
class TickObserver(Protocol):
    """A broker that must fill its own resting stops and so needs traded prices.

    A live adapter does not implement it: its stops rest at the exchange. A
    simulator does, because nothing else will ever trigger them, and a backtest
    that silently loses its stops reports fictional results.
    """

    def on_tick(self, key: InstrumentKey, price: Money, at: dt.datetime) -> None: ...


@runtime_checkable
class InstrumentSource(Protocol):
    """The instruments a venue trades, for universe and reference-data sync."""

    def instruments(self, exchange: str) -> list[Instrument]: ...


@runtime_checkable
class TokenState(Protocol):
    """Whether the adapter's credentials are usable right now.

    Indian brokers issue access tokens that expire daily, and a scheduler needs
    to know before the open rather than on the first rejected order.
    """

    def token_fresh(self) -> bool: ...


@runtime_checkable
class Clock(Protocol):
    """The source of "now".

    Live code uses the system clock; a backtest supplies simulated time, which
    is what lets the same strategy run in both without knowing which it is in.
    """

    def now(self) -> dt.datetime: ...


class SystemClock:
    """The production :class:`Clock`."""

    def now(self) -> dt.datetime:
        """Return the current time, timezone-aware in UTC."""
        return dt.datetime.now(dt.UTC)


@runtime_checkable
class HolidaySource(Protocol):
    """Days an exchange is closed.

    The returned mapping is keyed by ``"YYYY-MM-DD"`` in the exchange's own
    timezone. The key is a string rather than a date object so that it matches
    the Go implementation, the SQLite column and a JSON cache without
    conversion.

    Implementations are expected to cache: a backtest runs offline and must not
    need the network.
    """

    def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]: ...


@runtime_checkable
class Notifier(Protocol):
    """Operator-facing messages.

    Failures are reported but must never abort trading: an unsent alert is not a
    reason to skip a stop loss.
    """

    def notify(self, subject: str, body: str) -> None: ...


@dataclass(frozen=True, slots=True)
class GateVerdict:
    """Outcome of a :class:`NewsGate` consultation.

    ``reason`` is always populated: it is journalled with the signal so a
    blocked or allowed entry can be audited afterwards.
    """

    allow: bool
    reason: str


@runtime_checkable
class NewsGate(Protocol):
    """Whether a price move is explained by genuinely bad information.

    ``as_of`` is the market timestamp of the decision, not wall-clock time, so
    an implementation can bound "recent news" against the bar being evaluated.
    """

    def assess(self, key: InstrumentKey, as_of: dt.datetime) -> GateVerdict: ...
