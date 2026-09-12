"""Fill the store from a history provider, resumably and without letting one dead symbol kill a 500-symbol pass.

Mirrors ``go/marketdata/sync``. It replaces the downloader each project wrote
for itself. Two problems were solved separately in every one of them: splitting
a long range into request-sized windows, and working out which bars are
actually missing. Both live here.
"""

from __future__ import annotations

import datetime as dt
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Any

from tradekit.core.domain import Candle, InstrumentKey, Timeframe
from tradekit.core.ports import HistoryProvider
from tradekit.store import Database, StateNotFoundError

__all__ = [
    "ALWAYS",
    "DAILY",
    "WEEKLY",
    "Cadence",
    "Params",
    "Report",
    "RunReport",
    "Runner",
    "Task",
    "Window",
    "backfill",
    "chunks",
    "due",
]

_ONE_SECOND = dt.timedelta(seconds=1)


# ------------------------------------------------------------------ chunking


@dataclass(frozen=True, slots=True)
class Window:
    """One request-sized slice of a date range, inclusive at both ends."""

    start: dt.datetime
    end: dt.datetime


def chunks(start: dt.datetime, end: dt.datetime, span: dt.timedelta) -> list[Window]:
    """Split ``[start, end]`` into windows of at most ``span``, oldest first.

    Windows are contiguous and never overlap: each begins one second after the
    previous ends. An overlap would re-request bars already held -- harmless but
    wasteful against a metered endpoint -- while a gap silently loses them, and a
    backtest cannot tell a missing bar from a day the instrument did not trade.

    A :class:`~tradekit.core.ports.HistoryProvider` chunks internally, so a
    caller going through one does not need this. It is for the callers that
    drive a raw endpoint themselves.

    An inverted range yields no windows rather than an error: the caller that
    cares checks it, and the ones that do not are asking for "everything since
    the last bar" on an already-current instrument, which is legitimately empty.
    A non-positive span would loop forever, so it yields nothing too.
    """
    if end < start or span <= dt.timedelta(0):
        return []
    out: list[Window] = []
    cursor = start
    while cursor <= end:
        stop = min(cursor + span - _ONE_SECOND, end)
        out.append(Window(cursor, stop))
        cursor += span
    return out


# ------------------------------------------------------------------ gap-fill


@dataclass(slots=True)
class Params:
    """Configuration for one :func:`backfill` pass."""

    db: Database
    history: HistoryProvider
    keys: list[InstrumentKey]
    timeframe: Timeframe
    lookback: dt.timedelta
    """How far back a full pull reaches, and how far back an instrument with no stored bars is filled."""
    full: bool = False
    """Re-request the whole lookback window rather than only the gap since the last stored bar.

    The incremental default can only extend history *forward*. Going from one
    year of stored data to five is impossible without this, because the last
    stored bar is already recent and the gap after it is empty -- the trap
    breakout500's own docstring names. Re-fetched bars are ``INSERT OR IGNORE``,
    so a full pull is safe and idempotent, and the only cost is bandwidth.
    """
    now: dt.datetime | None = None
    """The end of the range, injectable for tests. ``None`` means the current time."""


@dataclass(slots=True)
class Report:
    """What one backfill pass did."""

    requested: int = 0
    inserted: int = 0
    failed: list[InstrumentKey] = field(default_factory=list)
    """The instruments whose fetch or store failed, collected so the pass completes."""
    errors: list[Exception] = field(default_factory=list)
    """One exception per failed instrument, in the same order.

    So an operator can see whether 400 symbols failed for 400 reasons or for one.
    """


def backfill(p: Params) -> Report:
    """Fetch and store the bars missing for each instrument.

    One instrument's failure does not stop the pass: it is collected in the
    report and the next is attempted. A 500-symbol sync that aborts on the first
    delisted ticker leaves 499 instruments stale, and the operator finds out from
    a backtest weeks later.

    A :class:`KeyboardInterrupt` does stop the pass, because it means the
    operator asked it to; it is not an :class:`Exception` and so is not caught.
    """
    if p.db is None:
        raise ValueError("sync: Params.db is required")
    if p.history is None:
        raise ValueError("sync: Params.history is required")
    if p.lookback <= dt.timedelta(0):
        raise ValueError("sync: Params.lookback must be positive")

    now = p.now if p.now is not None else dt.datetime.now(dt.UTC)
    report = Report(requested=len(p.keys))
    for key in p.keys:
        try:
            report.inserted += _backfill_one(p, key, now)
        except Exception as exc:  # one dead symbol must not end the pass
            report.failed.append(key)
            report.errors.append(RuntimeError(f"sync: {key}: {exc}"))
    return report


