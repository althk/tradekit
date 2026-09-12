"""Core tests, mirroring go/core's suite assertion for assertion.

Where a case here has a counterpart in the Go tests, the expected value is
identical. That is what makes the two libraries one system rather than two that
happen to share a README.
"""

from __future__ import annotations

import datetime as dt
import math
from zoneinfo import ZoneInfo

import pytest

from tradekit.core import costs, indicators, money, risk, stats
from tradekit.core.calendar import Calendar, StaticHolidays, nse
from tradekit.core.domain import (
    Candle,
    ExitReason,
    InstrumentKey,
    Side,
    Signal,
    SignalKind,
    Timeframe,
    Trade,
    gross_for,
)
from tradekit.core.money import Money

IST = ZoneInfo("Asia/Kolkata")
KEY = InstrumentKey("NSE", "RELIANCE")


# --------------------------------------------------------------------------- money


@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("0", 0),
        ("1", 100),
        ("1.5", 150),
        ("1.05", 105),
        ("1234.56", 123456),
        ("-0.05", -5),
        ("-1234.56", -123456),
        ("+7.25", 725),
        (".5", 50),
        ("  12.34  ", 1234),
    ],
)
def test_parse(text: str, want: int) -> None:
    assert money.parse(text) == want


@pytest.mark.parametrize("text", ["", "abc", "1.234", "1.2.3", "--1", "1e5"])
def test_parse_rejects(text: str) -> None:
    with pytest.raises(ValueError):
        money.parse(text)


@pytest.mark.parametrize("text", ["0.00", "1.00", "1.05", "1234.56", "-0.05", "-1234.56"])
def test_format_round_trips(text: str) -> None:
    assert money.format(money.parse(text)) == text


@pytest.mark.parametrize(
    ("amount", "fraction", "want"),
    [(100, 0.005, 1), (100, 0.015, 2), (100, 0.025, 3), (-100, 0.025, -3), (123456, 0.001, 123)],
)
def test_mul_fraction_rounds_half_away_from_zero(amount: int, fraction: float, want: int) -> None:
    # Python's built-in round() is banker's rounding and would give 2 for the
    # 0.025 case, disagreeing with Go. This pins the shared rule.
    assert money.mul_fraction(Money(amount), fraction) == want


@pytest.mark.parametrize(
    ("amount", "want"),
    [(100, 100), (102, 100), (103, 105), (1237, 1235), (1238, 1240), (-102, -100), (-103, -105)],
)
def test_round_to_tick(amount: int, want: int) -> None:
    assert money.round_to_tick(Money(amount), Money(5)) == want


def test_floor_and_ceil_to_tick() -> None:
    assert money.floor_to_tick(Money(103), Money(5)) == 100
    assert money.ceil_to_tick(Money(101), Money(5)) == 105
    assert money.floor_to_tick(Money(100), Money(5)) == 100
    assert money.ceil_to_tick(Money(100), Money(5)) == 100
    assert money.floor_to_tick(Money(-103), Money(5)) == -105
    assert money.ceil_to_tick(Money(-103), Money(5)) == -100


def test_non_positive_tick_leaves_amount_unchanged() -> None:
    assert money.round_to_tick(Money(103), Money(0)) == 103


# -------------------------------------------------------------------------- domain


def test_gross_for_signs() -> None:
    assert gross_for(Side.BUY, 10, money.parse("100.00"), money.parse("110.00")) == money.parse("100.00")
    assert gross_for(Side.SELL, 10, money.parse("100.00"), money.parse("90.00")) == money.parse("100.00")


def test_signal_side() -> None:
    now = dt.datetime.now(dt.UTC)
    assert Signal(KEY, SignalKind.LONG, now).side is Side.BUY
    assert Signal(KEY, SignalKind.SHORT, now).side is Side.SELL
    assert Signal(KEY, SignalKind.EXIT_SHORT, now).side is Side.BUY


# ------------------------------------------------------------------------ calendar


