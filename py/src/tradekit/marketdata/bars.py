"""Build bars from ticks, convert them between timeframes, and measure a session against the sessions before it.

Mirrors ``go/marketdata/bars``. The aggregator replaces zerobha's, the only
tick-to-bar builder in the codebase, and the session-slot machinery replaces
breakout500's volume profile and relative volume -- which is where that
system's edge actually comes from.

Everything here anchors to the session open through
:meth:`~tradekit.core.calendar.Session.bucket_start` rather than to the wall
clock. With a 09:15 open and a 5-minute bar the two disagree from the first bar
onward, and a series bucketed by wall clock does not line up with the
exchange's own.
"""

from __future__ import annotations

import datetime as dt
import math
from collections.abc import Callable
from dataclasses import replace

from tradekit.core.calendar import Session
from tradekit.core.domain import Candle, Tick, Timeframe

__all__ = ["Builder", "relative_volume", "resample", "volume_profile"]


class Builder:
    """Aggregates ticks into bars, anchored to the session open.

    Not safe for concurrent use: a tick feed delivers on one thread, and
    serialising here would hide a caller fanning ticks out across several,
    which would interleave them and corrupt every bar.
    """

    def __init__(self, session: Session, timeframe: Timeframe, emit: Callable[[Candle], None]) -> None:
        """Build one that calls ``emit`` with each completed bar.

        ``emit`` is called synchronously from :meth:`add`, so a slow handler
        back-pressures the tick feed. That is deliberate: dropping a bar to keep
        up would leave a gap nothing downstream can detect.
        """
        self._session = session
        self._timeframe = timeframe
        self._emit = emit
        self._current: Candle | None = None

    def add(self, tick: Tick) -> None:
        """Ingest one tick.

        A tick before the session open lands in the first bar rather than in a
        phantom bar of its own -- ``bucket_start`` clamps it -- so a pre-open
        print does not create a bar the exchange never had.

        A tick that arrives out of order, stamped earlier than the bar already
        open, is folded into the current bar rather than opening an earlier
        one. A feed that reorders across a bucket boundary would otherwise emit
        a bar, then re-open it, and the consumer would see the same bucket
        twice.
        """
        start = self._session.bucket_start(tick.at, self._timeframe)
        if self._current is not None and start > self._current.start:
            self._close()
        if self._current is None:
            self._current = Candle(
                key=tick.key,
                timeframe=self._timeframe,
                start=start,
                open=tick.price,
                high=tick.price,
                low=tick.price,
                close=tick.price,
                volume=tick.volume,
            )
            return
        bar = self._current
        bar.high = max(bar.high, tick.price)
        bar.low = min(bar.low, tick.price)
        bar.close = tick.price
        bar.volume += tick.volume

    def flush(self) -> None:
        """Emit the bar still forming, if any.

        Must be called at the close and at shutdown. Without it the session's
        last bar is never emitted, which is the bar an end-of-day exit is priced
        from.
        """
        if self._current is not None:
            self._close()

    def _close(self) -> None:
        assert self._current is not None
        self._emit(self._current)
        self._current = None


def resample(candles: list[Candle], to: Timeframe, session: Session) -> list[Candle]:
    """Convert a bar series to a coarser timeframe.

    Bars are grouped by the coarser timeframe's session-anchored bucket: the
    group's open is its first bar's open, its close its last bar's close, its
    high and low the extremes, and its volume the sum. Open interest takes the
    last value rather than a sum, because it is a level and not a flow --
    summing it would multiply the outstanding contracts by the number of bars.

    Resampling to a finer timeframe is an error, not an interpolation. There is
    no information in a 15-minute bar about what happened in its third minute,
    and inventing some produces a backtest that trades on data that never
    existed.

    The input must be in ascending time order, which is what every feed yields.
    """
    if not candles:
        return []
    _check_coarser(candles[0].timeframe, to)

    out: list[Candle] = []
    bucket: dt.datetime | None = None
    for c in candles:
        start = session.bucket_start(c.start, to)
        if bucket is None or start != bucket:
            out.append(replace(c, timeframe=to, start=start))
            bucket = start
            continue
        agg = out[-1]
        agg.high = max(agg.high, c.high)
        agg.low = min(agg.low, c.low)
        agg.close = c.close
        agg.volume += c.volume
        agg.open_interest = c.open_interest
    return out