def _backfill_one(p: Params, key: InstrumentKey, now: dt.datetime) -> int:
    """Fill a single instrument, returning how many bars were new."""
    start = now - p.lookback
    if not p.full:
        last = p.db.last_candle_start(key, p.timeframe)
        if last is not None:
            # Resume from the bar after the last one held. Starting at the
            # last bar itself would re-request a bar that is already stored
            # on every single run.
            start = last + _ONE_SECOND
    if start > now:
        # Already current. This is the normal state of an intraday sync run
        # twice in one minute, not a failure.
        return 0
    candles: list[Candle] = p.history.candles(key, p.timeframe, start, now)
    if not candles:
        return 0
    return p.db.upsert_candles(candles)


# ------------------------------------------------------------------ cadence


@dataclass(frozen=True, slots=True)
class Cadence:
    """How often a task is worth refetching.

    A minimum age rather than a schedule. A schedule needs a clock to have fired
    at the right moment; a minimum age lets a run that started late, or a second
    run of the day, ask the same question and get a correct answer.
    """

    name: str
    min_age: dt.timedelta


DAILY = Cadence("daily", dt.timedelta(hours=20))
"""Once per calendar day's worth of elapsed time.

With enough slack that a run at 09:00 and one at 08:55 the next day do not skip.
"""
WEEKLY = Cadence("weekly", dt.timedelta(days=6))
"""For reference data that changes on a corporate timetable rather than a market one."""
ALWAYS = Cadence("always", dt.timedelta(0))
"""Every pass, for the intraday tasks whose answer changes minute to minute."""


def due(cadence: Cadence, last_success: dt.datetime | None, now: dt.datetime, force: bool) -> bool:
    """Whether a task should be refetched.

    A task that has never succeeded is always due -- ``None`` means "no record",
    not "succeeded at the epoch", and treating it as the latter would leave a
    fresh install permanently up to date and permanently empty.

    ``force`` overrides the gate, which is what an operator re-running a failed
    sync needs; without it the run they just triggered would report everything
    as still fresh and do nothing.
    """
    if force or last_success is None:
        return True
    if cadence.min_age <= dt.timedelta(0):
        return True
    return now >= last_success + cadence.min_age


# ------------------------------------------------------------------ resume


@dataclass(slots=True)
class Task:
    """One unit of sync work: fetch something, then persist it.

    The split is the whole point. ``fetch`` runs on a worker thread and must
    touch nothing but the network; ``persist`` runs on the runner's own thread
    and is the only place that writes. SQLite has one writer, and the donor's
    structure -- a pool fetching, results persisted on the caller's thread -- is
    what keeps a concurrent sync from serialising into lock contention or,
    worse, failing intermittently under "database is locked".
    """

    id: str
    """Identifies the task across runs.

    It is the key progress and last-success are recorded under, so it must be
    stable: a task whose id changes between runs looks like a new task that has
    never succeeded, and is refetched every pass forever.
    """
    cadence: Cadence
    fetch: Callable[[], Any]
    persist: Callable[[Any], None]


@dataclass(slots=True)
class RunReport:
    """What one runner pass did. Counts are tasks, not bars."""

    run_id: int = 0
    succeeded: int = 0
    skipped: int = 0
    """The cadence gate held, or the task was already done earlier in a run this one resumed."""
    failed: int = 0
    errors: list[Exception] = field(default_factory=list)


@dataclass(slots=True)
class _Progress:
    """The resume record for one run, stored as JSON under a ``kv_state`` key.

    It lives in ``kv_state`` rather than in a table of its own because it is
    transient: it exists only between a crash and the resumed run that consumes
    it. The JSON shape (``run_id``, ``done``) is shared with the Go runner, so
    either language can resume the other's interrupted pass.
    """

    run_id: int
    done: dict[str, str] = field(default_factory=dict)


