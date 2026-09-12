"""Throttled, retrying HTTP transport for the FYERS API v3.

Mirrors ``go/fyers/{errors,limiter,transport}.go``. The token bucket and the
idempotency rule are the Upstox transport's: order mutations are never retried,
because retrying a POST that placed an order places a second one.

What differs from Upstox is the envelope. Every FYERS response carries
``s`` (``ok``/``error``), ``code`` and ``message``, and a refusal can arrive
under HTTP 200 -- an order rejection is ``{"s":"error","code":-99}`` with a
200 status -- so the body is inspected on every response, not only on 4xx.
"""

from __future__ import annotations

import json
import logging
import threading
import time
from collections.abc import Callable
from typing import Any

import httpx

__all__ = [
    "APIError",
    "RateLimitedError",
    "RejectedError",
    "TokenBucket",
    "TokenExpiredError",
    "TransientError",
    "Transport",
]

log = logging.getLogger(__name__)

CODE_TOKEN_EXPIRED = -8
CODE_TOKEN_INVALID = -15
CODE_TOKEN_UNVERIFIABLE = -16
CODE_TOKEN_BAD = -17
CODE_RATE_LIMITED = -429
"""FYERS's application-level codes that change how a refusal is handled.

The token codes matter because FYERS answers an expired token with HTTP 401 on
most endpoints but not all; the body's code is the reliable signal. The
rate-limit code matters for the opposite reason: a rate limit read as a
rejection would stop a sync that only needed to wait.
"""

TOKEN_CODES = frozenset({CODE_TOKEN_EXPIRED, CODE_TOKEN_INVALID, CODE_TOKEN_UNVERIFIABLE, CODE_TOKEN_BAD})

DEFAULT_ATTEMPTS = 4
DEFAULT_RETRY_BASE = 2.0
MAX_RETRY_WAIT = 60.0


class APIError(RuntimeError):
    """A refusal from FYERS, carrying enough of the response to diagnose it."""

    def __init__(self, status: int, code: int, message: str) -> None:
        """Record the status, FYERS's own code, and its description."""
        detail = f"code {code}: {message}" if code else message
        super().__init__(f"fyers: HTTP {status}: {detail}")
        self.status = status
        self.code = code
        self.message = message

    @property
    def retryable(self) -> bool:
        """Whether repeating the call could succeed."""
        return False


class TokenExpiredError(APIError):
    """The access token is no longer valid; the caller must log in again.

    Distinct from :class:`TransientError` because retrying with the same token
    cannot succeed. This is the one refusal whose remedy is a login.
    """


class TransientError(APIError):
    """Transport, FYERS's infrastructure, or a rate limit. Worth repeating."""

    @property
    def retryable(self) -> bool:
        """Always true: that is what makes this class the transient one."""
        return True


class RateLimitedError(TransientError):
    """FYERS saying "ask again later", by status or by envelope code."""


class RejectedError(APIError):
    """FYERS refused the request on its merits.

    Repeating it unchanged will be refused again, so it is deliberately not
    retryable. An unrecognised refusal lands here rather than in
    :class:`TransientError`: the cost of not retrying a transient failure is one
    missed call, and the cost of retrying a rejection is the same bad order sent
    repeatedly.
    """


class TokenBucket:
    """Thread-safe client-side throttle.

    A burst of ``capacity`` then a steady ``rate`` per second. It cannot prevent
    a rate limit on a bulk job -- FYERS's per-minute window is longer than any
    burst -- only keep the burst from provoking one immediately. That matters
    more here than at Upstox: three per-minute breaches block the account for
    the rest of the day.
    """

    def __init__(self, rate: float, capacity: float, sleep: Callable[[float], None] = time.sleep) -> None:
        """Initialise the bucket. A non-positive rate disables throttling."""
        self.rate = rate
        self.capacity = capacity
        self._tokens = capacity
        self._updated = time.monotonic()
        self._lock = threading.Lock()
        self._sleep = sleep

    def acquire(self, tokens: float = 1.0) -> None:
        """Block until ``tokens`` are available, then consume them."""
        if self.rate <= 0:
            return
        while True:
            with self._lock:
                now = time.monotonic()
                self._tokens = min(self.capacity, self._tokens + (now - self._updated) * self.rate)
                self._updated = now
                if self._tokens >= tokens:
                    self._tokens -= tokens
                    return
                wait = (tokens - self._tokens) / self.rate
            self._sleep(min(wait, 1.0))


