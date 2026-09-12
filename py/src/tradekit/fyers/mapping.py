"""Translation between tradekit's domain and FYERS's wire vocabulary.

This module mirrors ``go/fyers/mapping.go`` exactly. Every function here has a
counterpart there with the same name in Go's casing, and the shared cases are
pinned by ``contracts/testdata/parity.json`` under the ``fyers`` key, so the two
clients cannot drift.

FYERS encodes most enumerations as small integers rather than strings, so a
typo here is not a misspelt word the broker rejects but a different, valid
value the broker accepts: 1 is a limit order and 2 a market order, 1 is buy and
-1 sell. The Go module pins these against the official SDK; this one is pinned
against the Go module through the parity fixture.

Nothing here does I/O, so it is testable without a network or a broker account.
"""

from __future__ import annotations

import datetime as dt

from tradekit.core.domain import (
    InstrumentKey,
    OrderStatus,
    OrderType,
    Product,
    Side,
    Timeframe,
    TimeInForce,
)
from tradekit.core.money import Money, _round_half_away

__all__ = [
    "DEFAULT_SERIES",
    "GTT_OCO",
    "GTT_SINGLE",
    "IST",
    "ORDER_TYPE_LIMIT",
    "ORDER_TYPE_MARKET",
    "ORDER_TYPE_STOP",
    "ORDER_TYPE_STOP_LIMIT",
    "PRODUCT_CNC",
    "PRODUCT_INTRADAY",
    "PRODUCT_MARGIN",
    "PRODUCT_MTF",
    "RESPONSE_ERROR",
    "RESPONSE_OK",
    "SEGMENT_CAPITAL_MARKET",
    "SEGMENT_COMMODITY",
    "SEGMENT_CURRENCY_DERIV",
    "SEGMENT_EQUITY_DERIV",
    "SIDE_BUY",
    "SIDE_SELL",
    "STATUS_CANCELLED",
    "STATUS_EXPIRED",
    "STATUS_PENDING",
    "STATUS_REJECTED",
    "STATUS_TRADED",
    "STATUS_TRANSIT",
    "VALIDITY_DAY",
    "VALIDITY_IOC",
    "chunk_days",
    "from_order_type",
    "from_product",
    "from_side",
    "from_validity",
    "key_for",
    "normalize_status",
    "paise",
    "parse_order_time",
    "rupees",
    "strip_tag_prefix",
    "symbol_for",
    "to_order_type",
    "to_product",
    "to_resolution",
    "to_side",
    "to_validity",
]

PRODUCT_CNC = "CNC"
PRODUCT_INTRADAY = "INTRADAY"
PRODUCT_MARGIN = "MARGIN"
PRODUCT_MTF = "MTF"

ORDER_TYPE_LIMIT = 1
ORDER_TYPE_MARKET = 2
ORDER_TYPE_STOP = 3  # SL-M: a stop that becomes a market order
ORDER_TYPE_STOP_LIMIT = 4  # SL-L: a stop with a limit price

SIDE_BUY = 1
SIDE_SELL = -1

VALIDITY_DAY = "DAY"
VALIDITY_IOC = "IOC"

STATUS_CANCELLED = 1
STATUS_TRADED = 2
STATUS_TRANSIT = 4
STATUS_REJECTED = 5
STATUS_PENDING = 6
STATUS_EXPIRED = 7

GTT_SINGLE = 1
GTT_OCO = 2

RESPONSE_OK = "ok"
RESPONSE_ERROR = "error"

SEGMENT_CAPITAL_MARKET = 10
SEGMENT_EQUITY_DERIV = 11
SEGMENT_CURRENCY_DERIV = 12
SEGMENT_COMMODITY = 20

DEFAULT_SERIES = "EQ"
"""The NSE series appended to a bare cash-equity symbol.

FYERS addresses cash equity as ``NSE:SBIN-EQ`` and the domain as ``(NSE, SBIN)``.
EQ is the rolling-settlement series nearly every listed company trades in; a
symbol in another series carries it in the domain symbol, as ``MODIRUBBER-BE``,
and is passed through untouched.
"""

IST = dt.timezone(dt.timedelta(hours=5, minutes=30), "IST")
"""The zone every FYERS timestamp is expressed in."""

