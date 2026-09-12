"""The ``BarFeed`` port and its three sources: memory, the store, and a CSV tree.

Mirrors ``go/marketdata/feed.go`` and ``export.go``. SQLite is the canonical
store and CSV is a view: a CSV tree cannot answer "what is the last bar I have
for RELIANCE" without reading the whole file, and that is the query the entire
sync layer is built on. :func:`from_csv` exists so zerobha's backtester keeps
working unchanged through its migration, not as a source of truth.
"""

from __future__ import annotations

import csv
import datetime as dt
import io
from collections.abc import Iterable, Iterator
from pathlib import Path
from typing import Protocol, runtime_checkable

from tradekit.core import money
from tradekit.core.domain import Candle, InstrumentKey, Timeframe
from tradekit.store import Database

__all__ = [
    "IST",
    "BarFeed",
    "SliceFeed",
    "csv_path",
    "export_csv",
    "from_csv",
    "from_slice",
    "from_store",
    "parse_csv_time",
    "read_csv",
]

IST = dt.timezone(dt.timedelta(hours=5, minutes=30), "IST")
"""The exchange's own clock, used for the timestamps that carry no zone.

A fixed offset rather than a loaded zone deliberately: India has no daylight
saving, so the offset is constant, and a fixed zone cannot fail at startup on a
machine with no tzdata. Anything that needs holiday-aware session logic uses
:mod:`tradekit.core.calendar`, which does load the real zone.
"""


@runtime_checkable
class BarFeed(Protocol):
    """Yields bars in ascending time order.

    An iterator rather than a list because a five-year minute series is millions
    of bars, and a backtest consumes them one at a time. A feed backed by a list
    is free; a feed backed by a query need not materialise the whole result.

    An error ends the feed: a caller must not treat an exception as a gap and
    carry on, because a truncated series produces a complete-looking backtest
    over the wrong data.
    """

    def __iter__(self) -> Iterator[Candle]:  # noqa: D105 - a Protocol stub under a documented class
        ...


class SliceFeed:
    """A feed over bars already in memory: the reference implementation."""

    def __init__(self, candles: Iterable[Candle]) -> None:
        """Wrap ``candles``, which must already be in ascending time order."""
        self._candles = list(candles)

    def __iter__(self) -> Iterator[Candle]:
        """Yield the bars in order."""
        return iter(self._candles)


def from_slice(candles: Iterable[Candle]) -> BarFeed:
    """A feed over bars already in memory, for tests and resampled series."""
    return SliceFeed(candles)


def from_store(db: Database, key: InstrumentKey, timeframe: Timeframe, start: dt.datetime, end: dt.datetime) -> BarFeed:
    """A feed over the bars held for one instrument and timeframe in ``[start, end]``.

    The rows are read eagerly. A window a backtest actually replays is bounded by
    its own date range, and a streaming cursor would hold a SQLite read
    transaction open for the length of the run -- which, with the store's
    single-connection default, blocks the sync that is trying to write into it.
    """
    return SliceFeed(db.candles(key, timeframe, start, end))


def from_csv(path: str | Path, key: InstrumentKey, timeframe: Timeframe) -> BarFeed:
    """A feed over one of zerobha's bar files.

    ``path`` is the file itself, so a caller with a different layout is not
    forced into this one; :func:`csv_path` builds the conventional location.
    """
    with Path(path).open(encoding="utf-8", newline="") as f:
        return SliceFeed(read_csv(f, key, timeframe))


def csv_path(directory: str | Path, interval: str, symbol: str) -> Path:
    """The conventional location of one instrument's bar file in zerobha's tree.

    ``<dir>/<interval>/<symbol>_real.csv``, lowercased. ``interval`` is the
    directory name, which is the vendor's interval spelling rather than a
    :class:`Timeframe`: the existing tree carries both ``1d`` and ``day``, and
    both ``5m`` and ``5minute``, written by different tools at different times.
    Making the caller name the directory keeps this function from guessing.
    """
    return Path(directory) / interval / f"{symbol.lower()}_real.csv"


_REQUIRED_COLUMNS = ("timestamp", "open", "high", "low", "close")


