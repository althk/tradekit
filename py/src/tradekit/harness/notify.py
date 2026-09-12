"""Notifications that cannot raise into the trading path and cannot block it.

Mirrors ``go/harness/notify.go``. Only two donors had a notifier, which is
under the design's own bar for extraction; it earns its place for the one
property both learned the hard way: a notifier that blocks or fails must not
stop trading. Telegram rate-limits, and one donor's notifier was called from
the execution path.

Threads and a bounded :class:`queue.Queue`, not :mod:`asyncio`: the donor
projects are threaded and synchronous, and a library that requires an event
loop to send a message gets worked around with a blocking ``requests.post`` at
the call site -- which is the call this module exists to prevent.
"""

from __future__ import annotations

import contextlib
import enum
import json
import logging
import queue
import threading
import time
import urllib.request
from collections.abc import Callable
from typing import Protocol, runtime_checkable

from .config import REDACTION, Secret

__all__ = ["Discard", "Level", "Multi", "Notifier", "Telegram"]

log = logging.getLogger("tradekit.harness.notify")

TELEGRAM_API = "https://api.telegram.org"


class Level(enum.IntEnum):
    """How urgent a notification is: routine, needs a look, needs a human now."""

    INFO = 0
    WARN = 1
    ALERT = 2


@runtime_checkable
class Notifier(Protocol):
    """Sends a message.

    Implementations must not block the caller and must not raise. Failures are
    logged and counted; a caller who wants to know can read the counter.
    """

    def notify(self, level: Level, msg: str) -> None:  # noqa: D102 - Protocol stub
        ...


class Discard:
    """Drops everything: the default for backtests and tests."""

    def notify(self, level: Level, msg: str) -> None:
        """Do nothing."""


class Multi:
    """Fans out to several notifiers."""

    def __init__(self, *notifiers: Notifier) -> None:
        """Deliver to each of ``notifiers`` in turn."""
        self.notifiers = list(notifiers)

    def notify(self, level: Level, msg: str) -> None:
        """Deliver to every notifier. Each is non-blocking by contract, so the fan-out is too."""
        for n in self.notifiers:
            n.notify(level, msg)


Transport = Callable[[str, bytes], None]
"""Posts a JSON body to a URL, raising on failure. Injectable for tests."""


def _urllib_transport(timeout: float) -> Transport:
    def post(url: str, body: bytes) -> None:
        req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if resp.status != 200:
                raise RuntimeError(f"telegram returned HTTP {resp.status}")

    return post


class Telegram:
    """Posts to a chat from a background thread with a bounded queue.

    The queue drops the oldest message on overflow: an unbounded queue in an
    alert storm is a memory leak that ends the process during exactly the
    incident the alerts were about. The newest message is the one describing
    the current state, which is what a human catching up needs.
    """

    def __init__(
        self,
        token: Secret,
        chat_id: str,
        *,
        queue_size: int = 256,
        rate_per_second: float = 1.0,
        base_url: str = TELEGRAM_API,
        transport: Transport | None = None,
        timeout: float = 10.0,
    ) -> None:
        """Start the delivery thread.

        ``rate_per_second`` paces sends; Telegram allows about one message a
        second to a single chat. ``transport`` replaces the HTTP call for tests.
        """
        if not token or not chat_id:
            raise ValueError("harness: Telegram needs a bot token and a chat id")
        self._token = token
        self._chat_id = chat_id
        self._base_url = base_url.rstrip("/")
        self._transport = transport or _urllib_transport(timeout)
        self._interval = 1.0 / rate_per_second if rate_per_second > 0 else 0.0
        self._queue: queue.Queue[tuple[Level, str] | None] = queue.Queue(maxsize=max(1, queue_size))
        self._lock = threading.Lock()
        self._closed = False
        self.dropped = 0
        self.failed = 0
        self.sent = 0
        self._thread = threading.Thread(target=self._run, name="tradekit-telegram", daemon=True)
        self._thread.start()

    def notify(self, level: Level, msg: str) -> None:
        """Queue a message and return at once."""
        item = (level, msg)
        with self._lock:
            if self._closed:
                return
            try:
                self._queue.put_nowait(item)
                return
            except queue.Full:
                pass
            try:
                self._queue.get_nowait()
                self.dropped += 1
            except queue.Empty:
                pass
            try:
                self._queue.put_nowait(item)
            except queue.Full:
                self.dropped += 1
        log.warning("harness: notification queue full, dropped oldest (dropped_total=%d)", self.dropped)

    def close(self, timeout: float = 5.0) -> None:
        """Drain pending messages within ``timeout`` seconds, then stop the thread. Idempotent.

        Nothing is registered with :mod:`atexit`: a daemon thread plus an
        explicit close is predictable, and an atexit hook in a library fires
        during interpreter shutdown when the HTTP stack may already be gone.
        """
        with self._lock:
            if self._closed:
                return
            self._closed = True
        # The sentinel is queued behind whatever is pending, so the thread
        # delivers the backlog before it sees the stop.
        deadline = time.monotonic() + timeout
        with contextlib.suppress(queue.Full):
            self._queue.put(None, timeout=timeout)
        self._thread.join(max(0.0, deadline - time.monotonic()))
        if self._thread.is_alive():
            log.warning("harness: Telegram closed with %d notifications undelivered", self._queue.qsize())

    def _run(self) -> None:
        last = 0.0
        while True:
            item = self._queue.get()
            if item is None:
                return
            if self._interval > 0:
                wait = last + self._interval - time.monotonic()
                if wait > 0:
                    time.sleep(wait)
            last = time.monotonic()
            level, msg = item
            try:
                self._send(level, msg)
                self.sent += 1
            except Exception as exc:  # the whole point is to contain it
                self.failed += 1
                log.warning("harness: notification failed: %s", _redact(str(exc), self._token))

    def _send(self, level: Level, msg: str) -> None:
        text = msg
        if level == Level.WARN:
            text = "[WARN] " + msg
        elif level == Level.ALERT:
            text = "[ALERT] " + msg
        body = json.dumps({"chat_id": self._chat_id, "text": text}).encode("utf-8")
        self._transport(f"{self._base_url}/bot{self._token.reveal()}/sendMessage", body)


def _redact(text: str, token: Secret) -> str:
    """Mask the bot token wherever it appears, because HTTP errors quote the URL."""
    return text.replace(token.reveal(), REDACTION)