_PRODUCT_BY_DOMAIN = {
    Product.CNC: PRODUCT_CNC,
    Product.MIS: PRODUCT_INTRADAY,
    Product.NRML: PRODUCT_MARGIN,
}

_PRODUCT_BY_WIRE = {
    PRODUCT_CNC: Product.CNC,
    PRODUCT_INTRADAY: Product.MIS,
    PRODUCT_MARGIN: Product.NRML,
}

_ORDER_TYPE_BY_DOMAIN = {
    OrderType.MARKET: ORDER_TYPE_MARKET,
    OrderType.LIMIT: ORDER_TYPE_LIMIT,
    OrderType.STOP: ORDER_TYPE_STOP,
    OrderType.STOP_LIMIT: ORDER_TYPE_STOP_LIMIT,
}

_ORDER_TYPE_BY_WIRE = {wire: domain for domain, wire in _ORDER_TYPE_BY_DOMAIN.items()}

_STATUS_BY_WIRE = {
    STATUS_TRADED: OrderStatus.COMPLETE,
    STATUS_REJECTED: OrderStatus.REJECTED,
    STATUS_CANCELLED: OrderStatus.CANCELLED,
    STATUS_EXPIRED: OrderStatus.CANCELLED,
    STATUS_PENDING: OrderStatus.OPEN,
}

_RESOLUTION_BY_TIMEFRAME = {
    Timeframe.M1: "1",
    Timeframe.M3: "3",
    Timeframe.M5: "5",
    Timeframe.M15: "15",
    Timeframe.M30: "30",
    Timeframe.M60: "60",
    Timeframe.D1: "D",
    Timeframe.W1: "1W",
}


def symbol_for(key: InstrumentKey) -> str:
    """Render an instrument in FYERS's ``EX:SYMBOL-SERIES`` form.

    Unlike Upstox, FYERS addresses instruments by a symbol the adapter can
    derive: the exchange, the trading symbol, and for cash equity a series. That
    is why this adapter needs no key-resolver argument. A symbol already
    carrying a colon is taken to be in FYERS's form and passed through, so a
    caller holding keys in FYERS's own vocabulary is not forced to round-trip
    them.

    Derivatives on NSE trade on FYERS under the NSE prefix rather than a
    separate NFO one, so the domain's NFO and CDS exchanges map to NSE and BFO
    to BSE.
    """
    symbol = key.symbol.strip()
    if ":" in symbol:
        return symbol
    exchange = key.exchange.strip().upper()
    if exchange in ("NSE", "BSE"):
        if "-" in symbol:
            return f"{exchange}:{symbol}"
        return f"{exchange}:{symbol}-{DEFAULT_SERIES}"
    if exchange in ("NFO", "CDS"):
        return f"NSE:{symbol}"
    if exchange in ("BFO", "BCD"):
        return f"BSE:{symbol}"
    return f"{exchange}:{symbol}"


def key_for(symbol: str, segment: int = 0) -> InstrumentKey:
    """Reconstruct an instrument key from a FYERS symbol.

    The segment code, when a response carries one, decides between cash and
    derivatives; when it is absent (zero) the symbol's shape decides, because a
    cash symbol always carries a ``-SERIES`` suffix and a derivative never does.
    The EQ series is stripped so the key round-trips through :func:`symbol_for`;
    any other series is kept in the symbol, since dropping it would make
    ``SBIN-BE`` and ``SBIN-EQ`` the same instrument.
    """
    symbol = symbol.strip()
    exchange, sep, rest = symbol.partition(":")
    if not sep:
        return InstrumentKey(exchange="", symbol=symbol)
    exchange = exchange.upper()

    base, dash, series = rest.partition("-")
    has_series = bool(dash)
    # "-INDEX" is a suffix FYERS puts on index quotes; it is not a series and
    # an index is not a cash equity.
    if has_series and series.upper() == "INDEX":
        return InstrumentKey(exchange=exchange, symbol=rest)

    cash = has_series
    if segment == SEGMENT_CAPITAL_MARKET:
        cash = True
    elif segment in (SEGMENT_EQUITY_DERIV, SEGMENT_CURRENCY_DERIV, SEGMENT_COMMODITY):
        cash = False

    if cash:
        if has_series and series.upper() == DEFAULT_SERIES:
            return InstrumentKey(exchange=exchange, symbol=base)
        return InstrumentKey(exchange=exchange, symbol=rest)

    if exchange == "NSE":
        return InstrumentKey(exchange="CDS" if segment == SEGMENT_CURRENCY_DERIV else "NFO", symbol=rest)
    if exchange == "BSE":
        return InstrumentKey(exchange="BCD" if segment == SEGMENT_CURRENCY_DERIV else "BFO", symbol=rest)
    return InstrumentKey(exchange=exchange, symbol=rest)


