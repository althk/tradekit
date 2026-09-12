"""The shared golden cases in contracts/testdata, run against the Python core.

``go/core/parity`` runs the same file. A disagreement between the two libraries
about any value in it is a failing build rather than a discovery six months
later, which is the whole reason the contracts directory exists.
"""

from __future__ import annotations

import datetime as dt
import json
from pathlib import Path
from typing import Any

import pytest

from tradekit.core import costs, money, risk, stats
from tradekit.core.domain import (
    Candle,
    ExitReason,
    InstrumentKey,
    OrderRequest,
    OrderType,
    Product,
    Protective,
    Side,
    Timeframe,
    Trade,
)
from tradekit.core.money import Money
from tradekit.core.paper import Broker, Options

FIXTURE = Path(__file__).resolve().parents[2] / "contracts" / "testdata" / "parity.json"


@pytest.fixture(scope="module")
def data() -> dict[str, Any]:
    """The shared fixture, loaded once."""
    return json.loads(FIXTURE.read_text())


def _date(s: str) -> dt.date:
    return dt.date.fromisoformat(s)


def test_money_parse(data: dict[str, Any]) -> None:
    for case in data["money_parse"]:
        assert money.parse(case["text"]) == case["want"], case["text"]


def test_money_parse_invalid(data: dict[str, Any]) -> None:
    for text in data["money_parse_invalid"]:
        with pytest.raises(ValueError):
            money.parse(text)


def test_money_mul_fraction(data: dict[str, Any]) -> None:
    for case in data["money_mul_fraction"]:
        got = money.mul_fraction(Money(case["amount"]), case["fraction"])
        assert got == case["want"], case


def test_money_round_to_tick(data: dict[str, Any]) -> None:
    for case in data["money_round_to_tick"]:
        got = money.round_to_tick(Money(case["amount"]), Money(case["tick"]))
        assert got == case["want"], case


def test_money_floor_ceil_to_tick(data: dict[str, Any]) -> None:
    for case in data["money_floor_ceil_to_tick"]:
        amount, tick = Money(case["amount"]), Money(case["tick"])
        assert money.floor_to_tick(amount, tick) == case["floor"], case
        assert money.ceil_to_tick(amount, tick) == case["ceil"], case


def test_sizing(data: dict[str, Any]) -> None:
    for case in data["sizing"]:
        got = risk.size(
            risk.SizeParams(
                capital=Money(case["capital"]),
                risk_fraction=case["risk_fraction"],
                entry=Money(case["entry"]),
                stop=Money(case["stop"]),
                lot_size=case["lot_size"],
                max_notional=Money(case["max_notional"]),
                leverage=case["leverage"],
            )
        )
        assert got.quantity == case["want_quantity"], f"{case['name']}: {got.reason}"


def test_charges(data: dict[str, Any]) -> None:
    spec = data["charges"]
    rates, since = spec["rates"], _date(spec["effective_from"])
    segment = costs.Segment(spec["segment"])

    table = costs.Table()
    for kind, key in (
        (costs.Kind.BROKERAGE, "brokerage"),
        (costs.Kind.STT_BUY, "stt_buy"),
        (costs.Kind.STT_SELL, "stt_sell"),
        (costs.Kind.EXCHANGE, "exchange"),
        (costs.Kind.SEBI, "sebi"),
        (costs.Kind.STAMP, "stamp"),
        (costs.Kind.GST, "gst"),
    ):
        table.set(spec["broker"], segment, kind, costs.Rate(rates[key], since))
    table.set(spec["broker"], segment, costs.Kind.DP, costs.Rate(rates["dp_flat"], since, flat=True))

    for case in spec["cases"]:
        got = costs.compute(
            table,
            costs.Trade(
                broker=spec["broker"],
                segment=segment,
                quantity=case["quantity"],
                entry_price=Money(case["entry_price"]),
                exit_price=Money(case["exit_price"]),
                entry_at=_date(case["entry_at"]),
                exit_at=_date(case["exit_at"]),
                buying=case["buying"],
            ),
        )
        want = case["want"]
        assert got == costs.Charges(
            brokerage=Money(want["brokerage"]),
            stt=Money(want["stt"]),
            exchange=Money(want["exchange"]),
            sebi=Money(want["sebi"]),
            stamp=Money(want["stamp"]),
            dp=Money(want["dp"]),
            gst=Money(want["gst"]),
            total=Money(want["total"]),
        ), case["name"]


