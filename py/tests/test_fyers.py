"""The FYERS adapter. No live calls: every test drives an httpx.MockTransport."""

from __future__ import annotations

import datetime as dt
import json
from itertools import pairwise
from pathlib import Path

import httpx
import pytest

from tradekit.core.domain import (
    InstrumentKey,
    MarginLeg,
    OrderRequest,
    OrderStatus,
    OrderType,
    Product,
    Protective,
    Side,
    Timeframe,
    TimeInForce,
)
from tradekit.core.money import Money
from tradekit.core.money import parse as money_parse
from tradekit.fyers import FyersClient, parse_symbol_master
from tradekit.fyers.client import _from_gtt, _gtt_is_live, _gtt_legs
from tradekit.fyers.mapping import (
    IST,
    chunk_days,
    from_order_type,
    from_product,
    from_side,
    key_for,
    normalize_status,
    paise,
    parse_order_time,
    rupees,
    strip_tag_prefix,
    symbol_for,
    to_order_type,
    to_product,
    to_resolution,
    to_side,
    to_validity,
)
from tradekit.fyers.transport import (
    RejectedError,
    TokenExpiredError,
    TransientError,
    Transport,
)

SBIN = InstrumentKey(exchange="NSE", symbol="SBIN")
IDEA = InstrumentKey(exchange="NSE", symbol="IDEA")
NIFTY_FUT = InstrumentKey(exchange="NFO", symbol="NIFTY24JANFUT")


class Recorder:
    """A MockTransport handler that answers from a route table and records."""

    def __init__(self, routes: dict[str, object], status: int = 200):
        """Answer requests for the given paths; every other path 404s."""
        self.routes = routes
        self.status = status
        self.requests: list[httpx.Request] = []

    def __call__(self, request: httpx.Request) -> httpx.Response:
        """Record the request and serve its route."""
        self.requests.append(request)
        body = self.routes.get(request.url.path)
        if body is None:
            return httpx.Response(404, json={"s": "error", "code": -50, "message": "no route"})
        return httpx.Response(self.status, json=body)

    def sent(self, path: str) -> dict:
        """The JSON body sent to ``path``."""
        for request in self.requests:
            if request.url.path == path and request.content:
                return json.loads(request.content)
        raise AssertionError(f"nothing was sent to {path}")


def make_transport(handler, **kwargs) -> Transport:
    """A transport over the given handler, with no real sleeping."""
    kwargs.setdefault("sleep", lambda _: None)
    return Transport(
        auth_provider=lambda: "APP-100:token",
        base_url="https://api.test/api/v3",
        data_url="https://api.test/data",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,  # throttling is not what these tests are about
        retry_base=1.0,
        **kwargs,
    )


def make_client(handler, **kwargs) -> FyersClient:
    """A client whose transport is the given handler."""
    return FyersClient(
        app_id="APP-100",
        access_token="token",
        transport=make_transport(handler),
        symbol_master_url="https://public.test",
        http_client=httpx.Client(transport=httpx.MockTransport(handler)),
        **kwargs,
    )


# --- mapping, pinned against the Go side by parity.json ---------------


def test_symbol_round_trips_through_key_for():
    for key in (
        SBIN,
        InstrumentKey(exchange="NSE", symbol="MODIRUBBER-BE"),
        NIFTY_FUT,
        InstrumentKey(exchange="NFO", symbol="NIFTY2410825000CE"),
        InstrumentKey(exchange="BFO", symbol="SENSEX24JANFUT"),
        InstrumentKey(exchange="MCX", symbol="GOLD24DECFUT"),
    ):
        assert key_for(symbol_for(key)) == key, "a key must survive the wire round trip or the store cannot find it"


def test_key_for_trusts_the_segment_over_the_symbol_shape():
    assert key_for("NSE:USDINR24JANFUT", 12).exchange == "CDS"
    assert key_for("NSE:NIFTY50-INDEX") == InstrumentKey(exchange="NSE", symbol="NIFTY50-INDEX")
    assert key_for("SBIN") == InstrumentKey(exchange="", symbol="SBIN")


def test_paise_rounds_half_away_from_zero_and_tolerates_junk():
    assert paise(1057.605) == 105761
    assert paise(-1057.605) == -105761
    assert paise("426.9") == 42690, "the quotes sample sends some numbers as strings"
    assert paise("") == 0 and paise(None) == 0 and paise("nan") == 0
    assert rupees(money_parse("2456.75")) == 2456.75


def test_margin_product_is_refused_and_mtf_reads_as_nrml():
    with pytest.raises(ValueError):
        to_product(Product.MARGIN)
    assert from_product("MTF") == Product.NRML, "a margin-funded position is not an unencumbered delivery holding"
    assert from_product("intraday") == Product.MIS