def _at(hour: int, minute: int) -> dt.datetime:
    """A timestamp on 2026-01-15, a Thursday, in IST."""
    return dt.datetime(2026, 1, 15, hour, minute, tzinfo=IST)


@pytest.mark.parametrize(
    ("hour", "minute", "want"),
    [(9, 14, False), (9, 15, True), (12, 0, True), (15, 30, True), (15, 31, False)],
)
def test_in_session(hour: int, minute: int, want: bool) -> None:
    assert nse().in_session(_at(hour, minute)) is want


def test_in_session_rejects_weekend() -> None:
    assert not nse().in_session(dt.datetime(2026, 1, 17, 12, 0, tzinfo=IST))


@pytest.mark.parametrize(
    ("hour", "minute", "tf", "want_h", "want_m"),
    [
        (9, 15, Timeframe.M5, 9, 15),
        (9, 19, Timeframe.M5, 9, 15),
        (9, 20, Timeframe.M5, 9, 20),
        (9, 29, Timeframe.M15, 9, 15),
        (9, 30, Timeframe.M15, 9, 30),
        (10, 7, Timeframe.M15, 10, 0),
        (9, 0, Timeframe.M5, 9, 15),  # a pre-open print clamps into the first bar
    ],
)
def test_bucket_start_anchors_to_open(hour: int, minute: int, tf: Timeframe, want_h: int, want_m: int) -> None:
    assert nse().bucket_start(_at(hour, minute), tf) == _at(want_h, want_m)


def test_slot_index_and_count() -> None:
    s = nse()
    assert s.slot_index(_at(9, 15), Timeframe.M5) == 0
    assert s.slot_index(_at(9, 22), Timeframe.M5) == 1
    # 09:15 to 15:30 is 375 minutes: 75 five-minute bars, 25 fifteens.
    assert s.slot_count(Timeframe.M5) == 75
    assert s.slot_count(Timeframe.M15) == 25


def test_trading_day_honours_holidays() -> None:
    cal = Calendar(nse(), StaticHolidays({"NSE": {"2026-01-26": "Republic Day"}}))
    ok, err = cal.trading_day(dt.datetime(2026, 1, 26, 10, 0, tzinfo=IST))
    assert err is None
    assert ok is False


def test_trading_days_excludes_weekends_and_holidays() -> None:
    cal = Calendar(nse(), StaticHolidays({"NSE": {"2026-01-26": "Republic Day"}}))
    days, err = cal.trading_days(dt.date(2026, 1, 26), dt.date(2026, 1, 30))
    assert err is None
    assert len(days) == 4
    assert dt.date(2026, 1, 26) not in days


def test_trading_day_fails_open() -> None:
    class Failing:
        def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
            raise TimeoutError("holiday API unreachable")

    ok, err = Calendar(nse(), Failing()).trading_day(_at(10, 0))
    assert isinstance(err, TimeoutError)
    # Skipping a real trading day is the worse error.
    assert ok is True


# ---------------------------------------------------------------------- indicators


def _bars(high: list[float], low: list[float], close: list[float]) -> list[Candle]:
    base = dt.datetime(2026, 1, 1, tzinfo=dt.UTC)
    return [
        Candle(
            KEY,
            Timeframe.D1,
            base + dt.timedelta(days=i),
            Money(int(c)),
            Money(int(high[i])),
            Money(int(low[i])),
            Money(int(c)),
            100,
        )
        for i, c in enumerate(close)
    ]


def test_sma() -> None:
    got = indicators.sma([1, 2, 3, 4, 5], 3)
    assert not indicators.is_valid(got[0])
    assert not indicators.is_valid(got[1])
    assert got[2:] == [2, 3, 4]


def test_ema_seeds_with_simple_average() -> None:
    got = indicators.ema([1, 2, 3, 4, 5], 3)
    assert not indicators.is_valid(got[1])
    assert got[2] == pytest.approx(2)
    assert got[3] == pytest.approx(3)
    assert got[4] == pytest.approx(4)