def _envelope(body: str) -> tuple[str, int, str]:
    """Extract ``s``, ``code`` and ``message`` from a response body.

    A body that is not a JSON object at all yields an empty envelope; the
    history endpoint on an empty range has been seen to answer with a bare
    array.
    """
    try:
        payload = json.loads(body)
    except (ValueError, TypeError):
        return "", 0, ""
    if not isinstance(payload, dict):
        return "", 0, ""
    code = payload.get("code")
    try:
        code = int(code) if code is not None else 0
    except (TypeError, ValueError):
        code = 0
    return str(payload.get("s") or ""), code, str(payload.get("message") or "")


def _refused(s: str, code: int) -> bool:
    """Whether the envelope says the call failed.

    ``s:"error"`` is the documented signal. A negative code with no ``s`` at
    all is treated the same way, because the token-failure responses seen in
    the wild carry the code and message but not always the status field.
    """
    if s.strip().lower() == "error":
        return True
    return not s and code < 0


def _classify(status: int, code: int, message: str) -> APIError:
    """Build the error class the status and envelope code call for.

    The envelope's code is consulted before the status, because FYERS reports
    both and the code is the more specific: a 400 carrying -8 is an expired
    token, not a bad request, and re-sending it would only be refused again.
    """
    if code in TOKEN_CODES or status == 401:
        return TokenExpiredError(status, code, message)
    if code == CODE_RATE_LIMITED or status == 429:
        return RateLimitedError(status, code, message)
    if status >= 500:
        return TransientError(status, code, message)
    return RejectedError(status, code, message)


