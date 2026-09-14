"""Store tests against real SQLite, mirroring go/store's integration suite.

Both libraries apply the same contracts/sqlite schema, so exercising it here
also validates the schema the Go side embeds.
"""

from __future__ import annotations

import datetime as dt
from pathlib import Path

import pytest

from tradekit.core import costs, money, risk
from tradekit.core.domain import (
    Candle,
    ExitReason,
    Instrument,
    InstrumentKey,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Product,
    Side,
    Signal,
    SignalKind,
    Timeframe,
    TimeInForce,
    Trade,
)
from tradekit.core.money import Money
from tradekit.store import (
    Database,
    NotFoundError,
    StateNotFoundError,
    UnknownMigrationError,
    connect,
    load_migrations,
    migrations,
)
from tradekit.store.migrations import _split_statements

KEY = InstrumentKey("NSE", "RELIANCE")
UTC = dt.UTC


@pytest.fixture
def db() -> Database:
    """A migrated in-memory database."""
    database = connect(":memory:")
    database.migrate()
    yield database
    database.close()


# ---------------------------------------------------------------------- migrations


def test_migrate_is_idempotent(db: Database) -> None:
    first = db.applied_versions()
    assert first
    db.migrate()  # calling on every startup must be safe
    assert db.applied_versions() == first


