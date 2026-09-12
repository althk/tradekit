"""The FYERS adapter, mirroring ``go/fyers``.

The client satisfies :class:`tradekit.core.ports.Broker`, and separately
``Quoter``, ``HistoryProvider``, ``ProtectiveOrders``, ``MarginEstimator``,
``InstrumentSource`` and ``TokenState``::

    from tradekit.fyers import FyersClient

    client = FyersClient(
        app_id="XXXXXXXXXX-100",
        access_token=token,
        token_issued_at=issued_at,
        tag="breakout500",
    )

FYERS addresses instruments by a symbol the adapter can derive --
``NSE:SBIN-EQ``, ``NSE:NIFTY24JANFUT`` -- so unlike Upstox no key-resolver is
needed. Authentication is ``Authorization: appId:accessToken`` on every
request, not a bearer token.

Requires the ``fyers`` extra (``httpx``).
"""

from .client import BASE_URL, DATA_URL, SYMBOL_MASTER_URL, FyersClient, parse_symbol_master
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
    "DATA_URL",
    "SYMBOL_MASTER_URL",
    "APIError",
    "FyersClient",
    "RateLimitedError",
    "RejectedError",
    "TokenBucket",
    "TokenExpiredError",
    "TransientError",
    "Transport",
    "parse_symbol_master",
]
