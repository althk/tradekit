"""The FYERS client: broker, quotes, history, GTT and reference data.

Mirrors ``go/fyers``. The client satisfies :class:`tradekit.core.ports.Broker`,
and separately ``Quoter``, ``HistoryProvider``, ``ProtectiveOrders``,
``MarginEstimator``, ``InstrumentSource`` and ``TokenState``; ask for a
capability with :func:`isinstance` against the protocol.

``Streamer`` is not implemented: FYERS's market-data socket speaks a
proprietary binary protocol that cannot be verified without a live session.
``TickObserver`` is not implemented either, as in the other live adapters: a
FYERS stop rests at the broker and triggers without the engine's help.
"""

from __future__ import annotations

import datetime as dt
import hashlib
import urllib.parse
from collections.abc import Iterator, Sequence
from typing import Any

import httpx

from tradekit.core.domain import (
    Account,
    Candle,
    Instrument,
    InstrumentKey,
    MarginLeg,
    Order,
    OrderRequest,
    OrderStatus,
    OrderType,
    Position,
    Protective,
    Quote,
    Side,
    Timeframe,
)
from tradekit.core.money import Money

from .mapping import (
    GTT_OCO,
    IST,
    ORDER_TYPE_LIMIT,
    ORDER_TYPE_MARKET,
    PRODUCT_INTRADAY,
    RESPONSE_OK,
    STATUS_CANCELLED,
    STATUS_EXPIRED,
    STATUS_REJECTED,
    STATUS_TRADED,
    chunk_days,
    from_order_type,
    from_product,
    from_side,
    from_validity,
    key_for,
    normalize_status,
    paise,
    parse_order_time,
    rupees,
    strip_tag_prefix,
    symbol_for,
    to_order_type,
    to_product,
    to_resolution,
    to_side,
    to_validity,
)
from .transport import Transport

__all__ = ["BASE_URL", "DATA_URL", "SYMBOL_MASTER_URL", "FyersClient", "parse_symbol_master"]

BASE_URL = "https://api-t1.fyers.in/api/v3"
"""The trading root: auth, profile, funds, orders, positions, GTT, margins."""

DATA_URL = "https://api-t1.fyers.in/data"
"""The market-data root: quotes, depth and history.

The split is FYERS's, not a migration half-done: quotes and history live under
``/data`` while everything that touches the account lives under ``/api/v3``.
"""

SYMBOL_MASTER_URL = "https://public.fyers.in/sym_details"
"""The directory the per-segment symbol master files are served from, without
authentication."""

QUOTE_CHUNK_SIZE = 50

FUND_TOTAL_BALANCE = 1
FUND_UTILIZED_AMOUNT = 2
FUND_AVAILABLE_BALANCE = 10
FUND_TITLE_TOTAL = "Total Balance"
FUND_TITLE_UTILIZED = "Utilized Amount"
FUND_TITLE_AVAILABLE = "Available Balance"
"""The funds ledger rows a risk check needs.

The funds response is a ledger of titled rows rather than named fields, and the
ids are what identify them; the titles are display text. They are matched by id
first and by title as a fallback, so a renumbering is survived if the wording
is kept.
"""

_MASTER_FILE_BY_EXCHANGE = {
    "": "NSE_CM_sym_master.json",
    "NSE": "NSE_CM_sym_master.json",
    "BSE": "BSE_CM_sym_master.json",
    "NFO": "NSE_FO_sym_master.json",
    "BFO": "BSE_FO_sym_master.json",
    "CDS": "NSE_CD_sym_master.json",
    "MCX": "MCX_COM_sym_master.json",
}

_FUTURES_INST_TYPES = frozenset({11, 12, 13, 16, 17, 18, 25, 30, 33, 34, 35})
_OPTIONS_INST_TYPES = frozenset({14, 15, 19, 31, 32, 36, 37})
_INDEX_INST_TYPE = 10

_DEAD_GTT_STATUSES = frozenset({STATUS_CANCELLED, STATUS_TRADED, STATUS_REJECTED, STATUS_EXPIRED})


def _chunked(items: Sequence[str], size: int) -> Iterator[list[str]]:
    """Yield ``items`` in lists of at most ``size``."""
    for start in range(0, len(items), size):
        yield list(items[start : start + size])


def _int(value: Any, fallback: int = 0) -> int:
    """Read a number the master may spell as a number, a string, or ``""``."""
    if value is None or value == "":
        return fallback
    try:
        return int(value)
    except (TypeError, ValueError):
        try:
            return int(float(value))
        except (TypeError, ValueError):
            return fallback


