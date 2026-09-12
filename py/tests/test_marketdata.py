"""The marketdata mirror: feeds, CSV, sync, universe, reference data, bars.

Mirrors ``go/marketdata``'s suites, plus the shared ``marketdata`` block of
``contracts/testdata/parity.json`` that ``go/marketdata/parity_test.go`` also
runs. No network: the NSE client is driven through a scripted fetcher.
"""

from __future__ import annotations

import datetime as dt
import itertools
import json
import math
import threading
import time
from dataclasses import replace
from pathlib import Path
from typing import Any

import pytest

from tradekit.core import calendar, money
from tradekit.core.domain import Candle, Instrument, InstrumentKey, Tick, Timeframe
from tradekit.core.money import Money
from tradekit.marketdata import (
    IST,
    BarFeed,
    bars,
    csv_path,
    export_csv,
    from_csv,
    from_slice,
    from_store,
    parse_csv_time,
    read_csv,
    reference,
    sync,
    universe,
)
from tradekit.store import Database, connect

FIXTURE = Path(__file__).resolve().parents[2] / "contracts" / "testdata" / "parity.json"
RELIANCE = InstrumentKey("NSE", "RELIANCE")
TCS = InstrumentKey("NSE", "TCS")
UTC = dt.UTC


@pytest.fixture
def db() -> Database:
    database = connect(":memory:")
    database.migrate()
    yield database
    database.close()


@pytest.fixture(scope="module")
def parity() -> dict[str, Any]:
    return json.loads(FIXTURE.read_text())["marketdata"]


def _bar(start: dt.datetime, price: int, volume: int = 100, tf: Timeframe = Timeframe.D1) -> Candle:
    p = Money(price)
    return Candle(key=RELIANCE, timeframe=tf, start=start, open=p, high=p, low=p, close=p, volume=volume)


def _daily(days: int, start: dt.datetime | None = None) -> list[Candle]:
    first = start or dt.datetime(2025, 4, 14, tzinfo=IST)
    return [
        Candle(
            key=RELIANCE,
            timeframe=Timeframe.D1,
            start=first + dt.timedelta(days=i),
            open=Money(10000 + i * 100),
            high=Money(10200 + i * 100),
            low=Money(9800 + i * 100),
            close=Money(10050 + i * 100),
            volume=1000 * (i + 1),
        )
        for i in range(days)
    ]


def _parity_bar(raw: dict[str, Any], tf: Timeframe) -> Candle:
    return Candle(
        key=RELIANCE,
        timeframe=tf,
        start=dt.datetime.fromisoformat(raw["start"]),
        open=Money(raw["open"]),
        high=Money(raw["high"]),
        low=Money(raw["low"]),
        close=Money(raw["close"]),
        volume=raw["volume"],
        open_interest=raw["open_interest"],
    )


def _same_bar(got: Candle, want: dict[str, Any]) -> bool:
    return (
        got.start == dt.datetime.fromisoformat(want["start"])
        and got.open == want["open"]
        and got.high == want["high"]
        and got.low == want["low"]
        and got.close == want["close"]
        and got.volume == want["volume"]
        and got.open_interest == want["open_interest"]
    )


# ------------------------------------------------------------------ feeds and CSV


def test_slice_and_csv_feeds_agree_on_the_same_bars(tmp_path: Path) -> None:
    want = _daily(1, dt.datetime(2025, 4, 17, tzinfo=IST))
    path = csv_path(tmp_path, "1d", "RELIANCE")
    path.parent.mkdir(parents=True)
    path.write_text("timestamp,open,high,low,close,volume\n2025-04-17T00:00:00+05:30,100.00,102.00,98.00,100.50,1000\n")

    from_file = list(from_csv(path, RELIANCE, Timeframe.D1))
    from_memory = list(from_slice(want))
    assert from_file == from_memory, "the two feeds must yield the same bar for the same data"
    assert isinstance(from_csv(path, RELIANCE, Timeframe.D1), BarFeed)


def test_read_csv_locates_columns_by_name() -> None:
    # The two orderings that coexist in zerobha's tree today. A positional
    # reader gets the second one exactly backwards.
    histdl = ["timestamp,open,high,low,close,volume", "2025-04-17T09:15:00+05:30,100.00,101.00,99.00,100.50,1000"]
    older = ["timestamp,close,high,low,open,volume", "2025-04-17T09:15:00+05:30,100.50,101.00,99.00,100.00,1000"]
    a = read_csv(histdl, RELIANCE, Timeframe.M5)
    b = read_csv(older, RELIANCE, Timeframe.M5)
    assert a == b, "both column orders must read to the same bar"
    assert a[0].open == money.parse("100.00") and a[0].close == money.parse("100.50")


def test_read_csv_rejects_a_file_missing_a_price_column() -> None:
    with pytest.raises(ValueError, match="no 'close' column"):
        read_csv(["timestamp,open,high,low,volume", "2025-04-17,1,1,1,1"], RELIANCE, Timeframe.D1)


