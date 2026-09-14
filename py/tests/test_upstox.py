"""The Upstox adapter. No live calls: every test drives an httpx.MockTransport."""

from __future__ import annotations

import datetime as dt
import gzip
import json
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
from tradekit.core.money import parse as money_parse
from tradekit.upstox import UpstoxClient, parse_instrument_csv, parse_instrument_keys
from tradekit.upstox.client import _gtt_is_live, _stop_direction, _target_direction
from tradekit.upstox.mapping import (
    RULE_STOPLOSS,
    RULE_TARGET,
    TRIGGER_ABOVE,
    TRIGGER_BELOW,
    chunk_days,
    from_order_type,
    from_product,
    instrument_key,
    normalize_status,
    paise,
    parse_instrument_key,
    rupees,
    to_interval,
    to_order_type,
    to_product,
    to_validity,
)
from tradekit.upstox.transport import (
    RejectedError,
    TokenExpiredError,
    TransientError,
    Transport,
)

RELIANCE = InstrumentKey(exchange="NSE", symbol="RELIANCE")


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
            return httpx.Response(404, json={"errors": [{"errorCode": "X", "message": "no route"}]})
        return httpx.Response(self.status, json=body)

    def sent(self, path: str) -> dict:
        """The JSON body sent to ``path``."""
        for request in self.requests:
            if request.url.path == path and request.content:
                return json.loads(request.content)
        raise AssertionError(f"nothing was sent to {path}")


def make_client(handler, **kwargs) -> UpstoxClient:
    """A client whose transport is the given handler, with no real sleeping."""
    transport = Transport(
        token_provider=lambda: "token",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,  # throttling is not what these tests are about
        retry_base=1.0,
        sleep=lambda _: None,
        **kwargs,
    )
    return UpstoxClient(
        api_key="key",
        access_token="token",
        instrument_key=lambda k: f"NSE_EQ|{k.symbol}",
        transport=transport,
    )


# --- mapping, pinned against the Go side by parity.json ---------------


def test_instrument_key_round_trips_an_expired_contract():
    segment, instrument_id = parse_instrument_key("NSE_FO|47983|17-04-2025")
    assert segment == "NSE_FO"
    assert instrument_id == "47983|17-04-2025", (
        "truncating the expiry makes the key unusable against the expired-candle endpoint"
    )


@pytest.mark.parametrize("bad", ["", "NSE_EQ", "|INE002A01018", "NSE_EQ|"])
def test_parse_instrument_key_rejects_malformed(bad):
    with pytest.raises(ValueError, match="malformed instrument key"):
        parse_instrument_key(bad)


@pytest.mark.parametrize("amount", [0, 5, 105760, -105760, 12345678])
def test_price_survives_the_wire_round_trip(amount):
    assert paise(rupees(amount)) == amount


def test_paise_rounds_half_away_from_zero():
    assert paise(1057.605) == 105761, "must match core.money, or Go and Python disagree on a fill price"


def test_margin_product_is_refused():
    with pytest.raises(ValueError, match="no Upstox product"):
        to_product(Product.MARGIN)


def test_gtt_is_not_an_order_validity():
    with pytest.raises(ValueError, match="separate trigger API"):
        to_validity(TimeInForce.GTT)


def test_unknown_status_is_pending_not_terminal():
    status = normalize_status("after market order req received")
    assert status == OrderStatus.PENDING
    assert not status.terminal, "an unrecognised status must not make a reconciler abandon a live order"


def test_to_interval_splits_unit_and_multiple():
    assert to_interval(Timeframe.M5) == ("minutes", 5)
    assert to_interval(Timeframe.M60) == ("hours", 1)
    with pytest.raises(ValueError, match="no Upstox interval"):
        to_interval("2m")  # type: ignore[arg-type] - a timeframe Upstox does not serve


def test_chunk_days_stays_under_the_minute_ceiling():
    assert chunk_days("minutes", 1) < 30, "minute data is refused beyond about a month"


# --- transport --------------------------------------------------------


def test_retry_after_is_honoured_then_the_call_succeeds():
    calls = {"n": 0}
    slept: list[float] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            return httpx.Response(
                429,
                headers={"Retry-After": "7"},
                json={"errors": [{"errorCode": "UDAPI10005", "message": "too many"}]},
            )
        return httpx.Response(200, json={"status": "success", "data": {"ok": True}})

    transport = Transport(
        token_provider=lambda: "t",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,
        sleep=slept.append,
    )
    assert transport.request("GET", "/x", retry=True)["data"]["ok"] is True
    assert calls["n"] == 2
    assert slept == [7.0], "Retry-After says when the server's window resets and must win over the schedule"


