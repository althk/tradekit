"""The decision journal: why a decision was made, keyed to the run.

Mirrors ``go/harness/journal.go``. A log line is prose; a journal entry is a
row you can query alongside the trade it produced. The ``decisions`` table is
in the shared schema, so a Python project and a Go project pointed at the same
database must write rows the other can read -- that, more than API symmetry,
is the requirement here.
"""

from __future__ import annotations

import datetime as dt
import json
import logging
from dataclasses import dataclass, field
from typing import Any

from tradekit.core.domain import InstrumentKey
from tradekit.store import Database
from tradekit.store._base import format_time, parse_time

__all__ = ["Decision", "Journal", "encode_detail"]

log = logging.getLogger("tradekit.harness.journal")


@dataclass(frozen=True, slots=True)
class Decision:
    """One considered action.

    Rejections matter as much as actions: "why did nothing happen today" is the
    harder question. A risk gate's reason goes straight into ``reason``, so
    "the kill switch stopped it" is queryable rather than buried in a log.
    """

    run_id: int
    at: dt.datetime
    key: InstrumentKey
    action: str
    """``"enter"``, ``"skip"``, ``"exit"``, ``"resize"``."""
    reason: str = ""
    detail: dict[str, Any] = field(default_factory=dict)
    """Free-form context, stored as JSON."""


def encode_detail(detail: dict[str, Any]) -> str:
    """Render a decision's detail as the JSON the row stores.

    Compact, keys sorted, HTML and non-ASCII left unescaped -- the same bytes
    Go's encoder writes. Sorted keys are not cosmetic: without them the same
    decision written by each language produces different bytes, and any future
    comparison or hash of journal rows diverges for no reason.
    """
    if not detail:
        return "{}"
    return json.dumps(detail, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


class Journal:
    """Records why a decision was made, keyed to the run."""

    def __init__(self, db: Database) -> None:
        """Journal into ``db``, which must have the shared schema applied."""
        self.db = db

    def record(self, d: Decision) -> None:
        """Write one decision.

        Recording must never fail the trade: an error here is logged and
        swallowed. An observability system that can stop trading is a
        liability, not an asset.
        """
        try:
            self.db.conn.execute(
                """
                INSERT INTO decisions (run_id, at, exchange, symbol, action, reason, detail_json)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    d.run_id,
                    format_time(d.at),
                    d.key.exchange,
                    d.key.symbol,
                    d.action,
                    d.reason,
                    encode_detail(d.detail),
                ),
            )
        except Exception:
            log.exception("harness: recording decision failed; trading continues")

    def since(self, run_id: int, frm: dt.datetime) -> list[Decision]:
        """A run's decisions made at or after ``frm``, oldest first."""
        rows = self.db.conn.execute(
            """
            SELECT at, exchange, symbol, action, reason, detail_json
            FROM decisions WHERE run_id = ? AND at >= ? ORDER BY at, id
            """,
            (run_id, format_time(frm)),
        )
        return [
            Decision(
                run_id=run_id,
                at=parse_time(r["at"]),
                key=InstrumentKey(r["exchange"], r["symbol"]),
                action=r["action"],
                reason=r["reason"],
                detail=json.loads(r["detail_json"]) if r["detail_json"] else {},
            )
            for r in rows
        ]