def test_order_type_codes_are_the_documented_integers():
    assert [to_order_type(t) for t in (OrderType.LIMIT, OrderType.MARKET, OrderType.STOP, OrderType.STOP_LIMIT)] == [
        1,
        2,
        3,
        4,
    ]
    assert from_order_type(3) == OrderType.STOP
    assert from_order_type(9) == OrderType.MARKET, "descriptive on read-back; placing goes through to_order_type"


def test_gtt_is_not_an_order_validity():
    with pytest.raises(ValueError):
        to_validity(TimeInForce.GTT)
    assert to_validity("") == "DAY"


def test_unknown_status_is_pending_not_terminal():
    assert normalize_status(6) == OrderStatus.OPEN, "6 is resting at the exchange"
    assert normalize_status(4) == OrderStatus.PENDING, "4 is in transit"
    assert normalize_status(7) == OrderStatus.CANCELLED, "expired ends the order as surely as a cancel"
    assert not normalize_status(3).terminal


def test_sides_are_signed_integers():
    assert to_side(Side.BUY) == 1 and to_side(Side.SELL) == -1
    assert from_side(-1) == Side.SELL and from_side(0) == Side.BUY


def test_resolution_and_chunk_days_stay_under_the_ceilings():
    assert to_resolution(Timeframe.M5) == "5" and to_resolution(Timeframe.D1) == "D"
    with pytest.raises(ValueError):
        to_resolution("2h")
    assert chunk_days(Timeframe.M1) < 100
    assert 100 < chunk_days(Timeframe.D1) < 366


def test_order_time_is_read_as_ist():
    assert parse_order_time("09-Mar-2023 09:34:38") == dt.datetime(2023, 3, 9, 9, 34, 38, tzinfo=IST)
    assert parse_order_time("garbage") is None, "a fabricated time silently reorders a book"


def test_strip_tag_prefix_restores_the_callers_tag():
    assert strip_tag_prefix("1:breakout") == "breakout"
    assert strip_tag_prefix("2:Untagged") == ""
    assert strip_tag_prefix("plain") == "plain"


# --- transport --------------------------------------------------------


def test_authorization_header_carries_the_app_id():
    recorder = Recorder({"/api/v3/funds": {"s": "ok", "code": 200, "fund_limit": [{"id": 10, "equityAmount": 1}]}})
    make_client(recorder).account()
    assert recorder.requests[0].headers["Authorization"] == "APP-100:token", (
        "FYERS authenticates with appId:token; a bare token is answered with -15 and no hint"
    )


def test_retry_after_milliseconds_is_preferred_then_the_call_succeeds():
    calls = {"n": 0}
    slept: list[float] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(
                429,
                headers={"Retry-After": "1", "X-Retry-After-Ms": "150"},
                json={"s": "error", "code": -429, "message": "Rate limit exceeded"},
            )
        return httpx.Response(200, json={"s": "ok", "code": 200, "orderBook": []})

    transport = make_transport(handler, sleep=slept.append)
    assert transport.request("GET", "/orders", retry=True)["s"] == "ok"
    assert calls["n"] == 2
    assert slept == [0.15], "the millisecond header is the precise one and must win over Retry-After"


def test_server_error_retries_and_gives_up_at_the_bound():
    calls = {"n": 0}
    slept: list[float] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(502, text="bad gateway")

    transport = make_transport(handler, sleep=slept.append)
    with pytest.raises(TransientError):
        transport.request("GET", "/orders", retry=True)
    assert calls["n"] == 4
    assert slept == [1.0, 2.0, 4.0], "backoff doubles from the base"


def test_client_error_is_not_retried_and_carries_the_code():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(400, json={"s": "error", "code": -50, "message": "Invalid input"})

    with pytest.raises(RejectedError) as excinfo:
        make_transport(handler).request("GET", "/orders", retry=True)
    assert calls["n"] == 1, "a rejection re-sent unchanged is refused again"
    assert excinfo.value.code == -50


def test_expired_token_under_a_200_is_still_a_token_failure():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(200, json={"s": "error", "code": -8, "message": "Token expired"})

    with pytest.raises(TokenExpiredError):
        make_transport(handler).request("GET", "/orders", retry=True)
    assert calls["n"] == 1, "retrying with the same token cannot succeed"


def test_a_refusal_under_a_200_is_not_read_as_success():
    recorder = Recorder({"/api/v3/orders/sync": {"s": "error", "code": -99, "message": "Insufficient funds"}})
    with pytest.raises(RejectedError):
        make_client(recorder).place_order(OrderRequest(key=SBIN, side=Side.BUY, quantity=1, product=Product.MIS))


