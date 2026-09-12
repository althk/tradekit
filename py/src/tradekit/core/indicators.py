"""Technical series shared by the strategies.

Mirrors ``go/core/indicators``. It replaces four separate implementations of the
same handful of functions -- ATR alone existed in four projects, two of them
subtly different in how they seeded the first value.

Representation
--------------
Every function returns a list of floats the same length as its input, with
``float("nan")`` in the positions before the indicator has warmed up. NaN is
used rather than zero because zero is a legitimate indicator value, and a
zero-filled warm-up silently biases any average taken over the series.

Price-valued outputs (SMA, EMA, ATR, Donchian, VWAP, pivots) are in the same
minor units as their :class:`~tradekit.core.money.Money` input. A float holds
those integers exactly up to 2**53, far beyond any realistic price, so no
precision is lost; use :func:`to_money` to convert a value back.
"""

from __future__ import annotations

import math
from collections.abc import Callable, Sequence
from dataclasses import dataclass

from .domain import Candle
from .money import Money, _round_half_away

__all__ = [
    "ADX",
    "Channel",
    "Pivots",
    "adx",
    "atr",
    "closes",
    "cpr",
    "donchian",
    "ema",
    "highs",
    "is_valid",
    "lows",
    "rsi",
    "sma",
    "to_money",
    "true_range",
    "vwap",
]

NAN = float("nan")


def to_money(v: float) -> Money:
    """Convert an indicator value to minor units, rounding half away from zero.

    A NaN input returns zero, so callers must guard with :func:`is_valid` rather
    than relying on the zero to mean anything.
    """
    if math.isnan(v) or math.isinf(v):
        return Money(0)
    return Money(_round_half_away(v))


def is_valid(v: float) -> bool:
    """Whether an indicator value has warmed up."""
    return not math.isnan(v)


def closes(candles: Sequence[Candle]) -> list[float]:
    """Closing prices as indicator input."""
    return [float(c.close) for c in candles]


def highs(candles: Sequence[Candle]) -> list[float]:
    """High prices as indicator input."""
    return [float(c.high) for c in candles]


def lows(candles: Sequence[Candle]) -> list[float]:
    """Low prices as indicator input."""
    return [float(c.low) for c in candles]


def sma(values: Sequence[float], period: int) -> list[float]:
    """Simple moving average over the trailing ``period`` values."""
    out = [NAN] * len(values)
    if period <= 0 or len(values) < period:
        return out
    total = 0.0
    for i, v in enumerate(values):
        total += v
        if i >= period:
            total -= values[i - period]
        if i >= period - 1:
            out[i] = total / period
    return out


def ema(values: Sequence[float], period: int) -> list[float]:
    """Exponential moving average, seeded with the mean of the first window.

    Seeding matters, and is the difference between two implementations this
    replaces: seeding with the first value instead makes the early series depend
    on how much history the caller happened to load.
    """
    out = [NAN] * len(values)
    if period <= 0 or len(values) < period:
        return out
    k = 2.0 / (period + 1)
    prev = sum(values[:period]) / period
    out[period - 1] = prev
    for i in range(period, len(values)):
        prev = (values[i] - prev) * k + prev
        out[i] = prev
    return out


def true_range(candles: Sequence[Candle]) -> list[float]:
    """Per-bar true range.

    The greatest of the bar's own span, the gap up from the previous close, and
    the gap down to it. The first bar has no previous close and is therefore
    NaN, not its own high-low span.
    """
    out = [NAN] * len(candles)
    for i in range(1, len(candles)):
        prev_close = float(candles[i - 1].close)
        out[i] = max(
            float(candles[i].high - candles[i].low),
            abs(float(candles[i].high) - prev_close),
            abs(float(candles[i].low) - prev_close),
        )
    return out


def atr(candles: Sequence[Candle], period: int) -> list[float]:
    """Wilder's average true range.

    A simple average of the first ``period`` true ranges, then Wilder smoothing.
    Wilder smoothing -- not an EMA of the same period -- is what "ATR(14)" means
    everywhere it is quoted; substituting an EMA gives a tighter series, which
    silently moves every ATR-derived stop.
    """
    out = [NAN] * len(candles)
    tr = true_range(candles)
    if period <= 0 or len(candles) < period + 1:
        return out
    prev = sum(tr[1 : period + 1]) / period
    out[period] = prev
    for i in range(period + 1, len(candles)):
        prev = (prev * (period - 1) + tr[i]) / period
        out[i] = prev
    return out


def rsi(values: Sequence[float], period: int) -> list[float]:
    """Wilder's relative strength index, on a 0-100 scale."""
    out = [NAN] * len(values)
    if period <= 0 or len(values) < period + 1:
        return out

    gain = loss = 0.0
    for i in range(1, period + 1):
        delta = values[i] - values[i - 1]
        if delta > 0:
            gain += delta
        else:
            loss -= delta
    avg_gain, avg_loss = gain / period, loss / period
    out[period] = _rsi_from(avg_gain, avg_loss)

    for i in range(period + 1, len(values)):
        delta = values[i] - values[i - 1]
        g = delta if delta > 0 else 0.0
        loss_step = -delta if delta < 0 else 0.0
        avg_gain = (avg_gain * (period - 1) + g) / period
        avg_loss = (avg_loss * (period - 1) + loss_step) / period
        out[i] = _rsi_from(avg_gain, avg_loss)
    return out


