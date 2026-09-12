"""The simulator, mirroring ``go/core/paper/paper_test.go`` case for case.

The cases that matter here encode decisions rather than mechanics: which way an
ambiguous bar resolves, which direction slippage and tick rounding move a fill,
and what happens to a resting stop when its position closes by another route.
Those are the answers a backtest's credibility rests on, and they must be the
same answers the Go simulator gives.
"""

from __future__ import annotations

import contextlib
import datetime as dt
import threading

import pytest

from tradekit.core import costs, money, ports
from tradekit.core.domain import (
    Candle,
    ExitReason,
    InstrumentKey,
    OrderRequest,
    OrderStatus,
    OrderType,
    Product,
    Protective,
    Side,
    Timeframe,
)
from tradekit.core.money import Money
from tradekit.core.paper import ORDER_ID_PREFIX, Broker, Options, RejectedError, Slippage

KEY = InstrumentKey("NSE", "RELIANCE")


def at(day: int, hour: int) -> dt.datetime:
    return dt.datetime(2026, 1, day, hour, 0, tzinfo=dt.UTC)


def rs(text: str) -> Money:
    return money.parse(text)


def sim() -> Broker:
    """A simulator with a round opening balance and no frictions.

    A test that is not about slippage or charges then reads as plain arithmetic.
    """
    return Broker(Options(cash=rs("1000000.00")))


def day_bar(day: int, o: str, h: str, low: str, c: str) -> Candle:
    return Candle(
        key=KEY,
        timeframe=Timeframe.D1,
        start=at(day, 15),
        open=rs(o),
        high=rs(h),
        low=rs(low),
        close=rs(c),
        volume=1000,
    )


def buy(qty: int) -> OrderRequest:
    return OrderRequest(key=KEY, side=Side.BUY, quantity=qty, type=OrderType.MARKET, product=Product.CNC)


def sell(qty: int) -> OrderRequest:
    return OrderRequest(key=KEY, side=Side.SELL, quantity=qty, type=OrderType.MARKET, product=Product.CNC)


def limit_buy(qty: int, price: str) -> OrderRequest:
    return OrderRequest(
        key=KEY,
        side=Side.BUY,
        quantity=qty,
        type=OrderType.LIMIT,
        product=Product.CNC,
        limit_price=rs(price),
    )


def test_satisfies_the_ports() -> None:
    b = Broker(Options())
    assert isinstance(b, ports.Broker), "must implement Broker: that is the whole point of the module"
    for protocol in (ports.Quoter, ports.ProtectiveOrders, ports.PositionCloser, ports.TickObserver):
        assert isinstance(b, protocol), f"must implement {protocol.__name__}"


def test_market_order_needs_a_price() -> None:
    b = sim()
    # Filling at a price the simulator invented is the error that makes a
    # backtest look better than the strategy is.
    with pytest.raises(RejectedError):
        b.place_order(buy(10))


def test_market_buy_then_sell() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    order = b.place_order(buy(10))
    assert order.status is OrderStatus.COMPLETE
    assert order.average_price == rs("100.00")

    positions = b.positions()
    assert len(positions) == 1
    assert positions[0].quantity == 10

    b.on_tick(KEY, rs("110.00"), at(2, 10))
    b.place_order(sell(10))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].gross_pnl == rs("100.00")
    assert trades[0].net_pnl == rs("100.00")
    assert trades[0].paper, "every simulated trade must be marked paper"
    assert b.realized_pnl() == rs("100.00")
    assert b.positions() == [], "the position should be flat"


def test_short_round_trip() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(sell(10))

    positions = b.positions()
    assert len(positions) == 1
    assert positions[0].quantity == -10, "a short must be a negative quantity"

    b.on_tick(KEY, rs("90.00"), at(2, 10))
    b.place_order(buy(10))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].side is Side.SELL
    assert trades[0].gross_pnl == rs("100.00"), "a short that fell 10.00 on 10 units makes 100.00"


def test_stop_fills_at_gap_price_not_at_the_stop() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00")))

    # The next bar opens at 90, straight through the 95 stop. A stop is a
    # trigger, not a guarantee: the fill is at 90.
    b.on_bar(day_bar(2, "90.00", "92.00", "88.00", "91.00"))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].exit_price == rs("90.00"), "a gap through the stop fills at the open"
    assert trades[0].exit_reason is ExitReason.STOP
    assert trades[0].gross_pnl == rs("-100.00")


def test_bar_containing_both_levels_resolves_as_the_stop() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00"), target=rs("110.00")))

    # Nothing in a daily bar says which level came first, so the stop is taken.
    b.on_bar(day_bar(2, "100.00", "112.00", "94.00", "105.00"))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].exit_reason is ExitReason.STOP, "an ambiguous bar must resolve against the position"
    assert trades[0].exit_price == rs("95.00")