def read_csv(source: io.TextIOBase | Iterable[str], key: InstrumentKey, timeframe: Timeframe) -> list[Candle]:
    """Parse a bar file.

    Columns are located by header name, not by position. The tree zerobha
    carries today holds both orderings -- ``cmd/histdl`` writes
    ``timestamp,open,high,low,close,volume`` while the older files are
    ``timestamp,close,high,low,open,volume`` -- so a positional reader silently
    transposes open and close on half the tree, which is exactly the kind of
    error a backtest cannot detect from its own results.

    A malformed row raises rather than being skipped, unlike the instrument
    master: a bar file is this series and nothing else, and quietly dropping a
    bar from it changes every indicator computed over the series without saying
    so.
    """
    reader = csv.reader(source)
    try:
        header = next(reader)
    except StopIteration:
        raise ValueError("marketdata: file is empty") from None

    col = {name.strip().lower(): i for i, name in enumerate(header)}
    for required in _REQUIRED_COLUMNS:
        if required not in col:
            raise ValueError(f"marketdata: file has no {required!r} column; header was {header}")

    out: list[Candle] = []
    for line, row in enumerate(reader, start=2):
        if not row:
            continue
        try:
            out.append(_row_to_candle(row, col, key, timeframe))
        except ValueError as exc:
            raise ValueError(f"marketdata: line {line}: {exc}") from None
    return out


def _row_to_candle(row: list[str], col: dict[str, int], key: InstrumentKey, timeframe: Timeframe) -> Candle:
    def field(name: str) -> str:
        i = col.get(name)
        if i is None or i >= len(row):
            return ""
        return row[i].strip()

    def price(name: str) -> money.Money:
        raw = field(name)
        try:
            return money.from_major(float(raw))
        except ValueError:
            raise ValueError(f"{name} {raw!r}: not a number") from None

    def count(name: str) -> int:
        raw = field(name)
        if raw == "":
            return 0
        try:
            return int(float(raw))
        except ValueError:
            raise ValueError(f"{name} {raw!r}: not a number") from None

    return Candle(
        key=key,
        timeframe=timeframe,
        start=parse_csv_time(field("timestamp")),
        open=price("open"),
        high=price("high"),
        low=price("low"),
        close=price("close"),
        volume=count("volume"),
        open_interest=count("open_interest"),
    )


def parse_csv_time(s: str) -> dt.datetime:
    """Read a bar file's timestamp.

    Four layouts are accepted because the files were written by three different
    tools over two years: histdl emits RFC 3339 with an IST offset, an older
    exporter emitted a UTC offset with no colon, and an older one still emitted
    a naive local datetime or a bare date. A naive timestamp is interpreted as
    IST, which is what it meant when it was written: these are NSE bars, and
    reading them as UTC would shift every bar five and a half hours and put the
    whole session in the wrong day.
    """
    if s == "":
        raise ValueError("empty timestamp")
    try:
        t = dt.datetime.fromisoformat(s)
    except ValueError:
        raise ValueError(f"timestamp {s!r} matches none of the known layouts") from None
    if t.tzinfo is None:
        t = t.replace(tzinfo=IST)
    return t


CSV_HEADER = ("timestamp", "open", "high", "low", "close", "volume")
"""The column order zerobha's ``cmd/histdl`` writes.

The order matters for byte-comparability with the existing tree, and only for
that: :func:`read_csv` locates columns by name.
"""


def export_csv(
    db: Database, directory: str | Path, interval: str, keys: Iterable[InstrumentKey], timeframe: Timeframe
) -> None:
    """Write each instrument's bars into zerobha's tree layout.

    It exists so zerobha's backtester keeps reading the files it already reads
    while its data moves into the store. Migrating the storage and rewriting the
    backtester at the same time makes a change nobody can review; this is what
    splits them.

    An instrument with no stored bars is skipped rather than written as a
    header-only file: histdl skips it too, and an empty file reads as an
    instrument with no history rather than one that was never exported.
    """
    target = Path(directory) / interval
    target.mkdir(parents=True, exist_ok=True)

    # The whole stored range, which is what histdl writes: the file is the
    # instrument's history, not a window into it.
    start = dt.datetime.fromtimestamp(0, dt.UTC)
    end = dt.datetime.now(dt.UTC) + dt.timedelta(days=366)

    for key in keys:
        candles = db.candles(key, timeframe, start, end)
        if not candles:
            continue
        _write_csv(csv_path(directory, interval, key.symbol), candles)


def _write_csv(path: Path, candles: list[Candle]) -> None:
    """Write one instrument's bars.

    Prices are written with two decimals and timestamps in RFC 3339 with the
    exchange's offset, matching histdl and the Go exporter byte for byte. The
    line terminator is LF rather than the csv module's CRLF default for the
    same reason.
    """
    with path.open("w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(CSV_HEADER)
        for c in candles:
            w.writerow(
                [
                    c.start.astimezone(IST).replace(microsecond=0).isoformat(),
                    money.format(c.open),
                    money.format(c.high),
                    money.format(c.low),
                    money.format(c.close),
                    str(c.volume),
                ]
            )
