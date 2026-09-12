"""Position sizing and the gates that decide whether a signal may be acted on.

Mirrors ``go/core/risk``. It replaces six near-identical risk managers, two of
which were a verbatim fork of each other. The differences between them were not
disagreements about the formula -- every one sized from the stop distance -- but
about which cap applied: one by leverage, one by a percentage of equity, one by
a notional ceiling, one not at all. Here the cap is a parameter, so those four
are one function.

Gates are composed rather than hardcoded into a single ``evaluate`` method, so a
project adds a rule without editing shared code and the order of evaluation is
visible at the call site.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from typing import Protocol, runtime_checkable
from zoneinfo import ZoneInfo

from . import money
from .domain import InstrumentKey, Side, Signal
from .money import Money

__all__ = [
    "Chain",
    "DailyLossLimit",
    "DailyState",
    "Gate",
    "KillSwitch",
    "MaxConcurrentPositions",
    "MaxOpenNotional",
    "MaxTradesPerDay",
    "MaxTradesPerSymbol",
    "RiskBlockedError",
    "SizeParams",
    "SizeResult",
    "Tracker",
    "TradingWindow",
    "breakeven",
    "size",
    "trail",
]


@dataclass(slots=True)
class SizeParams:
    """Everything needed to size one position."""

    capital: Money
    risk_fraction: float
    """Share of capital lost if the stop is hit; 0.01 is one percent."""
    entry: Money
    stop: Money
    lot_size: int = 1
    """Rounds the result down to whole lots. Use 0 or 1 for cash equity."""
    max_notional: Money = money.ZERO
    """Caps one position's value. Zero means only buying power applies."""
    leverage: float = 1.0
    """Multiplies buying power. Zero or 1 means cash."""


@dataclass(frozen=True, slots=True)
class SizeResult:
    """A quantity and the reason it came out as it did.

    The reason is always populated, including on success, because "why is this
    position smaller than I expected" is the question a sizer is asked most.
    """

    quantity: int
    reason: str


def size(p: SizeParams) -> SizeResult:
    """Compute a position size from the stop distance.

    The result is the smaller of the volatility-derived quantity and the
    notional cap, floored to a whole lot. A budget too small for one lot returns
    zero rather than a fractional contract: a fraction of a lot cannot be traded,
    and rounding up would silently exceed the risk budget.
    """
    if p.capital <= 0:
        return SizeResult(0, "capital is not positive")
    if p.risk_fraction <= 0:
        return SizeResult(0, "risk fraction is not positive")
    if p.entry <= 0:
        return SizeResult(0, "entry price is not positive")

    per_unit_risk = abs(int(p.entry) - int(p.stop))
    if per_unit_risk == 0:
        return SizeResult(0, "entry equals stop, so risk per unit is zero")

    budget = money.mul_fraction(p.capital, p.risk_fraction)
    qty = int(budget) // per_unit_risk
    reason = "sized by risk budget"

    cap, cap_reason = _notional_cap(p)
    by_notional = int(cap) // int(p.entry)
    if by_notional < qty:
        qty, reason = by_notional, cap_reason

    if p.lot_size > 1:
        lots = qty // p.lot_size
        if lots < 1:
            return SizeResult(0, f"risk budget affords less than one lot of {p.lot_size}")
        qty = lots * p.lot_size
        reason += ", floored to whole lots"

    if qty <= 0:
        return SizeResult(0, "risk budget affords less than one unit")
    return SizeResult(qty, reason)


def _notional_cap(p: SizeParams) -> tuple[Money, str]:
    """The position value ceiling and the reason for it.

    Buying power is always a ceiling, even with no ``max_notional`` set: a cash
    account cannot take a position larger than its capital, and a risk budget
    alone does not know that. A wide stop on a small account produces a large
    share count, and without this the sizer would return a position worth ten
    times the equity behind it.
    """
    leverage = max(p.leverage, 1.0)
    cap, reason = money.mul_fraction(p.capital, leverage), "capped by buying power"
    if p.max_notional > 0 and p.max_notional < cap:
        cap, reason = p.max_notional, "capped by notional limit"
    return cap, reason


