"""Translation between tradekit's domain and Upstox's wire vocabulary.

This module mirrors ``go/upstox/mapping.go`` exactly. Every function here has a
counterpart there with the same name in Go's casing, and the shared cases are
pinned by ``contracts/testdata/parity.json`` under the ``upstox`` key, so the
two clients cannot drift.

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
    "SEGMENT_BSE_EQUITY",
    "SEGMENT_BSE_INDEX",
    "SEGMENT_NSE_EQUITY",
    "SEGMENT_NSE_FO",
    "SEGMENT_NSE_INDEX",
    "chunk_days",
    "exchange_for",
    "from_order_type",
    "from_product",
    "from_transaction_type",
    "from_validity",
    "instrument_key",
    "normalize_status",
    "paise",
    "parse_instrument_key",
    "parse_upstox_time",
    "rupees",
    "segment_for",
    "to_interval",
    "to_order_type",
    "to_product",
    "to_transaction_type",
    "to_validity",
]

# Upstox's wire vocabulary. Unlike the Kite adapter's constants these are not
# restated from an SDK -- there is none -- so there is nothing for a
# constants test to pin them against. They are traced instead to donor code
# known to work against the live API; go/upstox/upstox-api.txt records the file
# and line for each.
PRODUCT_INTRADAY = "I"
PRODUCT_DELIVERY = "D"

ORDER_TYPE_MARKET = "MARKET"
ORDER_TYPE_LIMIT = "LIMIT"
ORDER_TYPE_SL = "SL"
ORDER_TYPE_SLM = "SL-M"

VALIDITY_DAY = "DAY"
VALIDITY_IOC = "IOC"

TRANSACTION_TYPE_BUY = "BUY"
TRANSACTION_TYPE_SELL = "SELL"

TRIGGER_BELOW = "BELOW"
TRIGGER_ABOVE = "ABOVE"

GTT_SINGLE = "SINGLE"
GTT_MULTIPLE = "MULTIPLE"

RULE_ENTRY = "ENTRY"
RULE_TARGET = "TARGET"
RULE_STOPLOSS = "STOPLOSS"

SEGMENT_NSE_EQUITY = "NSE_EQ"
SEGMENT_BSE_EQUITY = "BSE_EQ"
SEGMENT_NSE_FO = "NSE_FO"
SEGMENT_NSE_INDEX = "NSE_INDEX"
SEGMENT_BSE_INDEX = "BSE_INDEX"

_SEGMENT_BY_EXCHANGE = {
    "NSE": SEGMENT_NSE_EQUITY,
    "BSE": SEGMENT_BSE_EQUITY,
    "NFO": SEGMENT_NSE_FO,
    "INDICES": SEGMENT_NSE_INDEX,
    "NSE_INDICES": SEGMENT_NSE_INDEX,
}

_EXCHANGE_BY_SEGMENT = {
    SEGMENT_NSE_EQUITY: "NSE",
    SEGMENT_NSE_INDEX: "NSE",
    SEGMENT_BSE_EQUITY: "BSE",
    SEGMENT_BSE_INDEX: "BSE",
    SEGMENT_NSE_FO: "NFO",
}

_STATUS_BY_WIRE = {
    "complete": OrderStatus.COMPLETE,
    "completed": OrderStatus.COMPLETE,
    "rejected": OrderStatus.REJECTED,
    "cancelled": OrderStatus.CANCELLED,
    "canceled": OrderStatus.CANCELLED,
    "open": OrderStatus.OPEN,
    "open pending": OrderStatus.OPEN,
    "modify pending": OrderStatus.OPEN,
    "put order req received": OrderStatus.OPEN,
    "validation pending": OrderStatus.OPEN,
    "trigger pending": OrderStatus.TRIGGERED,
}

_INTERVAL_BY_TIMEFRAME: dict[Timeframe, tuple[str, int]] = {
    Timeframe.M1: ("minutes", 1),
    Timeframe.M3: ("minutes", 3),
    Timeframe.M5: ("minutes", 5),
    Timeframe.M15: ("minutes", 15),
    Timeframe.M30: ("minutes", 30),
    Timeframe.M60: ("hours", 1),
    Timeframe.D1: ("days", 1),
    Timeframe.W1: ("weeks", 1),
}


def segment_for(exchange: str) -> str:
    """The segment prefix of an instrument key for an exchange.

    An exchange that already looks like a segment (``"NSE_EQ"``) passes through,
    so a caller holding keys in Upstox's own vocabulary is not forced to
    round-trip them through a name this function happens not to know.
    """
    up = exchange.strip().upper()
    if "_" in up:
        return up
    return _SEGMENT_BY_EXCHANGE.get(up, up)


def exchange_for(segment: str) -> str:
    """The domain's exchange name for a segment prefix."""
    return _EXCHANGE_BY_SEGMENT.get(segment.strip().upper(), segment)


