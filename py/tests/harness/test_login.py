"""The browser-login callback server: one port, one token, never a hang."""

from __future__ import annotations

import socket
import threading
import urllib.error
import urllib.request
from collections.abc import Mapping

import pytest

from tradekit.core.ports import BrowserLogin
from tradekit.harness import Callback, browser_login


class FakeBroker:
    """A BrowserLogin that accepts ``?code=good`` and refuses the rest."""

    def __init__(self) -> None:  # noqa: D107
        self.callbacks = 0

    def login_url(self) -> str:  # noqa: D102
        return "https://broker.example/login?x=1&y=2"

    def login_callback(self, query: Mapping[str, str]) -> str:  # noqa: D102
        self.callbacks += 1
        if query.get("code") != "good":
            raise RuntimeError("fake: login refused")
        return "token-" + query["code"]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Login:
    """Runs browser_login on a thread against a fresh port and waits until the server answers."""

    def __init__(self, broker: FakeBroker, timeout: float = 10.0) -> None:  # noqa: D107
        port = free_port()
        self.url = f"http://127.0.0.1:{port}/auth/callback"
        self.result: str | None = None
        self.error: BaseException | None = None
        cb = Callback(self.url, listen_addr=("127.0.0.1", port))

        def run() -> None:
            try:
                self.result = browser_login(broker, cb, timeout=timeout)
            except BaseException as exc:  # re-raised by the test
                self.error = exc

        self.thread = threading.Thread(target=run, daemon=True)
        self.thread.start()
        for _ in range(500):
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.1):
                    return
            except OSError:
                threading.Event().wait(0.01)
        pytest.fail(f"callback server never came up on {port}")

    def wait(self) -> str:
        """Block until browser_login returns; re-raise what it raised."""
        self.thread.join(15)
        assert not self.thread.is_alive(), "browser_login must return once a callback succeeds"
        if self.error is not None:
            raise self.error
        assert self.result is not None
        return self.result


def get(url: str) -> tuple[int, str]:
    try:
        with urllib.request.urlopen(url, timeout=5) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


def test_protocol_is_satisfied_by_a_duck_typed_broker() -> None:
    assert isinstance(FakeBroker(), BrowserLogin)


def test_exchanges_the_callback_and_returns_the_token() -> None:
    broker = FakeBroker()
    login = Login(broker)
    status, body = get(login.url + "?code=good")
    assert status == 200 and "Login successful" in body, "a completed login must show success in the browser"
    assert login.wait() == "token-good"


def test_shows_a_refusal_and_keeps_waiting() -> None:
    broker = FakeBroker()
    login = Login(broker)
    status, body = get(login.url + "?status=error&message=cancelled")
    assert status == 400 and "login refused" in body, "a refused login must say so in the browser"
    assert 'href="https://broker.example/login?x=1&amp;y=2"' in body, "the refusal page must link to a retry"
    login.thread.join(0.1)
    assert login.thread.is_alive(), "a refusal must not end the login; the user is looking at the retry link"
    get(login.url + "?code=good")
    assert login.wait() == "token-good"


def test_ignores_callbacks_after_the_first_success() -> None:
    broker = FakeBroker()
    login = Login(broker)
    get(login.url + "?code=good")
    login.wait()
    # The server may already be down; a reload that gets through must not
    # exchange again, and one that does not is equally fine.
    try:
        _, body = get(login.url + "?code=good")
        assert "already completed" in body
    except OSError:
        pass
    assert broker.callbacks == 1, "a second callback must not reach the broker"


def test_rejects_requests_off_the_callback_path() -> None:
    broker = FakeBroker()
    login = Login(broker, timeout=1)
    status, _ = get(login.url.replace("/auth/callback", "/favicon.ico?code=good"))
    assert status == 404, "only the registered path is a callback"
    assert broker.callbacks == 0
    with pytest.raises(TimeoutError):
        login.wait()


def test_gives_up_when_the_timeout_ends() -> None:
    broker = FakeBroker()
    login = Login(broker, timeout=0.3)
    get(login.url + "?code=bad")
    with pytest.raises(TimeoutError, match="login refused"):
        login.wait()


def test_fails_fast_on_a_bad_redirect_url_or_a_busy_port() -> None:
    with pytest.raises(ValueError, match="absolute URL"):
        Callback("/auth/callback")
    with socket.socket() as busy:
        busy.bind(("127.0.0.1", 0))
        busy.listen(1)
        host, port = busy.getsockname()
        with pytest.raises(OSError):
            browser_login(FakeBroker(), Callback(f"http://{host}:{port}/cb", listen_addr=(host, port)), timeout=5)
