"""Session reuse: a stored token the venue accepts, or the browser once."""

from __future__ import annotations

import datetime as dt
import threading
import urllib.request

import pytest

from tradekit.core import money, ports
from tradekit.core.domain import Account
from tradekit.harness import Callback, Session, ensure_session
from tradekit.store import Database, connect

from .test_login import FakeBroker, free_port


class SessionFake(FakeBroker):
    """A FakeBroker that also holds a token.

    ``account`` accepts the token named by ``accepts`` and reports every other
    one as expired, unless ``account_error`` is set, which it raises as is.
    """

    def __init__(self, accepts: str, account_error: Exception | None = None) -> None:  # noqa: D107
        super().__init__()
        self.token = ""
        self.issued_at: dt.datetime | None = None
        self.accepts = accepts
        self.account_error = account_error

    def set_access_token(self, token: str, issued_at: dt.datetime) -> None:  # noqa: D102
        self.token, self.issued_at = token, issued_at

    def token_fresh(self) -> bool:  # noqa: D102
        return (
            bool(self.token)
            and self.issued_at is not None
            and dt.datetime.now(dt.UTC) - self.issued_at < dt.timedelta(hours=12)
        )

    def account(self) -> Account:  # noqa: D102
        if self.account_error is not None:
            raise self.account_error
        if self.token != self.accepts:
            raise ports.TokenExpiredError("fake: expired")
        return Account(equity=money.ZERO)


@pytest.fixture
def db() -> Database:
    database = connect(":memory:")
    database.migrate()
    yield database
    database.close()


def stored(db: Database, key: str, token: str) -> None:
    db.set_state(key, {"access_token": token, "issued_at": dt.datetime.now(dt.UTC).isoformat()})


def login_session(port: int) -> Session:
    url = f"http://127.0.0.1:{port}/auth/callback"
    return Session(Callback(url, listen_addr=("127.0.0.1", port)), key="s", timeout=10.0)


def complete_login(port: int) -> None:
    """Answer the callback server once it is up, on a thread, since ensure_session blocks."""

    def run() -> None:
        for _ in range(500):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/auth/callback?code=good", timeout=1):
                    return
            except OSError:
                threading.Event().wait(0.01)

    threading.Thread(target=run, daemon=True).start()


def test_reuses_a_stored_token_the_venue_accepts(db: Database) -> None:
    stored(db, "s", "stored")
    broker = SessionFake(accepts="stored")
    ensure_session(broker, db, Session(Callback("http://127.0.0.1:1/cb"), key="s"))
    assert broker.token == "stored" and broker.callbacks == 0, (
        "a fresh, accepted token must be reused without a browser login"
    )


def test_logs_in_when_the_venue_rejects_the_stored_token(db: Database) -> None:
    stored(db, "s", "stale")
    broker = SessionFake(accepts="token-good")
    port = free_port()
    complete_login(port)
    ensure_session(broker, db, login_session(port))
    assert broker.callbacks == 1, "a token the venue reports expired must lead to one browser login"
    assert db.get_state("s")["access_token"] == "token-good", "the new token must be persisted for the next run"


def test_logs_in_when_nothing_is_stored(db: Database) -> None:
    broker = SessionFake(accepts="token-good")
    port = free_port()
    complete_login(port)
    session = login_session(port)
    session.key = "broker_session"
    ensure_session(broker, db, session)
    assert db.get_state("broker_session")["access_token"] == "token-good", (
        "a cold start must log in, not fail on the missing key"
    )


def test_does_not_log_in_over_an_unrelated_failure(db: Database) -> None:
    stored(db, "s", "stored")
    down = ConnectionError("fake: connection refused")
    broker = SessionFake(accepts="stored", account_error=down)
    with pytest.raises(ConnectionError):
        ensure_session(broker, db, Session(Callback("http://127.0.0.1:1/cb"), key="s"))
    assert broker.callbacks == 0, "a network failure must not burn a browser login"