def _float(value: Any, fallback: float = 0.0) -> float:
    """Read a float the master may spell as a number, a string, or ``""``."""
    if value is None or value == "":
        return fallback
    try:
        return float(value)
    except (TypeError, ValueError):
        return fallback


def _gtt_is_live(row: dict[str, Any]) -> bool:
    """Whether a trigger is still protecting a position.

    The book returns cancelled and triggered rows alongside live ones -- the
    reference's own sample is two cancelled triggers -- and reading it as a set
    of live stops is wrong in the silent direction: a dead trigger still looks
    like protection, so a missing stop is never noticed. A status this adapter
    has not seen reads as live: an unknown status raising a false "stop is
    missing" every morning would bury the real alerts.
    """
    return _int(row.get("ord_status"), -1) not in _DEAD_GTT_STATUSES


def _gtt_legs(p: Protective) -> dict[str, Any]:
    """The legs of a trigger, in FYERS's positional order.

    The leg order follows FYERS's rule, not the leg's role: ``leg1`` is the one
    that triggers above the market and ``leg2`` the one below. A sell that
    protects a long has its target above and its stop below, so the target is
    leg1; a buy that protects a short is the mirror image, with the stop as
    leg1. Assigning by role would have FYERS reject every short's OCO.

    The exit is placed as a limit at the trigger price, which is what FYERS's
    own web client does. A limit exactly at the trigger can be left unfilled by
    a gap; that is the same exposure a Kite GTT has, and the alternative -- a
    limit some distance through the trigger -- is a policy the strategy, not
    the adapter, should set.

    Raises:
        ValueError: For a non-positive quantity or a protective with no level.

    """
    if p.quantity <= 0:
        raise ValueError(f"fyers: protective quantity must be positive, got {p.quantity}")
    if p.stop <= 0 and p.target <= 0:
        raise ValueError("fyers: a protective order needs a stop or a target")

    def leg(level: Money) -> dict[str, Any]:
        return {"price": rupees(level), "triggerPrice": rupees(level), "qty": p.quantity}

    if p.oco:
        above, below = (p.target, p.stop) if p.side == Side.SELL else (p.stop, p.target)
        return {"leg1": leg(above), "leg2": leg(below)}
    return {"leg1": leg(p.stop if p.stop > 0 else p.target)}


def _from_gtt(row: dict[str, Any]) -> Protective:
    """Convert a trigger row into a Protective.

    FYERS does not label a leg as stop or target; the levels are read by their
    position relative to the market. For an OCO the two legs are on opposite
    sides of the price by construction, so the order's own side says which is
    which: a sell's stop is the lower level, a buy's the higher. A single leg
    is judged against the row's last traded price when the book carries one,
    and read as a stop when it does not -- outside market hours the book
    reports an LTP of zero, and a stop is what a single-leg protective order
    nearly always is.
    """
    side = from_side(_int(row.get("tran_side"), 1))
    leg1 = paise(row.get("price_trigger"))
    leg2 = paise(row.get("price2_trigger"))
    stop = Money(0)
    target = Money(0)

    if _int(row.get("gtt_oco_ind")) == GTT_OCO or leg2 > 0:
        high, low = (leg1, leg2) if leg1 >= leg2 else (leg2, leg1)
        stop, target = (low, high) if side == Side.SELL else (high, low)
    else:
        ltp = paise(row.get("ltp"))
        is_target = ltp > 0 and ((side == Side.SELL and leg1 > ltp) or (side == Side.BUY and leg1 < ltp))
        if is_target:
            target = leg1
        else:
            stop = leg1

    return Protective(
        id=str(row.get("id") or ""),
        key=key_for(str(row.get("symbol") or ""), _int(row.get("segment"))),
        side=side,
        quantity=_int(row.get("qty")),
        stop=stop,
        target=target,
        product=from_product(str(row.get("product_type") or "")),
    )


def _segment_of(inst_type: int) -> str:
    """Classify an exchange instrument type into the domain's vocabulary.

    The codes are FYERS's appendix table; anything unlisted is equity, which
    is what the cash-market codes for preference shares, debentures and ETFs
    amount to for sizing purposes.
    """
    if inst_type == _INDEX_INST_TYPE:
        return "index"
    if inst_type in _FUTURES_INST_TYPES:
        return "futures"
    if inst_type in _OPTIONS_INST_TYPES:
        return "options"
    return "equity"