def test_read_csv_fails_on_a_malformed_row_rather_than_skipping_it() -> None:
    rows = [
        "timestamp,open,high,low,close,volume",
        "2025-04-17,100,101,99,100.5,1000",
        "2025-04-18,abc,101,99,100.5,1000",
    ]
    with pytest.raises(ValueError, match="line 3"):
        read_csv(rows, RELIANCE, Timeframe.D1)


def test_read_csv_rejects_an_empty_file() -> None:
    with pytest.raises(ValueError, match="empty"):
        read_csv([], RELIANCE, Timeframe.D1)


def test_parse_csv_time_accepts_every_layout_in_the_tree() -> None:
    want = dt.datetime(2025, 4, 17, 9, 15, tzinfo=IST)
    for raw in ("2025-04-17T09:15:00+05:30", "2025-04-17T09:15:00+0530", "2025-04-17T09:15:00", "2025-04-17T03:45:00Z"):
        assert parse_csv_time(raw) == want, raw
    assert parse_csv_time("2025-04-17") == dt.datetime(2025, 4, 17, tzinfo=IST)


def test_parse_csv_time_reads_a_naive_stamp_as_ist() -> None:
    got = parse_csv_time("2025-04-17T09:15:00")
    assert got.utcoffset() == dt.timedelta(hours=5, minutes=30), (
        "a naive stamp read as UTC would shift every bar five and a half hours into the wrong session"
    )


def test_parse_csv_time_rejects_garbage() -> None:
    for raw in ("", "yesterday", "17/04/2025"):
        with pytest.raises(ValueError):
            parse_csv_time(raw)


def test_csv_path_matches_the_existing_tree(tmp_path: Path) -> None:
    assert csv_path(tmp_path, "5minute", "RELIANCE") == tmp_path / "5minute" / "reliance_real.csv"


def test_from_store_yields_the_stored_bars_in_order_and_bounds_the_range(db: Database) -> None:
    stored = _daily(5)
    db.upsert_candles(stored)
    got = list(from_store(db, RELIANCE, Timeframe.D1, stored[1].start, stored[3].start))
    assert got == stored[1:4], "the feed must honour both ends of the inclusive range"


def test_export_csv_matches_the_histdl_layout_byte_for_byte(db: Database, tmp_path: Path) -> None:
    db.upsert_candles(_daily(3))
    export_csv(db, tmp_path, "day", [RELIANCE, TCS], Timeframe.D1)

    raw = csv_path(tmp_path, "day", "RELIANCE").read_bytes()
    assert raw == (
        b"timestamp,open,high,low,close,volume\n"
        b"2025-04-14T00:00:00+05:30,100.00,102.00,98.00,100.50,1000\n"
        b"2025-04-15T00:00:00+05:30,101.00,103.00,99.00,101.50,2000\n"
        b"2025-04-16T00:00:00+05:30,102.00,104.00,100.00,102.50,3000\n"
    ), "the file must be what cmd/histdl and go/marketdata write, LF line endings included"
    assert not csv_path(tmp_path, "day", "TCS").exists(), (
        "an instrument with no bars is skipped; a header-only file reads as an instrument with no history"
    )


def test_exported_csv_reads_back_identically(db: Database, tmp_path: Path) -> None:
    stored = _daily(3)
    db.upsert_candles(stored)
    export_csv(db, tmp_path, "day", [RELIANCE], Timeframe.D1)
    assert list(from_csv(csv_path(tmp_path, "day", "RELIANCE"), RELIANCE, Timeframe.D1)) == stored


# ------------------------------------------------------------------ sync: chunks and cadence


def test_chunks_are_contiguous_with_no_gap_or_overlap() -> None:
    start = dt.datetime(2025, 1, 1, tzinfo=UTC)
    end = start + dt.timedelta(days=100)
    span = dt.timedelta(days=25)
    windows = sync.chunks(start, end, span)
    assert windows[0].start == start and windows[-1].end == end
    for prev, cur in itertools.pairwise(windows):
        assert cur.start - prev.end == dt.timedelta(seconds=1), (
            "anything but one second is a gap that loses bars or an overlap that re-requests them"
        )
    for w in windows:
        assert w.end >= w.start and w.end - w.start < span


def test_chunks_cover_every_instant_exactly_once() -> None:
    start = dt.datetime(2025, 1, 1, tzinfo=UTC)
    end = start + dt.timedelta(days=63)
    windows = sync.chunks(start, end, dt.timedelta(days=10))
    t = start
    while t <= end:
        assert sum(1 for w in windows if w.start <= t <= w.end) == 1, t
        t += dt.timedelta(hours=6)


def test_parity_chunks(parity: dict[str, Any]) -> None:
    for case in parity["chunks"]:
        got = sync.chunks(
            dt.datetime.fromisoformat(case["from"]),
            dt.datetime.fromisoformat(case["to"]),
            dt.timedelta(seconds=case["span_seconds"]),
        )
        want = [(dt.datetime.fromisoformat(w["from"]), dt.datetime.fromisoformat(w["to"])) for w in case["want"]]
        assert [(w.start, w.end) for w in got] == want, case["name"]


