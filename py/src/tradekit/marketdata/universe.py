"""Which instruments a system trades, and the daily bhavcopy that prices them.

Mirrors ``go/marketdata/universe``. It replaces the copy of
``ind_nifty500list.csv`` sitting in five project roots, and the NSE cookie
warm-up that two projects independently discovered.

Everything is cached to disk. An index list changes monthly and a bhavcopy never
changes at all once published, so both are cached. A backtest must run offline;
re-fetching a static file on every run makes that impossible and, against
nseindia.com, invites the rate limiting the warm-up exists to get around.

Only the standard library is used for HTTP, so the core paths need no extra.
"""

from __future__ import annotations

import csv
import datetime as dt
import http.cookiejar
import io
import threading
import urllib.error
import urllib.request
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

from tradekit.core import money
from tradekit.core.domain import Candle, Instrument, InstrumentKey, Timeframe
from tradekit.core.money import Money
from tradekit.store import Database

__all__ = [
    "Client",
    "Fetcher",
    "NotPublishedError",
    "Row",
    "SyncReport",
    "UrllibFetcher",
    "candles",
    "parse_bhavcopy",
    "parse_index_csv",
]

NSE_HOME = "https://www.nseindia.com/"
"""Fetched first, for its cookies. See :meth:`Client._warm_up`."""
INDEX_LIST_URL = "https://nsearchives.nseindia.com/content/indices/{}.csv"
"""An index's constituents, e.g. ``ind_nifty500list.csv``."""
BHAVCOPY_URL = "https://nsearchives.nseindia.com/products/content/sec_bhavdata_full_{}.csv"
"""The full securities bhavcopy for one day, stamped ``DDMMYYYY``."""

BROWSER_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)
"""Sent on every NSE request.

NSE serves 403 to a request that does not look like a browser, whatever cookies
it carries. Both donors send a desktop Chrome string; this is not evasion of a
rate limit but the minimum needed to be served at all.
"""


class NotPublishedError(LookupError):
    """NSE has no file for the requested day.

    Distinct from a failure because it is the normal answer for a holiday, or
    for today before the file is published, and a sync must not log those as
    errors.
    """


class Fetcher(Protocol):
    """One HTTP GET, returning the status and body.

    The only seam between the client and the network: a test supplies a fake
    that scripts a 403 or a 404 without a server, and production uses
    :class:`UrllibFetcher`, which carries the cookie jar the warm-up fills.
    """

    def get(self, url: str, headers: Mapping[str, str]) -> tuple[int, bytes]:  # noqa: D102 - Protocol stub
        ...


class UrllibFetcher:
    """A :class:`Fetcher` over :mod:`urllib` with a cookie jar."""

    def __init__(self, timeout: float = 30.0) -> None:
        """Build an opener that keeps cookies between requests."""
        self._opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self._timeout = timeout

    def get(self, url: str, headers: Mapping[str, str]) -> tuple[int, bytes]:
        """Perform the request. An HTTP error status is returned, not raised."""
        req = urllib.request.Request(url, headers=dict(headers))
        try:
            with self._opener.open(req, timeout=self._timeout) as resp:
                return int(resp.status), resp.read()
        except urllib.error.HTTPError as exc:
            body = exc.read() if hasattr(exc, "read") else b""
            return int(exc.code), body