def test_place_order_is_never_retried():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(503, text="unavailable")

    with pytest.raises(TransientError):
        make_client(handler).place_order(OrderRequest(key=SBIN, side=Side.BUY, quantity=1, product=Product.MIS))
    assert calls["n"] == 1, "retrying a POST that may have placed an order places a second one"


# --- broker -----------------------------------------------------------

FUNDS = {
    "code": 200,
    "message": "",
    "s": "ok",
    "fund_limit": [
        {"id": 1, "title": "Total Balance", "equityAmount": 150000.25, "commodityAmount": 9999},
        {"id": 2, "title": "Utilized Amount", "equityAmount": 25000.00, "commodityAmount": 0},
        {"id": 3, "title": "Clear Balance", "equityAmount": 149000.00, "commodityAmount": 0},
        {"id": 10, "title": "Available Balance", "equityAmount": 125000.25, "commodityAmount": 0},
    ],
}


def test_account_reads_the_available_balance_row():
    account = make_client(Recorder({"/api/v3/funds": FUNDS})).account()
    assert account.available == money_parse("125000.25"), "id 10, not Clear Balance"
    assert account.used == money_parse("25000.00")
    assert account.equity == money_parse("150000.25")


def test_account_falls_back_to_titles_and_refuses_an_empty_ledger():
    renumbered = {"s": "ok", "fund_limit": [{"id": 99, "title": "Available Balance", "equityAmount": 10.5}]}
    account = make_client(Recorder({"/api/v3/funds": renumbered})).account()
    assert account.available == money_parse("10.50")
    assert account.equity == account.available + account.used
    with pytest.raises(RuntimeError):
        make_client(Recorder({"/api/v3/funds": {"s": "ok", "fund_limit": []}})).account()


def test_place_order_returns_pending_with_the_brokers_id():
    recorder = Recorder({"/api/v3/orders/sync": {"s": "ok", "code": 1101, "message": "ok", "id": "808058117761"}})
    order = make_client(recorder, tag="default").place_order(
        OrderRequest(
            key=SBIN,
            side=Side.BUY,
            quantity=10,
            type=OrderType.LIMIT,
            product=Product.CNC,
            limit_price=money_parse("612.35"),
            tag="breakout",
        )
    )
    assert order.id == "808058117761"
    assert order.status == OrderStatus.PENDING, "an acknowledgement is not a fill"
    sent = recorder.sent("/api/v3/orders/sync")
    assert sent == {
        "symbol": "NSE:SBIN-EQ",
        "qty": 10,
        "type": 1,
        "side": 1,
        "productType": "CNC",
        "limitPrice": 612.35,
        "stopPrice": 0.0,
        "validity": "DAY",
        "disclosedQty": 0,
        "offlineOrder": False,
        "stopLoss": 0.0,
        "takeProfit": 0.0,
        "orderTag": "breakout",
    }


def test_place_order_refuses_an_acknowledgement_with_no_id():
    recorder = Recorder({"/api/v3/orders/sync": {"s": "ok", "code": 201, "message": "in transit"}})
    with pytest.raises(RuntimeError):
        make_client(recorder).place_order(OrderRequest(key=SBIN, side=Side.BUY, quantity=1, product=Product.MIS))


def test_place_order_refuses_a_stop_with_no_trigger():
    with pytest.raises(ValueError):
        make_client(Recorder({})).place_order(
            OrderRequest(key=SBIN, side=Side.SELL, quantity=1, type=OrderType.STOP, product=Product.MIS)
        )


def test_cancel_order_sends_the_id_in_the_body():
    recorder = Recorder({"/api/v3/orders/sync": {"s": "ok", "code": 1103, "id": "808058117761"}})
    make_client(recorder).cancel_order("808058117761")
    assert recorder.requests[0].method == "DELETE"
    assert recorder.sent("/api/v3/orders/sync") == {"id": "808058117761"}


ORDER_BOOK = {
    "s": "ok",
    "code": 200,
    "orderBook": [
        {
            "id": "23030900015105",
            "qty": 10,
            "filledQty": 6,
            "limitPrice": 6.95,
            "stopPrice": 0,
            "tradedPrice": 6.9,
            "type": 1,
            "segment": 10,
            "symbol": "NSE:IDEA-EQ",
            "orderDateTime": "09-Mar-2023 09:34:38",
            "orderValidity": "DAY",
            "productType": "CNC",
            "side": -1,
            "status": 6,
            "orderTag": "1:breakout",
        },
        {
            "id": "23030900015106",
            "qty": 1,
            "filledQty": 1,
            "tradedPrice": 100.5,
            "type": 2,
            "segment": 11,
            "symbol": "NSE:NIFTY24JANFUT",
            "productType": "MARGIN",
            "side": 1,
            "status": 2,
            "orderTag": "2:Untagged",
        },
        {
            "id": "23030900015107",
            "qty": 1,
            "type": 1,
            "segment": 10,
            "symbol": "NSE:SBIN-EQ",
            "productType": "INTRADAY",
            "side": 1,
            "status": 5,
            "message": "Insufficient funds",
        },
    ],
}