def test_server_error_retries_and_gives_up_at_the_bound():
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(502)

    transport = Transport(
        token_provider=lambda: "t",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,
        attempts=4,
        sleep=lambda _: None,
    )
    with pytest.raises(TransientError):
        transport.request("GET", "/x", retry=True)
    assert calls["n"] == 4


def test_client_error_is_not_retried():
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(400, json={"errors": [{"errorCode": "UDAPI1148", "message": "range too long"}]})

    transport = Transport(
        token_provider=lambda: "t",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,
        sleep=lambda _: None,
    )
    with pytest.raises(RejectedError) as excinfo:
        transport.request("GET", "/x", retry=True)
    assert calls["n"] == 1, "retrying a rejection only burns the quota the next caller needs"
    assert excinfo.value.code == "UDAPI1148", "the broker's own code must reach the caller"


def test_invalid_token_is_not_read_as_a_rate_limit():
    """UDAPI100050 contains UDAPI10005; a prefix match would make it retryable."""

    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(401, json={"errors": [{"errorCode": "UDAPI100050", "message": "Invalid token"}]})

    transport = Transport(
        token_provider=lambda: "t",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,
        sleep=lambda _: None,
    )
    with pytest.raises(TokenExpiredError) as excinfo:
        transport.request("GET", "/x", retry=True)
    assert not excinfo.value.retryable, "the same token cannot succeed; the remedy is a login"


def test_rate_limit_code_under_a_2xx_is_still_retried():
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] == 1:
            # Upstox sometimes answers a breached rate limit with a 2xx.
            return httpx.Response(200, json={"errors": [{"errorCode": "UDAPI10005", "message": "rate limit"}]})
        return httpx.Response(200, json={"status": "success", "data": {}})

    transport = Transport(
        token_provider=lambda: "t",
        base_url="https://api.test/v2",
        base_url_v3="https://api.test/v3",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
        rate_per_sec=0,
        sleep=lambda _: None,
    )
    transport.request("GET", "/x", retry=True)
    assert calls["n"] == 2, "a rate limit read as success leaves the caller hammering an empty result"