def parse_symbol_master(rows: dict[str, Any]) -> list[Instrument]:
    """Read the decoded symbol master JSON, keyed by symbol ticker.

    A malformed row is skipped rather than failing the batch. The master
    carries tens of thousands of rows and one bad entry must not cost a sync
    its whole universe. Numbers are not consistently quoted -- ``minLotSize``
    is a number and ``expiryDate`` a string of digits -- and inapplicable
    fields are ``""``, so every numeric field is read tolerantly.
    """
    out: list[Instrument] = []
    for ticker, row in rows.items():
        if not isinstance(row, dict):
            continue
        symbol = str(row.get("symTicker") or ticker or "")
        if not symbol:
            continue
        segment = _int(row.get("segment"))
        expiry_secs = _int(row.get("expiryDate"))
        option_type = str(row.get("optType") or "").strip().upper()
        out.append(
            Instrument(
                key=key_for(symbol, segment),
                name=str(row.get("symDetails") or ""),
                isin=str(row.get("isin") or ""),
                segment=_segment_of(_int(row.get("exInstType"), -1)),
                # The domain uses 1 for cash equity so sizing can multiply
                # by it unconditionally.
                lot_size=max(1, _int(row.get("minLotSize"), 1)),
                tick_size=paise(_float(row.get("tickSize"))),
                expiry=dt.datetime.fromtimestamp(expiry_secs, tz=IST).date() if expiry_secs > 0 else None,
                strike=paise(_float(row.get("strikePrice"))),
                option_type=option_type.lower() if option_type in ("CE", "PE") else "",
                # tradeStatus is 1 for active and 0 for suspended. A row with
                # no flag is read as active, since the master lists what is
                # tradable today.
                active=_int(row.get("tradeStatus"), 1) != 0,
            )
        )
    return out