class Client:
    """Fetches from NSE, caching what it downloads."""

    def __init__(
        self,
        cache_dir: str | Path | None = None,
        *,
        fetcher: Fetcher | None = None,
        base_url: str = "",
        home_url: str = "",
    ) -> None:
        """Build a client caching into ``cache_dir``.

        ``None`` disables caching, which is for tests only: a production caller
        wants the cache. ``base_url`` overrides the archives host and
        ``home_url`` the warm-up target, for a test server.
        """
        self.cache_dir = Path(cache_dir) if cache_dir else None
        self.base_url = base_url
        self.home_url = home_url
        self._fetcher: Fetcher | None = fetcher
        self._warmed = False
        self._lock = threading.Lock()

    def _http(self) -> Fetcher:
        if self._fetcher is None:
            self._fetcher = UrllibFetcher()
        return self._fetcher

    def _warm_up(self) -> None:
        """Fetch the NSE home page to obtain session cookies.

        NSE rejects a bare request to its archives with 403. Both donors
        discovered independently that fetching the home page first, and reusing
        the cookie jar, is what makes the subsequent download work. It is done
        once per client, and again after a 403, because that is what an expired
        session looks like.
        """
        self._http().get(
            self.home_url or NSE_HOME,
            {"User-Agent": BROWSER_USER_AGENT, "Accept": "text/html,application/xhtml+xml"},
        )
        with self._lock:
            self._warmed = True

    def _get(self, url: str) -> bytes:
        """Fetch a URL, warming up first and retrying once on a 403."""
        with self._lock:
            warmed = self._warmed
        if not warmed:
            try:
                self._warm_up()
            except Exception:  # a failed warm-up is not fatal; the 403 retry covers it
                with self._lock:
                    self._warmed = True

        headers = {
            "User-Agent": BROWSER_USER_AGENT,
            "Accept": "*/*",
            "Accept-Language": "en-US,en;q=0.9",
            "Referer": NSE_HOME,
        }
        for attempt in range(2):
            status, body = self._http().get(url, headers)
            if status == 200:
                return body
            if status == 403 and attempt == 0:
                # The session expired. Warm up again and retry once.
                self._warm_up()
                continue
            if status == 404:
                raise NotPublishedError(f"universe: {url}: NSE has published no file for this date")
            raise RuntimeError(f"universe: {url}: HTTP {status}")
        raise RuntimeError(f"universe: {url}: refused twice, even after warming up")

    def _fetch_cached(self, url: str, cache_name: str) -> bytes:
        """A file's bytes, reading the cache when it holds them."""
        if self.cache_dir is None:
            return self._get(url)
        path = self.cache_dir / cache_name
        try:
            body = path.read_bytes()
        except OSError:
            body = b""
        if body:
            return body
        body = self._get(url)
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        path.write_bytes(body)
        return body

    def _archives(self, template: str, arg: str) -> str:
        """The archives URL, honouring ``base_url``.

        The scheme and host are replaced and the path kept, so a test server
        sees the same paths production does.
        """
        url = template.format(arg)
        if not self.base_url:
            return url
        rest = url[len("https://") :]
        slash = rest.find("/")
        return self.base_url.rstrip("/") + (rest[slash:] if slash >= 0 else "")

    def index_constituents(self, index: str) -> list[InstrumentKey]:
        """The instruments in an NSE index.

        ``index`` is the file's base name, ``ind_nifty500list`` for the Nifty
        500. The result is cached: an index list changes monthly, and
        re-downloading it on every run is both wasteful and the reason five
        projects ended up with a stale copy checked into their repositories.
        """
        return [i.key for i in self.constituents(index)]

    def constituents(self, index: str) -> list[Instrument]:
        """The index's members with the name the list carries, for a caller populating the instruments table."""
        body = self._fetch_cached(self._archives(INDEX_LIST_URL, index), index + ".csv")
        return parse_index_csv(body)

    def bhavcopy(self, day: dt.date) -> list[Row]:
        """The day's full securities file.

        A day NSE has published nothing for raises :class:`NotPublishedError`,
        which is the normal answer for a weekend, a holiday, or today before
        publication. It is cached: a published bhavcopy never changes.
        """
        stamp = day.strftime("%d%m%Y")
        body = self._fetch_cached(self._archives(BHAVCOPY_URL, stamp), f"sec_bhavdata_full_{stamp}.csv")
        return parse_bhavcopy(body, day)

    def sync(self, db: Database, index: str) -> SyncReport:
        """Bring the instruments table into line with an index.

        Instruments that left the index are deactivated, never deleted. Deleting
        one orphans every candle and trade that references it -- and those rows
        are the history a backtest of the strategy that held it is computed
        from.
        """
        instruments = self.constituents(index)
        if not instruments:
            # Refusing an empty list is deliberate: an NSE outage that served
            # an empty file would otherwise deactivate the entire universe,
            # and the next run would find nothing to trade.
            raise ValueError(
                f"universe: index {index!r} returned no constituents; refusing to deactivate the whole universe"
            )
        db.upsert_instruments(instruments)
        deactivated = db.deactivate_missing("NSE", [i.key for i in instruments])
        return SyncReport(fetched=len(instruments), deactivated=deactivated)