def test_target_fills_when_the_stop_is_untouched() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00"), target=rs("110.00")))

    b.on_bar(day_bar(2, "101.00", "112.00", "99.00", "108.00"))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].exit_reason is ExitReason.TARGET
    assert trades[0].exit_price == rs("110.00")


def test_oco_cancels_the_sibling_leg() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00"), target=rs("110.00")))

    b.on_bar(day_bar(2, "101.00", "112.00", "99.00", "108.00"))  # target
    b.on_bar(day_bar(3, "100.00", "101.00", "90.00", "92.00"))  # would have hit the stop

    assert len(b.trades()) == 1, "filling one leg must cancel the other"
    assert b.list_protective() == [], "resting legs remain after the position closed"


def test_protective_is_dropped_when_the_position_closes_elsewhere() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00")))

    # Closed by an explicit exit rather than by the stop.
    b.on_tick(KEY, rs("105.00"), at(2, 10))
    b.place_order(sell(10))
    assert b.list_protective() == [], "a stop outlived its position"

    # The old stop level must not manufacture a second trade.
    b.on_bar(day_bar(3, "94.00", "94.00", "90.00", "91.00"))
    assert len(b.trades()) == 1


def test_modify_stop_moves_the_resting_leg() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    protective_id = b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=rs("95.00")))

    # Trail the stop up to breakeven.
    b.modify_stop(protective_id, rs("100.00"))
    b.on_bar(day_bar(2, "104.00", "105.00", "99.00", "101.00"))

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].exit_price == rs("100.00")
    assert trades[0].gross_pnl == 0, "a breakeven stop should close flat"


def test_limit_order_rests_then_fills() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    order = b.place_order(limit_buy(10, "95.00"))
    assert order.status is OrderStatus.OPEN, "a limit away from the market must rest"
    assert len(b.open_orders()) == 1

    # A bar that never reaches 95 leaves it resting.
    b.on_bar(day_bar(2, "99.00", "101.00", "96.00", "100.00"))
    assert len(b.open_orders()) == 1, "the limit filled without the price reaching it"

    b.on_bar(day_bar(3, "97.00", "98.00", "94.00", "96.00"))
    got = b.order_status(order.id)
    assert got.status is OrderStatus.COMPLETE
    assert got.average_price == rs("95.00")


def test_limit_order_gapping_through_fills_at_the_open() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    order = b.place_order(limit_buy(10, "95.00"))

    # The bar opens below the limit: the price was already better than asked
    # for, so the fill is at the open.
    b.on_bar(day_bar(2, "90.00", "93.00", "89.00", "92.00"))

    assert b.order_status(order.id).average_price == rs("90.00"), "want the gap open, not the limit"


def test_cancel_order() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    order = b.place_order(limit_buy(10, "95.00"))
    b.cancel_order(order.id)

    b.on_bar(day_bar(2, "97.00", "98.00", "90.00", "96.00"))
    assert b.order_status(order.id).status is OrderStatus.CANCELLED
    assert b.trades() == [], "a cancelled order filled anyway"
    with pytest.raises(RejectedError):
        b.cancel_order(order.id)


def test_slippage_always_works_against_the_position() -> None:
    b = Broker(Options(cash=rs("1000000.00"), slippage=Slippage(fraction=0.001)))  # 10 bps
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    entry = b.place_order(buy(10))
    assert entry.average_price == rs("100.10"), "a buy pays up"

    b.on_tick(KEY, rs("110.00"), at(2, 10))
    b.place_order(sell(10))

    trade = b.trades()[0]
    assert trade.exit_price == rs("109.89"), "a sell receives less"
    assert trade.gross_pnl < rs("100.00"), "both legs erode the result; neither improves it"


def test_tick_rounding_never_improves_a_fill() -> None:
    b = Broker(Options(cash=rs("1000000.00"), tick_size=lambda _key: rs("0.05")))

    # 100.02 is not a printable price on a 5-paise tick.
    b.on_tick(KEY, rs("100.02"), at(1, 10))
    entry = b.place_order(buy(10))
    # Rounding to nearest would hand the simulation free edge on half of all
    # fills, so a buy rounds up, never down.
    assert entry.average_price == rs("100.05")

    b.on_tick(KEY, rs("110.02"), at(2, 10))
    b.place_order(sell(10))
    assert b.trades()[0].exit_price == rs("110.00"), "a sell rounds down"


def test_charges_are_deducted_from_net_pnl() -> None:
    table = costs.Table()
    table.set(
        "testbroker",
        costs.Segment.EQUITY_DELIVERY,
        costs.Kind.BROKERAGE,
        costs.Rate(value=0.001, effective_from=dt.date(2020, 1, 1)),
    )
    b = Broker(
        Options(
            cash=rs("1000000.00"),
            charges=table,
            broker="testbroker",
            segment=costs.Segment.EQUITY_DELIVERY,
        )
    )

    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.on_tick(KEY, rs("110.00"), at(2, 10))
    b.place_order(sell(10))

    trade = b.trades()[0]
    # 0.1% of each leg: 1.00 on the buy, 1.10 on the sell.
    assert trade.charges == rs("2.10")
    assert trade.net_pnl == trade.gross_pnl - trade.charges
    assert b.realized_pnl() == rs("97.90")