def test_stats(data: dict[str, Any]) -> None:
    spec = data["stats"]
    base = dt.datetime(2026, 1, 1, 10, 0, tzinfo=dt.UTC)
    key = InstrumentKey("NSE", "X")

    trades = []
    for i, net in enumerate(spec["net_pnls"]):
        exit_at = base + dt.timedelta(days=i)
        trades.append(
            Trade(
                key=key,
                side=Side.BUY,
                quantity=1,
                entry_price=money.parse("100.00"),
                exit_price=Money(money.parse("100.00") + net),
                entry_at=exit_at - dt.timedelta(hours=2),
                exit_at=exit_at,
                gross_pnl=Money(net),
                net_pnl=Money(net),
                exit_reason=ExitReason.TARGET,
            )
        )

    got, want = stats.summarize(trades), spec["want"]
    assert got.trades == want["trades"]
    assert got.wins == want["wins"]
    assert got.losses == want["losses"]
    assert got.gross_profit == want["gross_profit"]
    assert got.gross_loss == want["gross_loss"]
    assert got.net_pnl == want["net_pnl"]
    assert got.win_rate == pytest.approx(want["win_rate"])
    assert got.profit_factor == pytest.approx(want["profit_factor"])
    assert got.expectancy == want["expectancy"]
    assert got.avg_win == want["avg_win"]
    assert got.avg_loss == want["avg_loss"]
    assert got.max_drawdown == want["max_drawdown"]


def test_fills(data: dict[str, Any]) -> None:
    """The bar-replay fill rules, driven through the paper simulator.

    ``go/core/parity`` runs this same list against the Go simulator. Wiring both
    to it is what stops an ambiguous bar resolving one way in a Go backtest and
    the other way in a Python one.
    """
    key = InstrumentKey("NSE", "X")
    entry_at = dt.datetime(2026, 1, 1, 10, 0, tzinfo=dt.UTC)
    cases = data["fills"]["cases"]
    assert cases, "no fill cases found in the shared fixture"

    for case in cases:
        side = Side(case["side"])
        b = Broker(Options(cash=money.parse("10000000.00")))

        # Enter at a price that sits between the stop and the target of every
        # case, so the position exists and neither leg is already triggered when
        # the bar under test arrives.
        b.on_tick(key, money.parse("100.00"), entry_at)
        b.place_order(OrderRequest(key=key, side=side, quantity=10, type=OrderType.MARKET, product=Product.CNC))
        b.place_protective(
            Protective(
                key=key,
                side=side.opposite,
                quantity=10,
                stop=Money(case["stop"]),
                target=Money(case["target"]),
            )
        )

        b.on_bar(
            Candle(
                key=key,
                timeframe=Timeframe.D1,
                start=entry_at + dt.timedelta(days=1),
                open=Money(case["open"]),
                high=Money(case["high"]),
                low=Money(case["low"]),
                close=Money(case["close"]),
                volume=1000,
            )
        )

        trades = b.trades()
        if case["want_reason"] == "":
            assert trades == [], case["name"]
            continue
        assert len(trades) == 1, case["name"]
        assert trades[0].exit_reason == ExitReason(case["want_reason"]), case["name"]
        assert trades[0].exit_price == case["want_price"], case["name"]


def test_options(data: dict[str, Any]) -> None:
    from tradekit.core.options import Model

    spec = data["options"]
    m = Model(risk_free=spec["risk_free"], div_yield=spec["div_yield"])
    for c in spec["cases"]:
        if c["op"] == "price":
            got = m.price(c["s"], c["k"], c["t"], c["sigma"], c["call"])
        elif c["op"] == "delta":
            got = m.delta(c["s"], c["k"], c["t"], c["sigma"], c["call"])
        elif c["op"] == "implied_vol":
            iv = m.implied_vol(c["price"], c["s"], c["k"], c["t"], c["call"])
            assert iv is not None, f"{c['name']}: inversion refused"
            got = iv
        else:
            got = m.strike_for_delta(c["s"], c["t"], c["sigma"], c["target"], c["call"])
        assert got == pytest.approx(c["want"], rel=1e-6), (
            f"{c['name']}: the two libraries would choose different strikes"
        )
