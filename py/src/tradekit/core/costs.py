"""Transaction charges on a trade.

Mirrors ``go/core/costs``. Four implementations of this existed across the
projects tradekit replaces, and two disagreed on the DP charge -- correctly,
because one priced Zerodha and the other Upstox, and neither could express
which. That is the design constraint here: a rate is never a constant. It is
looked up by broker, segment and trade date, so a backtest over 2024 prices its
trades with the schedule in force in 2024 rather than with today's.

This module ships no real rate card. Statutory rates change with each budget and
broker plans more often; a hardcoded table would be silently wrong. Rates come
from a :class:`Table` the caller populates -- from the ``charge_rates`` SQLite
table, from config, or from a literal in a test.
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass
from enum import StrEnum

from . import money
from .money import Money

__all__ = [
    "Charges",
    "Kind",
    "Rate",
    "Segment",
    "Table",
    "Trade",
    "compute",
    "india_equity_delivery_template",
]


class Segment(StrEnum):
    """The charge schedule a trade falls under.

    Schedules differ in which components apply at all, not merely in their
    rates: delivery pays a DP charge and stamp duty on the buy leg, intraday
    pays neither.
    """

    EQUITY_DELIVERY = "equity_delivery"
    EQUITY_INTRADAY = "equity_intraday"
    FUTURES = "futures"
    OPTIONS = "options"


class Kind(StrEnum):
    """One component of a charge schedule."""

    BROKERAGE = "brokerage"
    STT_BUY = "stt_buy"
    STT_SELL = "stt_sell"
    EXCHANGE = "exchange"
    SEBI = "sebi"
    STAMP = "stamp"
    DP = "dp"
    GST = "gst"


@dataclass(frozen=True, slots=True)
class Rate:
    """One component's value, effective from a date."""

    value: float
    """Fraction of turnover (0.001 is 0.1%), or a flat minor-unit amount."""
    effective_from: dt.date
    flat: bool = False
    """Marks ``value`` as an absolute amount rather than a fraction."""
    cap: Money = money.ZERO
    """Per-order ceiling in minor units; zero means uncapped."""


class Table:
    """Effective-dated rates, keyed by broker, segment and kind."""

    def __init__(self) -> None:
        """Create an empty table."""
        self._rates: dict[tuple[str, Segment, Kind], list[Rate]] = {}

    def set(self, broker: str, segment: Segment, kind: Kind, rate: Rate) -> None:
        """Record a rate.

        Repeated calls for the same broker, segment and kind build the
        effective-dated history; the order of calls does not matter.
        """
        key = (broker, segment, kind)
        rates = [*self._rates.get(key, []), rate]
        rates.sort(key=lambda r: r.effective_from)
        self._rates[key] = rates

    def lookup(self, broker: str, segment: Segment, kind: Kind, on: dt.date) -> Rate | None:
        """The rate in force on ``on``, or ``None`` if the component does not apply.

        A missing component is not an error: it is exactly how intraday differs
        from delivery. ``None`` distinguishes "does not apply" from "zero rate".
        """
        found: Rate | None = None
        for r in self._rates.get((broker, segment, kind), []):
            if r.effective_from > on:
                break
            found = r
        return found


@dataclass(slots=True)
class Trade:
    """One round trip to be priced."""

    broker: str
    segment: Segment
    quantity: int
    entry_price: Money
    exit_price: Money
    entry_at: dt.date
    exit_at: dt.date
    buying: bool = True
    """Whether the entry leg was a buy.

    A short sale pays stamp duty on its buy leg too, but that leg is the exit.
    """


@dataclass(frozen=True, slots=True)
class Charges:
    """Itemised cost of a round trip, every field in minor units."""

    brokerage: Money = money.ZERO
    stt: Money = money.ZERO
    exchange: Money = money.ZERO
    sebi: Money = money.ZERO
    stamp: Money = money.ZERO
    dp: Money = money.ZERO
    gst: Money = money.ZERO
    total: Money = money.ZERO


