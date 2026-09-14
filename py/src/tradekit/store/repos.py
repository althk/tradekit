"""The repository mixins: instruments, candles, trading rows and metadata.

Method names mirror ``go/store`` in Python's casing, so ``db.UpsertInstruments``
there is ``db.upsert_instruments`` here and the two libraries read each other's
rows without a translation layer.
"""

from __future__ import annotations

import datetime as dt
import json
import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager, suppress
from typing import Any

from ..core import costs, money, risk
from ..core.domain import (
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
from ..core.money import Money
from ..core.stats import Point
from ._base import (
    Base,
    NotFoundError,
    StateNotFoundError,
    format_date,
    format_time,
    parse_date,
    parse_time,
    to_int,
)

__all__ = ["CandleMixin", "InstrumentMixin", "MetaMixin", "TradingMixin"]

_INSTRUMENT_COLUMNS = "exchange, symbol, name, isin, segment, lot_size, tick_size, expiry, strike, option_type, active"

_ORDER_COLUMNS = (
    "id, exchange, symbol, side, quantity, type, product, limit_price, trigger_price, "
    "time_in_force, tag, status, filled_quantity, average_price, placed_at, updated_at, message, protective_id"
)


class InstrumentMixin(Base):
    """The instrument master and the broker identifier mappings."""

    def upsert_instruments(self, instruments: list[Instrument]) -> None:
        """Write the instrument master, replacing rows that exist.

        One transaction for the whole batch: a partially-refreshed universe is
        worse than a stale one, because a scan over it would silently cover only
        the symbols written before the failure.
        """
        if not instruments:
            return
        now = format_time(dt.datetime.now(dt.UTC))
        with self.tx() as conn:
            conn.executemany(
                """
                INSERT INTO instruments
                    (exchange, symbol, name, isin, segment, lot_size, tick_size,
                     expiry, strike, option_type, active, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(exchange, symbol) DO UPDATE SET
                    name = excluded.name, isin = excluded.isin, segment = excluded.segment,
                    lot_size = excluded.lot_size, tick_size = excluded.tick_size,
                    expiry = excluded.expiry, strike = excluded.strike,
                    option_type = excluded.option_type, active = excluded.active,
                    updated_at = excluded.updated_at
                """,
                [
                    (
                        i.key.exchange,
                        i.key.symbol,
                        i.name,
                        i.isin,
                        i.segment,
                        i.lot_size,
                        int(i.tick_size),
                        format_date(i.expiry) if i.expiry else None,
                        int(i.strike) if i.strike else None,
                        i.option_type,
                        to_int(i.active),
                        now,
                    )
                    for i in instruments
                ],
            )

    @staticmethod
    def _row_to_instrument(row: sqlite3.Row) -> Instrument:
        return Instrument(
            key=InstrumentKey(row["exchange"], row["symbol"]),
            name=row["name"],
            isin=row["isin"],
            segment=row["segment"],
            lot_size=row["lot_size"],
            tick_size=Money(row["tick_size"]),
            expiry=parse_date(row["expiry"]) if row["expiry"] else None,
            strike=Money(row["strike"] or 0),
            option_type=row["option_type"],
            active=row["active"] == 1,
        )

    def instrument(self, key: InstrumentKey) -> Instrument:
        """Return one instrument.

        Raises:
            NotFoundError: If no such instrument is stored.

        """
        row = self.conn.execute(
            f"SELECT {_INSTRUMENT_COLUMNS} FROM instruments WHERE exchange = ? AND symbol = ?",
            (key.exchange, key.symbol),
        ).fetchone()
        if row is None:
            raise NotFoundError(f"store: instrument {key}")
        return self._row_to_instrument(row)

    def active_instruments(self, segment: str = "") -> list[Instrument]:
        """The tradable universe, optionally narrowed to one segment."""
        query = f"SELECT {_INSTRUMENT_COLUMNS} FROM instruments WHERE active = 1"
        args: tuple[Any, ...] = ()
        if segment:
            query += " AND segment = ?"
            args = (segment,)
        query += " ORDER BY exchange, symbol"
        return [self._row_to_instrument(r) for r in self.conn.execute(query, args)]

    def set_broker_id(self, key: InstrumentKey, broker: str, broker_id: str) -> None:
        """Record a broker's own identifier for an instrument.

        These live in their own table so adding a broker is not a schema change,
        and so one instrument can carry a Kite token and an Upstox instrument
        key at the same time.
        """
        self.conn.execute(
            """
            INSERT INTO instrument_ids (exchange, symbol, broker, broker_id) VALUES (?, ?, ?, ?)
            ON CONFLICT(exchange, symbol, broker) DO UPDATE SET broker_id = excluded.broker_id
            """,
            (key.exchange, key.symbol, broker, broker_id),
        )

    def broker_id(self, key: InstrumentKey, broker: str) -> str:
        """A broker's identifier for an instrument.

        Raises:
            NotFoundError: If the mapping has not been recorded.

        """
        row = self.conn.execute(
            "SELECT broker_id FROM instrument_ids WHERE exchange = ? AND symbol = ? AND broker = ?",
            (key.exchange, key.symbol, broker),
        ).fetchone()
        if row is None:
            raise NotFoundError(f"store: {broker} id for {key}")
        return row["broker_id"]

    def key_for_broker_id(self, broker: str, broker_id: str) -> InstrumentKey:
        """Resolve a broker's identifier back to an instrument key.

        This is the lookup a tick stream needs, where every message carries the
        broker's token and nothing else.

        Raises:
            NotFoundError: If the mapping has not been recorded.

        """
        row = self.conn.execute(
            "SELECT exchange, symbol FROM instrument_ids WHERE broker = ? AND broker_id = ?",
            (broker, broker_id),
        ).fetchone()
        if row is None:
            raise NotFoundError(f"store: {broker} id {broker_id!r}")
        return InstrumentKey(row["exchange"], row["symbol"])

    def deactivate_missing(self, exchange: str, keep: list[InstrumentKey]) -> int:
        """Mark instruments inactive unless they appear in ``keep``.

        Index membership changes and delistings must stop being scanned, but
        deleting the rows would orphan the candles and trades that reference
        them. Deactivating keeps the history readable.

        Returns:
            How many instruments were deactivated.

        """
        present = {k.symbol for k in keep}
        rows = self.conn.execute(
            "SELECT symbol FROM instruments WHERE exchange = ? AND active = 1", (exchange,)
        ).fetchall()
        stale = [r["symbol"] for r in rows if r["symbol"] not in present]
        if not stale:
            return 0
        now = format_time(dt.datetime.now(dt.UTC))
        with self.tx() as conn:
            conn.executemany(
                "UPDATE instruments SET active = 0, updated_at = ? WHERE exchange = ? AND symbol = ?",
                [(now, exchange, s) for s in stale],
            )
        return len(stale)


class CandleMixin(Base):
    """Bar storage and the queries an incremental sync is built on."""

    def upsert_candles(self, candles: list[Candle]) -> int:
        """Store bars, ignoring ones already held.

        ``INSERT OR IGNORE`` rather than ``REPLACE``: re-fetching a window is
        the normal way to deepen history, and a vendor occasionally returns a
        slightly different volume for an old bar. Keeping the first value read
        makes a backtest reproducible; overwriting would silently change
        yesterday's results.

        Returns:
            How many rows were new.

        """
        if not candles:
            return 0
        before = self.conn.total_changes
        with self.tx() as conn:
            conn.executemany(
                """
                INSERT OR IGNORE INTO candles
                    (exchange, symbol, timeframe, start, open, high, low, close, volume, open_interest)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                [
                    (
                        c.key.exchange,
                        c.key.symbol,
                        str(c.timeframe),
                        format_time(c.start),
                        int(c.open),
                        int(c.high),
                        int(c.low),
                        int(c.close),
                        c.volume,
                        c.open_interest,
                    )
                    for c in candles
                ],
            )
        return self.conn.total_changes - before

    def candles(self, key: InstrumentKey, timeframe: Timeframe, start: dt.datetime, end: dt.datetime) -> list[Candle]:
        """Bars in ``[start, end]`` inclusive, oldest first."""
        rows = self.conn.execute(
            """
            SELECT start, open, high, low, close, volume, open_interest
            FROM candles
            WHERE exchange = ? AND symbol = ? AND timeframe = ? AND start >= ? AND start <= ?
            ORDER BY start
            """,
            (key.exchange, key.symbol, str(timeframe), format_time(start), format_time(end)),
        )
        return [
            Candle(
                key=key,
                timeframe=timeframe,
                start=parse_time(r["start"]),
                open=Money(r["open"]),
                high=Money(r["high"]),
                low=Money(r["low"]),
                close=Money(r["close"]),
                volume=r["volume"],
                open_interest=r["open_interest"],
            )
            for r in rows
        ]

    def last_candle_start(self, key: InstrumentKey, timeframe: Timeframe) -> dt.datetime | None:
        """The newest stored bar's start, or ``None`` when none is held.

        This is the query an incremental sync is built on: fetch only from here
        forward. Every project tradekit replaces wrote its own version of it,
        and it is why candles are keyed by start rather than by a row id.
        """
        row = self.conn.execute(
            "SELECT max(start) AS latest FROM candles WHERE exchange = ? AND symbol = ? AND timeframe = ?",
            (key.exchange, key.symbol, str(timeframe)),
        ).fetchone()
        if row is None or row["latest"] is None:
            return None
        return parse_time(row["latest"])

    def candle_span(self, key: InstrumentKey, timeframe: Timeframe) -> tuple[dt.datetime, dt.datetime] | None:
        """The oldest and newest stored bar starts, or ``None`` when none is held.

        Answers "how much history do I actually have", which a backtest must
        know before trusting its own warm-up.
        """
        row = self.conn.execute(
            "SELECT min(start) AS lo, max(start) AS hi FROM candles "
            "WHERE exchange = ? AND symbol = ? AND timeframe = ?",
            (key.exchange, key.symbol, str(timeframe)),
        ).fetchone()
        if row is None or row["lo"] is None:
            return None
        return parse_time(row["lo"]), parse_time(row["hi"])

    def candle_count(self, key: InstrumentKey, timeframe: Timeframe) -> int:
        """How many bars are stored for an instrument and timeframe."""
        row = self.conn.execute(
            "SELECT count(*) AS n FROM candles WHERE exchange = ? AND symbol = ? AND timeframe = ?",
            (key.exchange, key.symbol, str(timeframe)),
        ).fetchone()
        return int(row["n"])


class TradingMixin(Base):
    """Signals, orders, trades and equity snapshots."""

    def insert_signal(self, signal: Signal, run_id: int | None = None, paper: bool = False) -> int:
        """Record a signal and return its row id."""
        cur = self.conn.execute(
            """
            INSERT INTO signals (run_id, exchange, symbol, kind, at, price, stop, target, strategy, metadata, paper)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                run_id,
                signal.key.exchange,
                signal.key.symbol,
                str(signal.kind),
                format_time(signal.at),
                int(signal.price),
                int(signal.stop),
                int(signal.target),
                signal.strategy,
                json.dumps(signal.metadata),
                to_int(paper),
            ),
        )
        return int(cur.lastrowid or 0)

    def signals(self, since: dt.datetime, paper: bool = False) -> list[Signal]:
        """Signals recorded at or after ``since``, oldest first."""
        rows = self.conn.execute(
            "SELECT exchange, symbol, kind, at, price, stop, target, strategy, metadata "
            "FROM signals WHERE at >= ? AND paper = ? ORDER BY at",
            (format_time(since), to_int(paper)),
        )
        return [self._row_to_signal(r) for r in rows]

    def recent_signals(self, limit: int, paper: bool = False) -> list[Signal]:
        """The newest ``limit`` signals in one execution mode, newest first."""
        rows = self.conn.execute(
            "SELECT exchange, symbol, kind, at, price, stop, target, strategy, metadata "
            "FROM signals WHERE paper = ? ORDER BY at DESC, id DESC LIMIT ?",
            (to_int(paper), limit),
        )
        return [self._row_to_signal(r) for r in rows]

    @staticmethod
    def _row_to_signal(r: sqlite3.Row) -> Signal:
        return Signal(
            key=InstrumentKey(r["exchange"], r["symbol"]),
            kind=SignalKind(r["kind"]),
            at=parse_time(r["at"]),
            price=Money(r["price"]),
            stop=Money(r["stop"]),
            target=Money(r["target"]),
            strategy=r["strategy"],
            metadata=json.loads(r["metadata"]),
        )

    def upsert_order(self, order: Order, paper: bool = False) -> None:
        """Write an order, updating the mutable fields if it already exists.

        Placing and then reconciling the same order is the normal path, so this
        is called repeatedly for one id.
        """
        r = order.request
        self.conn.execute(
            """
            INSERT INTO orders
                (id, exchange, symbol, side, quantity, type, product, limit_price, trigger_price,
                 time_in_force, tag, status, filled_quantity, average_price, placed_at, updated_at, message,
                 protective_id, paper)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(id) DO UPDATE SET
                status = excluded.status, filled_quantity = excluded.filled_quantity,
                average_price = excluded.average_price, updated_at = excluded.updated_at,
                message = excluded.message
            """,
            (
                order.id,
                r.key.exchange,
                r.key.symbol,
                str(r.side),
                r.quantity,
                str(r.type),
                str(r.product),
                int(r.limit_price),
                int(r.trigger_price),
                str(r.time_in_force),
                r.tag,
                str(order.status),
                order.filled_quantity,
                int(order.average_price),
                format_time(order.placed_at) if order.placed_at else "",
                format_time(order.updated_at) if order.updated_at else "",
                order.message,
                order.protective_id,
                to_int(paper),
            ),
        )

    def record_fill(self, signal: Signal, order: Order, run_id: int | None = None, paper: bool = False) -> int:
        """Write the signal that was acted on and the order it produced in one transaction.

        Written separately, a crash between the two leaves a signal with no
        order or an order with no signal, and the morning's reconciliation
        cannot tell that from a signal that was skipped. Together they are one
        fact: this signal became this order.

        Returns:
            The signal's row id.

        """
        with self.tx():
            signal_id = self.insert_signal(signal, run_id, paper)
            self.upsert_order(order, paper)
        return signal_id

    def set_order_protective(self, order_id: str, protective_id: str) -> None:
        """Record the resting stop placed for a filled order.

        Revisiting orders without one is what stops a stop that failed to place
        from being silently lost.
        """
        self.conn.execute("UPDATE orders SET protective_id = ? WHERE id = ?", (protective_id, order_id))

    @staticmethod
    def _row_to_order(row: sqlite3.Row) -> Order:
        return Order(
            id=row["id"],
            request=OrderRequest(
                key=InstrumentKey(row["exchange"], row["symbol"]),
                side=Side(row["side"]),
                quantity=row["quantity"],
                type=OrderType(row["type"]),
                product=Product(row["product"]),
                limit_price=Money(row["limit_price"]),
                trigger_price=Money(row["trigger_price"]),
                time_in_force=TimeInForce(row["time_in_force"]),
                tag=row["tag"],
            ),
            status=OrderStatus(row["status"]),
            filled_quantity=row["filled_quantity"],
            average_price=Money(row["average_price"]),
            placed_at=parse_time(row["placed_at"]) if row["placed_at"] else None,
            updated_at=parse_time(row["updated_at"]) if row["updated_at"] else None,
            message=row["message"],
            protective_id=row["protective_id"],
        )

    def order(self, order_id: str) -> Order:
        """One order by id.

        Raises:
            NotFoundError: If no such order is stored.

        """
        row = self.conn.execute(f"SELECT {_ORDER_COLUMNS} FROM orders WHERE id = ?", (order_id,)).fetchone()
        if row is None:
            raise NotFoundError(f"store: order {order_id}")
        return self._row_to_order(row)

    def pending_orders(self, paper: bool = False) -> list[Order]:
        """Orders that have not reached a terminal state.

        These are what a reconciler must keep checking against the broker.
        """
        rows = self.conn.execute(
            f"SELECT {_ORDER_COLUMNS} FROM orders WHERE paper = ? AND status NOT IN (?, ?, ?) ORDER BY placed_at",
            (
                to_int(paper),
                str(OrderStatus.COMPLETE),
                str(OrderStatus.CANCELLED),
                str(OrderStatus.REJECTED),
            ),
        )
        return [self._row_to_order(r) for r in rows]

    def orders_awaiting_protective(self, paper: bool = False) -> list[Order]:
        """Filled entries with no resting stop recorded yet."""
        rows = self.conn.execute(
            f"SELECT {_ORDER_COLUMNS} FROM orders "
            "WHERE paper = ? AND status = ? AND filled_quantity > 0 AND protective_id = '' "
            "ORDER BY placed_at",
            (to_int(paper), str(OrderStatus.COMPLETE)),
        )
        return [self._row_to_order(r) for r in rows]

    def recent_orders(self, limit: int, paper: bool = False) -> list[Order]:
        """The newest ``limit`` orders in one execution mode, newest first.

        The dashboard query: what the bot has done lately, live or not.
        """
        rows = self.conn.execute(
            f"SELECT {_ORDER_COLUMNS} FROM orders WHERE paper = ? ORDER BY placed_at DESC LIMIT ?",
            (to_int(paper), limit),
        )
        return [self._row_to_order(r) for r in rows]

    def insert_trade(self, key: str, trade: Trade, run_id: int | None = None) -> None:
        """Record a closed round trip.

        ``key`` must be unique per matched lot, conventionally
        ``"<exit_order_id>:<index>"``, which is what makes re-running a
        reconciler over the same broker fills idempotent rather than
        duplicating every trade.
        """
        self.conn.execute(
            """
            INSERT OR IGNORE INTO trades
                (key, run_id, exchange, symbol, strategy, side, quantity, entry_price, exit_price,
                 entry_at, exit_at, gross_pnl, charges, net_pnl, exit_reason, paper)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                key,
                run_id,
                trade.key.exchange,
                trade.key.symbol,
                trade.strategy,
                str(trade.side),
                trade.quantity,
                int(trade.entry_price),
                int(trade.exit_price),
                format_time(trade.entry_at),
                format_time(trade.exit_at),
                int(trade.gross_pnl),
                int(trade.charges),
                int(trade.net_pnl),
                str(trade.exit_reason),
                to_int(trade.paper),
            ),
        )

    def trades(self, since: dt.datetime | None = None, paper: bool = False) -> list[Trade]:
        """Closed trades exited at or after ``since``, oldest first.

        Results are scoped to one execution mode. Paper and live rows share this
        table, and blending them contaminates every aggregate derived from it
        with fills that were never real, so the caller must say which it wants.
        """
        cutoff = format_time(since) if since else ""
        rows = self.conn.execute(
            """
            SELECT exchange, symbol, strategy, side, quantity, entry_price, exit_price,
                   entry_at, exit_at, gross_pnl, charges, net_pnl, exit_reason, paper
            FROM trades WHERE exit_at >= ? AND paper = ? ORDER BY exit_at
            """,
            (cutoff, to_int(paper)),
        )
        return [
            Trade(
                key=InstrumentKey(r["exchange"], r["symbol"]),
                strategy=r["strategy"],
                side=Side(r["side"]),
                quantity=r["quantity"],
                entry_price=Money(r["entry_price"]),
                exit_price=Money(r["exit_price"]),
                entry_at=parse_time(r["entry_at"]),
                exit_at=parse_time(r["exit_at"]),
                gross_pnl=Money(r["gross_pnl"]),
                charges=Money(r["charges"]),
                net_pnl=Money(r["net_pnl"]),
                exit_reason=ExitReason(r["exit_reason"]),
                paper=r["paper"] == 1,
            )
            for r in rows
        ]

    def insert_equity_snapshot(
        self,
        at: dt.datetime,
        balance: Money,
        realized: Money = money.ZERO,
        unrealized: Money = money.ZERO,
        open_positions: int = 0,
        paper: bool = False,
        run_id: int | None = None,
    ) -> None:
        """Record one sample of account state."""
        self.conn.execute(
            """
            INSERT INTO equity_snapshots (run_id, at, balance, realized_pnl, unrealized_pnl, open_positions, paper)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                run_id,
                format_time(at),
                int(balance),
                int(realized),
                int(unrealized),
                open_positions,
                to_int(paper),
            ),
        )

    def equity_curve(self, since: dt.datetime | None = None, paper: bool = False) -> list[Point]:
        """Balance samples at or after ``since``, in the shape ``core.stats`` consumes."""
        cutoff = format_time(since) if since else ""
        rows = self.conn.execute(
            "SELECT at, balance FROM equity_snapshots WHERE at >= ? AND paper = ? ORDER BY at",
            (cutoff, to_int(paper)),
        )
        return [Point(parse_time(r["at"]), Money(r["balance"])) for r in rows]


