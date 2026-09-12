"""The Upstox adapter, mirroring ``go/upstox``.

Replaces the Upstox clients in ``breakout500``, ``fanse`` and
``breakoutscreener``. The client satisfies :class:`tradekit.core.ports.Broker`,
and separately ``Quoter``, ``HistoryProvider``, ``ProtectiveOrders``,
``MarginEstimator``, ``InstrumentSource`` and ``TokenState``::

    from tradekit.upstox import UpstoxClient

    client = UpstoxClient(
        api_key=key,
        access_token=token,
        token_issued_at=issued_at,
        instrument_key=lambda k: db.broker_id(k, "upstox"),
    )

Upstox addresses instruments by ``SEGMENT|ID`` -- an ISIN for cash equity, an
exchange token for derivatives -- which the adapter cannot derive from an
exchange and a symbol, so ``instrument_key`` is required.

Requires the ``upstox`` extra (``httpx``).
"""

from .client import BASE_URL, BASE_URL_V3, INSTRUMENTS_URL, UpstoxClient, parse_instrument_csv
from .holidays import DEFAULT_HOLIDAYS_URL, Holidays, parse_holidays
from .transport import (
    APIError,
    RateLimitedError,
    RejectedError,
    TokenBucket,
    TokenExpiredError,
    TransientError,
    Transport,
)

__all__ = [
    "BASE_URL",
    "BASE_URL_V3",
    "DEFAULT_HOLIDAYS_URL",
    "INSTRUMENTS_URL",
    "APIError",
    "Holidays",
    "RateLimitedError",
    "RejectedError",
    "TokenBucket",
    "TokenExpiredError",
    "TransientError",
    "Transport",
    "UpstoxClient",
    "parse_holidays",
    "parse_instrument_csv",
]