def test_order_status_maps_the_row_and_carries_the_message():
    client = make_client(Recorder({"/api/v3/orders": ORDER_BOOK}))
    order = client.order_status("23030900015105")
    assert order.status == OrderStatus.OPEN
    assert order.filled_quantity == 6 and order.average_price == money_parse("6.90")
    assert order.request.key == IDEA, "the EQ series must be stripped so the key matches the store's"
    assert (order.request.side, order.request.product, order.request.type) == (Side.SELL, Product.CNC, OrderType.LIMIT)
    assert order.request.tag == "breakout"
    assert order.placed_at == dt.datetime(2023, 3, 9, 9, 34, 38, tzinfo=IST)

    rejected = client.order_status("23030900015107")
    assert rejected.status == OrderStatus.REJECTED and rejected.message == "Insufficient funds"


def test_order_status_reports_an_unknown_order():
    with pytest.raises(RuntimeError):
        make_client(Recorder({"/api/v3/orders": {"s": "ok", "orderBook": []}})).order_status("nope")


def test_open_orders_drops_terminal_ones():
    orders = make_client(Recorder({"/api/v3/orders": ORDER_BOOK})).open_orders()
    assert [o.id for o in orders] == ["23030900015105"]


def test_positions_filter_zero_quantity_rows_and_map_derivatives():
    body = {
        "s": "ok",
        "netPositions": [
            {
                "netQty": 25,
                "netAvg": 22000.5,
                "buyAvg": 22000.5,
                "productType": "MARGIN",
                "unrealized_profit": 1250.25,
                "segment": 11,
                "symbol": "NSE:NIFTY24JANFUT",
            },
            {
                "netQty": 0,
                "buyAvg": 610,
                "sellAvg": 615,
                "productType": "INTRADAY",
                "segment": 10,
                "symbol": "NSE:X-EQ",
            },
            {
                "netQty": -10,
                "netAvg": 0,
                "sellAvg": 99.5,
                "productType": "INTRADAY",
                "unrealized_profit": -5,
                "segment": 10,
                "symbol": "NSE:IDEA-EQ",
            },
        ],
    }
    positions = make_client(Recorder({"/api/v3/positions": body})).positions()
    assert len(positions) == 2, "a closed row is history, not a holding"
    fut, short = positions
    assert fut.key == NIFTY_FUT and fut.product == Product.NRML
    assert fut.average_price == money_parse("22000.50") and fut.unrealized_pnl == money_parse("1250.25")
    assert short.quantity == -10
    assert short.average_price == money_parse("99.50"), "with no netAvg a short's average is its sell average"


def test_basket_margin_uses_the_basket_total():
    recorder = Recorder(
        {
            "/api/v3/multiorder/margin": {
                "s": "ok",
                "data": {"margin_avail": 1999.9, "margin_total": 147738.05, "margin_new_order": 247738.05},
            }
        }
    )
    got = make_client(recorder).basket_margin(
        [
            MarginLeg(key=NIFTY_FUT, side=Side.BUY, quantity=25),
            MarginLeg(
                key=InstrumentKey(exchange="NFO", symbol="NIFTY24JAN22000CE"),
                side=Side.SELL,
                quantity=25,
                price=money_parse("150.00"),
            ),
        ]
    )
    assert got == money_parse("147738.05"), "margin_total is the basket after hedge benefit"
    legs = recorder.sent("/api/v3/multiorder/margin")["data"]
    assert legs[0]["symbol"] == "NSE:NIFTY24JANFUT" and legs[0]["type"] == 2 and legs[0]["side"] == 1
    assert legs[1]["type"] == 1 and legs[1]["limitPrice"] == 150.0 and legs[1]["side"] == -1
    assert make_client(Recorder({})).basket_margin([]) == 0


# --- protective orders ------------------------------------------------

GTT_ACK = {"code": 1101, "message": "Successfully placed order", "s": "ok", "id": "25012400002074"}


