"""Notifications: never block, never raise into the trading path."""

from __future__ import annotations

import json
import logging
import threading
import time

import pytest

from tradekit.harness import Discard, Level, Multi, Notifier, Secret, Telegram


class Chat:
    """A scripted transport recording what it was sent, optionally hanging or failing."""

    def __init__(self, *, hang: threading.Event | None = None, fail: bool = False) -> None:  # noqa: D107
        self.texts: list[str] = []
        self.urls: list[str] = []
        self.hang = hang
        self.fail = fail
        self.lock = threading.Lock()

    def __call__(self, url: str, body: bytes) -> None:  # noqa: D102
        if self.hang is not None:
            self.hang.wait()
        if self.fail:
            raise RuntimeError(f"HTTP 429 from {url}")
        with self.lock:
            self.urls.append(url)
            self.texts.append(json.loads(body)["text"])


def _telegram(chat: Chat, **kw: object) -> Telegram:
    return Telegram(Secret("s3cret-token"), "chat", transport=chat, rate_per_second=0, **kw)  # type: ignore[arg-type]


def test_notify_returns_promptly_when_the_transport_hangs() -> None:
    release = threading.Event()
    chat = Chat(hang=release)
    tg = _telegram(chat)
    start = time.monotonic()
    for _ in range(50):
        tg.notify(Level.ALERT, "position open with no stop")
    assert time.monotonic() - start < 0.5, (
        "a notifier that can stall the trading loop is the failure this module exists to prevent"
    )
    tg.close(timeout=0.1)
    release.set()


def test_overflow_drops_oldest_and_counts_it() -> None:
    release = threading.Event()
    chat = Chat(hang=release)
    tg = _telegram(chat, queue_size=3)
    # The first message is taken by the thread and hangs in flight; the next
    # three fill the queue; the last two evict the oldest queued.
    for m in ("m0", "m1", "m2", "m3", "m4", "m5"):
        tg.notify(Level.INFO, m)
        time.sleep(0.01)
    assert tg.dropped == 2
    release.set()
    tg.close(timeout=2.0)
    assert chat.texts == ["m0", "m3", "m4", "m5"], "the newest messages survive"


def test_close_drains_and_is_idempotent() -> None:
    chat = Chat()
    tg = _telegram(chat)
    for _ in range(20):
        tg.notify(Level.WARN, "w")
    tg.close()
    tg.close()
    assert len(chat.texts) == 20 and tg.sent == 20 and tg.failed == 0
    assert chat.texts[0] == "[WARN] w", "a warning is prefixed so the chat shows urgency"
    assert chat.urls[0].endswith("/bots3cret-token/sendMessage")
    tg.notify(Level.INFO, "after close")
    assert len(chat.texts) == 20, "a closed notifier drops silently rather than raising"


def test_a_raising_transport_is_counted_and_redacted(caplog: pytest.LogCaptureFixture) -> None:
    chat = Chat(fail=True)
    tg = _telegram(chat)
    with caplog.at_level(logging.WARNING, logger="tradekit.harness.notify"):
        tg.notify(Level.INFO, "hello")
        tg.close()
    assert tg.failed == 1 and tg.sent == 0
    assert "notification failed" in caplog.text
    assert "s3cret-token" not in caplog.text, "HTTP errors quote the URL, and the URL carries the token"


def test_rate_limit_spaces_sends() -> None:
    chat = Chat()
    tg = Telegram(Secret("t"), "c", transport=chat, rate_per_second=20)
    start = time.monotonic()
    for _ in range(4):
        tg.notify(Level.INFO, "x")
    tg.close()
    assert time.monotonic() - start >= 0.12, "four sends at 20/s take at least three intervals"
    assert len(chat.texts) == 4


def test_discard_and_multi_satisfy_the_protocol() -> None:
    assert isinstance(Discard(), Notifier)
    chat = Chat()
    tg = _telegram(chat)
    multi = Multi(Discard(), tg)
    assert isinstance(multi, Notifier)
    multi.notify(Level.INFO, "fan out")
    tg.close()
    assert chat.texts == ["fan out"]


def test_telegram_needs_credentials() -> None:
    with pytest.raises(ValueError):
        Telegram(Secret(""), "chat")
