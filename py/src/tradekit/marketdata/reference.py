"""Tick sizes, lot sizes, the holiday calendar and corporate actions.

Mirrors ``go/marketdata/reference``. Each of the first three had a donor and
each had a wrong version in use somewhere. Tick sizes were hardcoded as a slab
table that silently rots at the next NSE revision; the holiday calendar was
fetched with no cache, so a backtest could not run offline. The data-driven and
the cached versions are the ones carried over.
"""

from __future__ import annotations

import datetime as dt
import json
import threading
from collections.abc import Callable
from dataclasses import dataclass, replace
from pathlib import Path

from tradekit.core import money
from tradekit.core.domain import Candle, InstrumentKey
from tradekit.core.money import Money
from tradekit.core.ports import HolidaySource
from tradekit.store import Database

__all__ = [
    "FALLBACK_LOT",
    "FALLBACK_TICK",
    "KIND_BONUS",
    "KIND_DIVIDEND",
    "KIND_SPLIT",
    "Action",
    "Holidays",
    "actions",
    "adjust",
    "lot_sizes",
    "tick_sizes",
    "upsert_actions",
]

FALLBACK_TICK: Money = Money(10)
"""The tick used for an instrument the master does not name.

It is Rs 0.10, which is neither the most common tick (Rs 0.01) nor the one most
people would name (Rs 0.05). The reasoning, measured by breakout500 against the
live NSE master, is that a fallback is safe only when it is a *multiple* of the
instrument's real tick: a multiple of 0.10 is also a multiple of 0.05 and of
0.01, so rounding coarser than the truth still lands on the grid while rounding
finer does not. 0.10 satisfies 9644 of 9724 NSE equities; 0.05 satisfies 9257,
missing every 0.10-tick name including Reliance.

It is deliberately not zero. A zero tick disables rounding silently, and an
unrounded stop is rejected outright -- which leaves a live position with no
protection at all, the failure this whole module exists to prevent.
"""

FALLBACK_LOT = 1
"""The lot size for an instrument the master does not name.

Cash equity trades in single shares, and a zero lot would size every position
to nothing.
"""


def tick_sizes(db: Database) -> Callable[[InstrumentKey], Money]:
    """A lookup from instrument to tick size, built from the instruments table.

    The shape is exactly what :class:`tradekit.core.paper.Options` wants, so it
    wires straight in. The lookup is a snapshot: the master changes daily and a
    process that runs for one session wants one consistent answer, not a value
    that changes under it mid-run.
    """
    ticks = {i.key: i.tick_size for i in db.active_instruments() if i.tick_size > 0}
    return lambda key: ticks.get(key, FALLBACK_TICK)


def lot_sizes(db: Database) -> Callable[[InstrumentKey], int]:
    """A lookup from instrument to lot size, built from the instruments table.

    A lot size of 0 in the master -- which is how the vendors spell "cash
    equity" -- becomes 1, so sizing can multiply by it unconditionally.
    """
    lots = {i.key: i.lot_size for i in db.active_instruments() if i.lot_size > 0}
    return lambda key: lots.get(key, FALLBACK_LOT)


# ------------------------------------------------------------------ holidays