def test_place_protective_oco_puts_the_upper_leg_first_for_a_long():
    recorder = Recorder({"/api/v3/gtt/orders/sync": GTT_ACK})
    gtt_id = make_client(recorder).place_protective(
        Protective(
            key=SBIN,
            side=Side.SELL,
            quantity=10,
            stop=money_parse("590.00"),
            target=money_parse("650.00"),
            product=Product.CNC,
        )
    )
    assert gtt_id == "25012400002074"
    sent = recorder.sent("/api/v3/gtt/orders/sync")
    assert (sent["side"], sent["symbol"], sent["productType"]) == (-1, "NSE:SBIN-EQ", "CNC")
    assert sent["orderInfo"]["leg1"] == {"price": 650.0, "triggerPrice": 650.0, "qty": 10}, "leg1 triggers above"
    assert sent["orderInfo"]["leg2"] == {"price": 590.0, "triggerPrice": 590.0, "qty": 10}, "leg2 triggers below"


def test_place_protective_on_a_short_mirrors_the_legs():
    recorder = Recorder({"/api/v3/gtt/orders/sync": GTT_ACK})
    make_client(recorder).place_protective(
        Protective(
            key=NIFTY_FUT,
            side=Side.BUY,
            quantity=25,
            stop=money_parse("22500.00"),
            target=money_parse("21500.00"),
            product=Product.NRML,
        )
    )
    sent = recorder.sent("/api/v3/gtt/orders/sync")
    assert sent["orderInfo"]["leg1"]["triggerPrice"] == 22500.0, "a short's stop is above the market"
    assert sent["orderInfo"]["leg2"]["triggerPrice"] == 21500.0
    assert sent["productType"] == "MARGIN"


def test_place_protective_single_leg_sends_no_leg2_and_refuses_intraday():
    recorder = Recorder({"/api/v3/gtt/orders/sync": GTT_ACK})
    make_client(recorder).place_protective(
        Protective(key=SBIN, side=Side.SELL, quantity=5, stop=money_parse("590.00"), product=Product.CNC)
    )
    assert "leg2" not in recorder.sent("/api/v3/gtt/orders/sync")["orderInfo"]
    with pytest.raises(ValueError):
        make_client(Recorder({})).place_protective(
            Protective(key=SBIN, side=Side.SELL, quantity=5, stop=money_parse("590.00"), product=Product.MIS)
        )


GTT_BOOK = {
    "s": "ok",
    "orderBook": [
        {
            "id": "25012400002074",
            "symbol": "NSE:SBIN-EQ",
            "segment": 10,
            "product_type": "CNC",
            "tran_side": -1,
            "qty": 10,
            "qty2": 10,
            "price_trigger": 650,
            "price2_trigger": 590,
            "gtt_oco_ind": 2,
            "ord_status": 6,
            "ltp": 612.5,
        },
        {
            "id": "25012400002099",
            "symbol": "NSE:SBIN-EQ",
            "segment": 10,
            "product_type": "CNC",
            "tran_side": -1,
            "qty": 3,
            "price_trigger": 600,
            "gtt_oco_ind": 1,
            "ord_status": 1,
        },
        {
            "id": "25012400002100",
            "symbol": "NSE:NIFTY24JANFUT",
            "segment": 11,
            "product_type": "MARGIN",
            "tran_side": 1,
            "qty": 25,
            "price_trigger": 22500,
            "gtt_oco_ind": 1,
            "ord_status": 6,
            "ltp": 22000,
        },
        {
            "id": "25012400002101",
            "symbol": "NSE:SBIN-EQ",
            "segment": 10,
            "product_type": "CNC",
            "tran_side": -1,
            "qty": 4,
            "price_trigger": 700,
            "gtt_oco_ind": 1,
            "ord_status": 6,
            "ltp": 612.5,
        },
    ],
}


def test_list_protective_drops_dead_triggers_and_reads_legs_by_side():
    got = make_client(Recorder({"/api/v3/gtt/orders": GTT_BOOK})).list_protective()
    assert [p.id for p in got] == ["25012400002074", "25012400002100", "25012400002101"]
    oco, short_stop, target = got
    assert (oco.stop, oco.target) == (money_parse("590.00"), money_parse("650.00")), "a sell's stop is the lower level"
    assert oco.key == SBIN and oco.side == Side.SELL and oco.quantity == 10
    assert short_stop.stop == money_parse("22500.00") and short_stop.target == 0
    assert short_stop.key == NIFTY_FUT and short_stop.product == Product.NRML
    assert target.target == money_parse("700.00") and target.stop == 0, "a sell leg above the market is a target"


def test_a_single_leg_with_no_ltp_reads_as_a_stop():
    row = {"id": "x", "symbol": "NSE:SBIN-EQ", "tran_side": -1, "qty": 1, "price_trigger": 700, "gtt_oco_ind": 1}
    p = _from_gtt(row)
    assert p.stop == money_parse("700.00") and p.target == 0


def test_an_unknown_gtt_status_reads_as_live():
    assert _gtt_is_live({"ord_status": 42})
    assert not _gtt_is_live({"ord_status": 2})