@dataclass(slots=True)
class Runner:
    """Drives tasks with cadence gating and resume."""

    db: Database
    workers: int = 1
    """Bounds concurrent fetches. Zero or one fetches serially."""
    force: bool = False
    """Refetch every task regardless of cadence."""
    now: Callable[[], dt.datetime] | None = None
    """Injectable for tests. ``None`` means the current UTC time."""
    run_name: str = ""
    """Distinguishes one sync's progress from another's in the state table.

    So a candle sync and a fundamentals sync do not resume into each other.
    """

    def run(self, tasks: list[Task]) -> RunReport:
        """Execute the tasks, resuming an interrupted pass rather than repeating it.

        A pass records progress after each task, so a run killed halfway through
        500 symbols resumes at symbol 251 rather than at symbol 1 -- which,
        against a metered endpoint, is the difference between finishing the
        day's sync and burning the day's quota on work already done.

        One task's failure does not stop the pass.
        """
        if self.db is None:
            raise ValueError("sync: Runner.db is required")
        now = self._now()
        state = self._resume()
        report = RunReport(run_id=state.run_id)

        pending: list[Task] = []
        for task in tasks:
            if task.id in state.done:
                # Handled earlier in the run this one is resuming.
                report.skipped += 1
                continue
            if not self._due(task, now):
                report.skipped += 1
                state.done[task.id] = "skipped"
                self._save(state)
                continue
            pending.append(task)

        self._drive(pending, state, report, now)

        # The pass finished, so its resume record is spent. Clearing it means
        # the next run starts fresh rather than believing every task is
        # already done.
        self.db.delete_state(self._state_key())
        status, message = "ok", ""
        if report.failed > 0:
            status = "partial"
            message = f"{report.failed} of {len(tasks)} tasks failed"
        self.db.finish_run(state.run_id, status, message)
        return report

    def _drive(self, tasks: list[Task], state: _Progress, report: RunReport, now: dt.datetime) -> None:
        """Fetch concurrently and persist serially.

        Results are collected by index, so the persist order matches the task
        order however the pool completes. A resumed run then stops at a prefix
        rather than at a scatter of finished tasks, which is what makes the
        resume record small and its meaning obvious.
        """
        if not tasks:
            return
        workers = max(1, min(self.workers, len(tasks)))
        with ThreadPoolExecutor(max_workers=workers) as pool:
            results = list(pool.map(_fetch_safe, tasks))

        for task, (fetched, err) in zip(tasks, results, strict=True):
            outcome = "success"
            if err is not None:
                outcome = "error"
                report.failed += 1
                report.errors.append(RuntimeError(f"sync: {task.id}: {err}"))
            else:
                try:
                    task.persist(fetched)
                except Exception as exc:  # one task's failure must not end the pass
                    outcome = "error"
                    report.failed += 1
                    report.errors.append(RuntimeError(f"sync: {task.id}: persisting: {exc}"))
                else:
                    report.succeeded += 1
                    self._mark_success(task, now)
            state.done[task.id] = outcome
            # Saved per task, not at the end: a record written only on
            # completion is a record that never survives the crash it exists
            # for.
            self._save(state)

    def _due(self, task: Task, now: dt.datetime) -> bool:
        """Whether a task's cadence gate lets it through."""
        if self.force:
            return True
        try:
            stamp = self.db.get_state(self._success_key(task))
        except StateNotFoundError:
            return True
        try:
            last = dt.datetime.fromisoformat(str(stamp))
        except ValueError:
            # An unreadable timestamp is treated as no record rather than as a
            # failure: refetching once is cheap, and refusing to sync because
            # a state value is malformed is not.
            return True
        if last.tzinfo is None:
            last = last.replace(tzinfo=dt.UTC)
        return due(task.cadence, last, now, False)

    def _mark_success(self, task: Task, now: dt.datetime) -> None:
        """Record when a task last succeeded, which is what the cadence gate reads on the next pass.

        Written as RFC 3339 in UTC with a ``Z`` suffix, the form Go's runner
        writes, so the two gate each other correctly when they share a store.
        """
        stamp = now.astimezone(dt.UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")
        self.db.set_state(self._success_key(task), stamp)

    def _resume(self) -> _Progress:
        """Load an interrupted pass's progress, or start a new run."""
        try:
            raw = self.db.get_state(self._state_key())
        except StateNotFoundError:
            raw = None
        if isinstance(raw, dict) and raw.get("run_id"):
            done = raw.get("done") or {}
            return _Progress(run_id=int(raw["run_id"]), done=dict(done))
        run_id = self.db.start_run("sync", self.run_name, None)
        return _Progress(run_id=run_id)

    def _save(self, state: _Progress) -> None:
        self.db.set_state(self._state_key(), {"run_id": state.run_id, "done": state.done})

    def _state_key(self) -> str:
        return "sync.progress." + self._name()

    def _success_key(self, task: Task) -> str:
        """Scoped by run name as well as task id, so two syncs naming a task the same do not gate each other."""
        return "sync.success." + self._name() + "." + task.id

    def _name(self) -> str:
        return self.run_name or "default"

    def _now(self) -> dt.datetime:
        return self.now() if self.now is not None else dt.datetime.now(dt.UTC)


def _fetch_safe(task: Task) -> tuple[Any, Exception | None]:
    """Run a task's fetch, converting any exception into a value.

    An exception on a worker thread would otherwise surface only when the
    future is read, and a ``BaseException`` there would tear down the pool.
    fanse's ``_fetch_safe`` makes the same conversion for the same reason.
    """
    try:
        return task.fetch(), None
    except Exception as exc:  # the whole point is to contain it
        return None, exc
