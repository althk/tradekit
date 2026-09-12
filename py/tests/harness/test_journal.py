"""The decision journal and the log helpers, including the shared journal parity block."""

from __future__ import annotations

import datetime as dt
import json
import logging
from pathlib import Path

import pytest

from tradekit.core.domain import InstrumentKey, Side
from tradekit.core.money import Money
from tradekit.harness import Decision, Journal
from tradekit.harness.journal import encode_detail
from tradekit.harness.obs import attrs, money_str
from tradekit.store import Database, connect

FIXTURE = Path(__file__).resolve().parents[3] / "contracts" / "testdata" / "parity.json"
RELIANCE = InstrumentKey("NSE", "RELIANCE")
IST = dt.timezone(dt.timedelta(hours=5, minutes=30))


@pytest.fixture
def db() -> Database:
    database = connect(":memory:")
    database.migrate()
    yield database
    database.close()


def test_record_then_since_round_trips_including_detail(db: Database) -> None:
    j = Journal(db)
    at = dt.datetime(2025, 4, 17, 9, 20, tzinfo=IST)
    decisions = [
        Decision(1, at, RELIANCE, "skip", "kill switch engaged", {"positions": 3, "gate": "kill"}),
        Decision(1, at + dt.timedelta(minutes=1), RELIANCE, "enter", "breakout", {"stop": "1200.00"}),
        Decision(2, at, RELIANCE, "exit", "eod"),
        Decision(1, at - dt.timedelta(hours=1), RELIANCE, "skip", "before window"),
    ]
    for d in decisions:
        j.record(d)

    got = j.since(1, at)
    assert got == [decisions[0], decisions[1]], "run 1's decisions at or after the cutoff, oldest first"
    assert j.since(2, at) == [decisions[2]]
    assert j.since(3, at) == []


def test_a_decision_with_no_detail_stores_an_empty_object(db: Database) -> None:
    Journal(db).record(Decision(1, dt.datetime.now(dt.UTC), RELIANCE, "skip"))
    row = db.conn.execute("SELECT detail_json FROM decisions").fetchone()
    assert row["detail_json"] == "{}", "the column is always valid JSON"


def test_record_swallows_and_logs_its_own_errors(db: Database, caplog: pytest.LogCaptureFixture) -> None:
    db.conn.execute("DROP TABLE decisions")
    with caplog.at_level(logging.ERROR, logger="tradekit.harness.journal"):
        Journal(db).record(Decision(1, dt.datetime.now(dt.UTC), RELIANCE, "enter"))
    assert "recording decision failed" in caplog.text, (
        "an observability system that can stop trading is a liability; the error is logged, not raised"
    )


def test_parity_journal_detail_encoding(db: Database) -> None:
    block = json.loads(FIXTURE.read_text(encoding="utf-8"))["journal"]
    d = block["decision"]
    assert encode_detail(d["detail"]) == block["want_detail_json"], (
        "detail_json must be byte-identical to what Go writes for the same decision"
    )

    decision = Decision(
        run_id=d["run_id"],
        at=dt.datetime.fromisoformat(d["at"]),
        key=InstrumentKey(d["exchange"], d["symbol"]),
        action=d["action"],
        reason=d["reason"],
        detail=d["detail"],
    )
    Journal(db).record(decision)
    row = db.conn.execute("SELECT * FROM decisions").fetchone()
    assert row["detail_json"] == block["want_detail_json"]
    assert row["at"] == "2025-04-17T09:20:00+05:30", "the timestamp column is ISO-8601 with its offset, as Go writes it"
    assert (row["run_id"], row["exchange"], row["symbol"], row["action"], row["reason"]) == (
        7,
        "NSE",
        "RELIANCE",
        "skip",
        "kill switch engaged",
    )
    assert Journal(db).since(7, decision.at - dt.timedelta(days=1)) == [decision]


def test_a_row_written_by_go_reads_back(db: Database) -> None:
    # Go's store writes RFC 3339 with nanoseconds trimmed; the reader must
    # accept it, since both languages share the table.
    db.conn.execute(
        "INSERT INTO decisions (run_id, at, exchange, symbol, action, reason, detail_json) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        (9, "2025-04-17T03:50:00.5Z", "NSE", "TCS", "resize", "atr widened", '{"from":10,"to":8}'),
    )
    got = Journal(db).since(9, dt.datetime(2025, 4, 17, tzinfo=dt.UTC))
    assert len(got) == 1 and got[0].key == InstrumentKey("NSE", "TCS") and got[0].detail == {"from": 10, "to": 8}
    assert got[0].at == dt.datetime(2025, 4, 17, 3, 50, 0, 500000, tzinfo=dt.UTC)


def test_money_str_renders_rupees_not_paise() -> None:
    assert money_str(Money(123456)) == "1234.56"
    assert money_str(Money(-5)) == "-0.05"


def test_attrs_renders_domain_values_for_logging(caplog: pytest.LogCaptureFixture) -> None:
    extra = attrs(key=RELIANCE, side=Side.BUY, qty=10, reason="breakout", price=Money(123456))
    assert extra == {"key": "NSE:RELIANCE", "side": "buy", "qty": 10, "reason": "breakout", "price": "1234.56"}
    logger = logging.getLogger("tradekit.test")
    with caplog.at_level(logging.INFO, logger="tradekit.test"):
        logger.info("fill", extra=extra)
    record = caplog.records[-1]
    assert record.price == "1234.56" and record.key == "NSE:RELIANCE"  # type: ignore[attr-defined]
