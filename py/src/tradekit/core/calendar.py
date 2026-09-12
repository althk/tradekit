"""When an exchange trades, and where a timestamp falls within its session.

Mirrors ``go/core/calendar``. It replaces four incompatible answers to "is the
market open": one that hardcoded 09:15-15:30 with no holiday awareness at all,
one that consulted a broker API on every call, one that cached a JSON file, and
one that did not exist.

Holidays arrive through a :class:`~tradekit.core.ports.HolidaySource` so a
backtest can run offline against a cached list while a live process refreshes
from the venue.
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field
from zoneinfo import ZoneInfo

from .domain import Timeframe
from .ports import HolidaySource

__all__ = ["Calendar", "Session", "StaticHolidays", "nse", "us_equity"]

_MON_TO_FRI = frozenset({0, 1, 2, 3, 4})


@dataclass(frozen=True, slots=True)
class Session:
    """An exchange's regular trading day."""

    exchange: str
    tz: ZoneInfo
    open_minute: int
    close_minute: int
    trading_weekdays: frozenset[int] = field(default=_MON_TO_FRI)

    def date(self, t: dt.datetime) -> dt.date:
        """The calendar date of ``t`` in the session's timezone."""
        return t.astimezone(self.tz).date()

    def date_key(self, t: dt.datetime | dt.date) -> str:
        """The calendar date as ``"YYYY-MM-DD"``.

        This, not a date object, is the key for every date-indexed mapping in
        tradekit, so that Go, Python, SQLite and a JSON cache all agree.
        """
        d = t if isinstance(t, dt.date) and not isinstance(t, dt.datetime) else self.date(t)
        return d.isoformat()

    def open(self, t: dt.datetime) -> dt.datetime:
        """The session's opening instant on ``t``'s calendar date."""
        return self._at(self.date(t), self.open_minute)

    def close(self, t: dt.datetime) -> dt.datetime:
        """The session's closing instant on ``t``'s calendar date."""
        return self._at(self.date(t), self.close_minute)

    def _at(self, d: dt.date, minute: int) -> dt.datetime:
        return dt.datetime(d.year, d.month, d.day, minute // 60, minute % 60, tzinfo=self.tz)

    def is_weekday(self, t: dt.datetime | dt.date) -> bool:
        """Whether the exchange trades on this day of the week.

        Says nothing about holidays; use :meth:`Calendar.trading_day` for that.
        """
        d = t if isinstance(t, dt.date) and not isinstance(t, dt.datetime) else self.date(t)
        return d.weekday() in self.trading_weekdays

    def in_session(self, t: dt.datetime) -> bool:
        """Whether ``t`` falls inside the regular window, both bounds included."""
        if not self.is_weekday(t):
            return False
        local = t.astimezone(self.tz)
        minutes = local.hour * 60 + local.minute
        return self.open_minute <= minutes <= self.close_minute

    def bucket_start(self, t: dt.datetime, timeframe: Timeframe) -> dt.datetime:
        """Align a timestamp to the start of its bar, anchored to the open.

        Anchoring matters: wall-clock bucketing produces a bar chain that
        disagrees with the exchange's own for any open that is not on a clean
        boundary. A timestamp before the open clamps to the open, so a pre-market
        print lands in the first bar rather than in a phantom bar of its own.

        A non-intraday timeframe returns the session date at midnight, which is
        the correct bucket for a daily bar.
        """
        span = timeframe.duration
        if span <= dt.timedelta(0):
            d = self.date(t)
            return dt.datetime(d.year, d.month, d.day, tzinfo=self.tz)
        opening = self.open(t)
        local = t.astimezone(self.tz)
        if local < opening:
            return opening
        since = local - opening
        return opening + (since // span) * span

    def slot_index(self, t: dt.datetime, timeframe: Timeframe) -> int:
        """Zero-based position of ``t``'s bar within its session.

        This is what a volume profile and a relative-volume calculation index
        on. Returns ``-1`` for a timestamp after the close.
        """
        span = timeframe.duration
        if span <= dt.timedelta(0):
            return 0
        start = self.bucket_start(t, timeframe)
        if start > self.close(t):
            return -1
        return (start - self.open(t)) // span

    def slot_count(self, timeframe: Timeframe) -> int:
        """How many whole bars of this size fit in one regular session."""
        span = timeframe.duration
        if span <= dt.timedelta(0):
            return 1
        return dt.timedelta(minutes=self.close_minute - self.open_minute) // span


def nse() -> Session:
    """The NSE India equity session: Monday to Friday, 09:15-15:30 IST."""
    return Session("NSE", ZoneInfo("Asia/Kolkata"), 9 * 60 + 15, 15 * 60 + 30)


def us_equity() -> Session:
    """The NYSE/Nasdaq regular session: Monday to Friday, 09:30-16:00 ET."""
    return Session("NASDAQ", ZoneInfo("America/New_York"), 9 * 60 + 30, 16 * 60)


class StaticHolidays:
    """A :class:`~tradekit.core.ports.HolidaySource` backed by an in-memory set.

    Used for backtests, tests, and as the cache behind a live source.
    """

    def __init__(self, days: dict[str, dict[str, str]]) -> None:
        """Build from a mapping of exchange to ``{"YYYY-MM-DD": description}``."""
        self._days = days

    def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
        """Return the closed dates for the exchange within ``[start, end]``.

        The range is compared as date strings, which is sound because the format
        is fixed-width and zero-padded: lexical order is chronological order.
        """
        lo, hi = start.isoformat(), end.isoformat()
        return {d: desc for d, desc in self._days.get(exchange, {}).items() if lo <= d <= hi}


class Calendar:
    """A session combined with a holiday source."""

    def __init__(self, session: Session, holidays: HolidaySource | None = None) -> None:
        """Build a calendar.

        A ``None`` holiday source means weekends are the only closures known --
        which is what the code this replaces assumed silently. Here it is at
        least explicit.
        """
        self.session = session
        self.holidays = holidays

    def trading_day(self, t: dt.datetime) -> tuple[bool, Exception | None]:
        """Whether the exchange trades on ``t``'s calendar date.

        Fails open: if the holiday source raises, the day is reported as trading
        and the exception is returned alongside. Skipping a real trading day is
        a worse outcome than one wasted run, so the caller decides whether to
        care and the boolean is always usable.
        """
        if not self.session.is_weekday(t):
            return False, None
        if self.holidays is None:
            return True, None
        day = self.session.date(t)
        try:
            closed = self.holidays.closed(self.session.exchange, day, day)
        except Exception as exc:
            return True, exc
        return day.isoformat() not in closed, None

    def trading_days(self, start: dt.date, end: dt.date) -> tuple[list[dt.date], Exception | None]:
        """The exchange's trading dates in ``[start, end]``, inclusive.

        Makes one holiday lookup for the whole range rather than one per day.
        """
        if end < start:
            return [], None
        closed: dict[str, str] = {}
        error: Exception | None = None
        if self.holidays is not None:
            try:
                closed = self.holidays.closed(self.session.exchange, start, end)
            except Exception as exc:
                closed, error = {}, exc

        days: list[dt.date] = []
        day = start
        while day <= end:
            if self.session.is_weekday(day) and day.isoformat() not in closed:
                days.append(day)
            day += dt.timedelta(days=1)
        return days, error