@dataclass(frozen=True, slots=True)
class SyncReport:
    """What one universe sync did."""

    fetched: int
    """The index's members."""
    deactivated: int
    """The instruments that left the index."""


_BOM = "﻿"
"""What a Windows-authored CSV begins with.

It would otherwise make the first header name unmatchable -- the column is
there, the lookup misses, and the parser reports a file with no symbol column.
"""

# Column aliases accepted in an index or watchlist CSV. The published NSE list
# uses "Company Name"/"Industry"/"Symbol"; ad-hoc watchlist exports use
# lowercase names and often suffix symbols with ".NS". Reading by name rather
# than position is what lets one parser handle both, which is neev's approach
# and the reason its universe loader survived several changes to the published
# format.
_SYMBOL_COLUMNS = ("symbol",)
_NAME_COLUMNS = ("company name", "name")
# The ISIN is what Upstox keys cash equity by ("NSE_EQ|INE002A01018"), so a
# universe without it cannot be traded there.
_ISIN_COLUMNS = ("isin code", "isin")


def _decode(body: bytes) -> str:
    return body.decode("utf-8", errors="replace")


def parse_index_csv(body: bytes) -> list[Instrument]:
    """Read an index constituent list.

    A row with no symbol is skipped rather than failing the batch: the published
    files carry trailing blank lines, and refusing the whole universe over one
    is how a sync comes to do nothing at all.

    An index list's "Industry" column is a sector, which the domain has no field
    for; segment means the instrument class. Everything in an equity index is
    equity, so the sector is dropped rather than smuggled into a field that
    means something else.
    """
    reader = csv.reader(io.StringIO(_decode(body), newline=""))
    try:
        header = next(reader)
    except StopIteration:
        raise ValueError("universe: reading index list header: file is empty") from None
    col = {name.removeprefix(_BOM).strip().lower(): i for i, name in enumerate(header)}
    symbol_at = _find_column(col, _SYMBOL_COLUMNS)
    if symbol_at < 0:
        raise ValueError(f"universe: index list has no symbol column; header was {header}")
    name_at = _find_column(col, _NAME_COLUMNS)
    isin_at = _find_column(col, _ISIN_COLUMNS)

    out: list[Instrument] = []
    for row in reader:
        symbol = _clean_symbol(_at(row, symbol_at))
        if not symbol:
            continue
        out.append(
            Instrument(
                key=InstrumentKey("NSE", symbol),
                name=_at(row, name_at),
                isin=_at(row, isin_at).strip().upper(),
                segment="equity",
                lot_size=1,
                tick_size=Money(5),
                active=True,
            )
        )
    return out


@dataclass(frozen=True, slots=True)
class Row:
    """One line of the full securities bhavcopy."""

    key: InstrumentKey
    series: str
    """Distinguishes EQ and BE from the many non-equity series in the same file."""
    date: dt.date
    open: Money
    high: Money
    low: Money
    close: Money
    prev_close: Money = money.ZERO
    volume: int = 0
    deliverable_qty: int = 0
    """The delivery figures the file carries and no candle feed does.

    They are why a screener reads the bhavcopy at all rather than the broker's
    own history.
    """
    deliverable_pct: float = 0.0


