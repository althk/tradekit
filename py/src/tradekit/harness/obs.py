"""Log helpers that render domain types readably.

Mirrors ``go/harness/obs.go``. Logging is for operating the process and the
stdlib :mod:`logging` module is what every project uses; this adds no
framework, only the two or three helpers that make a trading log readable.
An integer of paise in a log line is unreadable and, worse, is silently
mistaken for rupees.
"""

from __future__ import annotations

from typing import Any

from tradekit.core import money
from tradekit.core.domain import InstrumentKey, Side
from tradekit.core.money import Money

__all__ = ["attrs", "money_str"]


def money_str(m: Money) -> str:
    """Render minor units as a decimal string for logs: ``123456`` paise is ``"1234.56"``."""
    return money.format(m)


def attrs(
    *,
    key: InstrumentKey | None = None,
    side: Side | None = None,
    qty: int | None = None,
    reason: str | None = None,
    **amounts: Money,
) -> dict[str, Any]:
    """Build an ``extra`` mapping for :meth:`logging.Logger.info` with domain values rendered.

    Keyword arguments other than the named ones are amounts and render through
    :func:`money_str`, so ``attrs(key=k, price=Money(123456))`` logs
    ``price=1234.56``. The result is a plain dict so it fits any handler and
    any formatter.
    """
    out: dict[str, Any] = {}
    if key is not None:
        out["key"] = str(key)
    if side is not None:
        out["side"] = str(side)
    if qty is not None:
        out["qty"] = qty
    if reason is not None:
        out["reason"] = reason
    for name, amount in amounts.items():
        out[name] = money_str(amount)
    return out