def instrument_key(key: InstrumentKey, instrument_id: str) -> str:
    """Render an instrument in Upstox's ``SEGMENT|ID`` form.

    The id is an ISIN for cash equity (``NSE_EQ|INE002A01018``) and an exchange
    token for derivatives, so it cannot be derived from an exchange and a
    symbol. This only formats the key once the id is known; resolving it is the
    client's ``instrument_key`` callable, wired to the store.
    """
    return f"{segment_for(key.exchange)}|{instrument_id}"


def parse_instrument_key(raw: str) -> tuple[str, str]:
    """Split ``SEGMENT|ID`` into its parts.

    An expired contract's key carries a third field --
    ``NSE_FO|47983|17-04-2025`` -- and the whole remainder after the first
    separator is returned as the id, so such a key survives the round trip
    instead of being truncated to something the expired-candle endpoint rejects.

    Raises:
        ValueError: If the string is not a well-formed instrument key.

    """
    segment, sep, instrument_id = raw.strip().partition("|")
    if not sep or not segment or not instrument_id:
        raise ValueError(f"upstox: malformed instrument key {raw!r}, want SEGMENT|ID")
    return segment, instrument_id


def rupees(amount: Money) -> float:
    """Convert minor units to the whole-currency float Upstox expects."""
    return amount / 100


def paise(value: float | None) -> Money:
    """Convert a rupee amount from a response to minor units.

    Rounds half away from zero through ``core.money``'s own rule rather than a
    second copy of it, which is what keeps this conversion and every other one
    in the library agreeing. A missing or non-finite value becomes zero rather
    than raising: one malformed price in a 500-instrument response must not
    fail the batch.
    """
    if value is None:
        return Money(0)
    return Money(_round_half_away(float(value) * 100))


def to_product(product: Product) -> str:
    """Map a domain product to Upstox's code.

    Upstox has only two buckets, so CNC and NRML both become ``D``. Margin is
    the US margin-account bucket and has no Upstox equivalent, so it raises
    rather than being silently substituted -- that would change a real order's
    settlement.

    Raises:
        ValueError: For a product Upstox cannot express.

    """
    if product == Product.MIS:
        return PRODUCT_INTRADAY
    if product in (Product.CNC, Product.NRML):
        return PRODUCT_DELIVERY
    raise ValueError(f"upstox: no Upstox product for {product!r}")


def from_product(code: str) -> Product:
    """Map Upstox's product code back to the domain.

    ``D`` becomes NRML rather than CNC. The mapping is genuinely lossy in this
    direction -- Upstox cannot tell the two apart -- and NRML is the safer
    guess: it never claims a carried derivative is a delivery holding.

    An unrecognised or absent code also becomes NRML rather than raising. Go can
    hold an unknown value in its string-typed ``Product``; a Python
    :class:`~enum.StrEnum` cannot, and raising here would lose a whole order
    book to one row with a field this adapter has not seen. The product is
    descriptive on a read-back order, while the status beside it is what a
    reconciler acts on.
    """
    up = code.strip().upper()
    if up == PRODUCT_INTRADAY:
        return Product.MIS
    try:
        return Product(code.strip().lower())
    except ValueError:
        return Product.NRML


def to_order_type(order_type: OrderType) -> str:
    """Map a domain order type to Upstox's.

    As at Kite, SL is the stop with a limit price and SL-M the stop that becomes
    a market order, so ``STOP`` maps to SL-M and ``STOP_LIMIT`` to SL.

    Raises:
        ValueError: For an order type Upstox does not offer.

    """
    mapping = {
        OrderType.MARKET: ORDER_TYPE_MARKET,
        OrderType.LIMIT: ORDER_TYPE_LIMIT,
        OrderType.STOP: ORDER_TYPE_SLM,
        OrderType.STOP_LIMIT: ORDER_TYPE_SL,
    }
    try:
        return mapping[order_type]
    except KeyError:
        raise ValueError(f"upstox: no Upstox order type for {order_type!r}") from None


def from_order_type(code: str) -> OrderType:
    """Map Upstox's order type back to the domain.

    An unrecognised or absent type becomes MARKET rather than raising, for the
    same reason as :func:`from_product`: this is a descriptive field on an order
    read back from the broker, and refusing to decode the row would discard the
    status, which is the part a reconciler acts on. Do not read the result as a
    reason to place a market order -- it describes what the broker reported, and
    an order being placed goes through :func:`to_order_type`, which raises.
    """
    mapping = {
        ORDER_TYPE_MARKET: OrderType.MARKET,
        ORDER_TYPE_LIMIT: OrderType.LIMIT,
        ORDER_TYPE_SLM: OrderType.STOP,
        ORDER_TYPE_SL: OrderType.STOP_LIMIT,
    }
    stripped = code.strip()
    if stripped.upper() in mapping:
        return mapping[stripped.upper()]
    try:
        return OrderType(stripped.lower())
    except ValueError:
        return OrderType.MARKET