def test_gtt_legs_refuse_an_empty_protective():
    with pytest.raises(ValueError):
        _gtt_legs(Protective(key=SBIN, side=Side.SELL, quantity=1, stop=Money(0)))


def test_modify_stop_preserves_the_target_leg():
    patched: dict = {}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/v3/gtt/orders" and request.method == "GET":
            return httpx.Response(200, json=GTT_BOOK)
        if request.url.path == "/api/v3/gtt/orders/sync" and request.method == "PATCH":
            patched.update(json.loads(request.content))
            return httpx.Response(200, json={"code": 1102, "s": "ok", "id": "25012400002074"})
        return httpx.Response(404, json={"s": "error", "code": -50})

    make_client(handler).modify_stop("25012400002074", money_parse("600.00"))
    assert patched["id"] == "25012400002074"
    assert patched["orderInfo"]["leg1"]["triggerPrice"] == 650.0, "the target must be carried across"
    assert patched["orderInfo"]["leg2"]["triggerPrice"] == 600.0


def test_modify_stop_reports_a_trigger_that_is_gone():
    with pytest.raises(RuntimeError):
        make_client(Recorder({"/api/v3/gtt/orders": {"s": "ok", "orderBook": []}})).modify_stop("missing", Money(1))


def test_cancel_protective_sends_the_id_in_the_body():
    recorder = Recorder({"/api/v3/gtt/orders/sync": {"code": 1103, "s": "ok", "id": "25012400002099"}})
    make_client(recorder).cancel_protective("25012400002099")
    assert recorder.requests[0].method == "DELETE"
    assert recorder.sent("/api/v3/gtt/orders/sync") == {"id": "25012400002099"}


# --- market data ------------------------------------------------------

QUOTES = {
    "s": "ok",
    "code": 200,
    "d": [
        {
            "n": "NSE:SBIN-EQ",
            "s": "ok",
            "v": {
                "lp": 426.9,
                "ask": 426.9,
                "bid": 426.85,
                "open_price": 430.5,
                "high_price": 433.65,
                "low_price": 423.6,
                "prev_close_price": 425.2,
                "tt": "1622160000",
            },
        },
        {"n": "NSE:IDEA-EQ", "s": "error", "code": -300, "message": "Invalid symbol"},
        {"n": "NSE:NOTASKED-EQ", "s": "ok", "v": {"lp": 1}},
    ],
}


def test_ltp_omits_instruments_fyers_does_not_know():
    got = make_client(Recorder({"/data/quotes": QUOTES})).ltp([SBIN, IDEA])
    assert got == {SBIN: money_parse("426.90")}, "a failed symbol is omitted, an unasked one dropped"


def test_quote_reads_the_previous_close_and_the_timestamp():
    q = make_client(Recorder({"/data/quotes": QUOTES})).quote([SBIN])[SBIN]
    assert q.close == money_parse("425.20"), "close is the previous session's close, not the last price"
    assert (q.open, q.high, q.low) == (money_parse("430.50"), money_parse("433.65"), money_parse("423.60"))
    assert (q.bid, q.ask) == (money_parse("426.85"), money_parse("426.90"))
    assert q.at == dt.datetime.fromtimestamp(1622160000, tz=dt.UTC)


def test_quote_chunks_at_the_documented_limit():
    recorder = Recorder({"/data/quotes": {"s": "ok", "d": []}})
    keys = [InstrumentKey(exchange="NSE", symbol=f"S{i}") for i in range(120)]
    make_client(recorder).quote(keys)
    assert len(recorder.requests) == 3
    assert all(len(r.url.params["symbols"].split(",")) <= 50 for r in recorder.requests)


def _history_recorder() -> tuple[Recorder, list[tuple[int, int]]]:
    """A recorder answering one bar per request at the range's start."""
    ranges: list[tuple[int, int]] = []

    class History(Recorder):
        def __call__(self, request: httpx.Request) -> httpx.Response:
            self.requests.append(request)
            assert request.url.params["date_format"] == "0", "the range is sent as epoch seconds"
            start = int(request.url.params["range_from"])
            end = int(request.url.params["range_to"])
            ranges.append((start, end))
            return httpx.Response(200, json={"s": "ok", "candles": [[start, 417.0, 419.2, 405.3, 412.05, 142964052]]})

    return History({}), ranges


def test_candles_make_one_request_for_a_short_range():
    recorder, ranges = _history_recorder()
    start = dt.datetime(2024, 1, 1, tzinfo=IST)
    got = make_client(recorder).candles(SBIN, Timeframe.M5, start, dt.datetime(2024, 1, 31, tzinfo=IST))
    assert len(ranges) == 1
    assert got[0].start == start and got[0].start.tzinfo == IST
    assert got[0].open == money_parse("417.00") and got[0].volume == 142964052