def _rsi_from(avg_gain: float, avg_loss: float) -> float:
    if avg_loss == 0:
        return 50.0 if avg_gain == 0 else 100.0
    rs = avg_gain / avg_loss
    return 100 - 100 / (1 + rs)


@dataclass(slots=True)
class Channel:
    """A Donchian channel: trailing highest high, lowest low, and midpoint."""

    upper: list[float]
    lower: list[float]
    middle: list[float]


def donchian(candles: Sequence[Candle], period: int) -> Channel:
    """Donchian channel over the trailing ``period`` bars.

    The window excludes the current bar. A breakout strategy that includes it can
    never fire: the current bar's own high is by definition not greater than the
    highest high of a window containing it.
    """
    n = len(candles)
    ch = Channel([NAN] * n, [NAN] * n, [NAN] * n)
    if period <= 0 or n <= period:
        return ch
    for i in range(period, n):
        window = candles[i - period : i]
        hi = max(float(c.high) for c in window)
        lo = min(float(c.low) for c in window)
        ch.upper[i], ch.lower[i], ch.middle[i] = hi, lo, (hi + lo) / 2
    return ch


def vwap(candles: Sequence[Candle], new_session: Callable[[int], bool]) -> list[float]:
    """Volume-weighted average price, accumulated from each session's start.

    ``new_session(i)`` reports whether the bar at index ``i`` opens a new
    session. Passing a callable rather than a calendar keeps this module free of
    a calendar dependency and lets a backtest group bars however its data is
    shaped.
    """
    out = [NAN] * len(candles)
    pv = vol = 0.0
    for i, c in enumerate(candles):
        if i == 0 or new_session(i):
            pv = vol = 0.0
        typical = float(c.high + c.low + c.close) / 3
        v = float(c.volume)
        pv += typical * v
        vol += v
        # A zero-volume session has no volume-weighted price; the typical price
        # is the honest fallback, and avoids dividing by zero on an illiquid bar.
        out[i] = pv / vol if vol > 0 else typical
    return out


@dataclass(slots=True)
class Pivots:
    """Central pivot range and the classic support/resistance levels."""

    pivot: Money
    bc: Money
    tc: Money
    r1: Money
    r2: Money
    r3: Money
    s1: Money
    s2: Money
    s3: Money


def cpr(prev: Candle) -> Pivots:
    """Pivot levels for the session following ``prev``.

    ``bc`` and ``tc`` are ordered so ``bc`` is always the lower of the two. The
    raw formulas produce them in either order depending on where the close sat,
    and a caller comparing price to "the top of the range" needs them sorted.
    """
    h, low, c = float(prev.high), float(prev.low), float(prev.close)
    pivot = (h + low + c) / 3
    bc = (h + low) / 2
    tc = pivot - bc + pivot
    if bc > tc:
        bc, tc = tc, bc
    return Pivots(
        pivot=to_money(pivot),
        bc=to_money(bc),
        tc=to_money(tc),
        r1=to_money(2 * pivot - low),
        s1=to_money(2 * pivot - h),
        r2=to_money(pivot + (h - low)),
        s2=to_money(pivot - (h - low)),
        r3=to_money(h + 2 * (pivot - low)),
        s3=to_money(low - 2 * (h - pivot)),
    )


@dataclass(slots=True)
class ADX:
    """Average directional index with its directional indicators, 0-100."""

    adx: list[float]
    plus_di: list[float]
    minus_di: list[float]


def adx(candles: Sequence[Candle], period: int) -> ADX:
    """Wilder's ADX and the +DI / -DI series it is built from."""
    n = len(candles)
    res = ADX([NAN] * n, [NAN] * n, [NAN] * n)
    if period <= 0 or n < 2 * period + 1:
        return res

    tr = true_range(candles)
    plus_dm, minus_dm = [0.0] * n, [0.0] * n
    for i in range(1, n):
        up = float(candles[i].high - candles[i - 1].high)
        down = float(candles[i - 1].low - candles[i].low)
        if up > down and up > 0:
            plus_dm[i] = up
        if down > up and down > 0:
            minus_dm[i] = down

    s_tr = sum(tr[1 : period + 1])
    s_plus = sum(plus_dm[1 : period + 1])
    s_minus = sum(minus_dm[1 : period + 1])
    dx = [NAN] * n

    def set_di(i: int) -> None:
        nonlocal s_tr, s_plus, s_minus
        if s_tr == 0:
            res.plus_di[i] = res.minus_di[i] = dx[i] = 0.0
            return
        p = 100 * s_plus / s_tr
        m = 100 * s_minus / s_tr
        res.plus_di[i], res.minus_di[i] = p, m
        dx[i] = 0.0 if p + m == 0 else 100 * abs(p - m) / (p + m)

    set_di(period)
    for i in range(period + 1, n):
        s_tr = s_tr - s_tr / period + tr[i]
        s_plus = s_plus - s_plus / period + plus_dm[i]
        s_minus = s_minus - s_minus / period + minus_dm[i]
        set_di(i)

    first = 2 * period
    prev = sum(dx[period : first + 1]) / (period + 1)
    res.adx[first] = prev
    for i in range(first + 1, n):
        prev = (prev * (period - 1) + dx[i]) / period
        res.adx[i] = prev
    return res