class MetaMixin(Base):
    """Runs, key-value state and the charge rate card."""

    def set_state(self, key: str, value: Any) -> None:
        """Store a JSON-encoded value under a key."""
        self.conn.execute(
            """
            INSERT INTO kv_state (key, value, updated_at) VALUES (?, ?, ?)
            ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
            """,
            (key, json.dumps(value), format_time(dt.datetime.now(dt.UTC))),
        )

    def get_state(self, key: str) -> Any:
        """Decode the value stored under a key.

        Raises:
            StateNotFoundError: If the key has never been written. A cold start
                is normal and must be distinguishable from a successful read.

        """
        row = self.conn.execute("SELECT value FROM kv_state WHERE key = ?", (key,)).fetchone()
        if row is None:
            raise StateNotFoundError(f"store: state key {key!r}")
        return json.loads(row["value"])

    def delete_state(self, key: str) -> None:
        """Remove a key. Deleting one that does not exist is not an error."""
        self.conn.execute("DELETE FROM kv_state WHERE key = ?", (key,))

    def start_run(self, kind: str, name: str = "", params: Any = None) -> int:
        """Open a named execution and return its id.

        Every row that run produces should carry the id, so a backtest's output
        can be told apart from a live session's.
        """
        cur = self.conn.execute(
            "INSERT INTO runs (kind, name, params, started_at, status) VALUES (?, ?, ?, ?, 'running')",
            (
                kind,
                name,
                json.dumps(params if params is not None else {}),
                format_time(dt.datetime.now(dt.UTC)),
            ),
        )
        return int(cur.lastrowid or 0)

    def finish_run(self, run_id: int, status: str, message: str = "") -> None:
        """Close a run with a terminal status."""
        self.conn.execute(
            "UPDATE runs SET ended_at = ?, status = ?, message = ? WHERE id = ?",
            (format_time(dt.datetime.now(dt.UTC)), status, message, run_id),
        )

    @contextmanager
    def run(self, kind: str, name: str = "", params: Any = None) -> Iterator[int]:
        """Bracket a block in a run: :meth:`start_run` before, :meth:`finish_run` after.

        The run is closed ``"ok"`` when the block completes and ``"error"``
        with the exception's message when it raises; the exception propagates.
        A run left ``running`` forever because the process died between the
        work and ``finish_run`` is the ordinary outcome of writing the bracket
        by hand at every return; this makes the finish unconditional.

        Yields:
            The run id, for every row the block writes.

        """
        run_id = self.start_run(kind, name, params)
        try:
            yield run_id
        except BaseException as exc:
            # A failure to record the failure must not mask the failure.
            with suppress(Exception):
                self.finish_run(run_id, "error", str(exc))
            raise
        self.finish_run(run_id, "ok")

    def load_daily_state(self, key: str, date: str) -> risk.DailyState:
        """The risk counters stored under ``key`` if they belong to ``date``, else empty counters.

        Empty on a cold start, on the first run of a new day, and when the key
        was written by another date. A read that fails for any other reason
        propagates, because starting the day with zeroed counters after a
        failed read is exactly the silent failure the counters exist to prevent.

        The stored shape is the Go library's JSON for ``risk.DailyState``, so
        the two read each other's rows.
        """
        try:
            raw = self.get_state(key)
        except StateNotFoundError:
            return risk.DailyState(date=date)
        return risk.DailyState(
            date=str(raw.get("date", "")),
            trades_today=int(raw.get("trades_today", 0)),
            trades_per_key={str(k): int(v) for k, v in (raw.get("trades_per_key") or {}).items()},
            realized_pnl=Money(int(raw.get("realized_pnl", 0))),
            open_positions=int(raw.get("open_positions", 0)),
            notional_open=Money(int(raw.get("notional_open", 0))),
            kill_switch=str(raw.get("kill_switch", "")),
        ).fresh_for(date)

    def save_daily_state(self, key: str, state: risk.DailyState) -> None:
        """Persist the counters under ``key``, typically from ``Tracker.snapshot`` after each trade."""
        value: dict[str, Any] = {
            "date": state.date,
            "trades_today": state.trades_today,
            "trades_per_key": dict(state.trades_per_key),
            "realized_pnl": int(state.realized_pnl),
            "open_positions": state.open_positions,
            "notional_open": int(state.notional_open),
        }
        if state.kill_switch:
            value["kill_switch"] = state.kill_switch
        self.set_state(key, value)

    def upsert_charge_rate(self, broker: str, segment: costs.Segment, kind: costs.Kind, rate: costs.Rate) -> None:
        """Record one effective-dated charge rate."""
        self.conn.execute(
            """
            INSERT INTO charge_rates (broker, segment, kind, effective_from, rate, flat)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(broker, segment, kind, effective_from) DO UPDATE SET
                rate = excluded.rate, flat = excluded.flat
            """,
            (
                broker,
                str(segment),
                str(kind),
                format_date(rate.effective_from),
                rate.value,
                to_int(rate.flat),
            ),
        )

    def charge_table(self) -> costs.Table:
        """Load every stored rate into a :class:`~tradekit.core.costs.Table`.

        This is the bridge that keeps ``costs`` free of a database dependency:
        the rate card is data, and this is where the data comes from.
        """
        table = costs.Table()
        rows = self.conn.execute(
            "SELECT broker, segment, kind, effective_from, rate, flat FROM charge_rates ORDER BY effective_from"
        )
        for r in rows:
            table.set(
                r["broker"],
                costs.Segment(r["segment"]),
                costs.Kind(r["kind"]),
                costs.Rate(
                    value=r["rate"],
                    effective_from=parse_date(r["effective_from"]),
                    flat=r["flat"] == 1,
                ),
            )
        return table