class Holidays:
    """A :class:`~tradekit.core.ports.HolidaySource` with a disk cache in front of a live one.

    A backtest runs offline and must not need the network, and a live process
    must not lose its calendar because the broker's holiday endpoint is down.
    The cache satisfies both: a fetch that fails falls back to it, and a fetch
    that succeeds refreshes it.

    The cache file is the same JSON the Go side writes -- ``fetched_at`` and a
    ``days`` map of exchange to ``{"YYYY-MM-DD": description}`` -- so the two
    can share one.
    """

    def __init__(
        self,
        source: HolidaySource | None,
        cache_path: str | Path | None,
        *,
        ttl: dt.timedelta = dt.timedelta(days=1),
        now: Callable[[], dt.datetime] | None = None,
    ) -> None:
        """Build a cached source. A ``None`` live source is permitted and means cache-only.

        ``ttl`` is how long a cached calendar is used without refetching. One
        day by default: exchanges publish the year's calendar in advance, so a
        daily refresh is already far more often than it changes.
        """
        if source is None and not cache_path:
            raise ValueError("reference: a holiday source needs either a live source or a cache path")
        self.source = source
        self.cache_path = Path(cache_path) if cache_path else None
        self.ttl = ttl if ttl > dt.timedelta(0) else dt.timedelta(days=1)
        self._now = now
        self._lock = threading.Lock()
        self._loaded: dict[str, dict[str, str]] | None = None
        self._at: dt.datetime | None = None

    def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
        """The closed dates for an exchange within ``[start, end]``.

        Fails **open**: when neither the live source nor the cache can answer,
        the result is empty, so every day in the range reads as a trading day.
        Skipping a real trading day because a calendar could not be loaded is a
        worse outcome than running on a holiday, where the exchange rejects the
        orders anyway and the cost is one wasted pass.
        """
        try:
            days = self._calendar(exchange, start, end)
        except Exception:  # failing open is the documented contract
            return {}
        lo, hi = start.isoformat(), end.isoformat()
        # The format is fixed-width and zero-padded, so lexical order is
        # chronological order and a string comparison bounds the range.
        return {day: desc for day, desc in days.items() if lo <= day <= hi}

    def _calendar(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
        """The exchange's closed days, from memory, the cache, or the live source in that order."""
        with self._lock:
            if self._loaded is None:
                self._load_cache()
            assert self._loaded is not None
            if not self._stale() and exchange in self._loaded:
                return self._loaded[exchange]

            if self.source is None:
                if exchange in self._loaded:
                    return self._loaded[exchange]
                raise LookupError(f"reference: no cached calendar for {exchange} and no live source")

            try:
                fresh = self.source.closed(exchange, start, end)
            except Exception:
                # The live fetch failed. A stale calendar is far better than
                # none: exchanges publish the year in advance, so last week's
                # copy is almost certainly still correct.
                if exchange in self._loaded:
                    return self._loaded[exchange]
                raise

            self._loaded[exchange] = dict(fresh)
            self._at = self._clock()
            self._save_cache()
            return self._loaded[exchange]

    def _stale(self) -> bool:
        if self._at is None:
            return True
        return self._clock() - self._at >= self.ttl

    def _load_cache(self) -> None:
        """Read the calendar from disk, tolerating its absence.

        A missing or unreadable cache is not an error: it is what a first run
        looks like, and the live source is about to be consulted anyway.
        """
        self._loaded = {}
        if self.cache_path is None:
            return
        try:
            file = json.loads(self.cache_path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return
        days = file.get("days") if isinstance(file, dict) else None
        if isinstance(days, dict):
            self._loaded = {ex: dict(d) for ex, d in days.items()}
            try:
                self._at = dt.datetime.fromisoformat(str(file.get("fetched_at")))
            except ValueError:
                self._at = None
            if self._at is not None and self._at.tzinfo is None:
                self._at = self._at.replace(tzinfo=dt.UTC)

    def _save_cache(self) -> None:
        """Mirror the calendar to disk.

        A write failure is deliberately ignored: the calendar is already in
        memory and the process can trade. Failing a sync because a cache file
        could not be written would turn a convenience into a dependency.
        """
        if self.cache_path is None or self._at is None:
            return
        payload = {"fetched_at": self._at.isoformat(), "days": self._loaded}
        try:
            self.cache_path.parent.mkdir(parents=True, exist_ok=True)
            self.cache_path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
        except OSError:
            return

    def _clock(self) -> dt.datetime:
        return self._now() if self._now is not None else dt.datetime.now(dt.UTC)


# ------------------------------------------------------------------ corporate actions

KIND_SPLIT = "split"
"""A share split: the share count multiplies by ``ratio`` and the price divides by it."""
KIND_BONUS = "bonus"
"""A bonus issue.

Arithmetically identical to a split once expressed as a ratio -- a 1:1 bonus
doubles the share count, so ``ratio`` is 2 -- and kept as a separate kind only
because the two are separate events in every data source, and collapsing them
would make an imported action impossible to reconcile against its source.
"""
KIND_DIVIDEND = "dividend"
"""A cash dividend. ``ratio`` is 1; ``amount`` carries the per-share payment."""


@dataclass(frozen=True, slots=True)
class Action:
    """One corporate action."""

    key: InstrumentKey
    ex_date: dt.date
    """The first day the instrument trades WITHOUT the entitlement.

    Bars strictly before it are adjusted; the ex-date's own bar already reflects
    the action and must not be touched.
    """
    kind: str
    ratio: float = 1.0
    """The factor the share count multiplies by: 10 for a 1:10 split, 2 for a 1:1 bonus, 1 for a dividend."""
    amount: Money = money.ZERO
    """The per-share dividend, in minor units. Zero otherwise."""


def actions(db: Database, key: InstrumentKey, start: dt.date, end: dt.date) -> list[Action]:
    """The corporate actions for an instrument with an ex-date in ``[start, end]``, oldest first."""
    rows = db.conn.execute(
        """
        SELECT ex_date, kind, ratio, amount
        FROM corporate_actions
        WHERE exchange = ? AND symbol = ? AND ex_date >= ? AND ex_date <= ?
        ORDER BY ex_date
        """,
        (key.exchange, key.symbol, start.isoformat(), end.isoformat()),
    )
    return [
        Action(
            key=key,
            ex_date=dt.date.fromisoformat(r["ex_date"]),
            kind=r["kind"],
            ratio=float(r["ratio"]),
            amount=Money(int(r["amount"])),
        )
        for r in rows
    ]


def upsert_actions(db: Database, items: list[Action]) -> None:
    """Store corporate actions, replacing one already held for the same instrument, ex-date and kind.

    ``REPLACE`` rather than ``IGNORE``, unlike candles: a corrected ratio must
    take effect, and unlike a bar an action has no history worth preserving --
    there is one right answer for a split that has already happened.
    """
    if not items:
        return
    for a in items:
        _validate(a)
    with db.tx() as conn:
        conn.executemany(
            """
            INSERT OR REPLACE INTO corporate_actions (exchange, symbol, ex_date, kind, ratio, amount)
            VALUES (?, ?, ?, ?, ?, ?)
            """,
            [(a.key.exchange, a.key.symbol, a.ex_date.isoformat(), a.kind, a.ratio, int(a.amount)) for a in items],
        )


def _validate(a: Action) -> None:
    """Reject an action that would silently corrupt a series."""
    if a.kind in (KIND_SPLIT, KIND_BONUS):
        if a.ratio <= 0:
            raise ValueError(f"reference: a {a.kind} for {a.key} needs a positive ratio, got {a.ratio}")
    elif a.kind == KIND_DIVIDEND:
        if a.amount <= 0:
            raise ValueError(f"reference: a dividend for {a.key} needs a positive amount, got {money.format(a.amount)}")
    else:
        raise ValueError(f"reference: unknown corporate action kind {a.kind!r} for {a.key}")


def adjust(candles: list[Candle], items: list[Action]) -> list[Candle]:
    """Back-adjust a bar series for corporate actions.

    Prices strictly before an action's ex-date are divided by its factor and
    volumes multiplied by it, which is the convention every charting package
    uses: the most recent bar keeps its real traded price and history is
    restated in today's terms, so an indicator computed over the series sees a
    continuous price rather than a 90% single-day crash where a 1:10 split
    happened.

    A dividend adjusts prices but NOT volume. No shares were created, so the
    share count is unchanged; only the price gapped down by the payment.

    Multiple actions compound, applied in ex-date order. Two 1:2 splits a year
    apart mean a bar before both is divided by four, and applying them in the
    wrong order -- or applying only the later one -- leaves an error large
    enough to change every result computed over the series.

    The input is not modified; an empty action list returns the input unchanged.
    """
    if not candles or not items:
        return candles

    out = [replace(c) for c in candles]
    for a in sorted(items, key=lambda a: a.ex_date):
        factor, volume_factor = _adjustment(a, out)
        if factor == 1 and volume_factor == 1:
            continue
        for c in out:
            # Compared as calendar dates, not instants: a daily bar is stamped
            # at midnight and an intraday bar at 09:15, and an instant
            # comparison would adjust the ex-date's own morning bars while
            # leaving its afternoon ones alone.
            if not _before(c.start, a.ex_date):
                continue
            c.open = money.mul_fraction(c.open, factor)
            c.high = money.mul_fraction(c.high, factor)
            c.low = money.mul_fraction(c.low, factor)
            c.close = money.mul_fraction(c.close, factor)
            if volume_factor != 1:
                c.volume = int(c.volume * volume_factor + 0.5)
    return out


def _adjustment(a: Action, candles: list[Candle]) -> tuple[float, float]:
    """The price and volume factors for one action.

    A dividend's factor depends on the price it was paid against -- the last
    close before the ex-date -- so the series is needed as well as the action.
    That close is the standard reference: the adjustment is
    ``(close - dividend) / close``, which removes exactly the value that left
    the company.
    """
    if a.kind in (KIND_SPLIT, KIND_BONUS):
        if a.ratio <= 0:
            return 1.0, 1.0
        return 1 / a.ratio, a.ratio
    if a.kind == KIND_DIVIDEND:
        last = _last_close_before(candles, a.ex_date)
        if last is None or last <= 0 or a.amount <= 0 or a.amount >= last:
            # A dividend larger than the price it was paid against is a data
            # error, and applying it would produce a negative or zero price --
            # which every indicator downstream would happily compute over.
            # Leaving the series unadjusted is a small, visible error; a
            # negative price is a large, invisible one.
            return 1.0, 1.0
        return (last - a.amount) / last, 1.0
    return 1.0, 1.0


def _last_close_before(candles: list[Candle], ex_date: dt.date) -> Money | None:
    last: Money | None = None
    for c in candles:
        if not _before(c.start, ex_date):
            break
        last = c.close
    return last


def _before(t: dt.datetime, ex_date: dt.date) -> bool:
    """Compare a bar's timestamp with the ex-date by calendar date.

    The ex-date is a date, not an instant, and the bar's own zone is the
    exchange's, so the bar's local calendar date is the one that counts.
    """
    return t.date() < ex_date
