"""Reuse a stored broker token, or log in and store one.

Mirrors ``go/harness/session.go``. Every process that logs into an Indian
broker also wrote the same forty lines around :func:`browser_login`: read
yesterday's token from the store, try it, fall back to the browser, save what
came back. This module is those lines, once, over the ports rather than over
one adapter.
"""

from __future__ import annotations

import datetime as dt
import logging
from dataclasses import dataclass
from typing import Protocol

from tradekit.core import ports
from tradekit.core.domain import Account
from tradekit.store import Database, StateNotFoundError

from .login import DEFAULT_TIMEOUT, Callback, browser_login

__all__ = ["DEFAULT_SESSION_KEY", "Session", "SessionBroker", "ensure_session"]

log = logging.getLogger("tradekit.harness.session")

DEFAULT_SESSION_KEY = "broker_session"


class SessionBroker(ports.BrowserLogin, ports.TokenState, Protocol):
    """What resuming a session needs of an adapter.

    The browser flow for a new token, the freshness check and installer for a
    stored one, and one cheap authenticated call to find out whether the venue
    still honours it. ``UpstoxClient`` and ``FyersClient`` both satisfy it.
    """

    def account(self) -> Account:
        """The funds view; the one cheap call that proves the venue accepts the token."""
        ...


@dataclass(slots=True)
class Session:
    """Where a token is kept and how a new one is obtained.

    Args:
        callback: Where the broker redirects the browser. See :func:`browser_login`.
        key: The store's ``kv_state`` key for the token. Two brokers sharing
            one store need two keys.
        timeout: Bounds the browser login, in seconds. The default is long
            enough to find the phone for the OTP and short enough that a
            scheduled run nobody is watching does not hang until the next one.

    """

    callback: Callback
    key: str = DEFAULT_SESSION_KEY
    timeout: float = DEFAULT_TIMEOUT


def ensure_session(broker: SessionBroker, db: Database, session: Session) -> None:
    """Make ``broker`` usable for today.

    The stored token if it is still fresh and the venue accepts it, otherwise
    a browser login, persisted for the next run.

    The stored token is checked against the venue and not just by date,
    because a login elsewhere -- the broker's web app, another bot --
    invalidates it without changing its age. Only :class:`ports.TokenExpiredError`
    from that check leads to the browser; any other failure propagates, since
    logging in again cannot fix a network that is down and would burn the
    day's login on it.

    A token that could not be persisted is not an error: the client already
    holds it, and the worst case is the browser again next run.
    """
    try:
        saved = db.get_state(session.key)
    except StateNotFoundError:
        saved = None
    if saved and saved.get("access_token"):
        issued_at = dt.datetime.fromisoformat(saved["issued_at"])
        broker.set_access_token(saved["access_token"], issued_at)
        if broker.token_fresh():
            try:
                broker.account()
            except ports.TokenExpiredError:
                log.info("stored broker session is no longer valid; logging in again")
            else:
                log.info("reusing stored broker session issued at %s", saved["issued_at"])
                return

    token = browser_login(broker, session.callback, timeout=session.timeout)
    try:
        db.set_state(
            session.key,
            {"access_token": token, "issued_at": dt.datetime.now(dt.UTC).isoformat()},
        )
    except Exception:  # the token is installed; only the next run is affected
        log.warning("could not persist the broker session; the next run will need the browser again", exc_info=True)
