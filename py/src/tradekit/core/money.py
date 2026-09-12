"""Monetary amounts as integer minor units.

A ``Money`` value is a count of the currency's smallest unit -- paise for INR,
cents for USD. Integers are used rather than ``float`` because accumulated
profit and loss must be exact, and rather than :class:`decimal.Decimal` because
the same representation has to exist unchanged in Go, in Python and in a SQLite
``INTEGER`` column.

This module mirrors ``go/core/money`` exactly. Every function here has a
counterpart there with the same name in Go's casing, and both round half away
from zero.
"""

from __future__ import annotations

import math
import re
from decimal import ROUND_HALF_UP, Decimal
from typing import NewType

__all__ = [
    "ZERO",
    "Money",
    "ceil_to_tick",
    "floor_to_tick",
    "format",
    "from_major",
    "major",
    "mul",
    "mul_fraction",
    "parse",
    "round_to_tick",
]

Money = NewType("Money", int)
"""An amount in the currency's minor unit."""

ZERO: Money = Money(0)

_SCALE = 100
"""Minor units in one major unit. Both currencies tradekit targets use 100."""

_AMOUNT = re.compile(r"^[+-]?(?:\d+(?:\.\d{0,2})?|\.\d{1,2})$")


def parse(s: str) -> Money:
    """Read a decimal string such as ``"1234.56"`` into minor units.

    The conversion never passes through binary floating point, so a price that
    a broker sent as text arrives exactly.

    Args:
        s: A decimal amount with at most two fractional digits.

    Returns:
        The amount in minor units.

    Raises:
        ValueError: If ``s`` is not a decimal amount, or carries more than two
            decimal places. More precision than the exchange can quote is a bug
            upstream, and rounding it away here would hide it.

    """
    text = s.strip()
    if not _AMOUNT.match(text):
        raise ValueError(f"money: cannot parse amount: {s!r}")
    return Money(int(Decimal(text).scaleb(2).to_integral_value(rounding=ROUND_HALF_UP)))


def format(m: Money) -> str:  # noqa: A001 - mirrors Go's Money.String
    """Render an amount with exactly two decimals, so it round-trips ``parse``."""
    sign = "-" if m < 0 else ""
    v = abs(int(m))
    return f"{sign}{v // _SCALE}.{v % _SCALE:02d}"


def from_major(v: float) -> Money:
    """Convert a whole-currency amount, rounding half away from zero.

    Intended for configuration and fixtures. Prices from a broker API should go
    through :func:`parse` instead, which avoids floating point entirely.
    """
    return Money(_round_half_away(v * _SCALE))


def major(m: Money) -> float:
    """Return the amount as a whole-currency float, for display and plotting.

    Never round-trip through this: ``from_major(major(m))`` is not guaranteed to
    equal ``m`` for very large amounts.
    """
    return int(m) / _SCALE


def mul(m: Money, qty: int) -> Money:
    """Scale an amount by a whole quantity, as in price times shares."""
    return Money(int(m) * int(qty))


def mul_fraction(m: Money, f: float) -> Money:
    """Scale an amount by a ratio, rounding half away from zero.

    Used for charge rates, percentages and ATR multiples.
    """
    return Money(_round_half_away(int(m) * f))


def round_to_tick(m: Money, tick: Money) -> Money:
    """Return the nearest multiple of ``tick``.

    A non-positive tick returns the amount unchanged: refusing to round is safer
    than inventing a tick size the venue may not use.
    """
    if tick <= 0:
        return m
    return Money(_round_quotient(int(m), int(tick)) * int(tick))


def floor_to_tick(m: Money, tick: Money) -> Money:
    """Return the largest multiple of ``tick`` not greater than the amount.

    Use it for a sell limit, where rounding up could place the order outside
    the book.
    """
    if tick <= 0:
        return m
    # Python's // already floors for negatives; using float division here
    # would lose precision on large amounts.
    return Money((int(m) // int(tick)) * int(tick))


def ceil_to_tick(m: Money, tick: Money) -> Money:
    """Return the smallest multiple of ``tick`` not less than the amount.

    Use it for a buy limit, for the mirror-image reason to :func:`floor_to_tick`.
    """
    if tick <= 0:
        return m
    return Money(-((-int(m)) // int(tick)) * int(tick))


def _round_half_away(v: float) -> int:
    """Round half away from zero.

    Python's built-in ``round`` uses banker's rounding, which would disagree
    with the Go implementation on every exact half. This is the single rounding
    rule tradekit uses.
    """
    if math.isnan(v) or math.isinf(v):
        return 0
    return math.floor(v + 0.5) if v >= 0 else math.ceil(v - 0.5)


def _round_quotient(a: int, b: int) -> int:
    """Divide and round half away from zero, without floating point.

    Computed on magnitudes and re-signed at the end. Working with Python's
    flooring ``divmod`` directly gets the negative cases wrong, because
    "half away from zero" is defined against truncation, not against the floor.
    """
    magnitude, divisor = abs(a), abs(b)
    q, r = divmod(magnitude, divisor)
    if 2 * r >= divisor:
        q += 1
    return q if (a < 0) == (b < 0) else -q