def test_place_order_is_never_retried():
    calls = {"n": 0}

    def handler(_: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(503)

    client = make_client(handler)
    with pytest.raises(TransientError):
        client.place_order(
            OrderRequest(key=RELIANCE, side=Side.BUY, quantity=10, type=OrderType.MARKET, product=Product.CNC)
        )
    assert calls["n"] == 1, "retrying a POST that placed an order places a second one"


# --- broker -----------------------------------------------------------


def test_account_reads_the_equity_segment():
    handler = Recorder(
        {
            "/v2/user/get-funds-and-margin": {
                "status": "success",
                "data": {
                    "equity": {"available_margin": 125000.50, "used_margin": 25000.00},
                    "commodity": {"available_margin": 9999.00, "used_margin": 0.0},
                },
            }
        }
    )
    account = make_client(handler).account()
    assert account.available == money_parse("125000.50")
    assert account.used == money_parse("25000.00")
    assert account.equity == account.available + account.used
    assert account.available != money_parse("9999.00"), "commodity margin is not equity capital"


def test_account_refuses_an_empty_funds_response():
    handler = Recorder({"/v2/user/get-funds-and-margin": {"status": "success", "data": {}}})
    with pytest.raises(RuntimeError, match="no segment data"):
        make_client(handler).account()


def test_place_order_returns_pending_with_the_brokers_id():
    handler = Recorder({"/v3/order/place": {"status": "success", "data": {"order_ids": ["250905000123456"]}}})
    client = make_client(handler)
    order = client.place_order(
        OrderRequest(
            key=RELIANCE,
            side=Side.BUY,
            quantity=7,
            type=OrderType.LIMIT,
            product=Product.CNC,
            limit_price=money_parse("1057.60"),
            tag="breakout",
        )
    )
    assert order.id == "250905000123456", "the v3 endpoint answers with a list"
    assert order.status == OrderStatus.PENDING, "an acknowledgement is not a fill"

    body = handler.sent("/v3/order/place")
    assert body["instrument_token"] == "NSE_EQ|RELIANCE", "the key goes in instrument_token despite the name"
    assert body["price"] == 1057.60
    assert body["product"] == "D"
    assert body["tag"] == "breakout"


def test_place_order_refuses_a_limit_with_no_price():
    client = make_client(Recorder({}))
    with pytest.raises(ValueError, match="needs a limit price"):
        client.place_order(
            OrderRequest(key=RELIANCE, side=Side.BUY, quantity=1, type=OrderType.LIMIT, product=Product.CNC)
        )


def test_order_status_maps_the_status_and_carries_the_message():
    for wire, want in {
        "complete": OrderStatus.COMPLETE,
        "rejected": OrderStatus.REJECTED,
        "cancelled": OrderStatus.CANCELLED,
        "open": OrderStatus.OPEN,
        "trigger pending": OrderStatus.TRIGGERED,
        "something new": OrderStatus.PENDING,
    }.items():
        handler = Recorder(
            {
                "/v2/order/details": {
                    "status": "success",
                    "data": {
                        "order_id": "1",
                        "exchange": "NSE",
                        "trading_symbol": "RELIANCE",
                        "transaction_type": "BUY",
                        "quantity": 10,
                        "order_type": "LIMIT",
                        "product": "D",
                        "validity": "DAY",
                        "price": 1057.60,
                        "status": wire,
                        "filled_quantity": 10,
                        "average_price": 1057.55,
                        "status_message": "RMS rule check failed",
                    },
                }
            }
        )
        order = make_client(handler).order_status("1")
        assert order.status == want, f"Upstox status {wire!r} must map to {want}"
        assert order.message == "RMS rule check failed"
        assert order.average_price == money_parse("1057.55")


def test_order_status_reports_an_unknown_order():
    handler = Recorder({"/v2/order/details": {"status": "success", "data": {}}})
    with pytest.raises(RuntimeError, match="not found"):
        make_client(handler).order_status("missing")


def test_an_unreadable_field_does_not_lose_the_order():
    """Go holds an unknown value in a string type; a StrEnum cannot.

    Falling back rather than raising is what keeps one row with a field this
    adapter has not seen from discarding the whole order book -- and with it the
    statuses a reconciler acts on.
    """
    assert from_order_type("") == OrderType.MARKET
    assert from_order_type("SOMETHING NEW") == OrderType.MARKET
    assert from_product("") == Product.NRML
    assert from_product("cnc") == Product.CNC, "a value the domain does know must still round-trip"


def test_open_orders_drops_terminal_ones():
    handler = Recorder(
        {
            "/v2/order/retrieve-all": {
                "status": "success",
                "data": [
                    {"order_id": "1", "status": "open", "exchange": "NSE", "trading_symbol": "RELIANCE"},
                    {"order_id": "2", "status": "complete", "exchange": "NSE", "trading_symbol": "TCS"},
                    {"order_id": "3", "status": "cancelled", "exchange": "NSE", "trading_symbol": "INFY"},
                    {"order_id": "4", "status": "trigger pending", "exchange": "NSE", "trading_symbol": "WIPRO"},
                ],
            }
        }
    )
    orders = make_client(handler).open_orders()
    assert [o.id for o in orders] == ["1", "4"]
    assert not any(o.status.terminal for o in orders)


def test_positions_filters_zero_quantity_rows():
    handler = Recorder(
        {
            "/v2/portfolio/short-term-positions": {
                "status": "success",
                "data": [
                    {
                        "exchange": "NSE",
                        "trading_symbol": "RELIANCE",
                        "product": "I",
                        "quantity": 10,
                        "average_price": 1057.60,
                        "unrealised": 125.50,
                    },
                    {
                        "exchange": "NSE",
                        "tradingsymbol": "TCS",
                        "product": "D",
                        "quantity": 0,
                        "average_price": 3500.00,
                        "realised": 250.00,
                    },
                    {
                        "exchange": "NSE",
                        "trading_symbol": "INFY",
                        "product": "D",
                        "quantity": -5,
                        "average_price": 1400.00,
                    },
                ],
            }
        }
    )
    positions = make_client(handler).positions()
    assert [p.key.symbol for p in positions] == ["RELIANCE", "INFY"], (
        "a closed position's row is history, not a holding"
    )
    assert positions[0].product == Product.MIS
    assert positions[0].unrealized_pnl == money_parse("125.50")
    assert positions[1].quantity == -5, "a short position is a negative quantity"


def test_basket_margin_uses_the_margin_after_benefit():
    handler = Recorder(
        {"/v2/charges/margin": {"status": "success", "data": {"required_margin": 250000.0, "final_margin": 85000.0}}}
    )
    got = make_client(handler).basket_margin(
        [
            MarginLeg(key=RELIANCE, side=Side.BUY, quantity=75, product=Product.NRML),
            MarginLeg(key=RELIANCE, side=Side.SELL, quantity=75, product=Product.NRML),
        ]
    )
    assert got == money_parse("85000.00"), (
        "required_margin is the gross figure and would refuse spreads the account can afford"
    )


# --- GTT --------------------------------------------------------------


def test_place_protective_oco_sends_both_legs_with_the_right_directions():
    handler = Recorder({"/v3/order/gtt/place": {"status": "success", "data": {"gtt_order_id": "GTT-1"}}})
    client = make_client(handler)
    gtt_id = client.place_protective(
        Protective(
            id="",
            key=RELIANCE,
            side=Side.SELL,  # protecting a long
            quantity=10,
            stop=money_parse("1000.00"),
            target=money_parse("1150.00"),
            product=Product.CNC,
        )
    )
    assert gtt_id == "GTT-1"

    rules = {r["strategy"]: r for r in handler.sent("/v3/order/gtt/place")["rules"]}
    assert rules[RULE_STOPLOSS]["trigger_type"] == TRIGGER_BELOW, "a sell that protects a long stops below"
    assert rules[RULE_TARGET]["trigger_type"] == TRIGGER_ABOVE
    assert rules[RULE_STOPLOSS]["trigger_price"] == 1000.0


def test_place_protective_on_a_short_mirrors_the_directions():
    handler = Recorder({"/v3/order/gtt/place": {"status": "success", "data": {"gtt_order_id": "GTT-2"}}})
    make_client(handler).place_protective(
        Protective(
            id="",
            key=RELIANCE,
            side=Side.BUY,  # protecting a short
            quantity=10,
            stop=money_parse("1150.00"),
            target=money_parse("1000.00"),
            product=Product.MIS,
        )
    )
    rules = {r["strategy"]: r for r in handler.sent("/v3/order/gtt/place")["rules"]}
    assert rules[RULE_STOPLOSS]["trigger_type"] == TRIGGER_ABOVE, (
        "assigning by role rather than side turns a short's stop into a target"
    )
    assert rules[RULE_TARGET]["trigger_type"] == TRIGGER_BELOW


def test_modify_stop_preserves_the_target_leg():
    handler = Recorder(
        {
            "/v3/order/gtt": {
                "status": "success",
                "data": [
                    {
                        "gtt_order_id": "GTT-1",
                        "exchange": "NSE",
                        "trading_symbol": "RELIANCE",
                        "transaction_type": "SELL",
                        "product": "D",
                        "quantity": 10,
                        "rules": [
                            {
                                "strategy": "STOPLOSS",
                                "trigger_type": "BELOW",
                                "trigger_price": 1000.0,
                                "status": "ACTIVE",
                            },
                            {
                                "strategy": "TARGET",
                                "trigger_type": "ABOVE",
                                "trigger_price": 1150.0,
                                "status": "ACTIVE",
                            },
                        ],
                    }
                ],
            },
            "/v3/order/gtt/modify": {"status": "success", "data": {"gtt_order_id": "GTT-1"}},
        }
    )
    make_client(handler).modify_stop("GTT-1", money_parse("1050.00"))

    body = handler.sent("/v3/order/gtt/modify")
    rules = {r["strategy"]: r["trigger_price"] for r in body["rules"]}
    assert body["gtt_order_id"] == "GTT-1"
    assert rules[RULE_STOPLOSS] == 1050.0
    assert rules[RULE_TARGET] == 1150.0, (
        "Upstox replaces the whole rule set; sending only the stop drops the target silently"
    )
    assert body["quantity"] == 10


def test_modify_stop_reports_a_trigger_that_is_gone():
    handler = Recorder({"/v3/order/gtt": {"status": "success", "data": []}})
    with pytest.raises(RuntimeError, match="not in the trigger book"):
        make_client(handler).modify_stop("GTT-gone", money_parse("1050.00"))


def test_cancel_protective_sends_the_id_in_the_body():
    handler = Recorder({"/v3/order/gtt/cancel": {"status": "success"}})
    make_client(handler).cancel_protective("GTT-1")
    assert handler.sent("/v3/order/gtt/cancel") == {"gtt_order_id": "GTT-1"}, (
        "the query form is answered with UDAPI100038 and no indication of what was wrong"
    )


def test_list_protective_drops_dead_triggers():
    handler = Recorder(
        {
            "/v3/order/gtt": {
                "status": "success",
                "data": [
                    {
                        "gtt_order_id": "live",
                        "exchange": "NSE",
                        "trading_symbol": "RELIANCE",
                        "transaction_type": "SELL",
                        "product": "D",
                        "quantity": 10,
                        "rules": [
                            {"strategy": "ENTRY", "trigger_type": "BELOW", "trigger_price": 1000.0, "status": "ACTIVE"}
                        ],
                    },
                    {
                        "gtt_order_id": "cancelled",
                        "exchange": "NSE",
                        "trading_symbol": "TCS",
                        "transaction_type": "SELL",
                        "product": "D",
                        "quantity": 5,
                        "rules": [
                            {
                                "strategy": "ENTRY",
                                "trigger_type": "BELOW",
                                "trigger_price": 3400.0,
                                "status": "CANCELLED",
                            }
                        ],
                    },
                ],
            }
        }
    )
    live = make_client(handler).list_protective()
    assert [p.id for p in live] == ["live"], (
        "the book returns cancelled rows alongside live ones; returning them makes a dead stop look like protection"
    )
    assert live[0].stop == money_parse("1000.00")


def test_a_gtt_rule_with_an_unknown_status_reads_as_live():
    assert _gtt_is_live({"rules": [{"status": "SOMETHING NEW"}]}), (
        "a false missing-stop alert on every position every morning would bury the real ones"
    )
    assert _gtt_is_live({}), "a row with no rules to judge on must be assumed live"


# --- market data ------------------------------------------------------


def test_ltp_omits_instruments_upstox_does_not_know():
    handler = Recorder(
        {
            "/v2/market-quote/ltp": {
                "status": "success",
                "data": {
                    "NSE_EQ:RELIANCE": {"last_price": 1057.60, "instrument_token": "NSE_EQ|RELIANCE"},
                    "NSE_EQ:TCS": {"last_price": 3500.05, "instrument_token": "NSE_EQ|TCS"},
                },
            }
        }
    )
    keys = [RELIANCE, InstrumentKey("NSE", "TCS"), InstrumentKey("NSE", "DELISTED")]
    prices = make_client(handler).ltp(keys)
    assert len(prices) == 2, "one delisted symbol must not fail a 500-symbol scan"
    assert prices[RELIANCE] == money_parse("1057.60")
    assert keys[2] not in prices, "an unanswered instrument must be absent, not zero-priced"


def test_quote_reads_the_previous_close_and_the_depth_top():
    handler = Recorder(
        {
            "/v2/market-quote/quotes": {
                "status": "success",
                "data": {
                    "NSE_EQ:RELIANCE": {
                        "instrument_token": "NSE_EQ|RELIANCE",
                        "last_price": 1057.60,
                        "ohlc": {"open": 1040.0, "high": 1060.0, "low": 1035.0, "close": 1038.0},
                        "depth": {"buy": [{"price": 1057.55}], "sell": [{"price": 1057.65}]},
                    }
                },
            }
        }
    )
    quote = make_client(handler).quote([RELIANCE])[RELIANCE]
    assert quote.close == money_parse("1038.00"), (
        "ohlc.close is the previous session's close; conflating it with the last price makes every gap zero"
    )
    assert quote.last == money_parse("1057.60")
    assert quote.bid == money_parse("1057.55")
    assert quote.ask == money_parse("1057.65")


def test_quote_drops_an_entry_that_names_no_instrument():
    handler = Recorder(
        {"/v2/market-quote/quotes": {"status": "success", "data": {"NSE_EQ:MYSTERY": {"last_price": 99.0}}}}
    )
    assert make_client(handler).quote([RELIANCE]) == {}, (
        "a quote that cannot be attributed would size a spike against another company's history"
    )


CANDLE_BODY = {
    "status": "success",
    "data": {
        "candles": [
            ["2025-04-17T15:25:00+05:30", 100.5, 101.0, 100.0, 100.75, 1500, 0],
            ["2025-04-17T15:20:00+05:30", 100.0, 100.6, 99.9, 100.5, 1200, 0],
        ]
    },
}


def candle_handler():
    """Answers any historical request with two bars, and records the paths."""
    paths: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        paths.append(request.url.path)
        return httpx.Response(200, json=CANDLE_BODY)

    return handler, paths


def test_candles_return_oldest_first():
    handler, _ = candle_handler()
    start = dt.datetime(2025, 4, 17, tzinfo=dt.UTC)
    candles = make_client(handler).candles(RELIANCE, Timeframe.M5, start, start + dt.timedelta(days=1))
    assert len(candles) == 2
    assert candles[0].start < candles[1].start, "Upstox returns bars newest first; a backtest replays oldest first"
    assert candles[0].open == money_parse("100.00")
    assert candles[0].volume == 1200


def test_candles_chunk_contiguously_with_no_gap_or_overlap():
    handler, paths = candle_handler()
    start = dt.datetime(2025, 1, 1, tzinfo=dt.UTC)
    end = start + dt.timedelta(days=100)
    make_client(handler).candles(RELIANCE, Timeframe.M1, start, end)

    span = chunk_days("minutes", 1)
    windows = [(p.rsplit("/", 2)[-1], p.rsplit("/", 2)[-2]) for p in paths]
    assert len(windows) > 1, f"a 100-day minute range needs several chunks of {span} days"
    for i, (window_start, _) in enumerate(windows):
        want = (start + dt.timedelta(days=i * span)).date().isoformat()
        assert window_start == want, "a gap or overlap here silently loses or duplicates bars"
    assert windows[-1][1] == end.date().isoformat(), "the last chunk must end exactly at the requested end"


def test_candles_make_one_request_for_a_short_range():
    handler, paths = candle_handler()
    start = dt.datetime(2025, 4, 14, tzinfo=dt.UTC)
    make_client(handler).candles(RELIANCE, Timeframe.M5, start, start + dt.timedelta(days=2))
    assert len(paths) == 1


def test_candles_reject_an_inverted_range():
    handler, paths = candle_handler()
    end = dt.datetime(2025, 4, 1, tzinfo=dt.UTC)
    with pytest.raises(ValueError, match=r"ends .* before it starts"):
        make_client(handler).candles(RELIANCE, Timeframe.M5, end + dt.timedelta(days=10), end)
    assert paths == [], "an inverted range must be caught before any request"


def test_candles_send_the_later_date_first():
    handler, paths = candle_handler()
    start = dt.datetime(2025, 4, 14, tzinfo=dt.UTC)
    end = start + dt.timedelta(days=2)
    make_client(handler).candles(RELIANCE, Timeframe.M5, start, end)
    assert paths[0].endswith(f"/minutes/5/{end.date().isoformat()}/{start.date().isoformat()}"), (
        "reversing the dates returns an empty series rather than an error"
    )


def test_candles_skip_a_malformed_row_without_failing_the_batch():
    def handler(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "status": "success",
                "data": {
                    "candles": [
                        ["2025-04-17T00:00:00+05:30", 100.0, 101.0, 99.0, 100.5, 1000, 0],
                        ["not-a-timestamp", 1, 2, 3, 4, 5, 6],
                        [1, 2],
                        ["2025-04-16T00:00:00+05:30", 99.0, 100.0, 98.0, 99.5, 900, 0],
                    ]
                },
            },
        )

    start = dt.datetime(2025, 4, 16, tzinfo=dt.UTC)
    candles = make_client(handler).candles(RELIANCE, Timeframe.D1, start, start + dt.timedelta(days=1))
    assert len(candles) == 2, "one unparseable bar in a five-year backfill is a gap, not a reason to return nothing"