_PRICE_COLUMNS = ("OPEN_PRICE", "HIGH_PRICE", "LOW_PRICE", "CLOSE_PRICE")
_EQUITY_SERIES = frozenset({"EQ", "BE"})
"""The series a cash-equity strategy trades.

The bhavcopy carries dozens more -- government securities, warrants, partly paid
shares -- and treating them as equity puts instruments in a universe that cannot
be traded the way the strategy assumes.
"""


def parse_bhavcopy(body: bytes, day: dt.date) -> list[Row]:
    """Read the full securities bhavcopy.

    A malformed row is skipped rather than failing the batch. The file is a few
    thousand rows and a single unparseable price -- the file uses ``-`` for an
    absent delivery figure, among others -- must not cost the whole day's data.
    """
    reader = csv.reader(io.StringIO(_decode(body), newline=""), skipinitialspace=True)
    try:
        header = next(reader)
    except StopIteration:
        raise ValueError("universe: reading bhavcopy header: file is empty") from None
    col = {name.strip().upper(): i for i, name in enumerate(header)}

    out: list[Row] = []
    for record in reader:
        symbol = _field(col, record, "SYMBOL")
        series = _field(col, record, "SERIES").upper()
        if not symbol or series not in _EQUITY_SERIES:
            continue
        prices = [_parse_price(_field(col, record, n)) for n in _PRICE_COLUMNS]
        if any(p is None for p in prices):
            continue
        open_, high, low, close = (p for p in prices if p is not None)
        out.append(
            Row(
                key=InstrumentKey("NSE", symbol),
                series=series,
                date=day,
                open=open_,
                high=high,
                low=low,
                close=close,
                prev_close=_parse_price(_field(col, record, "PREV_CLOSE")) or money.ZERO,
                volume=_parse_int(_field(col, record, "TTL_TRD_QNTY")),
                deliverable_qty=_parse_int(_field(col, record, "DELIV_QTY")),
                deliverable_pct=_parse_float(_field(col, record, "DELIV_PER")) or 0.0,
            )
        )
    return out


def candles(rows: list[Row]) -> list[Candle]:
    """Convert bhavcopy rows into daily bars.

    The bar is stamped at midnight IST, the instant the CSV tree and the store
    use for a daily bar, so a bhavcopy-sourced bar and a broker-sourced one for
    the same day dedupe in the store rather than coexisting.
    """
    from .feed import IST

    return [
        Candle(
            key=r.key,
            timeframe=Timeframe.D1,
            start=dt.datetime(r.date.year, r.date.month, r.date.day, tzinfo=IST),
            open=r.open,
            high=r.high,
            low=r.low,
            close=r.close,
            volume=r.volume,
        )
        for r in rows
    ]


def _field(col: Mapping[str, int], record: list[str], name: str) -> str:
    """A bhavcopy field by header name, tolerating a short row."""
    i = col.get(name)
    if i is None or i >= len(record):
        return ""
    return record[i].strip()


def _find_column(col: Mapping[str, int], candidates: tuple[str, ...]) -> int:
    for c in candidates:
        if c in col:
            return col[c]
    return -1


def _at(row: list[str], i: int) -> str:
    if i < 0 or i >= len(row):
        return ""
    return row[i].strip()


def _clean_symbol(s: str) -> str:
    """Normalise a symbol, stripping the ``.NS`` suffix a watchlist export carries."""
    return s.strip().upper().removesuffix(".NS")


def _parse_float(s: str) -> float | None:
    s = s.strip()
    if s in ("", "-"):
        return None
    try:
        return float(s)
    except ValueError:
        return None


def _parse_price(s: str) -> Money | None:
    """Read a rupee price into paise, or ``None`` when it is not readable."""
    v = _parse_float(s)
    return None if v is None else money.from_major(v)


def _parse_int(s: str) -> int:
    v = _parse_float(s)
    return 0 if v is None else int(v)