@dataclass(slots=True)
class DailyState:
    """Per-day counters the gates read and the executor updates.

    Persisted through the store's key-value repository between restarts: a
    process that restarts at noon must not forget it has already lost its daily
    budget.
    """

    date: str
    """Trading date as ``"YYYY-MM-DD"`` in the exchange's timezone."""
    trades_today: int = 0
    trades_per_key: dict[str, int] = field(default_factory=dict)
    realized_pnl: Money = money.ZERO
    open_positions: int = 0
    notional_open: Money = money.ZERO
    kill_switch: str = ""

    def fresh_for(self, date: str) -> DailyState:
        """Return this state if it belongs to ``date``, else empty counters.

        Loading yesterday's counters into today is the failure this prevents,
        and every implementation tradekit replaces had to write it out.
        """
        if self.date != date:
            return DailyState(date=date)
        return self


class Tracker:
    """Guards a :class:`DailyState` for concurrent use.

    A scanner thread and an order-update thread both touch these counters.
    """

    def __init__(self, state: DailyState) -> None:
        """Wrap a state for concurrent access."""
        self._lock = threading.Lock()
        self._state = state

    def snapshot(self) -> DailyState:
        """A copy safe to read without holding the lock."""
        with self._lock:
            s = self._state
            return DailyState(
                date=s.date,
                trades_today=s.trades_today,
                trades_per_key=dict(s.trades_per_key),
                realized_pnl=s.realized_pnl,
                open_positions=s.open_positions,
                notional_open=s.notional_open,
                kill_switch=s.kill_switch,
            )

    def record_trade(self, key: InstrumentKey) -> None:
        """Increment the counters after an entry is placed."""
        with self._lock:
            self._state.trades_today += 1
            k = str(key)
            self._state.trades_per_key[k] = self._state.trades_per_key.get(k, 0) + 1

    def set_realized_pnl(self, value: Money) -> None:
        """Replace the day's realised profit with the broker's figure.

        A replace rather than an add: the daily-loss kill switch is only
        meaningful against a number the broker agrees with, and accumulating
        locally drifts over a day of partial fills.
        """
        with self._lock:
            self._state.realized_pnl = value

    def set_exposure(self, open_positions: int, notional: Money) -> None:
        """Record the current open position count and notional."""
        with self._lock:
            self._state.open_positions = open_positions
            self._state.notional_open = notional

    def trip(self, name: str) -> None:
        """Latch a kill switch by name.

        Once tripped it stays tripped for the rest of the trading day: a kill
        switch that resets when the condition momentarily clears is not one.
        """
        with self._lock:
            if not self._state.kill_switch:
                self._state.kill_switch = name


class RiskBlockedError(Exception):
    """A gate refused a signal.

    Carries the gate's name so a caller can distinguish a risk refusal from a
    transport failure without parsing the message.
    """

    def __init__(self, gate: str, reason: str) -> None:
        """Build a refusal naming the gate and the reason."""
        super().__init__(f"risk: blocked by {gate}: {reason}")
        self.gate = gate
        self.reason = reason


@runtime_checkable
class Gate(Protocol):
    """Decides whether one signal may proceed."""

    @property
    def name(self) -> str:
        """Identify the gate in refusals and logs."""
        ...

    def check(self, sig: Signal, state: DailyState) -> None:
        """Return normally to allow, or raise :class:`RiskBlockedError` to refuse."""
        ...


class Chain:
    """Evaluates gates in order and raises on the first refusal.

    Order is the caller's choice and it matters: a hard stop such as the daily
    loss limit belongs first, so a day that has blown its budget reports that
    rather than whichever per-symbol limit also happened to trip.
    """

    def __init__(self, *gates: Gate) -> None:
        """Build a chain from gates, evaluated in the order given."""
        self._gates = gates

    def check(self, sig: Signal, state: DailyState) -> None:
        """Run every gate until one refuses.

        Raises:
            RiskBlockedError: From the first gate that refuses.

        """
        for g in self._gates:
            g.check(sig, state)


@dataclass(frozen=True, slots=True)
class KillSwitch:
    """Refuses everything once a switch has been latched."""

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "kill_switch"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse if any kill switch has tripped today."""
        if state.kill_switch:
            raise RiskBlockedError(self.name, f"tripped: {state.kill_switch}")


@dataclass(frozen=True, slots=True)
class DailyLossLimit:
    """Refuses new entries once the day's realised loss reaches ``limit``.

    ``limit`` is a positive amount: the loss the account may absorb.
    """

    limit: Money

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "daily_loss_limit"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse when realised loss has reached the limit."""
        if self.limit <= 0:
            return
        if state.realized_pnl < 0 and abs(int(state.realized_pnl)) >= int(self.limit):
            raise RiskBlockedError(
                self.name,
                f"realised loss {money.format(Money(abs(int(state.realized_pnl))))} "
                f"has reached the {money.format(self.limit)} limit",
            )