def test_expired_historical_candles_use_the_v2_root_and_keep_the_expiry():
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(str(request.url))
        return httpx.Response(
            200,
            json={
                "status": "success",
                "data": {"candles": [["2025-04-17T15:25:00+05:30", 12.5, 14.0, 11.0, 13.25, 75000, 120000]]},
            },
        )

    candles = make_client(handler).expired_historical_candles(
        "NSE_FO|47983|17-04-2025",
        Timeframe.M5,
        dt.datetime(2025, 4, 17, tzinfo=dt.UTC),
        dt.datetime(2025, 4, 17, tzinfo=dt.UTC),
    )
    assert "/v2/expired-instruments/historical-candle/" in seen[0], (
        "the expired-candle endpoint is on the v2 root while its sibling listings are v3"
    )
    assert "47983%7C17-04-2025" in seen[0], "the expiry is the key's third field and must survive escaping"
    assert candles[0].open_interest == 120000, "open interest matters for an option series"


INSTRUMENT_CSV = """instrument_key,tradingsymbol,name,isin,lot_size,tick_size,instrument_type,expiry,strike
NSE_EQ|INE002A01018,RELIANCE,Reliance Industries,INE002A01018,0,0.05,EQUITY,,
NSE_EQ|INE467B01029,TCS,Tata Consultancy Services,INE467B01029,1,0.05,EQ,,
,BROKEN,No instrument key at all,,1,0.05,EQUITY,,
NSE_FO|54321,NIFTY24500CE,Nifty 50,,75,0.05,CE,2025-04-17,24500
NSE_INDEX|Nifty 50,Nifty 50,Nifty 50,,0,0,INDEX,,
"""