def test_candles_chunk_contiguously_with_no_gap_or_overlap():
    recorder, ranges = _history_recorder()
    start = dt.datetime(2023, 1, 1, tzinfo=IST)
    end = dt.datetime(2023, 12, 31, tzinfo=IST)
    make_client(recorder).candles(SBIN, Timeframe.M1, start, end)
    assert len(ranges) >= 4, "a year of minute bars is chunked under the 100-day ceiling"
    assert ranges[0][0] == int(start.timestamp())
    for (_, prev_end), (next_start, _) in pairwise(ranges):
        assert next_start == prev_end + 1, "a gap loses bars and an overlap duplicates them"
    assert all(e - s <= 100 * 86400 for s, e in ranges)
    assert ranges[-1][1] == int(end.timestamp())


def test_candles_reject_an_inverted_range():
    recorder, ranges = _history_recorder()
    now = dt.datetime.now(dt.UTC)
    with pytest.raises(ValueError):
        make_client(recorder).candles(SBIN, Timeframe.D1, now, now - dt.timedelta(hours=1))
    assert ranges == []


def test_candles_skip_a_malformed_row_without_failing_the_batch():
    body = {
        "s": "ok",
        "candles": [
            [1622160000, 417.0, 419.2, 405.3, 412.05, 100],
            ["bad", 1, 2, 3, 4, 5],
            [1622160300, 1, 2],
            [1622160600, 418.0, 420.0, 417.0, 419.0, 200],
        ],
    }
    got = make_client(Recorder({"/data/history": body})).candles(
        SBIN,
        Timeframe.M5,
        dt.datetime.fromtimestamp(1622160000, tz=dt.UTC),
        dt.datetime.fromtimestamp(1622160600, tz=dt.UTC),
    )
    assert len(got) == 2


SYMBOL_MASTER = {
    "NSE:SBIN-EQ": {
        "isin": "INE062A01020",
        "symDetails": "STATE BANK OF INDIA",
        "symTicker": "NSE:SBIN-EQ",
        "segment": 10,
        "exSeries": "EQ",
        "optType": "XX",
        "exInstType": 0,
        "minLotSize": 1,
        "tickSize": 0.05,
        "expiryDate": "",
        "strikePrice": -1,
        "tradeStatus": 1,
    },
    "NSE:NIFTY24JAN22000CE": {
        "symTicker": "NSE:NIFTY24JAN22000CE",
        "segment": 11,
        "optType": "CE",
        "exInstType": 14,
        "minLotSize": 50,
        "tickSize": "0.05",
        "expiryDate": "1706178600",
        "strikePrice": 22000,
        "tradeStatus": 1,
        "qtyFreeze": "",
    },
    "NSE:SUSPENDED-EQ": {
        "symTicker": "NSE:SUSPENDED-EQ",
        "segment": 10,
        "exInstType": 0,
        "minLotSize": 0,
        "tradeStatus": 0,
    },
    "NSE:NIFTY50-INDEX": {"symTicker": "NSE:NIFTY50-INDEX", "segment": 10, "exInstType": 10, "minLotSize": 1},
}


def test_parse_symbol_master_tolerates_mixed_types_and_empty_strings():
    by_key = {i.key: i for i in parse_symbol_master(SYMBOL_MASTER)}
    eq = by_key[SBIN]
    assert (eq.isin, eq.segment, eq.lot_size, eq.tick_size, eq.active) == ("INE062A01020", "equity", 1, 5, True)
    assert eq.expiry is None, "an empty expiryDate is no expiry, not the epoch"
    opt = by_key[InstrumentKey(exchange="NFO", symbol="NIFTY24JAN22000CE")]
    assert (opt.segment, opt.option_type, opt.lot_size, opt.strike) == ("options", "ce", 50, money_parse("22000.00"))
    assert opt.expiry == dt.date(2024, 1, 25)
    assert opt.tick_size == 5, "a quoted tickSize must still be read"
    sus = by_key[InstrumentKey(exchange="NSE", symbol="SUSPENDED")]
    assert not sus.active and sus.lot_size == 1
    assert by_key[InstrumentKey(exchange="NSE", symbol="NIFTY50-INDEX")].segment == "index"


def test_instruments_fetches_the_file_for_the_exchange():
    recorder = Recorder({"/NSE_FO_sym_master.json": SYMBOL_MASTER})
    client = make_client(recorder)
    assert client.instruments("NFO")
    assert recorder.requests[0].url.path == "/NSE_FO_sym_master.json"
    assert "Authorization" not in recorder.requests[0].headers, "the symbol master is public"
    with pytest.raises(ValueError):
        client.instruments("NASDAQ")


# --- token state ------------------------------------------------------