@dataclass(frozen=True, slots=True)
class MaxTradesPerDay:
    """Caps how many entries may be taken in one session."""

    max: int

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "max_trades_per_day"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse once the day's entry count reaches the cap."""
        if self.max > 0 and state.trades_today >= self.max:
            raise RiskBlockedError(self.name, f"{state.trades_today} trades already taken today")


@dataclass(frozen=True, slots=True)
class MaxTradesPerSymbol:
    """Caps re-entries into the same instrument in one session.

    This is what stops a chopping symbol consuming the whole day's budget.
    """

    max: int

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "max_trades_per_symbol"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse once this instrument's entry count reaches the cap."""
        if self.max <= 0:
            return
        n = state.trades_per_key.get(str(sig.key), 0)
        if n >= self.max:
            raise RiskBlockedError(self.name, f"{sig.key} already traded {n} times today")


@dataclass(frozen=True, slots=True)
class MaxConcurrentPositions:
    """Caps how many positions may be open at once."""

    max: int

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "max_concurrent_positions"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse once the open position count reaches the cap."""
        if self.max > 0 and state.open_positions >= self.max:
            raise RiskBlockedError(self.name, f"{state.open_positions} positions already open")


@dataclass(frozen=True, slots=True)
class MaxOpenNotional:
    """Caps the total value of open positions."""

    max: Money

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "max_open_notional"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse once open exposure reaches the cap."""
        if self.max > 0 and state.notional_open >= self.max:
            raise RiskBlockedError(
                self.name,
                f"open notional {money.format(state.notional_open)} has reached the {money.format(self.max)} cap",
            )


@dataclass(frozen=True, slots=True)
class TradingWindow:
    """Refuses entries outside a time-of-day window.

    It exists because the two most common intraday mistakes are entering in the
    first minutes before a range has formed, and entering close enough to the
    bell that the position is squared off before the thesis can play out.

    ``start`` and ``end`` are minutes from midnight in ``tz``.
    """

    start: int
    end: int
    tz: ZoneInfo | None = None

    @property
    def name(self) -> str:
        """Gate name, used in refusals and logs."""
        return "trading_window"

    def check(self, sig: Signal, state: DailyState) -> None:
        """Refuse a signal timestamped outside the window."""
        local = sig.at.astimezone(self.tz) if self.tz else sig.at
        minutes = local.hour * 60 + local.minute
        if minutes < self.start or minutes > self.end:
            raise RiskBlockedError(
                self.name,
                f"{local:%H:%M} is outside the "
                f"{self.start // 60:02d}:{self.start % 60:02d}-{self.end // 60:02d}:{self.end % 60:02d} window",
            )


def trail(side: Side, current: Money, extreme: Money, atr_value: float, mult: float) -> Money:
    """Chandelier trailing stop: the extreme since entry, pulled back by ATR.

    The stop is monotonic -- it never moves against the position. Every trailing
    implementation this replaces had to state that separately, and one of them
    enforced it only for longs.
    """
    offset = money._round_half_away(atr_value * mult)
    if side is Side.BUY:
        candidate = int(extreme) - offset
        return Money(candidate) if candidate > int(current) else current
    candidate = int(extreme) + offset
    if candidate < int(current) or current == 0:
        return Money(candidate)
    return current


def breakeven(
    side: Side,
    current: Money,
    entry: Money,
    initial_stop: Money,
    last: Money,
    enabled: bool,
) -> Money:
    """Raise the stop to entry once the position has gained one unit of risk."""
    if not enabled:
        return current
    one_r = abs(int(entry) - int(initial_stop))
    if one_r == 0:
        return current
    if side is Side.BUY:
        if int(last) >= int(entry) + one_r and int(entry) > int(current):
            return entry
        return current
    if int(last) <= int(entry) - one_r and (int(entry) < int(current) or current == 0):
        return entry
    return current