@pytest.mark.parametrize("compress", [True, False])
def test_parse_instrument_csv_handles_both_encodings(compress):
    raw = gzip.compress(INSTRUMENT_CSV.encode()) if compress else INSTRUMENT_CSV.encode()
    instruments = {i.key.symbol: i for i in parse_instrument_csv(raw)}

    assert len(instruments) == 4, "the row with no instrument key must be skipped"
    assert instruments["RELIANCE"].lot_size == 1, (
        "the master reports 0 for cash equity; the domain uses 1 so sizing can multiply unconditionally"
    )
    assert instruments["RELIANCE"].tick_size == money_parse("0.05")
    assert instruments["TCS"].segment == "equity", (
        'the CSV master spells equity "EQUITY" and the JSON one "EQ"; both must classify the same'
    )
    option = instruments["NIFTY24500CE"]
    assert option.segment == "options"
    assert option.option_type == "ce"
    assert option.lot_size == 75
    assert option.expiry == dt.date(2025, 4, 17)
    assert option.strike == money_parse("24500.00")
    assert instruments["Nifty 50"].segment == "index"


def test_parse_instrument_keys_reads_the_raw_key_per_instrument() -> None:
    keys = parse_instrument_keys(INSTRUMENT_CSV.encode())
    assert keys[RELIANCE] == "NSE_EQ|INE002A01018"
    assert keys[InstrumentKey("NFO", "NIFTY24500CE")] == "NSE_FO|54321", (
        "a derivative's key is its exchange token, which no ISIN lookup could produce"
    )
    assert InstrumentKey("NSE", "BROKEN") not in keys, "the row with no instrument key must be skipped"