def rupees(amount: Money) -> float:
    """Convert minor units to the whole-currency float FYERS expects."""
    return amount / 100


def paise(value: float | str | None) -> Money:
    """Convert a rupee amount from a response to minor units.

    Rounds half away from zero through ``core.money``'s own rule rather than a
    second copy of it, which is what keeps this conversion and every other one
    in the library agreeing. A missing or malformed value becomes zero rather
    than raising: one malformed price in a 50-instrument response must not fail
    the batch.
    """
    if value is None or value == "":
        return Money(0)
    try:
        number = float(value)
    except (TypeError, ValueError):
        return Money(0)
    if number != number or number in (float("inf"), float("-inf")):
        return Money(0)
    return Money(_round_half_away(number * 100))


def to_product(product: Product) -> str:
    """Map a domain product to FYERS's code.

    FYERS keeps the three Indian buckets distinct, so unlike Upstox nothing is
    collapsed. Margin is the US margin-account bucket and has no FYERS
    equivalent, so it raises rather than being silently substituted -- that
    would change a real order's settlement.

    Raises:
        ValueError: For a product FYERS cannot express.

    """
    try:
        return _PRODUCT_BY_DOMAIN[Product(product)]
    except (KeyError, ValueError):
        raise ValueError(f"fyers: no FYERS product for {product!r}") from None


def from_product(code: str) -> Product:
    """Map FYERS's product code back to the domain.

    An unrecognised code -- MTF, which the domain does not name, or an absent
    field -- becomes NRML rather than raising. Go can hold an unknown value in
    its string-typed ``Product``; a Python :class:`~enum.StrEnum` cannot, and
    raising here would lose a whole order book to one row with a field this
    adapter has not seen. NRML is the safer guess: it never claims a
    margin-funded position is an unencumbered delivery holding. The product is
    descriptive on a read-back order, while the status beside it is what a
    reconciler acts on.
    """
    return _PRODUCT_BY_WIRE.get(code.strip().upper(), Product.NRML)


def to_order_type(order_type: OrderType) -> int:
    """Map a domain order type to FYERS's integer code.

    FYERS's 3 is the stop that becomes a market order and 4 the stop with a
    limit price, so tradekit's STOP maps to 3 and STOP_LIMIT to 4.

    Raises:
        ValueError: For an order type FYERS cannot express.

    """
    try:
        return _ORDER_TYPE_BY_DOMAIN[OrderType(order_type)]
    except (KeyError, ValueError):
        raise ValueError(f"fyers: no FYERS order type for {order_type!r}") from None


def from_order_type(code: int) -> OrderType:
    """Map FYERS's integer code back to the domain.

    An unrecognised code becomes MARKET rather than raising, for the same
    reason as :func:`from_product`: this is a descriptive field on an order
    read back from the broker, and refusing to decode the row would discard the
    status, which is the part a reconciler acts on. Do not read the result as a
    reason to place a market order -- an order being placed goes through
    :func:`to_order_type`, which raises.
    """
    return _ORDER_TYPE_BY_WIRE.get(code, OrderType.MARKET)