def test_token_of_unknown_age_is_stale():
    assert not FyersClient(app_id="a", access_token="t", transport=make_transport(Recorder({}))).token_fresh()


def test_yesterdays_token_is_stale_and_todays_is_fresh():
    now = dt.datetime.now(IST)
    fresh = FyersClient(app_id="a", access_token="t", token_issued_at=now, transport=make_transport(Recorder({})))
    stale = FyersClient(
        app_id="a", access_token="t", token_issued_at=now - dt.timedelta(days=1), transport=make_transport(Recorder({}))
    )
    assert fresh.token_fresh() and not stale.token_fresh()


def test_login_url_and_hash():
    recorder = Recorder({"/api/v3/validate-authcode": {"s": "ok", "code": 200, "access_token": "fresh"}})
    client = FyersClient(
        app_id="APP-100", app_secret="secret", redirect_uri="https://cb", transport=make_transport(recorder)
    )
    assert "client_id=APP-100" in client.login_url("xyz") and "state=xyz" in client.login_url("xyz")
    assert client.login("code") == "fresh"
    sent = recorder.sent("/api/v3/validate-authcode")
    assert sent["grant_type"] == "authorization_code" and sent["code"] == "code"
    assert len(sent["appIdHash"]) == 64 and "secret" not in sent["appIdHash"], "the secret itself never leaves"
    assert client.token_fresh()


# --- parity with Go ---------------------------------------------------

FIXTURE = Path(__file__).resolve().parents[2] / "contracts" / "testdata" / "parity.json"


@pytest.fixture(scope="module")
def parity() -> dict:
    """The FYERS block of the shared fixture, which ``go/fyers/parity_test.go`` also runs."""
    block = json.loads(FIXTURE.read_text())["fyers"]
    assert block["symbol"], "the fyers block is missing or misnamed"
    return block


def test_parity_symbol(parity: dict) -> None:
    for case in parity["symbol"]:
        assert symbol_for(InstrumentKey(exchange=case["exchange"], symbol=case["symbol"])) == case["want"], case


def test_parity_key(parity: dict) -> None:
    for case in parity["key"]:
        got = key_for(case["symbol"], case["segment"])
        assert (got.exchange, got.symbol) == (case["exchange"], case["want"]), case


def test_parity_status(parity: dict) -> None:
    for case in parity["status"]:
        assert normalize_status(case["wire"]) == OrderStatus(case["want"]), case


def test_parity_product(parity: dict) -> None:
    for case in parity["product"]:
        assert to_product(Product(case["domain"])) == case["wire"], case
    for unsupported in parity["product_unsupported"]:
        with pytest.raises(ValueError):
            to_product(Product(unsupported))
    for case in parity["from_product"]:
        assert from_product(case["wire"]) == Product(case["want"]), case


def test_parity_order_type_side_and_validity(parity: dict) -> None:
    for case in parity["order_type"]:
        assert to_order_type(OrderType(case["domain"])) == case["wire"], case
        assert from_order_type(case["wire"]) == OrderType(case["domain"]), case
    for case in parity["side"]:
        assert to_side(Side(case["domain"])) == case["wire"], case
        assert from_side(case["wire"]) == Side(case["domain"]), case
    for case in parity["validity"]:
        assert to_validity(case["domain"]) == case["wire"], case
    for unsupported in parity["validity_unsupported"]:
        with pytest.raises(ValueError):
            to_validity(unsupported)


def test_parity_resolution(parity: dict) -> None:
    for case in parity["resolution"]:
        assert to_resolution(Timeframe(case["timeframe"])) == case["resolution"], case
        assert chunk_days(Timeframe(case["timeframe"])) == case["chunk_days"], case
    for unsupported in parity["resolution_unsupported"]:
        with pytest.raises(ValueError):
            to_resolution(unsupported)


def test_parity_paise(parity: dict) -> None:
    for case in parity["paise"]:
        assert paise(case["rupees"]) == case["want"], case


def test_parity_gtt_legs(parity: dict) -> None:
    for case in parity["gtt_legs"]:
        p = Protective(
            key=SBIN, side=Side(case["side"]), quantity=1, stop=Money(case["stop"]), target=Money(case["target"])
        )
        legs = _gtt_legs(p)
        assert paise(legs["leg1"]["triggerPrice"]) == case["leg1"], case
        leg2 = paise(legs["leg2"]["triggerPrice"]) if "leg2" in legs else 0
        assert leg2 == case["leg2"], case


def test_parity_order_time(parity: dict) -> None:
    for case in parity["order_time"]:
        got = parse_order_time(case["wire"])
        if not case["want"]:
            assert got is None, case
        else:
            assert got == dt.datetime.fromisoformat(case["want"]), case
