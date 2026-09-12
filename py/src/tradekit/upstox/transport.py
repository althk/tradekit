"""Throttled, retrying HTTP transport for the Upstox REST API.

Mirrors ``go/upstox/{errors,limiter,transport}.go``. The token bucket is
breakout500's, the ``Retry-After`` handling is fanse's, and the idempotency rule
is dhaara's: order mutations are never retried, because retrying a POST that
placed an order places a second one.
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

RATE_LIMIT_CODE = "UDAPI10005"
"""Upstox's rate-limit error code.

Checked in the body as well as against the status, because Upstox sometimes
returns it under a 2xx -- and a rate limit read as success is a caller that
carries on hammering the endpoint with an empty result set.
"""

DEFAULT_ATTEMPTS = 4
DEFAULT_RETRY_BASE = 2.0
MAX_RETRY_WAIT = 60.0


class APIError(RuntimeError):
    """A refusal from Upstox, carrying enough of the response to diagnose it."""

    def __init__(self, status: int, code: str, message: str, body: str = "") -> None:
        """Record the status, Upstox's own error code, its description, and the raw body.

        The body is kept because Upstox's structured fields are sometimes
        empty (``UDAPI100038`` names no ``propertyPath``) and the raw text is
        then the only clue an operator gets.
        """
        super().__init__(f"upstox: HTTP {status}: {code}: {message}" if code else f"upstox: HTTP {status}: {message}")
        self.status = status
        self.code = code
        self.message = message
        self.body = body or message

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
    """Transport, Upstox's infrastructure, or a rate limit. Worth repeating."""

    @property
    def retryable(self) -> bool:
        """Always true: that is what makes this class the transient one."""
        return True


class RateLimitedError(TransientError):
    """Upstox saying "ask again later", by status or by error code."""


class RejectedError(APIError):
    """Upstox refused the request on its merits.

    Repeating it unchanged will be refused again, so it is deliberately not
    retryable. An unrecognised refusal lands here rather than in
    :class:`TransientError`: the cost of not retrying a transient failure is one
    missed call, and the cost of retrying a rejection is the same bad order sent
    repeatedly.
    """


class TokenBucket:
    """Thread-safe client-side throttle.

    A burst of ``capacity`` then a steady ``rate`` per second. It cannot prevent
    a rate limit on a bulk job -- the server's binding window is longer than any
    client can see -- only keep the burst from provoking one immediately.
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


def _api_error(body: str) -> tuple[str, str]:
    """Extract the code and message from Upstox's error envelope."""
    try:
        payload = json.loads(body)
    except (ValueError, TypeError):
        return "", ""
    errors = payload.get("errors") if isinstance(payload, dict) else None
    if not isinstance(errors, list) or not errors or not isinstance(errors[0], dict):
        return "", ""
    return str(errors[0].get("errorCode") or ""), str(errors[0].get("message") or "")


def _is_rate_limited(code: str, body: str) -> bool:
    """Whether a response is Upstox saying "ask again later".

    The envelope code is compared exactly and the body only for the quoted form,
    because a prefix match is wrong here: UDAPI100050 is "invalid token", and
    reading it as UDAPI10005 would classify an expired token as retryable -- so
    a scheduler would spend its whole retry budget re-sending calls that cannot
    succeed instead of logging in again.
    """
    return code.upper() == RATE_LIMIT_CODE or f'"{RATE_LIMIT_CODE}"' in body


def _classify(status: int, code: str, message: str, body: str) -> APIError:
    """Build the error class the status and body call for."""
    if _is_rate_limited(code, body):
        return RateLimitedError(status, code, message, body)
    if status == 401:
        return TokenExpiredError(status, code, message, body)
    if status == 429 or status >= 500:
        return TransientError(status, code, message, body)
    return RejectedError(status, code, message, body)


class Transport:
    """Issues requests, throttles them, retries the ones worth retrying."""

    def __init__(
        self,
        *,
        token_provider: Callable[[], str],
        base_url: str,
        base_url_v3: str,
        client: httpx.Client | None = None,
        rate_per_sec: float = 10.0,
        burst: float = 10.0,
        timeout: float = 30.0,
        attempts: int = DEFAULT_ATTEMPTS,
        retry_base: float = DEFAULT_RETRY_BASE,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        """Build a transport.

        Args:
            token_provider: Returns the current access token, called per
                request so a token replaced mid-session is picked up.
            base_url: The v2 API root.
            base_url_v3: The v3 API root. The split is real: order placement and
                GTT are v3 while order details and quotes are v2.
            client: Pre-built httpx client. Tests inject a MockTransport.
            rate_per_sec: Throttle refill rate.
            burst: Throttle bucket size.
            timeout: Per-request timeout, when no client is supplied.
            attempts: How many times a retryable refusal is sent. 1 disables
                retrying.
            retry_base: First backoff, doubled per attempt and overridden by a
                ``Retry-After`` header.
            sleep: Injected so tests do not actually wait.

        """
        self._token_provider = token_provider
        self.base_url = base_url.rstrip("/")
        self.base_url_v3 = base_url_v3.rstrip("/")
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
        json_body: dict[str, Any] | None = None,
        retry: bool = False,
    ) -> dict[str, Any]:
        """Issue a request and return the decoded JSON body.

        Args:
            method: HTTP method.
            path: Path below ``base``.
            base: API root, defaulting to the v2 one.
            params: Query parameters.
            json_body: JSON request body.
            retry: Whether a transient refusal may be re-sent. False for every
                order mutation: retrying a POST that placed an order places a
                second one, and a transport error gives no way to tell which
                happened. It is explicit rather than derived from the method,
                because Upstox cancels an order with DELETE and modifies one
                with PUT.

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
                last = TransientError(0, "", str(exc))
                if attempt == attempts:
                    raise last from exc
                self._sleep(self._backoff(None, attempt))
                continue

            body = response.text
            code, message = _api_error(body)
            if response.status_code < 400 and not _is_rate_limited(code, body):
                try:
                    payload = response.json()
                except ValueError as exc:
                    # A body that will not decode is Upstox answering something
                    # other than what it documents. The same malformed answer
                    # will arrive again, so it is not retryable.
                    raise RejectedError(response.status_code, "", f"decoding {path}: {exc}") from exc
                return payload if isinstance(payload, dict) else {}

            last = _classify(response.status_code, code, message or body[:300], body)
            if attempt == attempts or not last.retryable:
                raise last
            delay = self._backoff(response, attempt)
            log.warning(
                "upstox %s %s returned %s - retrying in %.1fs (attempt %d/%d)",
                method,
                path,
                response.status_code,
                delay,
                attempt,
                attempts,
            )
            self._sleep(delay)

        assert last is not None
        raise last

    def _headers(self) -> dict[str, str]:
        """Build request headers with the current bearer token."""
        headers = {"accept": "application/json"}
        token = self._token_provider()
        if token:
            headers["Authorization"] = f"Bearer {token}"
        return headers

    def _backoff(self, response: httpx.Response | None, attempt: int) -> float:
        """Seconds to wait before the next attempt.

        The server's ``Retry-After`` wins over the client's schedule, because it
        knows when its window resets and the client does not -- but it is
        capped, so an absurd value from a confused proxy cannot stall a sync for
        the rest of the day.
        """
        if response is not None:
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