def test_due_gates_on_age() -> None:
    now = dt.datetime(2025, 4, 17, 9, tzinfo=UTC)
    assert sync.due(sync.DAILY, None, now, False), "a task that has never succeeded is always due"
    assert not sync.due(sync.DAILY, now - dt.timedelta(hours=1), now, False)
    assert sync.due(sync.DAILY, now - dt.timedelta(hours=25), now, False)
    assert sync.due(sync.DAILY, now - dt.timedelta(hours=1), now, True), "force must override the gate"
    assert sync.due(sync.ALWAYS, now, now, False)
    assert not sync.due(sync.WEEKLY, now - dt.timedelta(hours=48), now, False)


# ------------------------------------------------------------------ sync: backfill


class _History:
    """A HistoryProvider serving one daily bar per day of the requested range."""

    def __init__(self, dead: set[InstrumentKey] | None = None) -> None:
        self.calls: list[tuple[InstrumentKey, dt.datetime, dt.datetime]] = []
        self.dead = dead or set()

    def candles(self, key: InstrumentKey, timeframe: Timeframe, start: dt.datetime, end: dt.datetime) -> list[Candle]:
        self.calls.append((key, start, end))
        if key in self.dead:
            raise RuntimeError("delisted")
        out = []
        day = start.astimezone(IST).replace(hour=0, minute=0, second=0, microsecond=0)
        if day < start:
            day += dt.timedelta(days=1)
        while day <= end:
            out.append(replace(_bar(day, 10000, 100), key=key))
            day += dt.timedelta(days=1)
        return out


NOW = dt.datetime(2025, 4, 20, 12, tzinfo=IST)


def test_backfill_on_an_empty_store_fetches_the_whole_lookback(db: Database) -> None:
    history = _History()
    report = sync.backfill(
        sync.Params(
            db=db, history=history, keys=[RELIANCE], timeframe=Timeframe.D1, lookback=dt.timedelta(days=5), now=NOW
        )
    )
    assert report.requested == 1 and not report.failed
    assert history.calls[0][1] == NOW - dt.timedelta(days=5)
    assert report.inserted == db.candle_count(RELIANCE, Timeframe.D1) == 5


def test_incremental_backfill_resumes_from_the_last_stored_bar(db: Database) -> None:
    db.upsert_candles([_bar(dt.datetime(2025, 4, 17, tzinfo=IST), 10000)])
    history = _History()
    report = sync.backfill(
        sync.Params(
            db=db, history=history, keys=[RELIANCE], timeframe=Timeframe.D1, lookback=dt.timedelta(days=30), now=NOW
        )
    )
    assert history.calls[0][1] == dt.datetime(2025, 4, 17, 0, 0, 1, tzinfo=IST), (
        "the request must start one second after the last stored bar, or that bar is re-requested every run"
    )
    assert report.inserted == 3, "18th, 19th and 20th"


def test_full_backfill_re_pulls_and_inserts_nothing_new(db: Database) -> None:
    history = _History()
    params = sync.Params(
        db=db,
        history=history,
        keys=[RELIANCE],
        timeframe=Timeframe.D1,
        lookback=dt.timedelta(days=5),
        now=NOW,
        full=True,
    )
    first = sync.backfill(params)
    second = sync.backfill(params)
    assert first.inserted == 5 and second.inserted == 0, "INSERT OR IGNORE makes a full pull idempotent"
    assert history.calls[1][1] == NOW - dt.timedelta(days=5), "a full pull ignores the last stored bar"


def test_one_dead_symbol_leaves_the_others_stored(db: Database) -> None:
    history = _History(dead={TCS})
    report = sync.backfill(
        sync.Params(
            db=db, history=history, keys=[TCS, RELIANCE], timeframe=Timeframe.D1, lookback=dt.timedelta(days=3), now=NOW
        )
    )
    assert report.failed == [TCS] and len(report.errors) == 1
    assert "delisted" in str(report.errors[0])
    assert db.candle_count(RELIANCE, Timeframe.D1) == 3, "the pass must continue past the dead symbol"


def test_backfill_requires_its_collaborators(db: Database) -> None:
    with pytest.raises(ValueError, match="lookback"):
        sync.backfill(sync.Params(db=db, history=_History(), keys=[], timeframe=Timeframe.D1, lookback=dt.timedelta(0)))


# ------------------------------------------------------------------ sync: runner


def _task(task_id: str, fetch: Any, persist: Any, cadence: sync.Cadence = sync.ALWAYS) -> sync.Task:
    return sync.Task(id=task_id, cadence=cadence, fetch=fetch, persist=persist)