def test_true_range_uses_previous_close() -> None:
    tr = indicators.true_range(_bars([110, 130, 120], [90, 115, 100], [100, 120, 110]))
    assert not indicators.is_valid(tr[0])
    assert tr[1] == pytest.approx(30)
    assert tr[2] == pytest.approx(20)


def test_atr_wilder_smoothing() -> None:
    n = 20
    cs = _bars([105] * n, [95] * n, [100] * n)
    got = indicators.atr(cs, 5)
    assert not indicators.is_valid(got[4])
    assert all(got[i] == pytest.approx(10) for i in range(5, n))


def test_rsi_bounds_and_neutral() -> None:
    rising = [float(100 + i) for i in range(30)]
    assert indicators.rsi(rising, 14)[-1] == pytest.approx(100)
    falling = [float(200 - i) for i in range(30)]
    assert indicators.rsi(falling, 14)[-1] == pytest.approx(0)
    assert indicators.rsi([100.0] * 30, 14)[-1] == pytest.approx(50)


def test_donchian_excludes_current_bar() -> None:
    ch = indicators.donchian(_bars([10, 12, 11, 20], [5, 6, 4, 3], [8, 9, 7, 15]), 3)
    assert not indicators.is_valid(ch.upper[2])
    assert ch.upper[3] == pytest.approx(12)
    assert ch.lower[3] == pytest.approx(4)
    assert ch.middle[3] == pytest.approx(8)


def test_vwap_resets_each_session() -> None:
    cs = _bars([100, 100, 200, 200], [100, 100, 200, 200], [100, 100, 200, 200])
    got = indicators.vwap(cs, lambda i: i == 2)
    assert got[1] == pytest.approx(100)
    # Without a reset the running average at index 2 would be 133.33.
    assert got[2] == pytest.approx(200)


def test_cpr_orders_central_range() -> None:
    p = indicators.cpr(
        Candle(KEY, Timeframe.D1, dt.datetime.now(dt.UTC), Money(11000), Money(12000), Money(10000), Money(10100))
    )
    assert p.bc <= p.tc
    assert p.pivot == 10700
    assert (p.bc, p.tc) == (10400, 11000)


def test_adx_detects_a_trend() -> None:
    n = 60
    close = [float(100 + i) for i in range(n)]
    got = indicators.adx(_bars([c + 1 for c in close], [c - 1 for c in close], close), 14)
    assert indicators.is_valid(got.adx[-1])
    assert 0 <= got.adx[-1] <= 100
    assert got.adx[-1] > 50
    assert got.plus_di[-1] > got.minus_di[-1]


def test_to_money_rounds_half_away_from_zero() -> None:
    assert indicators.to_money(1234.5) == 1235
    assert indicators.to_money(float("nan")) == 0
    assert indicators.is_valid(0.0)


# --------------------------------------------------------------------------- costs


def _table() -> costs.Table:
    t = costs.Table()
    epoch = dt.date(2020, 1, 1)
    b, seg = "testbroker", costs.Segment.EQUITY_DELIVERY
    t.set(b, seg, costs.Kind.BROKERAGE, costs.Rate(0.001, epoch))
    t.set(b, seg, costs.Kind.STT_BUY, costs.Rate(0.001, epoch))
    t.set(b, seg, costs.Kind.STT_SELL, costs.Rate(0.001, epoch))
    t.set(b, seg, costs.Kind.EXCHANGE, costs.Rate(0.0001, epoch))
    t.set(b, seg, costs.Kind.SEBI, costs.Rate(0.0001, epoch))
    t.set(b, seg, costs.Kind.STAMP, costs.Rate(0.0001, epoch))
    t.set(b, seg, costs.Kind.DP, costs.Rate(1500, epoch, flat=True))
    t.set(b, seg, costs.Kind.GST, costs.Rate(0.18, epoch))
    return t


