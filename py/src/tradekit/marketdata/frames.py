"""Optional pandas bridge: a bar list to a frame and back.

``pandas`` is an extra, not a dependency, because ``fanse`` and ``breakout500``
differ on whether they want frames. Nothing else in the package imports this
module, so the core paths work without it; importing it without pandas
installed raises :class:`ImportError` with the extra's name.

Prices cross the boundary as *float major units* -- rupees, not paise --
because that is what every indicator and every plot in the donor code expects
of a frame. The conversion back rounds half away from zero through
:func:`tradekit.core.money.from_major`, so a frame produced by :func:`to_frame`
round-trips exactly.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import TYPE_CHECKING, Any

from tradekit.core import money
from tradekit.core.domain import Candle, InstrumentKey, Timeframe

if TYPE_CHECKING:
    import pandas as pd

__all__ = ["from_frame", "to_frame"]

COLUMNS = ("open", "high", "low", "close", "volume", "open_interest")
"""The frame's columns, in this order, indexed by the bar's start."""


def _pandas() -> Any:
    try:
        import pandas
    except ImportError:  # pragma: no cover - exercised only without the extra
        raise ImportError("marketdata.frames needs pandas; install tradekit[pandas]") from None
    return pandas


def to_frame(candles: Iterable[Candle]) -> pd.DataFrame:
    """Build a frame indexed by bar start, one row per bar.

    The index is timezone-aware, carrying each bar's own offset, so a session
    filter on the frame agrees with :mod:`tradekit.core.calendar`.
    """
    pd = _pandas()
    rows = list(candles)
    frame = pd.DataFrame(
        {
            "open": [money.major(c.open) for c in rows],
            "high": [money.major(c.high) for c in rows],
            "low": [money.major(c.low) for c in rows],
            "close": [money.major(c.close) for c in rows],
            "volume": [c.volume for c in rows],
            "open_interest": [c.open_interest for c in rows],
        },
        index=pd.DatetimeIndex([c.start for c in rows], name="start"),
    )
    return frame


def from_frame(frame: pd.DataFrame, key: InstrumentKey, timeframe: Timeframe) -> list[Candle]:
    """Read bars back out of a frame shaped like :func:`to_frame` produces.

    A naive index is rejected rather than guessed at: a frame built by a donor
    from vendor data may be in IST or in UTC, and picking one silently would
    put the whole session in the wrong day for the other.
    """
    if frame.index.tz is None:
        raise ValueError("marketdata: the frame's index must be timezone-aware")
    has_oi = "open_interest" in frame.columns
    out: list[Candle] = []
    for start, row in frame.iterrows():
        out.append(
            Candle(
                key=key,
                timeframe=timeframe,
                start=start.to_pydatetime(),
                open=money.from_major(float(row["open"])),
                high=money.from_major(float(row["high"])),
                low=money.from_major(float(row["low"])),
                close=money.from_major(float(row["close"])),
                volume=int(row["volume"]) if "volume" in frame.columns else 0,
                open_interest=int(row["open_interest"]) if has_oi else 0,
            )
        )
    return out