def test_charges_are_zero_with_no_rate_table() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.on_tick(KEY, rs("110.00"), at(2, 10))
    b.place_order(sell(10))

    # Zero means "not computed", never an invented estimate.
    assert b.trades()[0].charges == 0


def test_buying_power_is_enforced() -> None:
    b = Broker(Options(cash=rs("1000.00")))
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    # 20 units at 100.00 is 2000.00 against 1000.00 of cash.
    with pytest.raises(RejectedError):
        b.place_order(buy(20))
    b.place_order(buy(10))  # affordable, must not raise


def test_leverage_widens_buying_power() -> None:
    b = Broker(Options(cash=rs("1000.00"), leverage=5))
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(40))  # 5x leverage affords 4000.00 of notional


def test_averaging_into_a_position() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.on_tick(KEY, rs("110.00"), at(1, 11))
    b.place_order(buy(10))

    positions = b.positions()
    assert len(positions) == 1
    assert positions[0].quantity == 20
    assert positions[0].average_price == rs("105.00"), "the quantity-weighted mean"


def test_partial_close() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))
    b.on_tick(KEY, rs("110.00"), at(2, 10))
    b.place_order(sell(4))

    positions = b.positions()
    assert len(positions) == 1
    assert positions[0].quantity == 6

    trades = b.trades()
    assert len(trades) == 1
    assert trades[0].quantity == 4
    assert trades[0].gross_pnl == rs("40.00"), "profit on the closed portion only"


def test_close_position_reports_whether_it_found_one() -> None:
    b = sim()
    assert not b.close_position(KEY, Side.BUY, rs("100.00"), at(1, 10), ExitReason.EOD)

    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))

    # A long-exit must not close a short, and vice versa.
    assert not b.close_position(KEY, Side.SELL, rs("110.00"), at(2, 10), ExitReason.EOD), (
        "a short-side close matched a long position"
    )
    assert b.close_position(KEY, Side.BUY, rs("110.00"), at(2, 10), ExitReason.EOD)
    assert b.trades()[0].exit_reason is ExitReason.EOD


def test_equity_tracks_unrealised_profit() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    b.place_order(buy(10))

    assert b.cash() == rs("1000000.00"), "cash is untouched until the position closes"
    b.on_tick(KEY, rs("110.00"), at(1, 11))
    assert b.equity() == rs("1000100.00"), "equity marks to market"
    assert b.account().used == rs("1000.00"), "exposure is the position at cost"


def test_order_ids_carry_the_paper_prefix() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    order = b.place_order(buy(10))
    # The id is the last-resort mode marker for a persistence path that forgot
    # to propagate the paper flag.
    assert order.id.startswith(ORDER_ID_PREFIX)


def test_ltp_omits_unseen_instruments() -> None:
    b = sim()
    other = InstrumentKey("NSE", "TCS")
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    assert b.ltp([KEY, other]) == {KEY: rs("100.00")}, "an instrument with no price must be omitted, not zero"


def test_quote_reports_the_last_completed_bar() -> None:
    b = sim()
    # A tick alone carries no bar, so the OHLC fields stay zero rather than
    # being synthesised from the tick stream.
    b.on_tick(KEY, rs("100.00"), at(1, 10))
    assert b.quote([KEY])[KEY].high == 0

    b.on_bar(day_bar(1, "99.00", "103.00", "98.00", "102.00"))
    quoted = b.quote([KEY])[KEY]
    assert quoted.last == rs("102.00")
    assert quoted.high == rs("103.00")


def test_rejects_degenerate_orders() -> None:
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    with pytest.raises(RejectedError):
        b.place_order(OrderRequest(key=KEY, side=Side.BUY, quantity=0, type=OrderType.MARKET))
    with pytest.raises(RejectedError):
        b.place_order(OrderRequest(key=KEY, side=Side.BUY, quantity=1, type=OrderType.LIMIT))
    with pytest.raises(RejectedError):
        b.place_protective(Protective(key=KEY, side=Side.SELL, quantity=10, stop=money.ZERO))


def test_concurrent_ticks_and_orders() -> None:
    # A live paper session has a scanner placing orders while a feed delivers
    # ticks. The two must not corrupt the book between them.
    b = sim()
    b.on_tick(KEY, rs("100.00"), at(1, 10))

    def feed() -> None:
        for i in range(200):
            b.on_tick(KEY, Money(rs("100.00") + i), at(1, 10))

    thread = threading.Thread(target=feed)
    thread.start()
    for _ in range(50):
        with contextlib.suppress(RejectedError):
            b.place_order(buy(1))
        b.positions()
        b.equity()
    thread.join()

    assert b.positions()[0].quantity == 50, "every order placed must be in the book exactly once"