def test_schema_creates_every_shared_table(db: Database) -> None:
    tables = {r[0] for r in db.conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    assert {
        "instruments",
        "instrument_ids",
        "candles",
        "signals",
        "orders",
        "trades",
        "equity_snapshots",
        "runs",
        "charge_rates",
        "kv_state",
        "schema_migrations",
    } <= tables


def test_apply_rejects_unknown_applied_version(db: Database) -> None:
    # A database written by a newer binary carries a version this build has
    # never seen; continuing would query a shape it does not know.
    db.conn.execute(
        "INSERT INTO schema_migrations (version, name, applied_at) VALUES (499, 'from_the_future', ?)",
        (dt.datetime.now(UTC).isoformat(),),
    )
    with pytest.raises(UnknownMigrationError):
        db.migrate()


def test_migrate_project_rejects_reserved_versions(db: Database, tmp_path: Path) -> None:
    (tmp_path / "001_project.sql").write_text("CREATE TABLE t (x INTEGER);")
    with pytest.raises(ValueError, match="500"):
        db.migrate_project(tmp_path)


def test_migrate_project_applies_and_coexists(db: Database, tmp_path: Path) -> None:
    (tmp_path / "500_project.sql").write_text("CREATE TABLE cycles (id INTEGER PRIMARY KEY);")
    db.migrate_project(tmp_path)
    assert 500 in db.applied_versions()
    # The library's own migrations still run without tripping over it.
    db.migrate()
    library = {m.version for m in migrations._library_migrations()}
    assert set(db.applied_versions()) == library | {500}


def test_load_migrations_sorts_by_version_and_ignores_others(tmp_path: Path) -> None:
    for name in ("010_ten.sql", "002_two.sql", "001_one.sql", "notes.txt", "nonumber.sql"):
        (tmp_path / name).write_text("SELECT 1;")
    got = load_migrations(tmp_path)
    # Lexical order would put 010 before 002; version order must not.
    assert [m.version for m in got] == [1, 2, 10]


def test_load_migrations_rejects_duplicate_versions(tmp_path: Path) -> None:
    (tmp_path / "001_first.sql").write_text("SELECT 1;")
    (tmp_path / "001_second.sql").write_text("SELECT 2;")
    with pytest.raises(ValueError, match="twice"):
        load_migrations(tmp_path)


def test_split_statements_respects_semicolons_in_literals() -> None:
    sql = "CREATE TABLE a (x TEXT DEFAULT 'has; semicolon');\nCREATE TABLE b (y INTEGER);\n"
    got = _split_statements(sql)
    assert len(got) == 2
    assert "has; semicolon" in got[0]


# --------------------------------------------------------------------- instruments


def test_instrument_round_trip(db: Database) -> None:
    expiry = dt.date(2026, 1, 29)
    equity = Instrument(
        key=KEY,
        name="Reliance Industries",
        isin="INE002A01018",
        segment="equity",
        lot_size=1,
        tick_size=money.parse("0.05"),
        active=True,
    )
    option = Instrument(
        key=InstrumentKey("NFO", "NIFTY26JAN23000CE"),
        segment="options",
        lot_size=50,
        tick_size=money.parse("0.05"),
        expiry=expiry,
        strike=money.parse("23000.00"),
        option_type="ce",
        active=True,
    )
    db.upsert_instruments([equity, option])

    got = db.instrument(KEY)
    assert got.name == equity.name
    assert got.isin == equity.isin
    assert got.tick_size == equity.tick_size
    # A cash equity must come back with no expiry, not the zero date.
    assert got.expiry is None

    got_option = db.instrument(option.key)
    assert got_option.expiry == expiry
    assert got_option.strike == option.strike
    assert got_option.lot_size == 50


def test_upsert_updates_rather_than_duplicates(db: Database) -> None:
    db.upsert_instruments([Instrument(key=KEY, name="Old", active=True)])
    db.upsert_instruments([Instrument(key=KEY, name="New", active=True)])
    active = db.active_instruments()
    assert len(active) == 1
    assert active[0].name == "New"


def test_active_instruments_filters_by_segment(db: Database) -> None:
    db.upsert_instruments(
        [
            Instrument(key=KEY, segment="equity", active=True),
            Instrument(key=InstrumentKey("NFO", "X"), segment="options", active=True),
            Instrument(key=InstrumentKey("NSE", "GONE"), segment="equity", active=False),
        ]
    )
    assert len(db.active_instruments()) == 2
    assert len(db.active_instruments("equity")) == 1


def test_missing_instrument_raises_not_found(db: Database) -> None:
    with pytest.raises(NotFoundError):
        db.instrument(InstrumentKey("NSE", "NOPE"))


def test_broker_id_mapping(db: Database) -> None:
    db.upsert_instruments([Instrument(key=KEY, active=True)])
    # One instrument carries identifiers for several brokers at once.
    db.set_broker_id(KEY, "zerodha", "738561")
    db.set_broker_id(KEY, "upstox", "NSE_EQ|INE002A01018")

    assert db.broker_id(KEY, "zerodha") == "738561"
    # The reverse lookup is what a tick stream needs.
    assert db.key_for_broker_id("upstox", "NSE_EQ|INE002A01018") == KEY
    with pytest.raises(NotFoundError):
        db.key_for_broker_id("zerodha", "999999")


def test_deactivate_missing_keeps_history_readable(db: Database) -> None:
    a, b = InstrumentKey("NSE", "AAA"), InstrumentKey("NSE", "BBB")
    db.upsert_instruments([Instrument(key=a, active=True), Instrument(key=b, active=True)])

    assert db.deactivate_missing("NSE", [a]) == 1
    assert [i.key for i in db.active_instruments()] == [a]
    # The dropped instrument's row survives so its candles and trades still resolve.
    assert db.instrument(b).active is False


# ------------------------------------------------------------------------- candles


def _candle(day: int, close: int) -> Candle:
    return Candle(
        key=KEY,
        timeframe=Timeframe.D1,
        start=dt.datetime(2026, 1, day, tzinfo=UTC),
        open=Money(close),
        high=Money(close),
        low=Money(close),
        close=Money(close),
        volume=1000,
    )


def test_candle_upsert_is_idempotent_and_does_not_overwrite(db: Database) -> None:
    assert db.upsert_candles([_candle(1, 10000), _candle(2, 10100), _candle(3, 10200)]) == 3

    # Re-fetching a window is the normal way to deepen history. It must not
    # duplicate rows, and it must not rewrite a bar already held -- otherwise
    # yesterday's backtest silently changes.
    assert db.upsert_candles([_candle(2, 99999)]) == 0

    got = db.candles(KEY, Timeframe.D1, dt.datetime(2026, 1, 1, tzinfo=UTC), dt.datetime(2026, 1, 31, tzinfo=UTC))
    assert len(got) == 3
    assert got[1].close == 10100
    assert got[0].start < got[1].start < got[2].start


def test_last_candle_start_drives_incremental_sync(db: Database) -> None:
    assert db.last_candle_start(KEY, Timeframe.D1) is None

    # Insertion order must not matter: the query is max(start), not "last row".
    db.upsert_candles([_candle(1, 100), _candle(5, 100), _candle(3, 100)])
    last = db.last_candle_start(KEY, Timeframe.D1)
    assert last is not None
    assert last.day == 5

    span = db.candle_span(KEY, Timeframe.D1)
    assert span is not None
    assert (span[0].day, span[1].day) == (1, 5)
    assert db.candle_count(KEY, Timeframe.D1) == 3


def test_timeframes_are_separate_series(db: Database) -> None:
    db.upsert_candles([_candle(1, 100)])
    assert db.last_candle_start(KEY, Timeframe.M5) is None
    assert db.candle_count(KEY, Timeframe.M5) == 0


def test_candle_range_is_inclusive(db: Database) -> None:
    db.upsert_candles([_candle(1, 100), _candle(2, 100), _candle(3, 100)])
    got = db.candles(KEY, Timeframe.D1, dt.datetime(2026, 1, 1, tzinfo=UTC), dt.datetime(2026, 1, 2, tzinfo=UTC))
    assert [c.start.day for c in got] == [1, 2]


# ------------------------------------------------------------------------- trading


def test_signal_round_trip(db: Database) -> None:
    run_id = db.start_run("backtest", "donchian-2026", {"period": 20})
    sig = Signal(
        key=KEY,
        kind=SignalKind.LONG,
        at=dt.datetime(2026, 1, 5, 9, 20, tzinfo=UTC),
        price=money.parse("100.00"),
        stop=money.parse("95.00"),
        strategy="donchian",
        metadata={"band": "upper"},
    )
    db.insert_signal(sig, run_id, paper=True)

    got = db.signals(dt.datetime(2026, 1, 1, tzinfo=UTC), paper=True)
    assert len(got) == 1
    assert got[0].kind is SignalKind.LONG
    assert got[0].stop == money.parse("95.00")
    assert got[0].metadata == {"band": "upper"}
    # The live view must not see a paper signal.
    assert db.signals(dt.datetime(2026, 1, 1, tzinfo=UTC), paper=False) == []

    db.finish_run(run_id, "succeeded")


def test_order_lifecycle(db: Database) -> None:
    placed = dt.datetime(2026, 1, 5, 9, 20, tzinfo=UTC)
    order = Order(
        id="ORDER-1",
        request=OrderRequest(
            key=KEY,
            side=Side.BUY,
            quantity=10,
            type=OrderType.LIMIT,
            product=Product.CNC,
            limit_price=money.parse("100.00"),
            time_in_force=TimeInForce.DAY,
            tag="donchian",
        ),
        status=OrderStatus.OPEN,
        placed_at=placed,
        updated_at=placed,
    )
    db.upsert_order(order)

    pending = db.pending_orders()
    assert len(pending) == 1
    assert pending[0].request.limit_price == money.parse("100.00")
    assert pending[0].request.tag == "donchian"

    # Reconciling the same id updates rather than duplicating.
    order.status = OrderStatus.COMPLETE
    order.filled_quantity = 10
    order.average_price = money.parse("100.25")
    order.updated_at = placed + dt.timedelta(minutes=1)
    db.upsert_order(order)

    assert db.pending_orders() == []
    assert len(db.orders_awaiting_protective()) == 1

    db.set_order_protective("ORDER-1", "GTT-99")
    assert db.orders_awaiting_protective() == []

    got = db.order("ORDER-1")
    assert got.average_price == money.parse("100.25")
    assert got.filled_quantity == 10
    assert got.status.terminal
    # The covering stop is read back, and a reconciler re-upserting the
    # order must not wipe it.
    assert got.protective_id == "GTT-99"
    db.upsert_order(order)
    assert db.order("ORDER-1").protective_id == "GTT-99"

    with pytest.raises(NotFoundError):
        db.order("NOPE")


def test_recent_orders_and_signals_are_newest_first_and_mode_scoped(db: Database) -> None:
    base = dt.datetime(2026, 1, 5, 9, 20, tzinfo=UTC)
    for i, oid in enumerate(["O-1", "O-2", "O-3"]):
        at = base + dt.timedelta(minutes=i)
        order = Order(
            id=oid,
            request=OrderRequest(key=KEY, side=Side.BUY, quantity=1, type=OrderType.MARKET, product=Product.CNC),
            status=OrderStatus.OPEN,
            placed_at=at,
            updated_at=at,
        )
        db.upsert_order(order, paper=(i == 2))
        sig = Signal(key=KEY, kind=SignalKind.LONG, at=at, price=Money(100), metadata={"n": oid})
        db.insert_signal(sig, paper=(i == 2))

    orders = db.recent_orders(10)
    # A paper order on a live dashboard looks like a fill that never happened.
    assert [o.id for o in orders] == ["O-2", "O-1"]
    assert len(db.recent_orders(1)) == 1

    signals = db.recent_signals(10)
    assert [s.metadata["n"] for s in signals] == ["O-2", "O-1"]


def _trade(net: str, *, key: str = "T-1", paper: bool = False) -> Trade:
    amount = money.parse(net)
    return Trade(
        key=KEY,
        strategy="donchian",
        side=Side.BUY,
        quantity=10,
        entry_price=money.parse("100.00"),
        exit_price=Money(money.parse("100.00") + amount),
        entry_at=dt.datetime(2026, 1, 5, 10, tzinfo=UTC),
        exit_at=dt.datetime(2026, 1, 6, 10, tzinfo=UTC),
        gross_pnl=amount,
        charges=money.parse("6.94"),
        net_pnl=Money(amount - money.parse("6.94")),
        exit_reason=ExitReason.TARGET,
        paper=paper,
    )


def test_trade_insert_is_idempotent(db: Database) -> None:
    trade = _trade("100.00")
    # Re-running a reconciler over the same broker fills must not duplicate.
    for _ in range(3):
        db.insert_trade("ORDER-1:0", trade)

    got = db.trades()
    assert len(got) == 1
    assert got[0].net_pnl == trade.net_pnl
    assert got[0].charges == money.parse("6.94")
    assert got[0].exit_reason is ExitReason.TARGET


def test_trades_are_scoped_by_mode(db: Database) -> None:
    db.insert_trade("LIVE-1", _trade("10.00"))
    db.insert_trade("PAPER-1", _trade("10.00", paper=True))

    live = db.trades(paper=False)
    paper = db.trades(paper=True)
    assert len(live) == 1 and not live[0].paper
    assert len(paper) == 1 and paper[0].paper


def test_equity_curve_is_scoped_by_mode(db: Database) -> None:
    at = dt.datetime(2026, 1, 5, 15, 30, tzinfo=UTC)
    db.insert_equity_snapshot(at, money.parse("100000.00"), money.parse("500.00"), paper=True)

    curve = db.equity_curve(paper=True)
    assert len(curve) == 1
    assert curve[0].equity == money.parse("100000.00")
    assert db.equity_curve(paper=False) == []


# ---------------------------------------------------------------------------- meta


def test_kv_state(db: Database) -> None:
    with pytest.raises(StateNotFoundError):
        db.get_state("risk_state")

    want = {"date": "2026-01-15", "trades_today": 3}
    db.set_state("risk_state", want)
    assert db.get_state("risk_state") == want

    want["trades_today"] = 7
    db.set_state("risk_state", want)
    assert db.get_state("risk_state")["trades_today"] == 7

    db.delete_state("risk_state")
    with pytest.raises(StateNotFoundError):
        db.get_state("risk_state")


def test_run_finishes_the_run_either_way(db: Database) -> None:
    def status(run_id: int) -> tuple[str, str]:
        row = db.conn.execute("SELECT status, message FROM runs WHERE id = ?", (run_id,)).fetchone()
        return row["status"], row["message"]

    with db.run("live", "test") as ok_id:
        pass
    assert status(ok_id) == ("ok", ""), "a block that completes must finish ok"

    with pytest.raises(RuntimeError, match="broker down"), db.run("live", "test") as fail_id:
        raise RuntimeError("broker down")
    assert status(fail_id) == ("error", "broker down"), "a failed block must close the run as error with the message"


def test_record_fill_writes_signal_and_order_together(db: Database) -> None:
    at = dt.datetime(2026, 1, 5, 9, 20, tzinfo=UTC)
    sig = Signal(key=KEY, kind=SignalKind.LONG, at=at, price=money.parse("100.00"), strategy="x")
    order = Order(
        id="ORDER-F",
        request=OrderRequest(key=KEY, side=Side.BUY, quantity=1, type=OrderType.MARKET, product=Product.CNC),
        status=OrderStatus.COMPLETE,
        placed_at=at,
        updated_at=at,
    )
    signal_id = db.record_fill(sig, order)
    assert signal_id > 0, "record_fill must return the signal's row id"
    assert db.order("ORDER-F").id == "ORDER-F"
    assert len(db.recent_signals(10)) == 1


def test_daily_state_survives_a_restart_but_not_a_day_change(db: Database) -> None:
    state = db.load_daily_state("risk", "2026-01-05")
    assert state.date == "2026-01-05" and state.trades_today == 0, "a cold start must yield empty counters"

    tracker = risk.Tracker(state)
    tracker.record_trade(KEY)
    db.save_daily_state("risk", tracker.snapshot())

    again = db.load_daily_state("risk", "2026-01-05")
    assert again.trades_today == 1 and again.trades_per_key[str(KEY)] == 1, (
        "counters saved today must come back on a restart"
    )
    assert set(db.get_state("risk")) == {
        "date",
        "trades_today",
        "trades_per_key",
        "realized_pnl",
        "open_positions",
        "notional_open",
    }, "the stored shape must be the one go/store writes, kill_switch omitted when empty"

    tomorrow = db.load_daily_state("risk", "2026-01-06")
    assert tomorrow.trades_today == 0 and tomorrow.date == "2026-01-06", (
        "yesterday's counters must not carry into a new day"
    )


def test_connect_migrated_is_ready_to_use() -> None:
    from tradekit.store import connect_migrated

    database = connect_migrated(":memory:")
    try:
        assert database.start_run("live", "x") > 0, "the schema must be in place after connect_migrated"
    finally:
        database.close()


def test_charge_table_round_trip(db: Database) -> None:
    epoch, later = dt.date(2020, 1, 1), dt.date(2026, 2, 1)
    db.upsert_charge_rate("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, costs.Rate(0.001, epoch))
    db.upsert_charge_rate("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, costs.Rate(0.002, later))
    db.upsert_charge_rate("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, costs.Rate(1534, epoch, flat=True))

    table = db.charge_table()

    # Effective dating must survive: a January trade prices at the old rate,
    # a March one at the new.
    early = table.lookup("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, dt.date(2026, 1, 20))
    late = table.lookup("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, dt.date(2026, 3, 1))
    assert early is not None and early.value == 0.001
    assert late is not None and late.value == 0.002

    dp = table.lookup("upstox", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, later)
    assert dp is not None and dp.flat and dp.value == 1534
    # A rate for a different broker must not be found -- this is the confusion
    # that had two projects disagreeing about the DP charge.
    assert table.lookup("zerodha", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, later) is None


def test_charge_table_feeds_costs_compute(db: Database) -> None:
    """The store-to-costs bridge: rates come from data, not from constants."""
    epoch = dt.date(2020, 1, 1)
    for kind, value in (
        (costs.Kind.BROKERAGE, 0.001),
        (costs.Kind.STT_BUY, 0.001),
        (costs.Kind.STT_SELL, 0.001),
        (costs.Kind.EXCHANGE, 0.0001),
        (costs.Kind.SEBI, 0.0001),
        (costs.Kind.STAMP, 0.0001),
        (costs.Kind.GST, 0.18),
    ):
        db.upsert_charge_rate("testbroker", costs.Segment.EQUITY_DELIVERY, kind, costs.Rate(value, epoch))
    db.upsert_charge_rate(
        "testbroker", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, costs.Rate(1500, epoch, flat=True)
    )

    charges = costs.compute(
        db.charge_table(),
        costs.Trade(
            broker="testbroker",
            segment=costs.Segment.EQUITY_DELIVERY,
            quantity=100,
            entry_price=money.parse("100.00"),
            exit_price=money.parse("110.00"),
            entry_at=dt.date(2026, 1, 5),
            exit_at=dt.date(2026, 1, 9),
        ),
    )
    # The same figures as the shared parity fixture, now sourced from the database.
    assert charges.total == 6944


def test_run_lifecycle(db: Database) -> None:
    run_id = db.start_run("sync", "daily-candles", {"lookback": 400})
    row = db.conn.execute("SELECT kind, name, status, ended_at FROM runs WHERE id = ?", (run_id,)).fetchone()
    assert row["kind"] == "sync"
    assert row["status"] == "running"
    assert row["ended_at"] is None

    db.finish_run(run_id, "succeeded", "400 bars")
    row = db.conn.execute("SELECT status, message, ended_at FROM runs WHERE id = ?", (run_id,)).fetchone()
    assert row["status"] == "succeeded"
    assert row["message"] == "400 bars"
    assert row["ended_at"]


# --------------------------------------------------------------------- conventions


def test_naive_datetimes_are_rejected(db: Database) -> None:
    """A naive timestamp is a bug, not a default: it silently loses the zone."""
    naive = Candle(
        key=KEY,
        timeframe=Timeframe.D1,
        start=dt.datetime(2026, 1, 1),
        open=Money(1),
        high=Money(1),
        low=Money(1),
        close=Money(1),
    )
    with pytest.raises(ValueError, match="naive"):
        db.upsert_candles([naive])


def test_timestamps_survive_a_zone_round_trip(db: Database) -> None:
    ist = dt.timezone(dt.timedelta(hours=5, minutes=30))
    start = dt.datetime(2026, 1, 15, 9, 15, tzinfo=ist)
    db.upsert_candles(
        [
            Candle(
                key=KEY,
                timeframe=Timeframe.M5,
                start=start,
                open=Money(1),
                high=Money(1),
                low=Money(1),
                close=Money(1),
            )
        ]
    )
    got = db.candles(KEY, Timeframe.M5, start - dt.timedelta(days=1), start + dt.timedelta(days=1))
    assert len(got) == 1
    assert got[0].start == start
