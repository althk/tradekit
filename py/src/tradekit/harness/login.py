"""The one callback server for the redirect-based broker login.

Mirrors ``go/harness/login.go``. Every project that logs into an Indian broker
wrote the same throwaway HTTP server to catch the redirect, and no two were
the same: one had no timeout, one indexed into the query and hung when the
broker sent ``status=error``, and on Windows the stdlib default lets a second
process bind the port and split the traffic. This module is that server, once.

Stdlib only, on purpose: it serves one request a day, must start and stop in
milliseconds, and must not depend on whatever web framework the project runs.
"""

from __future__ import annotations

import html
import logging
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

from tradekit.core.ports import BrowserLogin

__all__ = ["Callback", "browser_login"]

log = logging.getLogger("tradekit.harness.login")

DEFAULT_TIMEOUT = 300.0

_PAGE = (
    '<!doctype html><html><head><meta charset="utf-8"><title>{title}</title></head>'
    "<body><h1>{title}</h1><p>{message}</p>{extra}</body></html>"
)


class Callback:
    """Where the broker sends the browser after login.

    Args:
        redirect_url: The URL registered with the broker, e.g.
            ``http://127.0.0.1:8080/auth/callback``. Its path is the one served
            and its port the one listened on; the broker compares it to the
            registered value character for character, so it is taken whole
            rather than as separate host, port and path that could drift apart.
        listen_addr: Overrides the ``(host, port)`` the server binds. ``None``
            binds every interface on ``redirect_url``'s port. Set it when the
            registered URL is a public name that a proxy or tunnel forwards to
            a different local port.

    """

    def __init__(self, redirect_url: str, listen_addr: tuple[str, int] | None = None) -> None:  # noqa: D107
        parsed = urllib.parse.urlparse(redirect_url)
        if not parsed.scheme or not parsed.netloc:
            raise ValueError(f"login: redirect_url {redirect_url!r} is not an absolute URL")
        self.redirect_url = redirect_url
        self.path = parsed.path or "/"
        self.listen_addr = listen_addr or ("", parsed.port or 80)


class _Server(HTTPServer):
    """An HTTPServer that refuses to share its port.

    ``allow_reuse_address`` is the stdlib default, and on Windows it lets a
    second process bind a port someone is already listening on -- the two then
    split the incoming connections. For a port whose whole job is to receive
    one security-sensitive redirect, a clash must fail loudly.
    """

    allow_reuse_address = False

    def __init__(self, addr: tuple[str, int], broker: BrowserLogin, path: str, login_url: str) -> None:
        super().__init__(addr, _Handler)
        self.broker = broker
        self.path = path
        self.login_url = login_url
        self.lock = threading.Lock()
        self.token = ""
        self.last_error: Exception | None = None
        self.done = threading.Event()


class _Handler(BaseHTTPRequestHandler):
    """Serves the redirect.

    The exchange runs inside the handler so the browser shows the real outcome:
    a "success" page followed by a failed exchange in the log is exactly the
    confusion this replaces.
    """

    server: _Server

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib's signature
        log.debug(format, *args)

    def _respond(self, status: int, title: str, message: str, extra: str = "") -> None:
        body = _PAGE.format(title=html.escape(title), message=html.escape(message), extra=extra).encode()
        self.send_response(status)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        srv = self.server
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != srv.path:
            # Favicon requests, mostly; anything but the registered path is not a callback.
            self._respond(404, "Not found", "")
            return

        with srv.lock:
            if srv.token:
                # A reload of the success page, or the broker retrying the
                # redirect. The first callback already won; do not exchange a
                # second code.
                self._respond(200, "Login already completed", "You can close this tab.")
                return
            query = {k: v[0] for k, v in urllib.parse.parse_qs(parsed.query, keep_blank_values=True).items()}
            try:
                srv.token = srv.broker.login_callback(query)
            except Exception as exc:  # whatever the broker raised is the message
                srv.last_error = exc
                log.warning("broker login callback failed; waiting for another attempt: %s", exc)
                retry = f'<p><a href="{html.escape(srv.login_url)}">Try again</a></p>'
                self._respond(400, "Login failed", str(exc), retry)
                return
            srv.done.set()
        self._respond(200, "Login successful", "You can close this tab.")


def browser_login(broker: BrowserLogin, callback: Callback, *, timeout: float = DEFAULT_TIMEOUT) -> str:
    """Obtain the day's access token through the browser.

    Serves the redirect, logs the login URL for the user to visit, exchanges
    the code the broker sends back, and returns the token, which the adapter
    has already installed on itself. The caller persists the token so a
    restart does not need the browser again.

    Returns when a callback succeeds or ``timeout`` elapses; a refused login is
    shown in the browser with a link to try again and the server keeps waiting,
    because the person who can fix it is looking at that page. The timeout is
    what stops a login nobody completes from holding a scheduler forever.

    The URL is logged rather than opened in a browser: most of these processes
    run on a box with no browser, and the ones that do not can open it
    themselves from the log line.

    Raises:
        OSError: If the port is already in use -- immediately, not as a login
            that never arrives.
        TimeoutError: If no callback succeeded within ``timeout``; the last
            refusal, if any, is in the message.

    """
    # login_url before serving: a callback can only arrive after the user has
    # the URL, and FYERS mints the state it later checks in this call.
    login_url = broker.login_url()
    server = _Server(callback.listen_addr, broker, callback.path, login_url)
    thread = threading.Thread(target=server.serve_forever, name="tradekit-login", daemon=True)
    thread.start()
    try:
        log.info(
            "broker login required: open the login URL in a browser: %s (callback %s)", login_url, callback.redirect_url
        )
        if not server.done.wait(timeout):
            if server.last_error is not None:
                raise TimeoutError(
                    f"login: no completed login in {timeout:g}s; last callback failed: {server.last_error}"
                )
            raise TimeoutError(f"login: no completed login in {timeout:g}s")
        return server.token
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