def to_validity(tif: TimeInForce | str) -> str:
    """Map a domain time-in-force to Upstox's validity.

    GTT is not a validity at Upstox any more than it is at Kite: it is a
    separate trigger API reached through :class:`ProtectiveOrders`. Asking for
    it here raises rather than quietly downgrading to DAY, which would leave a
    position with no resting stop at all.

    Raises:
        ValueError: For GTT, or a validity Upstox does not offer.

    """
    if tif in (TimeInForce.DAY, ""):
        return VALIDITY_DAY
    if tif == TimeInForce.IOC:
        return VALIDITY_IOC
    if tif == TimeInForce.GTT:
        raise ValueError("upstox: GTT is a separate trigger API, not an order validity; use place_protective")
    raise ValueError(f"upstox: no Upstox validity for {tif!r}")


def from_validity(code: str) -> TimeInForce:
    """Map Upstox's validity back to the domain, defaulting to DAY.

    DAY is what Upstox applies when it is not told otherwise, and defaulting to
    it keeps a broker order round-tripping through :func:`to_validity`.
    """
    if code.strip().upper() == VALIDITY_IOC:
        return TimeInForce.IOC
    return TimeInForce.DAY


def to_transaction_type(side: Side) -> str:
    """Map a side to Upstox's transaction type."""
    return TRANSACTION_TYPE_SELL if side == Side.SELL else TRANSACTION_TYPE_BUY


def from_transaction_type(code: str) -> Side:
    """Map Upstox's transaction type back to a side."""
    return Side.SELL if code.strip().upper() == TRANSACTION_TYPE_SELL else Side.BUY


def normalize_status(status: str) -> OrderStatus:
    """Map Upstox's order status onto the domain's six.

    The vocabulary is dhaara's ``normalizeStatus``, which collapses the several
    "on its way" states onto OPEN. The one deliberate departure is the default:
    dhaara passes an unrecognised status through uppercased, while here it
    becomes PENDING. A status the adapter does not understand must never read as
    terminal, or a reconciler abandons an order that is still live.
    """
    return _STATUS_BY_WIRE.get(status.strip().lower(), OrderStatus.PENDING)


def to_interval(timeframe: Timeframe) -> tuple[str, int]:
    """Map a timeframe to Upstox's v3 unit and multiple.

    The v3 historical path is ``/{unit}/{interval}/``, so a 5-minute bar is
    ``("minutes", 5)`` rather than a single ``"5minute"`` token as at Kite.

    Raises:
        ValueError: For a timeframe Upstox does not serve.

    """
    try:
        return _INTERVAL_BY_TIMEFRAME[timeframe]
    except KeyError:
        raise ValueError(f"upstox: no Upstox interval for timeframe {timeframe!r}") from None


def chunk_days(unit: str, interval: int) -> int:
    """The largest span, in days, one request will serve for a unit.

    breakout500's client records that minute data is refused beyond about a
    month with UDAPI1148 and that callers must chunk. As in the Kite adapter the
    values sit under the ceiling rather than at it: requesting exactly the
    documented maximum fails intermittently around holidays, and a sync that
    dies two years into a backfill is worse than one that makes a few more
    requests.
    """
    lowered = unit.lower()
    if lowered == "minutes":
        return 25 if interval <= 1 else 60
    if lowered == "hours":
        return 180
    return 1800


def parse_upstox_time(raw: str) -> dt.datetime | None:
    """Read the several timestamp forms Upstox uses.

    Quote payloads carry epoch milliseconds as a string; order rows carry a
    local datetime with no offset, which is IST. A value that parses as neither
    returns ``None`` rather than the current time, which dhaara substitutes: a
    fabricated timestamp is indistinguishable from a real one and would
    silently reorder a time-sorted book.
    """
    if not raw:
        return None
    try:
        return dt.datetime.fromtimestamp(int(raw) / 1000, tz=dt.UTC)
    except (ValueError, OverflowError, OSError):
        pass
    try:
        parsed = dt.datetime.fromisoformat(raw)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=IST)
    return parsed


IST = dt.timezone(dt.timedelta(hours=5, minutes=30), "IST")
"""The exchange's own clock, for the order rows that carry no offset."""