def compute(table: Table, trade: Trade) -> Charges:
    """Price a round trip against the table.

    The arithmetic is explicit about which leg each component applies to: STT
    and stamp duty are leg-specific, exchange and SEBI fees apply to both legs'
    turnover, DP is charged once per sell of a delivery holding, and GST applies
    only to the service charges -- brokerage, exchange, SEBI and DP -- never to
    STT or stamp duty, which are taxes in their own right.

    Every component is rounded to the minor unit before being summed, so the
    total matches a contract note rather than differing by a paisa.

    Raises:
        ValueError: If the quantity is not positive.

    """
    if trade.quantity <= 0:
        raise ValueError(f"costs: quantity must be positive, got {trade.quantity}")

    entry_turnover = money.mul(trade.entry_price, trade.quantity)
    exit_turnover = money.mul(trade.exit_price, trade.quantity)

    if trade.buying:
        buy_turnover, buy_at = entry_turnover, trade.entry_at
        sell_turnover, sell_at = exit_turnover, trade.exit_at
    else:
        buy_turnover, buy_at = exit_turnover, trade.exit_at
        sell_turnover, sell_at = entry_turnover, trade.entry_at

    def apply(kind: Kind, turnover: Money, on: dt.date) -> Money:
        rate = table.lookup(trade.broker, trade.segment, kind, on)
        if rate is None:
            return money.ZERO
        value = Money(int(rate.value)) if rate.flat else money.mul_fraction(turnover, rate.value)
        if rate.cap > 0 and value > rate.cap:
            value = rate.cap
        return value

    brokerage = Money(
        apply(Kind.BROKERAGE, entry_turnover, trade.entry_at) + apply(Kind.BROKERAGE, exit_turnover, trade.exit_at)
    )
    stt = Money(apply(Kind.STT_BUY, buy_turnover, buy_at) + apply(Kind.STT_SELL, sell_turnover, sell_at))
    exchange = Money(
        apply(Kind.EXCHANGE, entry_turnover, trade.entry_at) + apply(Kind.EXCHANGE, exit_turnover, trade.exit_at)
    )
    sebi = Money(apply(Kind.SEBI, entry_turnover, trade.entry_at) + apply(Kind.SEBI, exit_turnover, trade.exit_at))
    stamp = apply(Kind.STAMP, buy_turnover, buy_at)
    # DP is flat per sell of a delivery holding; turnover is irrelevant to its
    # amount -- but a sell leg with no turnover is a position still open, and
    # an open position has not paid it yet.
    dp = apply(Kind.DP, money.ZERO, sell_at) if sell_turnover > 0 else money.ZERO

    gst = money.ZERO
    gst_rate = table.lookup(trade.broker, trade.segment, Kind.GST, trade.exit_at)
    if gst_rate is not None:
        gst = money.mul_fraction(Money(brokerage + exchange + sebi + dp), gst_rate.value)

    return Charges(
        brokerage=brokerage,
        stt=stt,
        exchange=exchange,
        sebi=sebi,
        stamp=stamp,
        dp=dp,
        gst=gst,
        total=Money(brokerage + stt + exchange + sebi + stamp + dp + gst),
    )


def india_equity_delivery_template(broker: str, since: dt.date) -> Table:
    """A table shaped like an Indian equity delivery schedule, all rates zero.

    It is a template, not a rate card. Fill it from the ``charge_rates`` table or
    from config before pricing anything: shipping real rates here would mean
    shipping rates that go stale without anyone noticing, which is the failure
    this module exists to prevent.
    """
    t = Table()
    for kind in (Kind.BROKERAGE, Kind.STT_BUY, Kind.STT_SELL, Kind.EXCHANGE, Kind.SEBI, Kind.STAMP, Kind.GST):
        t.set(broker, Segment.EQUITY_DELIVERY, kind, Rate(value=0.0, effective_from=since))
    t.set(broker, Segment.EQUITY_DELIVERY, Kind.DP, Rate(value=0.0, effective_from=since, flat=True))
    return t