def to_validity(tif: TimeInForce | str) -> str:
    """Map a domain time-in-force to FYERS's validity.

    GTT is not a validity at FYERS any more than it is at Kite or Upstox: it is
    a separate trigger API reached through the ``ProtectiveOrders`` capability.
    Asking for it here is a wiring mistake worth catching rather than quietly
    downgrading to DAY, which would leave a position with no resting stop.

    Raises:
        ValueError: For GTT, or a validity FYERS cannot express.

    """
    if tif in (TimeInForce.DAY, ""):
        return VALIDITY_DAY
    if tif == TimeInForce.IOC:
        return VALIDITY_IOC
    if tif == TimeInForce.GTT:
        raise ValueError("fyers: GTT is a separate trigger API, not an order validity; use place_protective")
    raise ValueError(f"fyers: no FYERS validity for {tif!r}")


def from_validity(code: str) -> TimeInForce:
    """Map FYERS's validity back to the domain, defaulting to DAY.

    DAY is what FYERS applies when it is not told otherwise, and what keeps a
    broker order round-tripping through :func:`to_validity`.
    """
    return TimeInForce.IOC if code.strip().upper() == VALIDITY_IOC else TimeInForce.DAY


def to_side(side: Side) -> int:
    """Map a side to FYERS's signed integer."""
    return SIDE_SELL if side == Side.SELL else SIDE_BUY


def from_side(code: int) -> Side:
    """Map FYERS's signed integer back to a side.

    Anything non-negative is a buy: FYERS also uses 0 for a closed position,
    and a flat position's side is never read by anything that would act on it.
    """
    return Side.SELL if code < 0 else Side.BUY


def normalize_status(code: int) -> OrderStatus:
    """Map FYERS's order status code onto the domain's.

    FYERS's 6 ("Pending") is an order resting at the exchange, which is the
    domain's OPEN; its 4 ("Transit") is one the exchange has not yet
    acknowledged, which is the domain's PENDING. The two are easy to swap and
    the swap is silent, so the names are written out here. 7 ("Expired") is an
    IOC or day order the exchange let lapse; it ends the order as surely as a
    cancel does and maps to CANCELLED rather than to something a reconciler
    would keep polling. An unrecognised code -- 3 is documented as "for future
    use" -- becomes PENDING: a status the adapter does not understand must never
    read as terminal, or a reconciler abandons an order that is still live.
    """
    return _STATUS_BY_WIRE.get(code, OrderStatus.PENDING)


def to_resolution(timeframe: Timeframe) -> str:
    """Map a timeframe to FYERS's resolution string.

    Intraday resolutions are the bar length in minutes as a bare number; daily
    is ``D`` and weekly ``1W``.

    Raises:
        ValueError: For a timeframe FYERS cannot serve.

    """
    try:
        return _RESOLUTION_BY_TIMEFRAME[Timeframe(timeframe)]
    except (KeyError, ValueError):
        raise ValueError(f"fyers: no FYERS resolution for timeframe {timeframe!r}") from None


def chunk_days(timeframe: Timeframe) -> int:
    """The largest span, in days, the history endpoint is asked for at once.

    FYERS documents 100 days per request for intraday resolutions and 366 for
    daily and above. As in the other adapters the values sit under the ceiling
    rather than at it: requesting exactly the documented maximum fails
    intermittently around boundaries, and a sync that dies two years into a
    backfill is worse than one that makes a few more requests.
    """
    return 90 if Timeframe(timeframe).intraday else 360


def parse_order_time(raw: str) -> dt.datetime | None:
    """Read the order book's ``DD-Mon-YYYY hh:mm:ss`` timestamp, which is IST.

    A value that will not parse returns ``None`` rather than the current time:
    a fabricated timestamp is indistinguishable from a real one and would
    silently reorder a time-sorted book.
    """
    raw = raw.strip()
    if not raw:
        return None
    for layout in ("%d-%b-%Y %H:%M:%S", "%Y-%m-%d %H:%M:%S"):
        try:
            return dt.datetime.strptime(raw, layout).replace(tzinfo=IST)
        except ValueError:
            continue
    return None


def strip_tag_prefix(tag: str) -> str:
    """Remove the ``1:`` FYERS prepends to a caller's tag.

    An order then round-trips with the tag the strategy gave it. A ``2:``
    prefix marks a tag FYERS generated itself (``2:Untagged``); it becomes an
    empty tag, because the strategy never set one.
    """
    if tag.startswith("1:"):
        return tag[2:]
    if tag.startswith("2:"):
        return ""
    return tag