class FyersClient:
    """A tradekit adapter for the FYERS API v3."""

    def __init__(
        self,
        *,
        app_id: str,
        app_secret: str = "",
        redirect_uri: str = "",
        access_token: str = "",
        token_issued_at: dt.datetime | None = None,
        tag: str = "",
        transport: Transport | None = None,
        base_url: str = BASE_URL,
        data_url: str = DATA_URL,
        symbol_master_url: str = SYMBOL_MASTER_URL,
        http_client: httpx.Client | None = None,
        **transport_kwargs: Any,
    ) -> None:
        """Build a client. It does not contact the broker.

        Args:
            app_id: The app's client id, of the form ``XXXXXXXXXX-100``. It is
                sent with every request: FYERS authenticates with
                ``appId:token`` rather than a bare bearer token.
            app_secret: Needed only to exchange an authorisation code.
            redirect_uri: Must match the app's configured redirect exactly.
            access_token: A token from a completed login, valid one trading day.
            token_issued_at: When that token was obtained. Drives
                :meth:`token_fresh`; ``None`` means unknown, which reads stale.
            tag: Attached to every order, so orders this system placed can be
                told from ones placed by hand. FYERS echoes it back prefixed
                with ``1:``.
            transport: A pre-built transport. Tests inject one carrying an
                ``httpx.MockTransport``.
            base_url: The trading root.
            data_url: The market-data root.
            symbol_master_url: Where the symbol master files are fetched from.
            http_client: The client used for the symbol master, which is on a
                different host and needs no token.
            **transport_kwargs: Passed to :class:`Transport` when none is given.

        """
        if not app_id:
            raise ValueError("fyers: app_id is required")
        self._app_id = app_id
        self._app_secret = app_secret
        self._redirect_uri = redirect_uri
        self._access_token = access_token
        self._token_issued_at = token_issued_at
        self._tag = tag
        self._symbol_master_url = symbol_master_url.rstrip("/")
        self._http = http_client or httpx.Client(timeout=60.0)
        self._transport = transport or Transport(
            auth_provider=self._auth_header,
            base_url=base_url,
            data_url=data_url,
            **transport_kwargs,
        )

    # --- auth ---------------------------------------------------------

    def _auth_header(self) -> str:
        """The ``Authorization`` value, or an empty string before login."""
        return f"{self._app_id}:{self._access_token}" if self._access_token else ""

    def login_url(self, state: str = "") -> str:
        """The URL a user visits to authorise the app and obtain a code.

        ``state`` is echoed back unchanged on the redirect; send a random value
        and check it, as the reference recommends.
        """
        query = urllib.parse.urlencode(
            {"client_id": self._app_id, "redirect_uri": self._redirect_uri, "response_type": "code", "state": state}
        )
        return f"{self._transport.base_url}/generate-authcode?{query}"

    def _app_id_hash(self) -> str:
        """SHA-256 of ``appId:appSecret``, which the token exchange takes instead of the secret."""
        return hashlib.sha256(f"{self._app_id}:{self._app_secret}".encode()).hexdigest()

    def login(self, auth_code: str) -> str:
        """Exchange an authorisation code for an access token and install it.

        FYERS tokens are valid for one trading day, so this runs once each
        morning; the returned token is what a scheduler stores and passes back
        on the next start.

        Never retried: an authorisation code is single-use, so a second attempt
        with the same code is refused even when the first failed in transport.

        Returns:
            The access token, so the caller can persist it.

        Raises:
            RuntimeError: If no app secret is configured, or the exchange
                returns no token.

        """
        if not self._app_secret:
            raise RuntimeError("fyers: an app secret is required to exchange an authorisation code")
        payload = self._transport.request(
            "POST",
            "/validate-authcode",
            json_body={"grant_type": "authorization_code", "appIdHash": self._app_id_hash(), "code": auth_code},
            retry=False,
        )
        token = str(payload.get("access_token") or "")
        if not token:
            raise RuntimeError("fyers: token exchange returned an empty access_token")
        self.set_access_token(token, dt.datetime.now(dt.UTC))
        return token

    def set_access_token(self, token: str, issued_at: dt.datetime) -> None:
        """Install a token and record when it was issued."""
        self._access_token = token
        self._token_issued_at = issued_at

    def token_fresh(self) -> bool:
        """Whether the access token is valid for the current trading day.

        FYERS tokens expire overnight, so a scheduler asks this before the open
        and re-authenticates rather than discovering the problem on its first
        order. The comparison is by IST calendar date, which is the granularity
        the expiry actually has; a token of unknown age reads stale, because
        assuming it is good is the failure this exists to prevent.
        """
        if not self._access_token or self._token_issued_at is None:
            return False
        issued = self._token_issued_at
        if issued.tzinfo is None:
            issued = issued.replace(tzinfo=dt.UTC)
        return issued.astimezone(IST).date() == dt.datetime.now(IST).date()

    # --- broker -------------------------------------------------------

    def account(self) -> Account:
        """The equity ledger's funds.

        Each row carries a separate figure for the commodity ledger, and it is
        deliberately not folded in: an equity strategy sizing against a
        combined figure would size against capital it cannot use.

        "Available Balance" (id 10) is what can be committed to a new order
        right now, after utilisation, collateral and the day's realised P&L;
        "Clear Balance" (id 3) is the ledger figure before collateral and is not
        what a margin check should size against.

        Raises:
            RuntimeError: If the ledger is empty or carries no available row.
                Reporting zero equity would read as a wiped account and stop
                every strategy at once.

        """
        payload = self._transport.request("GET", "/funds", retry=True)
        rows = payload.get("fund_limit")
        rows = [r for r in rows if isinstance(r, dict)] if isinstance(rows, list) else []
        if not rows:
            raise RuntimeError("fyers: funds response carried no fund_limit rows")

        available = self._fund_amount(rows, FUND_AVAILABLE_BALANCE, FUND_TITLE_AVAILABLE)
        if available is None:
            raise RuntimeError(f"fyers: funds response carried no {FUND_TITLE_AVAILABLE!r} row")
        used = self._fund_amount(rows, FUND_UTILIZED_AMOUNT, FUND_TITLE_UTILIZED) or Money(0)
        total = self._fund_amount(rows, FUND_TOTAL_BALANCE, FUND_TITLE_TOTAL)
        if total is None:
            total = Money(available + used)
        return Account(equity=total, available=available, used=used)

    @staticmethod
    def _fund_amount(rows: list[dict[str, Any]], row_id: int, title: str) -> Money | None:
        """A ledger row's equity figure, found by id or, failing that, by title."""
        for row in rows:
            if _int(row.get("id"), -1) == row_id:
                return paise(row.get("equityAmount"))
        for row in rows:
            if str(row.get("title") or "").strip().lower() == title.lower():
                return paise(row.get("equityAmount"))
        return None

    def place_order(self, req: OrderRequest) -> Order:
        """Submit an order.

        The returned order carries FYERS's id and a pending status: an
        acknowledgement is not a fill, and treating it as one is how a
        reconciler comes to believe in positions that do not exist. Call
        :meth:`order_status` to learn what actually happened.

        Not retried. Retrying a POST that placed an order places a second one.

        Raises:
            ValueError: For a request FYERS cannot express, or one missing a
                price its order type requires.
            RuntimeError: If the acknowledgement carries no order id. FYERS
                documents code 201 as "request made, no acknowledgement
                received; check the order book" -- there is no id to poll.

        """
        body = self._order_body(req)
        payload = self._transport.request("POST", "/orders/sync", json_body=body, retry=False)
        order_id = str(payload.get("id") or "")
        if not order_id:
            raise RuntimeError(
                f"fyers: placing {req.side} {req.quantity} {req.key}: response carried no order id "
                f"(code {payload.get('code')}: {payload.get('message')})"
            )
        now = dt.datetime.now(dt.UTC)
        return Order(id=order_id, request=req, status=OrderStatus.PENDING, placed_at=now, updated_at=now)

    def _order_body(self, req: OrderRequest) -> dict[str, Any]:
        """Translate a request into FYERS's payload.

        Every price field is sent, zero when unused, because the reference
        marks them mandatory. ``takeProfit`` and ``stopLoss`` are offsets FYERS
        would attach as bracket legs; they are never set here, since a resting
        stop is placed through :meth:`place_protective` where its lifecycle can
        be managed.
        """
        if req.quantity <= 0:
            raise ValueError(f"fyers: quantity must be positive, got {req.quantity}")
        body: dict[str, Any] = {
            "symbol": symbol_for(req.key),
            "qty": req.quantity,
            "type": to_order_type(req.type),
            "side": to_side(req.side),
            "productType": to_product(req.product),
            "limitPrice": 0.0,
            "stopPrice": 0.0,
            "validity": to_validity(req.time_in_force),
            "disclosedQty": 0,
            # offlineOrder is the after-market flag. Sent false: a strategy
            # that places orders while the market is closed is making a
            # mistake this adapter should surface, not paper over.
            "offlineOrder": False,
            "stopLoss": 0.0,
            "takeProfit": 0.0,
        }
        tag = req.tag or self._tag
        if tag:
            body["orderTag"] = tag

        if req.type == OrderType.LIMIT:
            if req.limit_price <= 0:
                raise ValueError("fyers: a limit order needs a limit price")
            body["limitPrice"] = rupees(req.limit_price)
        elif req.type == OrderType.STOP:
            if req.trigger_price <= 0:
                raise ValueError("fyers: a stop order needs a trigger price")
            body["stopPrice"] = rupees(req.trigger_price)
        elif req.type == OrderType.STOP_LIMIT:
            if req.trigger_price <= 0 or req.limit_price <= 0:
                raise ValueError("fyers: a stop-limit order needs both a trigger and a limit price")
            body["stopPrice"] = rupees(req.trigger_price)
            body["limitPrice"] = rupees(req.limit_price)
        return body

    def cancel_order(self, order_id: str) -> None:
        """Withdraw an open order.

        The id goes in a JSON body on a DELETE, which is FYERS's documented
        form. Not retried: a cancel that succeeded but whose response was lost
        would, on a second attempt, be refused for an order that no longer
        exists -- and that refusal reaches the caller as a failed cancel, which
        is the dangerous direction to be wrong in.
        """
        self._transport.request("DELETE", "/orders/sync", json_body={"id": order_id}, retry=False)

    def order_status(self, order_id: str) -> Order:
        """One order's current state.

        FYERS has no single-order endpoint; the book is filtered by id. The
        filter answers an unknown id with an empty book rather than an error.

        Raises:
            RuntimeError: If FYERS does not know the order. Returning an empty
                order would read as one that is pending and never settles.

        """
        payload = self._transport.request("GET", "/orders", params={"id": order_id}, retry=True)
        rows = self._rows(payload, "orderBook")
        for row in rows:
            if str(row.get("id") or "") == order_id:
                return self._to_order(row)
        if len(rows) == 1:
            # The filter honoured the id; trust it even if the row's own id
            # is spelled differently, as it is for a sliced order's child.
            return self._to_order(rows[0])
        raise RuntimeError(f"fyers: order {order_id} not found")

    def open_orders(self) -> list[Order]:
        """Orders that have not reached a terminal state."""
        payload = self._transport.request("GET", "/orders", retry=True)
        return [o for o in (self._to_order(r) for r in self._rows(payload, "orderBook")) if not o.status.terminal]

    @staticmethod
    def _rows(payload: dict[str, Any], name: str) -> list[dict[str, Any]]:
        """The list under ``name``, tolerating its absence."""
        rows = payload.get(name)
        return [r for r in rows if isinstance(r, dict)] if isinstance(rows, list) else []

    @staticmethod
    def _to_order(row: dict[str, Any]) -> Order:
        """Convert a FYERS order row into the domain's."""
        placed = parse_order_time(str(row.get("orderDateTime") or ""))
        return Order(
            id=str(row.get("id") or ""),
            request=OrderRequest(
                key=key_for(str(row.get("symbol") or ""), _int(row.get("segment"))),
                side=from_side(_int(row.get("side"), 1)),
                quantity=_int(row.get("qty")),
                type=from_order_type(_int(row.get("type"))),
                product=from_product(str(row.get("productType") or "")),
                limit_price=paise(row.get("limitPrice")),
                trigger_price=paise(row.get("stopPrice")),
                time_in_force=from_validity(str(row.get("orderValidity") or "")),
                tag=strip_tag_prefix(str(row.get("orderTag") or "")),
            ),
            status=normalize_status(_int(row.get("status"))),
            filled_quantity=_int(row.get("filledQty")),
            average_price=paise(row.get("tradedPrice")),
            placed_at=placed,
            updated_at=placed,
            message=str(row.get("message") or ""),
        )

    def positions(self) -> list[Position]:
        """The open positions.

        ``netPositions``, not holdings: the port means positions, and holdings
        are a separate concept. A strategy reading holdings would believe it is
        flat while holding an intraday position.

        Zero-quantity rows are dropped. FYERS keeps a closed position's row for
        the rest of the day; it is history, not a holding.
        """
        payload = self._transport.request("GET", "/positions", retry=True)
        out: list[Position] = []
        for row in self._rows(payload, "netPositions"):
            net_qty = _int(row.get("netQty"))
            if net_qty == 0:
                continue
            # netAvg is FYERS's own average for the net position. It is
            # preferred over the buy or sell average because a position that
            # has been partly scaled out has both, and only netAvg says what
            # the remaining quantity cost.
            avg = _float(row.get("netAvg"))
            if avg == 0:
                avg = _float(row.get("buyAvg")) if net_qty > 0 else _float(row.get("sellAvg"))
            out.append(
                Position(
                    key=key_for(str(row.get("symbol") or ""), _int(row.get("segment"))),
                    quantity=net_qty,
                    average_price=paise(avg),
                    product=from_product(str(row.get("productType") or "")),
                    realized_pnl=paise(row.get("realized_profit")),
                    unrealized_pnl=paise(row.get("unrealized_profit")),
                )
            )
        return out

    def basket_margin(self, legs: list[MarginLeg]) -> Money:
        """What a basket would block.

        ``margin_total`` is the figure for the basket as a whole, after any
        hedge benefit between legs, and is what a risk check must size against;
        ``margin_new_order`` includes the account's existing positions and would
        refuse a spread the account can afford.

        FYERS applies the hedge benefit only when the legs arrive in an order it
        recognises -- a buy before the sell it hedges -- so legs are sent in the
        order given and a caller building a spread should list the long leg
        first.
        """
        if not legs:
            return Money(0)
        data = []
        for leg in legs:
            item: dict[str, Any] = {
                "symbol": symbol_for(leg.key),
                "qty": leg.quantity,
                "side": to_side(leg.side),
                "type": ORDER_TYPE_MARKET,
                "productType": to_product(leg.product),
                "limitPrice": 0.0,
                "stopPrice": 0.0,
                "stopLoss": 0.0,
                "takeProfit": 0.0,
            }
            if leg.price > 0:
                item["type"] = ORDER_TYPE_LIMIT
                item["limitPrice"] = rupees(leg.price)
            data.append(item)
        payload = self._transport.request("POST", "/multiorder/margin", json_body={"data": data}, retry=True)
        return paise((payload.get("data") or {}).get("margin_total"))

    # --- protective orders --------------------------------------------

    def place_protective(self, p: Protective) -> str:
        """Rest a stop, or a stop and target as an OCO pair, as a FYERS GTT.

        A GTT rests at FYERS rather than at the exchange and survives the
        process exiting, which is the whole reason a swing strategy uses one
        instead of watching prices itself.

        Not retried: a retry landing after a lost acknowledgement rests a second
        stop on the same position, which sells the position twice when it fires.

        Returns:
            The trigger id, so the stop can later be modified.

        Raises:
            ValueError: For a protective order with no level, a non-positive
                quantity, or an INTRADAY product -- FYERS accepts no GTT for
                one, and an intraday position is squared off by the broker
                before a GTT could matter.
            RuntimeError: If the acknowledgement carries no trigger id.

        """
        legs = _gtt_legs(p)
        product = to_product(p.product)
        if product == PRODUCT_INTRADAY:
            raise ValueError("fyers: a GTT cannot protect an INTRADAY position; use a stop order")
        body: dict[str, Any] = {
            "side": to_side(p.side),
            "symbol": symbol_for(p.key),
            "productType": product,
            "orderInfo": legs,
        }
        if self._tag:
            body["orderTag"] = self._tag
        payload = self._transport.request("POST", "/gtt/orders/sync", json_body=body, retry=False)
        gtt_id = str(payload.get("id") or "")
        if not gtt_id:
            raise RuntimeError(
                f"fyers: placing GTT for {p.key}: response carried no trigger id "
                f"(code {payload.get('code')}: {payload.get('message')})"
            )
        return gtt_id

    def modify_stop(self, protective_id: str, stop: Money) -> None:
        """Move a resting trigger's stop, as a trailing adjustment does.

        FYERS replaces the whole leg set rather than patching one leg, so the
        existing trigger is read first and its target, quantity and side carried
        across. Sending only the stop would silently drop the target leg of an
        OCO, which is the mistake this read-before-write exists to prevent.

        Raises:
            RuntimeError: If the trigger is no longer in the book. Silently
                succeeding would leave a position unprotected while the caller
                believes it trailed.

        """
        existing = self._protective(protective_id)
        updated = Protective(
            id=existing.id,
            key=existing.key,
            side=existing.side,
            quantity=existing.quantity,
            stop=stop,
            target=existing.target,
            product=existing.product,
        )
        self._transport.request(
            "PATCH",
            "/gtt/orders/sync",
            json_body={"id": protective_id, "orderInfo": _gtt_legs(updated)},
            retry=False,
        )

    def cancel_protective(self, protective_id: str) -> None:
        """Delete a resting trigger. The id goes in a JSON body on a DELETE."""
        self._transport.request("DELETE", "/gtt/orders/sync", json_body={"id": protective_id}, retry=False)

    def list_protective(self) -> list[Protective]:
        """The triggers that are still live.

        Triggered and cancelled rows stay in FYERS's book; returning them would
        make a reconciler believe a position is protected by a trigger that has
        already fired.
        """
        return [_from_gtt(row) for row in self._gtt_book() if _gtt_is_live(row)]

    def _gtt_book(self) -> list[dict[str, Any]]:
        """The raw trigger book."""
        return self._rows(self._transport.request("GET", "/gtt/orders", retry=True), "orderBook")

    def _protective(self, protective_id: str) -> Protective:
        """One trigger by id.

        FYERS has no single-trigger endpoint, so the book is fetched and
        filtered. It is one request either way, which is what makes
        :meth:`modify_stop`'s read-before-write cost nothing extra.
        """
        for row in self._gtt_book():
            if str(row.get("id") or "") == protective_id:
                return _from_gtt(row)
        raise RuntimeError(f"fyers: GTT {protective_id} is not in the trigger book")

    # --- market data --------------------------------------------------

    def ltp(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Money]:
        """The last traded price for each instrument.

        FYERS has one quote endpoint serving both, so this is :meth:`quote`
        with the rest discarded; there is no cheaper call to make. Instruments
        FYERS does not recognise are omitted rather than raising, so one
        delisted symbol cannot fail a 500-symbol scan.
        """
        return {key: q.last for key, q in self.quote(keys).items()}

    def quote(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Quote]:
        """The still-forming session's OHLC and last price."""
        by_symbol: dict[str, InstrumentKey] = {}
        for key in keys:
            by_symbol.setdefault(symbol_for(key), key)

        out: dict[InstrumentKey, Quote] = {}
        now = dt.datetime.now(dt.UTC)
        for chunk in _chunked(list(by_symbol), QUOTE_CHUNK_SIZE):
            payload = self._transport.request(
                "GET", "/quotes", base=self._transport.data_url, params={"symbols": ",".join(chunk)}, retry=True
            )
            for entry in self._rows(payload, "d"):
                # An entry that FYERS itself marks failed, or that names a
                # symbol not asked for, is dropped: a quote that cannot be
                # attributed is worse than a missing one.
                if str(entry.get("s") or "").lower() != RESPONSE_OK:
                    continue
                key = by_symbol.get(str(entry.get("n") or ""))
                if key is None:
                    continue
                values = entry.get("v") if isinstance(entry.get("v"), dict) else {}
                at = now
                tt = _int(values.get("tt"))
                if tt > 0:
                    at = dt.datetime.fromtimestamp(tt, tz=dt.UTC)
                out[key] = Quote(
                    key=key,
                    at=at,
                    last=paise(values.get("lp")),
                    open=paise(values.get("open_price")),
                    high=paise(values.get("high_price")),
                    low=paise(values.get("low_price")),
                    # prev_close_price is the previous session's close, which
                    # is what Quote.close means. Conflating it with the last
                    # price makes every gap calculation silently zero.
                    close=paise(values.get("prev_close_price")),
                    bid=paise(values.get("bid")),
                    ask=paise(values.get("ask")),
                )
        return out

    def candles(self, key: InstrumentKey, timeframe: Timeframe, start: dt.datetime, end: dt.datetime) -> list[Candle]:
        """Bars over ``[start, end]``, oldest first.

        FYERS serves a bounded span per request -- 100 days for intraday
        resolutions, 366 for daily -- so a long backfill is fetched as a series
        of chunks and concatenated. The caller asks for ten years and does not
        have to know that.

        The range is sent as epoch seconds rather than dates, so an intraday
        caller can ask for exactly the completed bars it wants: the reference's
        own advice is to end the range one bar before now, because a bar that
        includes the current minute is returned partial.

        Raises:
            ValueError: For an inverted range. FYERS answers one with an empty
                series, which reads as "no trades".

        """
        resolution = to_resolution(timeframe)
        if end < start:
            raise ValueError(f"fyers: candle range ends ({end.date()}) before it starts ({start.date()})")
        symbol = symbol_for(key)
        span = dt.timedelta(days=chunk_days(timeframe))

        out: list[Candle] = []
        window_start = start
        while window_start <= end:
            window_end = min(window_start + span - dt.timedelta(seconds=1), end)
            params = {
                "symbol": symbol,
                "resolution": resolution,
                "date_format": "0",
                "range_from": str(int(window_start.timestamp())),
                "range_to": str(int(window_end.timestamp())),
                "cont_flag": "",
            }
            out.extend(self._candles(params, key, timeframe))
            window_start += span
        return out

    def _candles(self, params: dict[str, str], key: InstrumentKey, timeframe: Timeframe) -> list[Candle]:
        """One history request, converted to candles.

        FYERS returns bars oldest first as ``[epoch, o, h, l, c, v]``, which is
        the order a backtest replays, so no reversal is needed -- unlike
        Upstox. Timestamps are epoch seconds and are rendered in IST, the
        exchange's zone, so a bar keyed by date in the store lands on the right
        session.

        A malformed row is skipped rather than failing the batch: one
        unparseable bar in a five-year backfill is a gap, and refusing the whole
        range over it means no history at all.
        """
        payload = self._transport.request("GET", "/history", base=self._transport.data_url, params=params, retry=True)
        rows = payload.get("candles")
        out: list[Candle] = []
        for row in rows if isinstance(rows, list) else []:
            if not isinstance(row, list) or len(row) < 6:
                continue
            epoch = row[0]
            if not isinstance(epoch, int | float) or isinstance(epoch, bool) or epoch <= 0:
                continue
            out.append(
                Candle(
                    key=key,
                    timeframe=timeframe,
                    start=dt.datetime.fromtimestamp(int(epoch), tz=IST),
                    open=paise(row[1]),
                    high=paise(row[2]),
                    low=paise(row[3]),
                    close=paise(row[4]),
                    volume=_int(row[5]),
                    # Open interest is a seventh column only when oi_flag is
                    # set, which this adapter does not do.
                )
            )
        return out

    def instruments(self, exchange: str) -> list[Instrument]:
        """The symbol master for an exchange.

        The master is a JSON file on the public host and needs no token, so it
        is fetched directly rather than through the transport. It is measured
        in megabytes, so it belongs in a daily sync and not in a request path.

        Raises:
            ValueError: For an exchange FYERS has no master for.
            RuntimeError: If the download fails.

        """
        try:
            file = _MASTER_FILE_BY_EXCHANGE[exchange.strip().upper()]
        except KeyError:
            raise ValueError(f"fyers: no symbol master for exchange {exchange!r}") from None
        response = self._http.get(f"{self._symbol_master_url}/{file}")
        if response.status_code != 200:
            raise RuntimeError(f"fyers: downloading the symbol master: HTTP {response.status_code}")
        rows = response.json()
        if not isinstance(rows, dict):
            raise RuntimeError("fyers: the symbol master is not an object keyed by symbol")
        return parse_symbol_master(rows)

    def close(self) -> None:
        """Close the HTTP clients."""
        self._transport.close()
        self._http.close()