def test_client_downloads_and_caches_keys_when_no_resolver_is_wired() -> None:
    downloads = 0

    def master(request: httpx.Request) -> httpx.Response:
        nonlocal downloads
        downloads += 1
        return httpx.Response(200, content=INSTRUMENT_CSV.encode())

    client = UpstoxClient(
        api_key="k",
        access_token="t",
        http_client=httpx.Client(transport=httpx.MockTransport(master)),
    )
    assert client._key_for(RELIANCE) == "NSE_EQ|INE002A01018"
    assert client._key_for(InstrumentKey("NSE", "TCS")) == "NSE_EQ|INE467B01029"
    assert downloads == 1, "one exchange is one download per process, not one per lookup"
    with pytest.raises(RuntimeError, match="no instrument key known"):
        client._key_for(InstrumentKey("NSE", "NOPE"))
    assert downloads == 1, "an unknown symbol on a cached exchange must not refetch"


def test_client_prefers_the_wired_resolver() -> None:
    client = UpstoxClient(api_key="k", access_token="t", instrument_key=lambda k: "WIRED|1")
    client._cached_keys["NSE"] = {RELIANCE: "CACHED|1"}
    assert client._key_for(RELIANCE) == "WIRED|1", "a resolver the caller wired must win over the built-in cache"