class Transport:
    """Issues requests, throttles them, retries the ones worth retrying."""

    def __init__(
        self,
        *,
        auth_provider: Callable[[], str],
        base_url: str,
        data_url: str,
        client: httpx.Client | None = None,
        rate_per_sec: float = 8.0,
        burst: float = 8.0,
        timeout: float = 30.0,
        attempts: int = DEFAULT_ATTEMPTS,
        retry_base: float = DEFAULT_RETRY_BASE,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        """Build a transport.

        Args:
            auth_provider: Returns the current ``Authorization`` value --
                ``appId:accessToken``, or an empty string before login -- called
                per request so a token replaced mid-session is picked up.
            base_url: The trading root (``/api/v3``).
            data_url: The market-data root (``/data``). The split is FYERS's:
                quotes and history live on a different root from the account.
            client: Pre-built httpx client. Tests inject a MockTransport.
            rate_per_sec: Throttle refill rate. FYERS allows 10/s; the default
                sits under it.
            burst: Throttle bucket size.
            timeout: Per-request timeout, when no client is supplied.
            attempts: How many times a retryable refusal is sent. 1 disables
                retrying.
            retry_base: First backoff, doubled per attempt and overridden by a
                ``Retry-After`` header.
            sleep: Injected so tests do not actually wait.

        """
        self._auth_provider = auth_provider
        self.base_url = base_url.rstrip("/")
        self.data_url = data_url.rstrip("/")
        self._client = client or httpx.Client(timeout=timeout)
        self._bucket = TokenBucket(rate_per_sec, burst, sleep)
        self._attempts = max(1, attempts)
        self._retry_base = retry_base
        self._sleep = sleep

    def request(
        self,
        method: str,
        path: str,
        *,
        base: str | None = None,
        params: dict[str, Any] | None = None,
        json_body: dict[str, Any] | list[Any] | None = None,
        retry: bool = False,
    ) -> dict[str, Any]:
        """Issue a request and return the decoded JSON body.

        Args:
            method: HTTP method.
            path: Path below ``base``.
            base: API root, defaulting to the trading one.
            params: Query parameters.
            json_body: JSON request body.
            retry: Whether a transient refusal may be re-sent. False for every
                order mutation: retrying a POST that placed an order places a
                second one, and a transport error gives no way to tell which
                happened. It is explicit rather than derived from the method,
                because FYERS cancels an order with DELETE and modifies one
                with PATCH, and neither is safe to repeat either.

        Returns:
            The decoded response body.

        Raises:
            APIError: On a terminal refusal, or a retryable one that survives
                every attempt.

        """
        url = f"{(base or self.base_url).rstrip('/')}{path}"
        attempts = self._attempts if retry else 1
        last: APIError | None = None

        for attempt in range(1, attempts + 1):
            self._bucket.acquire()
            try:
                response = self._client.request(method, url, params=params, json=json_body, headers=self._headers())
            except httpx.HTTPError as exc:
                # A transport failure has no status to classify, and repeating
                # it is legitimate: the connection, not the request, failed.
                last = TransientError(0, 0, str(exc))
                if attempt == attempts:
                    raise last from exc
                self._sleep(self._backoff(None, attempt))
                continue

            body = response.text
            s, code, message = _envelope(body)
            if response.status_code < 400 and not _refused(s, code):
                try:
                    payload = response.json()
                except ValueError as exc:
                    # A body that will not decode is FYERS answering something
                    # other than what it documents. The same malformed answer
                    # will arrive again, so it is not retryable.
                    raise RejectedError(response.status_code, 0, f"decoding {path}: {exc}") from exc
                return payload if isinstance(payload, dict) else {}

            last = _classify(response.status_code, code, message or body[:300])
            if attempt == attempts or not last.retryable:
                raise last
            delay = self._backoff(response, attempt)
            log.warning(
                "fyers %s %s returned %s (code %s) - retrying in %.2fs (attempt %d/%d)",
                method,
                path,
                response.status_code,
                code,
                delay,
                attempt,
                attempts,
            )
            self._sleep(delay)

        assert last is not None
        raise last

    def _headers(self) -> dict[str, str]:
        """Build request headers with the current ``appId:token`` pair.

        The app id travels with every request. It is the one detail a caller
        porting from Kite or Upstox would get wrong: a bare bearer token is
        answered with "invalid token" (-15) rather than anything that names
        the missing part.
        """
        headers = {"accept": "application/json"}
        auth = self._auth_provider()
        if auth:
            headers["Authorization"] = auth
        return headers

    def _backoff(self, response: httpx.Response | None, attempt: int) -> float:
        """Seconds to wait before the next attempt.

        The server's own wait wins over the client's schedule, because it knows
        when its window resets and the client does not -- but it is capped, so
        an absurd value from a confused proxy cannot stall a sync for the rest
        of the day. FYERS sends both ``Retry-After`` in seconds and
        ``X-Retry-After-Ms``; the millisecond header is preferred because it is
        the precise one.
        """
        if response is not None:
            millis = response.headers.get("X-Retry-After-Ms")
            if millis:
                try:
                    ms = int(millis)
                except ValueError:
                    ms = 0
                if ms > 0:
                    return min(ms / 1000.0, MAX_RETRY_WAIT)
            header = response.headers.get("Retry-After")
            if header:
                try:
                    seconds = float(header)
                except ValueError:
                    seconds = 0.0
                if seconds > 0:
                    return min(seconds, MAX_RETRY_WAIT)
        return min(self._retry_base * (2 ** (attempt - 1)), MAX_RETRY_WAIT)

    def close(self) -> None:
        """Close the underlying HTTP client."""
        self._client.close()