class _WriteDetector:
    """Records whether two persists ever overlap: SQLite has one writer."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.concurrent = False
        self.order: list[str] = []

    def persist(self, value: Any) -> None:
        if not self.lock.acquire(blocking=False):
            self.concurrent = True
            raise RuntimeError("concurrent write")
        try:
            time.sleep(0.001)
            self.order.append(value)
        finally:
            self.lock.release()


def test_runner_fetches_concurrently_and_writes_serially(db: Database) -> None:
    detector = _WriteDetector()
    in_flight = 0
    peak = 0
    gauge = threading.Lock()

    def fetch_for(task_id: str) -> Any:
        def fetch() -> str:
            nonlocal in_flight, peak
            with gauge:
                in_flight += 1
                peak = max(peak, in_flight)
            time.sleep(0.005)
            with gauge:
                in_flight -= 1
            return task_id

        return fetch

    tasks = [_task(f"task-{i:02d}", fetch_for(f"task-{i:02d}"), detector.persist) for i in range(12)]
    report = sync.Runner(db=db, workers=4, run_name="concurrency").run(tasks)

    assert report.succeeded == 12, report.errors
    assert not detector.concurrent, "two persists overlapped; that surfaces as intermittent 'database is locked'"
    assert peak >= 2, "fetches must actually run concurrently"
    assert detector.order == sorted(detector.order), "persists must land in task order however the pool completes"


def test_a_task_inside_its_freshness_window_is_skipped(db: Database) -> None:
    now = dt.datetime(2025, 4, 17, 9, tzinfo=UTC)
    fetches = 0

    def fetch() -> str:
        nonlocal fetches
        fetches += 1
        return "x"

    daily = _task("fundamentals", fetch, lambda _v: None, sync.DAILY)
    runner = sync.Runner(db=db, run_name="fresh", now=lambda: now)
    assert runner.run([daily]).succeeded == 1
    second = runner.run([daily])
    assert second.skipped == 1 and second.succeeded == 0 and fetches == 1, (
        "a task that succeeded moments ago is inside its daily window"
    )
    runner.now = lambda: now + dt.timedelta(hours=25)
    assert runner.run([daily]).succeeded == 1 and fetches == 2, "a day later it is due again"


def test_force_overrides_the_freshness_gate(db: Database) -> None:
    fetches = 0

    def fetch() -> str:
        nonlocal fetches
        fetches += 1
        return "x"

    daily = _task("fundamentals", fetch, lambda _v: None, sync.DAILY)
    sync.Runner(db=db, run_name="force").run([daily])
    sync.Runner(db=db, run_name="force", force=True).run([daily])
    assert fetches == 2, "an operator re-running a failed sync needs force to do anything"


def test_a_failed_task_does_not_abort_the_pass_and_the_record_is_cleared(db: Database) -> None:
    persisted: list[str] = []

    def boom() -> None:
        raise RuntimeError("upstream 500")

    tasks = [
        _task("a", lambda: "a", persisted.append),
        _task("b", boom, persisted.append),
        _task("c", lambda: "c", lambda _v: (_ for _ in ()).throw(RuntimeError("disk full"))),
        _task("d", lambda: "d", persisted.append),
    ]
    report = sync.Runner(db=db, run_name="partial").run(tasks)
    assert report.succeeded == 2 and report.failed == 2
    assert persisted == ["a", "d"]
    assert any("upstream 500" in str(e) for e in report.errors) and any("disk full" in str(e) for e in report.errors)

    row = db.conn.execute("SELECT status, message FROM runs WHERE id = ?", (report.run_id,)).fetchone()
    assert row["status"] == "partial" and "2 of 4" in row["message"]
    with pytest.raises(Exception, match=r"sync[.]progress[.]partial"):
        db.get_state("sync.progress.partial")


def test_resume_skips_tasks_already_done(db: Database) -> None:
    # A previous run recorded two of three tasks done, then died. The record
    # has the shape the Go runner writes, so either language can resume it.
    run_id = db.start_run("sync", "resume")
    db.set_state("sync.progress.resume", {"run_id": run_id, "done": {"a": "success", "b": "error"}})
    fetched: list[str] = []
    tasks = [_task(t, (lambda t=t: fetched.append(t) or t), lambda _v: None) for t in ("a", "b", "c")]

    report = sync.Runner(db=db, run_name="resume").run(tasks)
    assert report.run_id == run_id, "the resumed run keeps the interrupted run's id"
    assert fetched == ["c"], "tasks recorded done are not refetched, whatever their outcome was"
    assert report.skipped == 2 and report.succeeded == 1


def test_an_exception_in_one_fetch_does_not_tear_down_the_pool(db: Database) -> None:
    def explode() -> None:
        raise ValueError("nil field in an unexpected response")

    tasks = [_task("bad", explode, lambda _v: None), _task("good", lambda: 1, lambda _v: None)]
    report = sync.Runner(db=db, workers=2, run_name="pool").run(tasks)
    assert report.succeeded == 1 and report.failed == 1


def test_runner_requires_a_store() -> None:
    with pytest.raises(ValueError):
        sync.Runner(db=None).run([])  # type: ignore[arg-type]


def test_success_stamp_is_the_form_the_go_runner_writes(db: Database) -> None:
    now = dt.datetime(2025, 4, 17, 9, 30, tzinfo=IST)
    sync.Runner(db=db, run_name="stamp", now=lambda: now).run([_task("t", lambda: 1, lambda _v: None)])
    assert db.get_state("sync.success.stamp.t") == "2025-04-17T04:00:00Z"


# ------------------------------------------------------------------ universe


INDEX_CSV = (
    "﻿Company Name,Industry,Symbol,Series,ISIN Code\n"
    "Reliance Industries Ltd.,Oil Gas & Consumable Fuels,RELIANCE,EQ,INE002A01018\n"
    "Tata Consultancy Services Ltd.,Information Technology,TCS,EQ,INE467B01029\n"
    "\n"
    ",,,,\n"
).encode()

BHAVCOPY = (
    "SYMBOL, SERIES, DATE1, PREV_CLOSE, OPEN_PRICE, HIGH_PRICE, LOW_PRICE, LAST_PRICE, CLOSE_PRICE, AVG_PRICE,"
    " TTL_TRD_QNTY, TURNOVER_LACS, NO_OF_TRADES, DELIV_QTY, DELIV_PER\n"
    "RELIANCE, EQ, 17-Apr-2025, 1250.00, 1255.00, 1270.50, 1248.00, 1260.00, 1261.25, 1258.10, 1000000, 12581.00,"
    " 50000, 400000, 40.00\n"
    "SOMEGSEC, GS, 17-Apr-2025, 100.00, 100.00, 100.00, 100.00, 100.00, 100.00, 100.00, 10, 0.01, 1, -, -\n"
    "BROKEN, EQ, 17-Apr-2025, 10.00, -, -, -, -, -, -, 0, 0, 0, -, -\n"
    "BEONE, BE, 17-Apr-2025, 50.00, 51.00, 52.00, 49.00, 50.50, 50.25, 50.40, 2000, 1.00, 20, -, -\n"
).encode("ascii")


def test_parse_index_csv_reads_the_published_list_and_strips_the_bom() -> None:
    got = universe.parse_index_csv(INDEX_CSV)
    assert [i.key for i in got] == [RELIANCE, TCS], "blank and empty rows are skipped, not fatal"
    assert got[0].name == "Reliance Industries Ltd." and got[0].segment == "equity" and got[0].active
    assert got[0].isin == "INE002A01018", "Upstox keys cash equity by ISIN, so the column must be read"


def test_parse_index_csv_reads_a_watchlist_export_too() -> None:
    got = universe.parse_index_csv(b"symbol,name\nreliance.NS,Reliance\ntcs.ns,TCS\n")
    assert [i.key.symbol for i in got] == ["RELIANCE", "TCS"]


def test_parse_index_csv_rejects_a_file_with_no_symbol_column() -> None:
    with pytest.raises(ValueError, match="no symbol column"):
        universe.parse_index_csv(b"Company Name,Industry\nX,Y\n")


def test_parse_bhavcopy_keeps_equity_series_and_skips_bad_rows() -> None:
    day = dt.date(2025, 4, 17)
    rows = universe.parse_bhavcopy(BHAVCOPY, day)
    assert [r.key.symbol for r in rows] == ["RELIANCE", "BEONE"], "GS is not equity and BROKEN has no prices"
    r = rows[0]
    assert r.close == money.parse("1261.25") and r.prev_close == money.parse("1250.00")
    assert r.volume == 1000000 and r.deliverable_qty == 400000 and r.deliverable_pct == 40.0
    assert rows[1].deliverable_qty == 0, "'-' reads as absent, not as a failure"


def test_candles_converts_bhavcopy_rows_to_midnight_ist_daily_bars() -> None:
    rows = universe.parse_bhavcopy(BHAVCOPY, dt.date(2025, 4, 17))
    c = universe.candles(rows)[0]
    assert c.timeframe == Timeframe.D1 and c.start == dt.datetime(2025, 4, 17, tzinfo=IST)
    assert (c.open, c.high, c.low, c.close, c.volume) == (
        money.parse("1255.00"),
        money.parse("1270.50"),
        money.parse("1248.00"),
        money.parse("1261.25"),
        1000000,
    )


class _Fetcher:
    """Scripts NSE: a home page, then the archive responses in order."""

    def __init__(self, responses: list[tuple[int, bytes]]) -> None:
        self.responses = list(responses)
        self.calls: list[str] = []

    def get(self, url: str, headers: Any) -> tuple[int, bytes]:
        self.calls.append(url)
        assert headers["User-Agent"].startswith("Mozilla/5.0"), (
            "NSE serves 403 to anything that does not look like a browser"
        )
        if url == universe.NSE_HOME:
            return 200, b"<html>"
        return self.responses.pop(0)


def test_nse_is_warmed_up_before_the_first_download() -> None:
    fetcher = _Fetcher([(200, INDEX_CSV)])
    keys = universe.Client(None, fetcher=fetcher).index_constituents("ind_nifty500list")
    assert keys == [RELIANCE, TCS]
    assert fetcher.calls[0] == universe.NSE_HOME, "the home page must be fetched first for its cookies"
    assert fetcher.calls[1].endswith("/content/indices/ind_nifty500list.csv")


def test_a_403_is_retried_after_a_fresh_warm_up() -> None:
    fetcher = _Fetcher([(403, b""), (200, INDEX_CSV)])
    got = universe.Client(None, fetcher=fetcher).constituents("ind_nifty500list")
    assert len(got) == 2
    assert fetcher.calls.count(universe.NSE_HOME) == 2, "an expired session is warmed up again, once"


def test_an_unpublished_day_is_not_an_error_but_a_distinct_exception() -> None:
    client = universe.Client(None, fetcher=_Fetcher([(404, b"")]))
    with pytest.raises(universe.NotPublishedError):
        client.bhavcopy(dt.date(2025, 4, 20))


def test_a_constituent_list_is_cached_and_not_refetched(tmp_path: Path) -> None:
    fetcher = _Fetcher([(200, INDEX_CSV)])
    client = universe.Client(tmp_path, fetcher=fetcher)
    client.constituents("ind_nifty500list")
    client.constituents("ind_nifty500list")
    assert sum(1 for u in fetcher.calls if u != universe.NSE_HOME) == 1
    assert (tmp_path / "ind_nifty500list.csv").read_bytes() == INDEX_CSV


def test_a_cached_file_is_used_with_no_network_call_at_all(tmp_path: Path) -> None:
    (tmp_path / "sec_bhavdata_full_17042025.csv").write_bytes(BHAVCOPY)
    fetcher = _Fetcher([])
    rows = universe.Client(tmp_path, fetcher=fetcher).bhavcopy(dt.date(2025, 4, 17))
    assert len(rows) == 2 and fetcher.calls == [], "a backtest must run offline"


def test_base_url_rewrites_the_host_and_keeps_the_path() -> None:
    fetcher = _Fetcher([(200, INDEX_CSV)])
    universe.Client(None, fetcher=fetcher, base_url="http://127.0.0.1:9/").constituents("x")
    assert fetcher.calls[1] == "http://127.0.0.1:9/content/indices/x.csv"


def test_sync_deactivates_instruments_that_left_the_index(db: Database) -> None:
    db.upsert_instruments([Instrument(key=InstrumentKey("NSE", "GONE"))])
    client = universe.Client(None, fetcher=_Fetcher([(200, INDEX_CSV)]))
    report = client.sync(db, "ind_nifty500list")
    assert report.fetched == 2 and report.deactivated == 1
    assert not db.instrument(InstrumentKey("NSE", "GONE")).active, "deactivated, never deleted"


def test_sync_refuses_an_empty_index(db: Database) -> None:
    db.upsert_instruments([Instrument(key=RELIANCE)])
    client = universe.Client(None, fetcher=_Fetcher([(200, b"Symbol,Company Name\n")]))
    with pytest.raises(ValueError, match="refusing"):
        client.sync(db, "ind_nifty500list")
    assert db.instrument(RELIANCE).active, "an outage serving an empty file must not deactivate the universe"


# ------------------------------------------------------------------ reference: ticks, lots, holidays


def test_tick_and_lot_sizes_fall_back_for_unknown_instruments(db: Database) -> None:
    db.upsert_instruments([Instrument(key=RELIANCE, tick_size=Money(5), lot_size=0)])
    tick = reference.tick_sizes(db)
    lot = reference.lot_sizes(db)
    assert tick(RELIANCE) == 5 and tick(TCS) == reference.FALLBACK_TICK
    assert lot(RELIANCE) == 1 and lot(TCS) == 1, "a zero lot in the master is cash equity, not nothing"
    assert reference.FALLBACK_TICK != 0, "a zero tick disables rounding silently"


class _Live:
    def __init__(self, days: dict[str, str] | None, fail: bool = False) -> None:
        self.days = days or {}
        self.fail = fail
        self.calls = 0

    def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
        self.calls += 1
        if self.fail:
            raise ConnectionError("holiday endpoint down")
        return dict(self.days)


def test_holidays_fetch_then_serve_from_cache(tmp_path: Path) -> None:
    live = _Live({"2025-04-18": "Good Friday", "2025-08-15": "Independence Day"})
    path = tmp_path / "cal" / "holidays.json"
    h = reference.Holidays(live, path)
    got = h.closed("NSE", dt.date(2025, 4, 1), dt.date(2025, 4, 30))
    assert got == {"2025-04-18": "Good Friday"}, "the range bounds the result"
    h.closed("NSE", dt.date(2025, 8, 1), dt.date(2025, 8, 31))
    assert live.calls == 1, "the second call is served from memory"

    cached = json.loads(path.read_text())
    assert cached["days"]["NSE"]["2025-08-15"] == "Independence Day"
    assert reference.Holidays(None, path).closed("NSE", dt.date(2025, 8, 1), dt.date(2025, 8, 31)) == {
        "2025-08-15": "Independence Day"
    }, "a cache-only source needs no network"


def test_holidays_fall_back_to_the_cache_when_the_fetch_fails(tmp_path: Path) -> None:
    path = tmp_path / "holidays.json"
    path.write_text(json.dumps({"fetched_at": "2020-01-01T00:00:00Z", "days": {"NSE": {"2025-04-18": "Good Friday"}}}))
    h = reference.Holidays(_Live(None, fail=True), path)
    assert h.closed("NSE", dt.date(2025, 4, 1), dt.date(2025, 4, 30)) == {"2025-04-18": "Good Friday"}, (
        "a stale calendar is far better than none"
    )


def test_a_cache_miss_and_a_failed_fetch_fail_open(tmp_path: Path) -> None:
    h = reference.Holidays(_Live(None, fail=True), tmp_path / "missing.json")
    assert h.closed("NSE", dt.date(2025, 4, 1), dt.date(2025, 4, 30)) == {}, (
        "skipping a real trading day is worse than one wasted pass on a holiday"
    )


def test_holidays_need_something_to_read_from() -> None:
    with pytest.raises(ValueError):
        reference.Holidays(None, None)


# ------------------------------------------------------------------ reference: corporate actions


def test_actions_round_trip_through_the_store_and_replace_on_conflict(db: Database) -> None:
    db.upsert_instruments([Instrument(key=RELIANCE)])
    split = reference.Action(RELIANCE, dt.date(2025, 4, 17), reference.KIND_SPLIT, ratio=10)
    reference.upsert_actions(db, [split])
    reference.upsert_actions(db, [reference.Action(RELIANCE, dt.date(2025, 4, 17), reference.KIND_SPLIT, ratio=5)])
    got = reference.actions(db, RELIANCE, dt.date(2025, 1, 1), dt.date(2025, 12, 31))
    assert got == [
        reference.Action(RELIANCE, dt.date(2025, 4, 17), reference.KIND_SPLIT, ratio=5.0, amount=Money(0))
    ], "a corrected ratio must take effect; there is one right answer for a split that has already happened"
    assert reference.actions(db, RELIANCE, dt.date(2025, 5, 1), dt.date(2025, 12, 31)) == []


def test_upsert_rejects_actions_that_would_corrupt_a_series(db: Database) -> None:
    db.upsert_instruments([Instrument(key=RELIANCE)])
    for bad in (
        reference.Action(RELIANCE, dt.date(2025, 4, 17), reference.KIND_SPLIT, ratio=0),
        reference.Action(RELIANCE, dt.date(2025, 4, 17), reference.KIND_DIVIDEND, amount=Money(0)),
        reference.Action(RELIANCE, dt.date(2025, 4, 17), "merger"),
    ):
        with pytest.raises(ValueError):
            reference.upsert_actions(db, [bad])


def test_adjust_does_not_modify_its_input_and_no_actions_returns_it_unchanged() -> None:
    bars_in = _daily(2)
    snapshot = [replace(c) for c in bars_in]
    out = reference.adjust(bars_in, [reference.Action(RELIANCE, dt.date(2025, 4, 15), reference.KIND_SPLIT, ratio=2)])
    assert out[0].close != bars_in[0].close and bars_in == snapshot
    assert reference.adjust(bars_in, []) is bars_in


def test_parity_adjust(parity: dict[str, Any]) -> None:
    for case in parity["adjust"]:
        candles = [_parity_bar(b, Timeframe.D1) for b in case["candles"]]
        actions = [
            reference.Action(
                RELIANCE, dt.date.fromisoformat(a["ex_date"]), a["kind"], ratio=a["ratio"], amount=Money(a["amount"])
            )
            for a in case["actions"]
        ]
        got = reference.adjust(candles, actions)
        assert len(got) == len(case["want"]), case["name"]
        for i, want in enumerate(case["want"]):
            assert _same_bar(got[i], want), f"{case['name']}: bar {i} is {got[i]}, want {want}"


# ------------------------------------------------------------------ bars


def _session() -> tuple[calendar.Session, dt.datetime]:
    s = calendar.nse()
    return s, dt.datetime(2025, 4, 17, 9, 15, tzinfo=s.tz)


def _series(start: dt.datetime, *closes: int, tf: Timeframe = Timeframe.M5) -> list[Candle]:
    return [
        Candle(
            key=RELIANCE,
            timeframe=tf,
            start=start + i * tf.duration,
            open=Money(c - 50),
            high=Money(c + 100),
            low=Money(c - 100),
            close=Money(c),
            volume=100 + i,
        )
        for i, c in enumerate(closes)
    ]


def test_builder_emits_one_bar_per_bucket_and_flushes_the_last() -> None:
    s, opening = _session()
    emitted: list[Candle] = []
    b = bars.Builder(s, Timeframe.M5, emitted.append)
    for at, price, vol in (
        (opening - dt.timedelta(minutes=5), "99.50", 1),  # pre-open print clamps into the first bar
        (opening, "100.00", 10),
        (opening + dt.timedelta(minutes=2), "102.00", 5),
        (opening + dt.timedelta(minutes=4), "99.00", 5),
        (opening + dt.timedelta(minutes=5), "101.00", 7),
        (opening + dt.timedelta(minutes=3), "50.00", 1),  # out of order: folded into the open bar, never re-opens one
    ):
        b.add(Tick(RELIANCE, at, money.parse(price), vol))
    assert len(emitted) == 1, "crossing into the second bucket closes exactly one bar"
    first = emitted[0]
    assert first.start == opening and first.open == money.parse("99.50") and first.close == money.parse("99.00")
    assert first.high == money.parse("102.00") and first.low == money.parse("99.00") and first.volume == 21
    b.flush()
    assert len(emitted) == 2 and emitted[1].low == money.parse("50.00") and emitted[1].volume == 8
    b.flush()
    assert len(emitted) == 2, "a second flush with nothing forming emits nothing"


def test_resample_5m_to_15m() -> None:
    s, opening = _session()
    series = _series(opening, 10000, 10100, 10200, 10300, 10400, 10500)
    out = bars.resample(series, Timeframe.M15, s)
    assert len(out) == 2
    first = out[0]
    assert first.start == opening and first.timeframe == Timeframe.M15
    assert first.open == series[0].open and first.close == series[2].close
    assert first.high == series[2].high and first.low == series[0].low
    assert first.volume == sum(c.volume for c in series[:3])


def test_resample_refuses_finer_and_non_dividing_targets() -> None:
    s, opening = _session()
    with pytest.raises(ValueError, match="finer"):
        bars.resample(_series(opening, 1, 2, tf=Timeframe.M15), Timeframe.M5, s)
    with pytest.raises(ValueError, match="divide"):
        bars.resample(_series(opening, 1, 2, tf=Timeframe.M3), Timeframe.M5, s)
    assert bars.resample([], Timeframe.M5, s) == []


def test_parity_resample(parity: dict[str, Any]) -> None:
    block = parity["resample"]
    assert block["session"] == "NSE"
    s = calendar.nse()
    source = Timeframe(block["from"])
    for case in block["cases"]:
        got = bars.resample([_parity_bar(b, source) for b in block["input"]], Timeframe(case["to"]), s)
        assert len(got) == len(case["want"]), case["to"]
        for i, want in enumerate(case["want"]):
            assert got[i].timeframe == Timeframe(case["to"])
            assert _same_bar(got[i], want), f"{source} -> {case['to']}: bar {i} is {got[i]}, want {want}"
    for bad in block["invalid"]:
        with pytest.raises(ValueError):
            bars.resample([_parity_bar(block["input"][0], Timeframe(bad["from"]))], Timeframe(bad["to"]), s)


def _sessions(days: int, per_slot: list[int]) -> list[Candle]:
    """``days`` sessions of 5-minute bars with the given per-bar volumes, weekdays only."""
    s = calendar.nse()
    out: list[Candle] = []
    day = dt.date(2025, 4, 7)
    made = 0
    while made < days:
        if s.is_weekday(day):
            opening = dt.datetime(day.year, day.month, day.day, 9, 15, tzinfo=s.tz)
            for i, v in enumerate(per_slot):
                out.append(_bar(opening + i * dt.timedelta(minutes=5), 10000, v, Timeframe.M5))
            made += 1
        day += dt.timedelta(days=1)
    return out


def test_volume_profile_excludes_the_current_session_and_is_nan_during_warm_up() -> None:
    s = calendar.nse()
    series = _sessions(5, [100, 100, 100])
    # Make the last session frantic; its own benchmark must not move.
    for c in series[-3:]:
        c.volume = 1000
    profile = bars.volume_profile(series, s, Timeframe.M5, 4)
    assert all(math.isnan(v) for v in profile[:6]), (
        "the first two sessions have too little history and must be NaN, never zero"
    )
    assert profile[-3:] == [100.0, 200.0, 300.0], "the median of the prior sessions' cumulative volume per slot"


def test_relative_volume_is_one_at_the_normal_pace_and_two_at_twice_it() -> None:
    s = calendar.nse()
    series = _sessions(5, [100, 100, 100])
    rel = bars.relative_volume(series, s, Timeframe.M5, 4)
    assert rel[-3:] == [1.0, 1.0, 1.0]
    for c in series[-3:]:
        c.volume = 200
    rel = bars.relative_volume(series, s, Timeframe.M5, 4)
    assert rel[-3:] == [2.0, 2.0, 2.0]
    assert math.isnan(rel[0]), "no history reads as NaN, not as 'nothing unusual'"


# ------------------------------------------------------------------ frames (optional pandas)


def test_frames_round_trip_when_pandas_is_installed() -> None:
    pytest.importorskip("pandas")
    from tradekit.marketdata import frames

    series = _series(dt.datetime(2025, 4, 17, 9, 15, tzinfo=IST), 10000, 10101, 10250)
    frame = frames.to_frame(series)
    assert list(frame.columns) == list(frames.COLUMNS) and frame.index.tz is not None
    assert frame["close"].tolist() == [100.0, 101.01, 102.5]
    assert frames.from_frame(frame, RELIANCE, Timeframe.M5) == series, "prices must survive the float round trip"
    with pytest.raises(ValueError, match="timezone-aware"):
        frames.from_frame(frame.tz_localize(None), RELIANCE, Timeframe.M5)