# --- token freshness --------------------------------------------------


def test_token_of_unknown_age_is_stale():
    client = UpstoxClient(api_key="k", instrument_key=lambda k: "x", access_token="t")
    assert not client.token_fresh(), "assuming a token of unknown age is good is the failure this prevents"


def test_yesterdays_token_is_stale_and_todays_is_fresh():
    fresh = UpstoxClient(
        api_key="k",
        instrument_key=lambda k: "x",
        access_token="t",
        token_issued_at=dt.datetime.now(dt.UTC),
    )
    assert fresh.token_fresh()

    stale = UpstoxClient(
        api_key="k",
        instrument_key=lambda k: "x",
        access_token="t",
        token_issued_at=dt.datetime.now(dt.UTC) - dt.timedelta(days=1),
    )
    assert not stale.token_fresh(), "Upstox tokens expire overnight"


# --- parity, against the same fixture go/upstox/parity_test.go runs ----

FIXTURE = Path(__file__).resolve().parents[2] / "contracts" / "testdata" / "parity.json"


@pytest.fixture(scope="module")
def parity() -> dict:
    """The Upstox block of the shared fixture.

    ``go/upstox/parity_test.go`` runs the same block. A disagreement between the
    two clients about any value here is a failing build rather than a discovery
    six months later: an instrument key one formats and the other cannot parse,
    or a status one calls terminal and the other calls open, is a Python
    screener and a Go executor disagreeing about the same live order.
    """
    block = json.loads(FIXTURE.read_text())["upstox"]
    assert block["instrument_key"], "the upstox block is missing or misnamed"
    return block


def test_parity_instrument_key(parity: dict) -> None:
    for case in parity["instrument_key"]:
        key = InstrumentKey(exchange=case["exchange"], symbol="IGNORED")
        assert instrument_key(key, case["id"]) == case["want"], case


def test_parity_parse_instrument_key(parity: dict) -> None:
    for case in parity["parse_instrument_key"]:
        assert parse_instrument_key(case["key"]) == (case["segment"], case["id"]), case
    for bad in parity["parse_instrument_key_invalid"]:
        with pytest.raises(ValueError):
            parse_instrument_key(bad)


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


def test_parity_order_type_and_validity(parity: dict) -> None:
    for case in parity["order_type"]:
        assert to_order_type(OrderType(case["domain"])) == case["wire"], case
    for case in parity["validity"]:
        domain_value = TimeInForce(case["domain"]) if case["domain"] else ""
        assert to_validity(domain_value) == case["wire"], case
    for unsupported in parity["validity_unsupported"]:
        with pytest.raises(ValueError):
            to_validity(TimeInForce(unsupported))


