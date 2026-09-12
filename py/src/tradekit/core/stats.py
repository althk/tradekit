"""Summaries of a set of closed trades.

Mirrors ``go/core/stats``. Every dashboard and every backtest report in the
projects tradekit replaces recomputed these same figures, and no two agreed on
all of them. Sharing the queries rather than the templates is the point: a win
rate should not depend on which project is displaying it.
"""

from __future__ import annotations

import datetime as dt
import math
from dataclasses import dataclass, field

from . import money
from .domain import ExitReason, Side, Trade
from .money import Money

__all__ = [
    "Metrics",
    "MixedModesError",
    "Point",
    "daily_returns",
    "equity_curve",
    "max_drawdown",
    "r_multiple",
    "sharpe",
    "summarize",
]


class MixedModesError(ValueError):
    """Paper and live trades were summarised together.

    An error rather than a silent merge because every aggregate -- win rate,
    profit factor, drawdown, the equity curve -- is meaningless when simulated
    fills are blended with real ones, and the resulting number looks entirely
    plausible.
    """


@dataclass(slots=True)
class Metrics:
    """Summary of a set of closed trades.

    Monetary fields are in minor units; ratios are plain fractions.
    """

    trades: int = 0
    wins: int = 0
    losses: int = 0
    breakeven: int = 0

    gross_profit: Money = money.ZERO
    gross_loss: Money = money.ZERO
    net_pnl: Money = money.ZERO
    charges: Money = money.ZERO

    win_rate: float = 0.0
    profit_factor: float = 0.0
    expectancy: Money = money.ZERO

    avg_win: Money = money.ZERO
    avg_loss: Money = money.ZERO

    max_drawdown: Money = money.ZERO
    max_drawdown_pct: float = 0.0

    avg_holding: dt.timedelta = dt.timedelta(0)

    by_exit_reason: dict[ExitReason, int] = field(default_factory=dict)


def summarize(trades: list[Trade]) -> Metrics:
    """Compute the metrics for a set of closed trades.

    Trades need not be sorted; they are ordered by exit time internally, because
    drawdown is meaningless over an arbitrary ordering.

    Raises:
        MixedModesError: If paper and live trades are mixed.

    """
    m = Metrics()
    if not trades:
        return m
    if len({t.paper for t in trades}) > 1:
        raise MixedModesError("stats: paper and live trades cannot be summarised together")

    ordered = sorted(trades, key=lambda t: t.exit_at)
    holding = dt.timedelta(0)
    gross_profit = gross_loss = net = charges = 0

    for t in ordered:
        m.trades += 1
        net += int(t.net_pnl)
        charges += int(t.charges)
        m.by_exit_reason[t.exit_reason] = m.by_exit_reason.get(t.exit_reason, 0) + 1
        holding += t.exit_at - t.entry_at
        if t.net_pnl > 0:
            m.wins += 1
            gross_profit += int(t.net_pnl)
        elif t.net_pnl < 0:
            m.losses += 1
            gross_loss += -int(t.net_pnl)
        else:
            m.breakeven += 1

    m.gross_profit, m.gross_loss = Money(gross_profit), Money(gross_loss)
    m.net_pnl, m.charges = Money(net), Money(charges)
    m.win_rate = m.wins / m.trades
    m.expectancy = Money(net // m.trades)
    m.avg_holding = holding / m.trades

    if m.wins:
        m.avg_win = Money(gross_profit // m.wins)
    if m.losses:
        m.avg_loss = Money(gross_loss // m.losses)

    if gross_loss > 0:
        m.profit_factor = gross_profit / gross_loss
    elif gross_profit > 0:
        # No losing trades. Infinity is the honest answer; a caller formatting
        # it must decide how to display that rather than being handed a
        # fabricated finite number.
        m.profit_factor = math.inf

    m.max_drawdown, m.max_drawdown_pct = max_drawdown(equity_curve(ordered, money.ZERO))
    return m


@dataclass(frozen=True, slots=True)
class Point:
    """One sample of the running equity curve."""

    at: dt.datetime
    equity: Money


def equity_curve(trades: list[Trade], opening: Money = money.ZERO) -> list[Point]:
    """Accumulate net P&L over trades in exit order, starting from ``opening``."""
    running = int(opening)
    curve: list[Point] = []
    for t in sorted(trades, key=lambda t: t.exit_at):
        running += int(t.net_pnl)
        curve.append(Point(t.exit_at, Money(running)))
    return curve


def max_drawdown(curve: list[Point]) -> tuple[Money, float]:
    """Largest peak-to-trough fall, as an amount and a fraction of that peak.

    The fraction is relative to each peak rather than to the starting equity, so
    a 10% fall late in a doubled account is reported as 10% and not as 5%.
    """
    if not curve:
        return money.ZERO, 0.0
    peak = int(curve[0].equity)
    worst, worst_pct = 0, 0.0
    for p in curve:
        peak = max(peak, int(p.equity))
        fall = peak - int(p.equity)
        if fall > worst:
            worst = fall
            worst_pct = fall / peak if peak > 0 else 0.0
    return Money(worst), worst_pct


def r_multiple(trade: Trade, initial_stop: Money) -> float | None:
    """A trade's result as a multiple of the risk taken on it.

    ``initial_stop`` is the stop in force at entry, not wherever it was trailed
    to. A trade risking 1 and making 3 is a 3R trade regardless of account size,
    which is what makes R comparable across instruments and across time.

    Returns ``None`` when the initial risk was zero, since no multiple exists.
    """
    risk = abs(int(trade.entry_price) - int(initial_stop))
    if risk == 0:
        return None
    move = int(trade.exit_price) - int(trade.entry_price)
    if trade.side is Side.SELL:
        move = -move
    return move / risk


def daily_returns(curve: list[Point], tz: dt.tzinfo | None = None) -> list[float]:
    """Aggregate an equity curve into per-day fractional returns.

    This is the input :func:`sharpe` needs.
    """
    if len(curve) < 2:
        return []

    days: list[tuple[str, int]] = []
    for p in curve:
        at = p.at.astimezone(tz) if tz else p.at
        key = at.date().isoformat()
        if days and days[-1][0] == key:
            days[-1] = (key, int(p.equity))
        else:
            days.append((key, int(p.equity)))

    out: list[float] = []
    for i in range(1, len(days)):
        prev = days[i - 1][1]
        if prev == 0:
            continue
        out.append((days[i][1] - prev) / abs(prev))
    return out


def sharpe(returns: list[float], periods_per_year: float = 252.0, risk_free: float = 0.0) -> float | None:
    """Annualised Sharpe ratio of a series of periodic returns.

    ``periods_per_year`` scales the result: 252 for daily returns on an equity
    calendar. ``risk_free`` is the per-period rate, not the annual one.

    Returns ``None`` when the series has no variance, since the ratio is
    undefined rather than infinite in any useful sense.
    """
    if len(returns) < 2 or periods_per_year <= 0:
        return None
    excess = [r - risk_free for r in returns]
    mean = sum(excess) / len(excess)
    # Sample standard deviation: the returns are a sample of the strategy's
    # behaviour, not the whole population of it.
    variance = sum((e - mean) ** 2 for e in excess) / (len(excess) - 1)
    if variance <= 0:
        return None
    return mean / math.sqrt(variance) * math.sqrt(periods_per_year)