def _check_coarser(source: Timeframe, to: Timeframe) -> None:
    """Raise unless resampling from ``source`` to ``to`` is a widening."""
    if source == to:
        return
    src, dst = source.duration, to.duration
    zero = dt.timedelta(0)
    if src > zero and dst > zero:
        if dst < src:
            raise ValueError(
                f"bars: cannot resample {source} to the finer {to}; "
                "a coarser bar carries no information about what happened inside it"
            )
        if dst % src != zero:
            raise ValueError(
                f"bars: cannot resample {source} to {to}; the source bars do not divide the target evenly, "
                "so a target bar would be built from a partial set"
            )
    elif src == zero and dst > zero:
        # Daily or weekly down to intraday.
        raise ValueError(f"bars: cannot resample {source} to the finer {to}")
    elif source == Timeframe.W1 and to == Timeframe.D1:
        # Both calendar-sized, so neither has a duration to compare; a week
        # is still coarser than a day.
        raise ValueError(f"bars: cannot resample {source} to the finer {to}")


def volume_profile(candles: list[Candle], session: Session, timeframe: Timeframe, lookback_days: int) -> list[float]:
    """The typical cumulative volume by session slot.

    For each bar, the value is the median cumulative volume through that slot
    across the previous ``lookback_days`` sessions. Median rather than mean,
    following breakout500: one frantic session should not set the benchmark for
    the next month.

    Only sessions STRICTLY BEFORE the one being evaluated are used. Including
    the current session's own volume in its own benchmark is the classic
    lookahead bug: the ratio it produces is partly a measurement of itself, and
    a screener built on it looks profitable in backtest and is not.

    The result is aligned to ``candles``, with NaN where there is not enough
    history -- never zero. A zero benchmark would make relative volume infinite,
    and a zero-filled warm-up biases every average over the series, which is the
    same rule :mod:`tradekit.core.indicators` follows.
    """
    out = [math.nan] * len(candles)
    if not candles or lookback_days <= 0:
        return out

    sessions: dict[str, dict[int, int]] = {}
    cumulative: dict[str, int] = {}
    slots = [session.slot_index(c.start, timeframe) for c in candles]
    dates = [session.date_key(c.start) for c in candles]
    for c, slot, day in zip(candles, slots, dates, strict=True):
        if slot < 0:
            continue
        per_slot = sessions.setdefault(day, {})
        cumulative[day] = cumulative.get(day, 0) + c.volume
        per_slot[slot] = cumulative[day]
    order = sorted(sessions)
    position = {day: i for i, day in enumerate(order)}

    # A benchmark from two sessions is noise. Half the lookback, floored at
    # two, is breakout500's minimum and is what keeps the first fortnight of a
    # new instrument from producing confident nonsense.
    min_sessions = max(2, lookback_days // 2)

    for i, (slot, day) in enumerate(zip(slots, dates, strict=True)):
        if slot < 0:
            continue
        at = position[day]
        prior = [
            float(sessions[order[j]][slot])
            for j in range(max(0, at - lookback_days), at)  # strictly before: never at itself
            if slot in sessions[order[j]]
        ]
        if len(prior) < min_sessions:
            continue
        out[i] = _median(prior)
    return out


def relative_volume(candles: list[Candle], session: Session, timeframe: Timeframe, lookback_days: int) -> list[float]:
    """Each bar's cumulative session volume over the typical value for its slot.

    1.0 means the session is tracking its normal pace; 2.0 means twice the usual
    volume has traded by this point in the day. NaN where the profile has no
    value, so a caller cannot mistake "no history" for "no unusual volume".
    """
    profile = volume_profile(candles, session, timeframe, lookback_days)
    out = [math.nan] * len(candles)
    cumulative: dict[str, int] = {}
    for i, c in enumerate(candles):
        day = session.date_key(c.start)
        cumulative[day] = cumulative.get(day, 0) + c.volume
        if math.isnan(profile[i]) or profile[i] <= 0:
            continue
        out[i] = cumulative[day] / profile[i]
    return out


def _median(values: list[float]) -> float:
    """The middle value, averaging the two middle ones for an even count."""
    if not values:
        return math.nan
    s = sorted(values)
    mid = len(s) // 2
    if len(s) % 2 == 1:
        return s[mid]
    return (s[mid - 1] + s[mid]) / 2