def test_parity_interval(parity: dict) -> None:
    for case in parity["interval"]:
        unit, interval = to_interval(Timeframe(case["timeframe"]))
        assert (unit, interval) == (case["unit"], case["interval"]), case
        assert chunk_days(unit, interval) == case["chunk_days"], "the two clients must chunk a backfill identically"
    for unsupported in parity["interval_unsupported"]:
        with pytest.raises(ValueError):
            to_interval(unsupported)  # type: ignore[arg-type]


def test_parity_paise(parity: dict) -> None:
    for case in parity["paise"]:
        assert paise(case["rupees"]) == case["want"], (
            f"{case}: a rounding disagreement here is a fill price the two libraries book differently"
        )


def test_parity_trigger_direction(parity: dict) -> None:
    for case in parity["trigger_direction"]:
        side = Side(case["side"])
        assert _stop_direction(side) == case["stop"], case
        assert _target_direction(side) == case["target"], case


# A calendar as Upstox serves it: a plain holiday, a special session that lists
# NSE as both closed and open (a trading day with odd hours), a settlement
# holiday that closes nothing on NSE, and a BSE-only closure.
_HOLIDAYS = {
    "status": "success",
    "data": [
        {
            "date": "2026-01-26",
            "description": "Republic Day",
            "holiday_type": "TRADING_HOLIDAY",
            "closed_exchanges": ["NSE", "BSE", "NFO"],
            "open_exchanges": [],
        },
        {
            "date": "2026-02-01",
            "description": "Budget Day",
            "holiday_type": "SPECIAL_TIMING",
            "closed_exchanges": ["NSE", "BSE"],
            "open_exchanges": [{"exchange": "NSE", "start_time": 1769916600000}],
        },
        {
            "date": "2026-03-30",
            "description": "Settlement",
            "holiday_type": "SETTLEMENT_HOLIDAY",
            "closed_exchanges": ["CDS"],
            "open_exchanges": [],
        },
        {
            "date": "2026-10-20",
            "description": "BSE only",
            "holiday_type": "TRADING_HOLIDAY",
            "closed_exchanges": ["BSE"],
            "open_exchanges": [],
        },
        {
            "date": "2026-12-25",
            "description": "Christmas",
            "holiday_type": "TRADING_HOLIDAY",
            "closed_exchanges": ["NSE", "BSE"],
            "open_exchanges": [],
        },
    ],
}


def test_holidays_apply_the_strict_reading() -> None:
    from tradekit.upstox import Holidays

    seen: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json=_HOLIDAYS)

    client = httpx.Client(transport=httpx.MockTransport(handler))
    closed = Holidays(url="https://example.test/holidays", client=client).closed(
        "NSE", dt.date(2026, 1, 1), dt.date(2026, 6, 30)
    )

    assert "authorization" not in seen[0].headers, "the holiday endpoint is public; no token must be sent"
    assert closed["2026-01-26"] == "Republic Day"
    # A special session lists NSE as open; treating it as a holiday skips a real trading day.
    assert "2026-02-01" not in closed
    assert "2026-03-30" not in closed, "a settlement holiday that does not close NSE"
    assert "2026-10-20" not in closed, "a BSE-only closure must not close NSE"
    assert "2026-12-25" not in closed, "days outside [start, end] must be excluded"


def test_holidays_reject_an_empty_calendar() -> None:
    from tradekit.upstox import parse_holidays

    with pytest.raises(ValueError):
        parse_holidays({"status": "success", "data": []}, "NSE")


def test_login_reports_a_non_json_refusal_as_a_failed_exchange() -> None:
    """Upstox refuses a bad code with plain text; the error must say so, not JSONDecodeError."""
    client = UpstoxClient(
        api_key="key",
        api_secret="secret",
        redirect_uri="http://localhost/cb",
        instrument_key=lambda k: str(k),
        http_client=httpx.Client(transport=httpx.MockTransport(lambda r: httpx.Response(400, text="bad code"))),
    )
    with pytest.raises(RuntimeError, match="HTTP 400"):
        client.login("nope")


def test_login_callback_exchanges_the_code_from_the_query() -> None:
    """The callback owns Upstox's parameter name; a query without one is a refused login, not a KeyError."""
    sent: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        sent.append(request.content.decode())
        return httpx.Response(200, json={"access_token": "fresh"})

    client = UpstoxClient(
        api_key="key",
        api_secret="secret",
        redirect_uri="http://localhost/cb",
        instrument_key=lambda k: str(k),
        http_client=httpx.Client(transport=httpx.MockTransport(handler)),
    )
    assert client.login_callback({"code": "abc123"}) == "fresh"
    assert "code=abc123" in sent[0]
    with pytest.raises(RuntimeError, match="access_denied"):
        client.login_callback({"error": "access_denied"})
    assert len(sent) == 1, "a refusal must not reach the token endpoint"