def test_compute_round_trip_matches_go() -> None:
    got = costs.compute(
        _table(),
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
    assert got == costs.Charges(
        brokerage=Money(2100),
        stt=Money(2100),
        exchange=Money(210),
        sebi=Money(210),
        stamp=Money(100),
        dp=Money(1500),
        gst=Money(724),
        total=Money(2100 + 2100 + 210 + 210 + 100 + 1500 + 724),
    )


def test_gst_excludes_stt_and_stamp() -> None:
    t = _table()
    epoch = dt.date(2020, 1, 1)
    t.set("testbroker", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_BUY, costs.Rate(0.5, epoch))
    t.set("testbroker", costs.Segment.EQUITY_DELIVERY, costs.Kind.STAMP, costs.Rate(0.5, epoch))
    got = costs.compute(
        t,
        costs.Trade(
            "testbroker",
            costs.Segment.EQUITY_DELIVERY,
            100,
            money.parse("100.00"),
            money.parse("110.00"),
            dt.date(2026, 1, 5),
            dt.date(2026, 1, 9),
        ),
    )
    assert got.gst == 724


def test_stamp_applies_to_buy_leg_of_a_short() -> None:
    got = costs.compute(
        _table(),
        costs.Trade(
            "testbroker",
            costs.Segment.EQUITY_DELIVERY,
            100,
            money.parse("100.00"),
            money.parse("110.00"),
            dt.date(2026, 1, 5),
            dt.date(2026, 1, 9),
            buying=False,
        ),
    )
    assert got.stamp == 110


def test_effective_dating_prices_each_leg_on_its_own_date() -> None:
    t = costs.Table()
    t.set("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_BUY, costs.Rate(0.001, dt.date(2020, 1, 1)))
    t.set("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, costs.Rate(0.001, dt.date(2020, 1, 1)))
    t.set("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.STT_SELL, costs.Rate(0.002, dt.date(2026, 2, 1)))
    got = costs.compute(
        t,
        costs.Trade(
            "b",
            costs.Segment.EQUITY_DELIVERY,
            100,
            money.parse("100.00"),
            money.parse("100.00"),
            dt.date(2026, 1, 20),
            dt.date(2026, 2, 10),
        ),
    )
    assert got.stt == 3000


def test_lookup_distinguishes_absent_from_zero() -> None:
    t = costs.Table()
    t.set("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, costs.Rate(0.0, dt.date(2020, 1, 1), flat=True))
    assert t.lookup("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, dt.date(2026, 1, 1)) is not None
    assert t.lookup("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.BROKERAGE, dt.date(2026, 1, 1)) is None
    assert t.lookup("b", costs.Segment.EQUITY_DELIVERY, costs.Kind.DP, dt.date(2019, 1, 1)) is None


def test_compute_rejects_non_positive_quantity() -> None:
    with pytest.raises(ValueError):
        costs.compute(
            _table(),
            costs.Trade(
                "testbroker",
                costs.Segment.EQUITY_DELIVERY,
                0,
                Money(1),
                Money(1),
                dt.date(2026, 1, 1),
                dt.date(2026, 1, 1),
            ),
        )


# ---------------------------------------------------------------------------- risk


def test_size_from_stop_distance() -> None:
    got = risk.size(risk.SizeParams(money.parse("100000.00"), 0.01, money.parse("500.00"), money.parse("490.00")))
    assert got.quantity == 100


def test_size_is_symmetric_for_shorts() -> None:
    long = risk.size(risk.SizeParams(money.parse("100000.00"), 0.01, money.parse("500.00"), money.parse("490.00")))
    short = risk.size(risk.SizeParams(money.parse("100000.00"), 0.01, money.parse("500.00"), money.parse("510.00")))
    assert long.quantity == short.quantity


def test_size_notional_cap() -> None:
    got = risk.size(
        risk.SizeParams(
            money.parse("100000.00"),
            0.01,
            money.parse("500.00"),
            money.parse("490.00"),
            max_notional=money.parse("10000.00"),
        )
    )
    assert got.quantity == 20
    assert got.reason == "capped by notional limit"


def test_size_caps_at_buying_power_by_default() -> None:
    p = risk.SizeParams(money.parse("10000.00"), 0.10, money.parse("500.00"), money.parse("495.00"))
    assert risk.size(p).quantity == 20
    p.leverage = 5
    assert risk.size(p).quantity == 100


def test_size_floors_to_whole_lots() -> None:
    got = risk.size(
        risk.SizeParams(
            money.parse("13700.00"),
            0.10,
            money.parse("500.00"),
            money.parse("490.00"),
            lot_size=50,
            leverage=100,
        )
    )
    assert got.quantity == 100


def test_size_returns_zero_below_one_lot() -> None:
    got = risk.size(
        risk.SizeParams(money.parse("1000.00"), 0.01, money.parse("500.00"), money.parse("490.00"), lot_size=50)
    )
    assert got.quantity == 0
    assert got.reason


@pytest.mark.parametrize(
    "params",
    [
        risk.SizeParams(Money(1000000), 0.01, Money(50000), Money(50000)),
        risk.SizeParams(Money(0), 0.01, Money(50000), Money(49000)),
        risk.SizeParams(Money(1000000), 0.0, Money(50000), Money(49000)),
        risk.SizeParams(Money(1000000), 0.01, Money(0), Money(49000)),
    ],
)
def test_size_rejects_degenerate_inputs(params: risk.SizeParams) -> None:
    got = risk.size(params)
    assert got.quantity == 0
    assert got.reason


def _sig(at: dt.datetime | None = None) -> Signal:
    return Signal(KEY, SignalKind.LONG, at or dt.datetime.now(dt.UTC))


def test_chain_reports_the_first_refusal() -> None:
    state = risk.DailyState("2026-01-15", trades_today=5, realized_pnl=money.parse("-5000.00"))
    chain = risk.Chain(risk.DailyLossLimit(money.parse("2000.00")), risk.MaxTradesPerDay(3))
    with pytest.raises(risk.RiskBlockedError) as exc:
        chain.check(_sig(), state)
    # A day that has blown its budget must say so, not report whichever
    # secondary limit also happens to be breached.
    assert exc.value.gate == "daily_loss_limit"


def test_daily_loss_limit_boundary() -> None:
    g = risk.DailyLossLimit(money.parse("2000.00"))
    g.check(_sig(), risk.DailyState("d", realized_pnl=money.parse("-1999.99")))
    with pytest.raises(risk.RiskBlockedError):
        g.check(_sig(), risk.DailyState("d", realized_pnl=money.parse("-2000.00")))


def test_zero_limits_disable_their_gates() -> None:
    state = risk.DailyState(
        "d",
        trades_today=999,
        trades_per_key={str(KEY): 999},
        open_positions=999,
        notional_open=money.parse("9999999.00"),
        realized_pnl=money.parse("-999999.00"),
    )
    risk.Chain(
        risk.DailyLossLimit(Money(0)),
        risk.MaxTradesPerDay(0),
        risk.MaxTradesPerSymbol(0),
        risk.MaxConcurrentPositions(0),
        risk.MaxOpenNotional(Money(0)),
    ).check(_sig(), state)


def test_kill_switch_latches() -> None:
    tracker = risk.Tracker(risk.DailyState("2026-01-15"))
    tracker.trip("max_slippage")
    tracker.trip("something_else")
    state = tracker.snapshot()
    assert state.kill_switch == "max_slippage"
    with pytest.raises(risk.RiskBlockedError):
        risk.KillSwitch().check(_sig(), state)


def test_trading_window() -> None:
    g = risk.TradingWindow(9 * 60 + 30, 15 * 60, IST)
    g.check(_sig(dt.datetime(2026, 1, 15, 10, 0, tzinfo=IST)), risk.DailyState("d"))
    for bad in (dt.datetime(2026, 1, 15, 9, 20, tzinfo=IST), dt.datetime(2026, 1, 15, 15, 20, tzinfo=IST)):
        with pytest.raises(risk.RiskBlockedError):
            g.check(_sig(bad), risk.DailyState("d"))


def test_daily_state_fresh_for_discards_another_day() -> None:
    yesterday = risk.DailyState("2026-01-14", trades_today=7, realized_pnl=money.parse("-5000.00"))
    got = yesterday.fresh_for("2026-01-15")
    assert got.trades_today == 0
    assert got.realized_pnl == 0
    assert risk.DailyState("2026-01-15", trades_today=3).fresh_for("2026-01-15").trades_today == 3


def test_tracker_snapshot_is_a_copy() -> None:
    tracker = risk.Tracker(risk.DailyState("2026-01-15"))
    tracker.record_trade(KEY)
    snap = tracker.snapshot()
    snap.trades_per_key[str(KEY)] = 99
    snap.trades_today = 99
    again = tracker.snapshot()
    assert again.trades_today == 1
    assert again.trades_per_key[str(KEY)] == 1


def test_trail_is_monotonic() -> None:
    got = risk.trail(Side.BUY, money.parse("500.00"), money.parse("520.00"), 200, 2)
    assert got == money.parse("516.00")
    assert risk.trail(Side.BUY, got, money.parse("505.00"), 200, 2) == money.parse("516.00")

    short = risk.trail(Side.SELL, money.parse("500.00"), money.parse("480.00"), 200, 2)
    assert short == money.parse("484.00")
    assert risk.trail(Side.SELL, short, money.parse("495.00"), 200, 2) == money.parse("484.00")


def test_breakeven() -> None:
    entry, initial = money.parse("500.00"), money.parse("490.00")
    assert risk.breakeven(Side.BUY, initial, entry, initial, money.parse("505.00"), True) == initial
    assert risk.breakeven(Side.BUY, initial, entry, initial, money.parse("510.00"), True) == entry
    assert risk.breakeven(Side.BUY, initial, entry, initial, money.parse("510.00"), False) == initial

    s_entry, s_initial = money.parse("500.00"), money.parse("510.00")
    assert risk.breakeven(Side.SELL, s_initial, s_entry, s_initial, money.parse("490.00"), True) == s_entry


# --------------------------------------------------------------------------- stats


def _trade(day: int, net: str, paper: bool = False) -> Trade:
    amount = money.parse(net)
    exit_at = dt.datetime(2026, 1, day, 15, 30, tzinfo=dt.UTC)
    return Trade(
        key=KEY,
        side=Side.BUY,
        quantity=1,
        entry_price=money.parse("100.00"),
        exit_price=Money(money.parse("100.00") + amount),
        entry_at=exit_at - dt.timedelta(hours=2),
        exit_at=exit_at,
        gross_pnl=amount,
        net_pnl=amount,
        exit_reason=ExitReason.TARGET,
        paper=paper,
    )


def test_summarize_basics() -> None:
    m = stats.summarize([_trade(1, "100.00"), _trade(2, "-50.00"), _trade(3, "200.00"), _trade(4, "-50.00")])
    assert (m.trades, m.wins, m.losses) == (4, 2, 2)
    assert m.net_pnl == money.parse("200.00")
    assert m.gross_profit == money.parse("300.00")
    assert m.gross_loss == money.parse("100.00")
    assert m.win_rate == 0.5
    assert m.profit_factor == 3
    assert m.expectancy == money.parse("50.00")
    assert m.avg_win == money.parse("150.00")
    assert m.avg_loss == money.parse("50.00")
    assert m.avg_holding == dt.timedelta(hours=2)


def test_summarize_refuses_mixed_modes() -> None:
    with pytest.raises(stats.MixedModesError):
        stats.summarize([_trade(1, "100.00", paper=False), _trade(2, "100.00", paper=True)])


def test_profit_factor_with_no_losses() -> None:
    m = stats.summarize([_trade(1, "100.00"), _trade(2, "50.00")])
    assert math.isinf(m.profit_factor)


def test_summarize_is_order_independent() -> None:
    ordered = [_trade(1, "100.00"), _trade(2, "-300.00"), _trade(3, "50.00")]
    shuffled = [ordered[2], ordered[0], ordered[1]]
    assert stats.summarize(ordered).max_drawdown == stats.summarize(shuffled).max_drawdown


def test_max_drawdown() -> None:
    now = dt.datetime.now(dt.UTC)
    curve = [stats.Point(now, money.parse(v)) for v in ("1000.00", "1200.00", "900.00", "1100.00", "1050.00")]
    amount, pct = stats.max_drawdown(curve)
    assert amount == money.parse("300.00")
    assert pct == pytest.approx(0.25)


def test_r_multiple() -> None:
    long = _trade(1, "30.00")
    assert stats.r_multiple(long, money.parse("90.00")) == pytest.approx(3)
    short = Trade(
        key=KEY,
        side=Side.SELL,
        quantity=1,
        entry_price=money.parse("100.00"),
        exit_price=money.parse("90.00"),
        entry_at=long.entry_at,
        exit_at=long.exit_at,
    )
    assert stats.r_multiple(short, money.parse("105.00")) == pytest.approx(2)
    assert stats.r_multiple(long, money.parse("100.00")) is None


def test_sharpe() -> None:
    assert stats.sharpe([0.01, 0.01, 0.01]) is None
    assert stats.sharpe([0.01]) is None
    positive = stats.sharpe([0.01, -0.005, 0.02, 0.0, 0.015])
    assert positive is not None and positive > 0
    negative = stats.sharpe([0.01, -0.005, 0.02, 0.0, 0.015], risk_free=0.05)
    assert negative is not None and negative < 0


def test_by_exit_reason() -> None:
    a, b, c = _trade(1, "10.00"), _trade(2, "10.00"), _trade(3, "10.00")
    a.exit_reason = ExitReason.STOP
    m = stats.summarize([a, b, c])
    assert m.by_exit_reason[ExitReason.TARGET] == 2
    assert m.by_exit_reason[ExitReason.STOP] == 1


# --- options ---------------------------------------------------------------


def test_implied_vol_round_trips_and_refuses_impossible_prints() -> None:
    from tradekit.core.options import Model

    m = Model(risk_free=0.065, div_yield=0.012)
    for years in (1 / 365, 3 / 365, 7 / 365):
        for want in (0.08, 0.12, 0.25, 0.60):
            price = m.price(24300, 24300, years, want, True)
            got = m.implied_vol(price, 24300, 24300, years, True)
            assert got is not None and abs(got - want) < 1e-3, f"T={years} sigma={want}: got {got}"
    # A print outside what the model can produce must be refused, not turned
    # into a made-up vol that then picks a strike.
    assert m.implied_vol(1, 24300, 24300, 2 / 365, True) is None
    assert m.implied_vol(24300, 24300, 24300, 2 / 365, True) is None


def test_strike_for_delta_inverts_delta_and_is_in_the_money() -> None:
    import datetime as dt

    from tradekit.core.options import MIN_YEARS, Model, years_to_expiry

    m = Model(risk_free=0.065, div_yield=0.012)
    for years in (0.5 / 365, 2 / 365, 6 / 365):
        for target in (0.55, 0.65, 0.80, 0.90):
            for is_call in (True, False):
                k = m.strike_for_delta(24300, years, 0.12, target, is_call)
                assert abs(abs(m.delta(24300, k, years, 0.12, is_call)) - target) < 1e-3
    assert m.strike_for_delta(24300, 3 / 365, 0.12, 0.8, True) < 24300, "a call at target delta is below spot"
    assert m.strike_for_delta(24300, 3 / 365, 0.12, 0.8, False) > 24300, "a put at target delta is above spot"

    close = dt.datetime(2026, 8, 18, 15, 30, tzinfo=dt.UTC)
    assert years_to_expiry(dt.datetime(2026, 8, 18, 10, 30, tzinfo=dt.UTC), close) == pytest.approx(5 / (24 * 365))
    assert years_to_expiry(close + dt.timedelta(hours=20), close) == MIN_YEARS, "past expiry floors, never negative"
