"""The Upstox client: broker, quotes, history, GTT and reference data.

Mirrors ``go/upstox``. The client satisfies :class:`tradekit.core.ports.Broker`,
and separately ``Quoter``, ``HistoryProvider``, ``ProtectiveOrders``,
``MarginEstimator``, ``InstrumentSource`` and ``TokenState``; ask for a
capability with :func:`isinstance` against the protocol.

``TickObserver`` is deliberately not implemented, as in Go: an Upstox stop rests
at the broker and triggers without the engine's help.
"""

from __future__ import annotations

import csv
import datetime as dt
import gzip
import io
import urllib.parse
from collections.abc import Callable, Iterator, Sequence
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
    GTT_MULTIPLE,
    GTT_SINGLE,
    IST,
    RULE_ENTRY,
    RULE_STOPLOSS,
    RULE_TARGET,
    TRIGGER_ABOVE,
    TRIGGER_BELOW,
    chunk_days,
    exchange_for,
    from_order_type,
    from_product,
    from_transaction_type,
    from_validity,
    normalize_status,
    paise,
    parse_instrument_key,
    parse_upstox_time,
    rupees,
    to_interval,
    to_order_type,
    to_product,
    to_transaction_type,
    to_validity,
)
from .transport import Transport

__all__ = ["BASE_URL", "BASE_URL_V3", "INSTRUMENTS_URL", "UpstoxClient"]

BASE_URL = "https://api.upstox.com/v2"
"""The v2 root: auth, quotes, the order book, positions, funds, margins."""

BASE_URL_V3 = "https://api.upstox.com/v3"
"""The v3 root: historical candles, order placement, GTT.

The split is real rather than a migration half-done. breakout500 runs against a
live account daily and reads order details on v2 while placing orders on v3.
"""

INSTRUMENTS_URL = "https://assets.upstox.com/market-quote/instruments/exchange/NSE.csv.gz"
"""The instrument master: a gzipped CSV on the assets host, needing no token."""

LTP_CHUNK_SIZE = 30
QUOTE_CHUNK_SIZE = 500

TERMINAL_GTT_STATUSES = frozenset(
    {"CANCELLED", "CANCELED", "TRIGGERED", "COMPLETED", "COMPLETE", "EXPIRED", "REJECTED"}
)
"""Rule states meaning a trigger is no longer protecting anything.

breakout500's reconciler records, verified against a live account, that the GTT
book returns cancelled triggers alongside live ones. Reading the book as a set
of live stops is wrong in the silent direction: a dead trigger still looks like
protection, so a missing stop is never noticed.
"""


def _chunked(items: Sequence[str], size: int) -> Iterator[list[str]]:
    """Yield ``items`` in lists of at most ``size``."""
    for start in range(0, len(items), size):
        yield list(items[start : start + size])


def _first(*values: str) -> str:
    """The first non-empty string.

    Upstox spells the instrument-key field ``instrument_token`` on some
    endpoints and ``instrument_key`` on others; both are read rather than one
    trusted.
    """
    for value in values:
        if value:
            return value
    return ""


class UpstoxClient:
    """A tradekit adapter for the Upstox REST API."""

    def __init__(
        self,
        *,
        api_key: str,
        instrument_key: Callable[[InstrumentKey], str],
        api_secret: str = "",
        redirect_uri: str = "",
        access_token: str = "",
        token_issued_at: dt.datetime | None = None,
        tag: str = "",
        transport: Transport | None = None,
        base_url: str = BASE_URL,
        base_url_v3: str = BASE_URL_V3,
        instruments_url: str = INSTRUMENTS_URL,
        http_client: httpx.Client | None = None,
        **transport_kwargs: Any,
    ) -> None:
        """Build a client. It does not contact the broker.

        Args:
            api_key: From the Upstox developer app.
            instrument_key: Resolves an instrument to Upstox's ``SEGMENT|ID``
                key. Upstox addresses cash equity by ISIN
                (``NSE_EQ|INE002A01018``) and derivatives by exchange token,
                neither of which the adapter can derive from an exchange and a
                symbol. It is injected rather than looked up here so the adapter
                does not depend on the store: wire it to the store's
                ``broker_id``.
            api_secret: Needed only to exchange an authorisation code.
            redirect_uri: Must match the app's configured redirect exactly.
            access_token: A token from a completed login, valid one trading day.
            token_issued_at: When that token was obtained. Drives
                :meth:`token_fresh`; ``None`` means unknown, which reads stale.
            tag: Attached to every order, so orders this system placed can be
                told from ones placed by hand.
            transport: A pre-built transport. Tests inject one carrying an
                ``httpx.MockTransport``.
            base_url: The v2 API root.
            base_url_v3: The v3 API root.
            instruments_url: Where the instrument master is fetched from.
            http_client: The client used for the instrument master, which is on
                a different host and needs no token.
            **transport_kwargs: Passed to :class:`Transport` when none is given.

        """
        self._api_key = api_key
        self._api_secret = api_secret
        self._redirect_uri = redirect_uri
        self._access_token = access_token
        self._token_issued_at = token_issued_at
        self._tag = tag
        self._instrument_key = instrument_key
        self._instruments_url = instruments_url
        self._http = http_client or httpx.Client(timeout=60.0)
        self._transport = transport or Transport(
            token_provider=lambda: self._access_token,
            base_url=base_url,
            base_url_v3=base_url_v3,
            **transport_kwargs,
        )

    # --- auth ---------------------------------------------------------

    def login_url(self) -> str:
        """The URL a user visits to authorise the app and obtain a code."""
        query = urllib.parse.urlencode(
            {"client_id": self._api_key, "redirect_uri": self._redirect_uri, "response_type": "code"}
        )
        return f"{self._transport.base_url}/login/authorization/dialog?{query}"

    def login(self, auth_code: str) -> str:
        """Exchange an authorisation code for an access token and install it.

        Upstox tokens are valid for one trading day, so this runs once each
        morning; the returned token is what a scheduler stores and passes back
        on the next start.

        The exchange is form-encoded rather than JSON -- the one call on the API
        that is -- and is never retried, because an authorisation code is
        single-use.

        Returns:
            The access token, so the caller can persist it.

        Raises:
            RuntimeError: If no API secret is configured, or the exchange
                returns no token.

        """
        if not self._api_secret:
            raise RuntimeError("upstox: an API secret is required to exchange an authorisation code")
        response = self._http.post(
            f"{self._transport.base_url}/login/authorization/token",
            data={
                "code": auth_code,
                "client_id": self._api_key,
                "client_secret": self._api_secret,
                "redirect_uri": self._redirect_uri,
                "grant_type": "authorization_code",
            },
            headers={"Content-Type": "application/x-www-form-urlencoded", "accept": "application/json"},
        )
        try:
            payload = response.json() if response.content else {}
        except ValueError:
            # A refusal is not always JSON; the status and text below say
            # enough, and a decode error would hide them.
            payload = {}
        token = str(payload.get("access_token") or "")
        if response.status_code >= 400 or not token:
            raise RuntimeError(f"upstox: token exchange failed (HTTP {response.status_code}): {response.text[:300]}")
        self.set_access_token(token, dt.datetime.now(dt.UTC))
        return token

    def set_access_token(self, token: str, issued_at: dt.datetime) -> None:
        """Install a token and record when it was issued."""
        self._access_token = token
        self._token_issued_at = issued_at

    def token_fresh(self) -> bool:
        """Whether the access token is valid for the current trading day.

        Upstox tokens expire overnight, so a scheduler asks this before the open
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
        """The equity segment's funds.

        Commodity margins are deliberately not folded in: an equity strategy
        sizing against a combined figure would size against capital it cannot
        use for the instrument it is about to trade.

        Upstox reports what is available and what is used but no explicit equity
        figure, so equity is their sum -- the capital the segment holds, whether
        or not it is currently committed.

        Raises:
            RuntimeError: If the response carries no segment at all. Reporting
                zero equity would read as a wiped account and stop every
                strategy at once.

        """
        payload = self._transport.request("GET", "/user/get-funds-and-margin", params={"segment": "SEC"}, retry=True)
        data = payload.get("data") or {}
        segment = data.get("equity")
        if segment is None:
            if not data:
                raise RuntimeError("upstox: funds response carried no segment data")
            segment = next(iter(data.values()))
        available = paise(segment.get("available_margin"))
        used = paise(segment.get("used_margin"))
        return Account(equity=Money(available + used), available=available, used=used)

    def place_order(self, req: OrderRequest) -> Order:
        """Submit an order.

        The returned order carries Upstox's id and a pending status: an
        acknowledgement is not a fill, and treating it as one is how a
        reconciler comes to believe in positions that do not exist. Call
        :meth:`order_status` to learn what actually happened.

        Not retried. Retrying a POST that placed an order places a second one.

        Raises:
            ValueError: For a request Upstox cannot express, or one missing a
                price its order type requires.
            RuntimeError: If the acknowledgement carries no order id.

        """
        body = self._order_body(req)
        payload = self._transport.request(
            "POST", "/order/place", base=self._transport.base_url_v3, json_body=body, retry=False
        )
        order_id = self._ack_id(payload.get("data") or {}, "order_id", "order_ids")
        if not order_id:
            raise RuntimeError(f"upstox: placing {req.side} {req.quantity} {req.key}: response carried no order id")
        now = dt.datetime.now(dt.UTC)
        return Order(
            id=order_id,
            request=req,
            status=OrderStatus.PENDING,
            placed_at=now,
            updated_at=now,
        )

    def _order_body(self, req: OrderRequest) -> dict[str, Any]:
        """Translate a request into Upstox's payload.

        ``instrument_token`` carries the instrument *key* despite the name; that
        is what Upstox calls the field, and every donor sends the key in it.
        """
        if req.quantity <= 0:
            raise ValueError(f"upstox: quantity must be positive, got {req.quantity}")
        body: dict[str, Any] = {
            "quantity": req.quantity,
            "product": to_product(req.product),
            "validity": to_validity(req.time_in_force),
            "price": 0.0,
            "instrument_token": self._key_for(req.key),
            "order_type": to_order_type(req.type),
            "transaction_type": to_transaction_type(req.side),
            "disclosed_quantity": 0,
            "trigger_price": 0.0,
            # is_amo is documented but dead: Upstox answers UDAPI1162 to any
            # order placed while the market is closed whatever the flag says,
            # verified against a live account. Sent false because the field is
            # required, not because any value of it helps.
            "is_amo": False,
        }
        tag = req.tag or self._tag
        if tag:
            body["tag"] = tag

        if req.type == OrderType.LIMIT:
            if req.limit_price <= 0:
                raise ValueError("upstox: a limit order needs a limit price")
            body["price"] = rupees(req.limit_price)
        elif req.type == OrderType.STOP:
            if req.trigger_price <= 0:
                raise ValueError("upstox: a stop order needs a trigger price")
            body["trigger_price"] = rupees(req.trigger_price)
        elif req.type == OrderType.STOP_LIMIT:
            if req.trigger_price <= 0 or req.limit_price <= 0:
                raise ValueError("upstox: a stop-limit order needs both a trigger and a limit price")
            body["trigger_price"] = rupees(req.trigger_price)
            body["price"] = rupees(req.limit_price)
        return body

    @staticmethod
    def _ack_id(data: dict[str, Any], *names: str) -> str:
        """Read an id from an acknowledgement, accepting the list spelling.

        The v3 endpoints answer with a list (``order_ids``) and the v2 ones with
        a single value; breakout500's live path accepts either, having met both.
        """
        for name in names:
            value = data.get(name)
            if isinstance(value, list):
                if value:
                    return str(value[0])
                continue
            if value:
                return str(value)
        return ""

    def cancel_order(self, order_id: str) -> None:
        """Withdraw an open order.

        Not retried: a cancel that succeeded but whose response was lost would,
        on a second attempt, be refused for an order that no longer exists --
        and that refusal reaches the caller as a failed cancel, which is the
        dangerous direction to be wrong in.
        """
        self._transport.request(
            "DELETE",
            "/order/cancel",
            base=self._transport.base_url_v3,
            params={"order_id": order_id},
            retry=False,
        )

    def order_status(self, order_id: str) -> Order:
        """One order's current state.

        Raises:
            RuntimeError: If Upstox does not know the order. Returning an empty
                order would read as one that is pending and never settles.

        """
        payload = self._transport.request("GET", "/order/details", params={"order_id": order_id}, retry=True)
        row = payload.get("data") or {}
        if not row.get("order_id") and not row.get("status"):
            raise RuntimeError(f"upstox: order {order_id} not found")
        order = self._to_order(row)
        return order if order.id else Order(**{**order.__dict__, "id": order_id})

    def open_orders(self) -> list[Order]:
        """Orders that have not reached a terminal state."""
        payload = self._transport.request("GET", "/order/retrieve-all", retry=True)
        rows = payload.get("data")
        rows = rows if isinstance(rows, list) else []
        return [order for order in (self._to_order(row) for row in rows) if not order.status.terminal]

    def _to_order(self, row: dict[str, Any]) -> Order:
        """Convert an Upstox order row into the domain's."""
        placed = parse_upstox_time(str(row.get("order_timestamp") or ""))
        return Order(
            id=str(row.get("order_id") or ""),
            request=OrderRequest(
                key=self._key_from_row(row),
                side=from_transaction_type(str(row.get("transaction_type") or "")),
                quantity=int(row.get("quantity") or 0),
                type=from_order_type(str(row.get("order_type") or "")),
                product=from_product(str(row.get("product") or "")),
                limit_price=paise(row.get("price")),
                trigger_price=paise(row.get("trigger_price")),
                time_in_force=from_validity(str(row.get("validity") or "")),
                tag=str(row.get("tag") or ""),
            ),
            status=normalize_status(str(row.get("status") or "")),
            filled_quantity=int(row.get("filled_quantity") or 0),
            average_price=paise(row.get("average_price")),
            placed_at=placed,
            updated_at=placed,
            message=str(row.get("status_message") or ""),
        )

    @staticmethod
    def _key_from_row(row: dict[str, Any]) -> InstrumentKey:
        """Reconstruct an instrument key from whatever a response row carried.

        The exchange and tradingsymbol fields are preferred; when a row names
        only the instrument key, the segment gives the exchange and the id
        stands in for the symbol. That id is an ISIN for equity, which is not a
        tradingsymbol -- but a key that names the right instrument in an unusual
        way is more useful than an empty one.
        """
        exchange = str(row.get("exchange") or "")
        symbol = _first(str(row.get("trading_symbol") or ""), str(row.get("tradingsymbol") or ""))
        if exchange and symbol:
            return InstrumentKey(exchange=exchange_for(exchange), symbol=symbol)
        raw = _first(str(row.get("instrument_token") or ""), str(row.get("instrument_key") or ""))
        try:
            segment, instrument_id = parse_instrument_key(raw)
        except ValueError:
            return InstrumentKey(exchange=exchange_for(exchange), symbol=symbol)
        return InstrumentKey(exchange=exchange_for(segment), symbol=symbol or instrument_id)

    def positions(self) -> list[Position]:
        """The open positions.

        short-term-positions, not long-term-holdings: the port means positions,
        and holdings are a separate concept. A strategy reading holdings would
        believe it is flat while holding an intraday position.

        Zero-quantity rows are dropped. Upstox keeps a closed position's row for
        the rest of the day; it is history, not a holding.
        """
        payload = self._transport.request("GET", "/portfolio/short-term-positions", retry=True)
        rows = payload.get("data")
        rows = rows if isinstance(rows, list) else []
        out = []
        for row in rows:
            quantity = int(row.get("quantity") or 0)
            if quantity == 0:
                continue
            out.append(
                Position(
                    key=self._key_from_row(row),
                    quantity=quantity,
                    average_price=paise(row.get("average_price")),
                    product=from_product(str(row.get("product") or "")),
                    realized_pnl=paise(row.get("realised")),
                    unrealized_pnl=paise(row.get("unrealised")),
                )
            )
        return out

    def basket_margin(self, legs: list[MarginLeg]) -> Money:
        """What a basket would block.

        ``final_margin`` is the figure after offsetting benefit -- the amount
        actually blocked -- and is what a risk check must size against.
        ``required_margin`` is the gross number before benefit and would refuse
        spreads the account can afford.
        """
        if not legs:
            return Money(0)
        instruments = [
            {
                "instrument_key": self._key_for(leg.key),
                "quantity": leg.quantity,
                "transaction_type": to_transaction_type(leg.side),
                "product": to_product(leg.product),
            }
            for leg in legs
        ]
        payload = self._transport.request("POST", "/charges/margin", json_body={"instruments": instruments}, retry=True)
        return paise((payload.get("data") or {}).get("final_margin"))

    # --- protective orders --------------------------------------------

    def place_protective(self, p: Protective) -> str:
        """Rest a stop, or a stop and target as an OCO pair, as an Upstox GTT.

        A GTT rests at Upstox rather than at the exchange and survives the
        process exiting, which is the whole reason a swing strategy uses one
        instead of watching prices itself.

        The trigger direction follows the protective order's own side rather
        than the leg's role: a sell that protects a long stops BELOW and targets
        ABOVE, and a buy that protects a short is the mirror image. Assigning by
        role would turn a short's stop into a target.

        Not retried: a retry landing after a lost acknowledgement rests a second
        stop on the same position, which sells the position twice when it fires.

        Returns:
            The trigger id, so the stop can later be modified.

        Raises:
            ValueError: For a protective order with no level, or a non-positive
                quantity.
            RuntimeError: If the acknowledgement carries no trigger id.

        """
        body = self._gtt_body(p)
        body["instrument_token"] = self._key_for(p.key)
        body["transaction_type"] = to_transaction_type(p.side)
        body["product"] = to_product(p.product)
        payload = self._transport.request(
            "POST", "/order/gtt/place", base=self._transport.base_url_v3, json_body=body, retry=False
        )
        gtt_id = self._ack_id(payload.get("data") or {}, "gtt_order_id", "gtt_order_ids", "order_id")
        if not gtt_id:
            raise RuntimeError(f"upstox: placing GTT for {p.key}: response carried no trigger id")
        return gtt_id

    @staticmethod
    def _gtt_body(p: Protective) -> dict[str, Any]:
        """The type, quantity and rules of a trigger.

        The instrument, side and product are filled in by the caller, because
        the modify endpoint takes none of them.
        """
        if p.quantity <= 0:
            raise ValueError(f"upstox: protective quantity must be positive, got {p.quantity}")
        if p.stop <= 0 and p.target <= 0:
            raise ValueError("upstox: a protective order needs a stop or a target")

        if p.oco:
            return {
                "type": GTT_MULTIPLE,
                "quantity": p.quantity,
                "rules": [
                    {
                        "strategy": RULE_STOPLOSS,
                        "trigger_type": _stop_direction(p.side),
                        "trigger_price": rupees(p.stop),
                    },
                    {
                        "strategy": RULE_TARGET,
                        "trigger_type": _target_direction(p.side),
                        "trigger_price": rupees(p.target),
                    },
                ],
            }
        level, direction = (p.stop, _stop_direction(p.side)) if p.stop > 0 else (p.target, _target_direction(p.side))
        return {
            "type": GTT_SINGLE,
            "quantity": p.quantity,
            "rules": [{"strategy": RULE_ENTRY, "trigger_type": direction, "trigger_price": rupees(level)}],
        }

    def modify_stop(self, protective_id: str, stop: Money) -> None:
        """Move a resting trigger's stop, as a trailing adjustment does.

        Upstox replaces the whole rule set rather than patching one leg, so the
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
        body = self._gtt_body(updated)
        body["gtt_order_id"] = protective_id
        self._transport.request(
            "PUT", "/order/gtt/modify", base=self._transport.base_url_v3, json_body=body, retry=False
        )

    def cancel_protective(self, protective_id: str) -> None:
        """Delete a resting trigger.

        The id goes in a JSON body, not the query string. breakout500's client
        records that the query form -- which looks obviously right -- is
        answered with UDAPI100038 and no indication of which input was wrong.
        The cost of getting this wrong is quiet: trailing raises a stop by
        cancelling and re-placing, so a cancel that always fails leaves every
        position on its original stop while the book reports a trailed one.
        """
        self._transport.request(
            "DELETE",
            "/order/gtt/cancel",
            base=self._transport.base_url_v3,
            json_body={"gtt_order_id": protective_id},
            retry=False,
        )

    def list_protective(self) -> list[Protective]:
        """The triggers that are still live.

        Triggered and cancelled rows stay in Upstox's book; returning them would
        make a reconciler believe a position is protected by a trigger that has
        already fired.
        """
        return [self._from_gtt(row) for row in self._gtt_book() if _gtt_is_live(row)]

    def _gtt_book(self) -> list[dict[str, Any]]:
        """The raw trigger book."""
        payload = self._transport.request("GET", "/order/gtt", base=self._transport.base_url_v3, retry=True)
        rows = payload.get("data")
        return rows if isinstance(rows, list) else []

    def _protective(self, protective_id: str) -> Protective:
        """One trigger by id.

        Upstox has no single-trigger endpoint, so the book is fetched and
        filtered. It is one request either way, which is what makes
        :meth:`modify_stop`'s read-before-write cost nothing extra.
        """
        for row in self._gtt_book():
            if _gtt_id(row) == protective_id:
                return self._from_gtt(row)
        raise RuntimeError(f"upstox: GTT {protective_id} is not in the trigger book")

    def _from_gtt(self, row: dict[str, Any]) -> Protective:
        """Convert a trigger row into a Protective.

        The rules are read by strategy where Upstox names one, and by trigger
        direction where it does not: a single-leg rule is labelled ENTRY
        whatever it protects, so the direction relative to the order's own side
        is what says whether it is the stop or the target.
        """
        side = from_transaction_type(str(row.get("transaction_type") or ""))
        stop = Money(0)
        target = Money(0)
        rules = row.get("rules")
        for rule in rules if isinstance(rules, list) else []:
            if not isinstance(rule, dict):
                continue
            price = paise(rule.get("trigger_price"))
            strategy = str(rule.get("strategy") or "").strip().upper()
            if strategy == RULE_STOPLOSS:
                stop = price
            elif strategy == RULE_TARGET:
                target = price
            elif str(rule.get("trigger_type") or "").strip().upper() == _stop_direction(side):
                stop = price
            else:
                target = price
        return Protective(
            id=_gtt_id(row),
            key=self._key_from_row(row),
            side=side,
            quantity=int(row.get("quantity") or 0),
            stop=stop,
            target=target,
            product=from_product(str(row.get("product") or "")),
        )

    # --- market data --------------------------------------------------

    def ltp(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Money]:
        """The last traded price for each instrument.

        Instruments Upstox does not recognise are omitted rather than raising,
        so one delisted symbol cannot fail a 500-symbol scan.
        """
        by_key, order = self._resolve_keys(keys)
        out: dict[InstrumentKey, Money] = {}
        for chunk in _chunked(order, LTP_CHUNK_SIZE):
            payload = self._transport.request(
                "GET", "/market-quote/ltp", params={"instrument_key": ",".join(chunk)}, retry=True
            )
            for entry in (payload.get("data") or {}).values():
                if not isinstance(entry, dict):
                    continue
                key = by_key.get(_entry_key(entry))
                if key is None:
                    continue
                out[key] = paise(entry.get("last_price"))
        return out

    def quote(self, keys: list[InstrumentKey]) -> dict[InstrumentKey, Quote]:
        """The still-forming session's OHLC and last price.

        The response is keyed by ``"NSE_EQ:RELIANCE"``, not by the instrument
        key that was asked for, so entries are re-keyed from the field each one
        carries. An entry naming no key at all is dropped: a quote that cannot
        be attributed is worse than a missing one, since a screener would size a
        spike against another company's history.
        """
        by_key, order = self._resolve_keys(keys)
        now = dt.datetime.now(dt.UTC)
        out: dict[InstrumentKey, Quote] = {}
        for chunk in _chunked(order, QUOTE_CHUNK_SIZE):
            payload = self._transport.request(
                "GET", "/market-quote/quotes", params={"instrument_key": ",".join(chunk)}, retry=True
            )
            for entry in (payload.get("data") or {}).values():
                if not isinstance(entry, dict):
                    continue
                key = by_key.get(_entry_key(entry))
                if key is None:
                    continue
                ohlc = entry.get("ohlc") or {}
                depth = entry.get("depth") or {}
                buy = depth.get("buy") or []
                sell = depth.get("sell") or []
                at = parse_upstox_time(str(entry.get("last_trade_time") or "")) or now
                out[key] = Quote(
                    key=key,
                    at=at,
                    last=paise(entry.get("last_price")),
                    open=paise(ohlc.get("open")),
                    high=paise(ohlc.get("high")),
                    low=paise(ohlc.get("low")),
                    # ohlc.close is the previous session's close, which is what
                    # Quote.close means. Conflating it with the last price makes
                    # every gap calculation silently zero.
                    close=paise(ohlc.get("close")),
                    bid=paise(buy[0].get("price")) if buy else Money(0),
                    ask=paise(sell[0].get("price")) if sell else Money(0),
                )
        return out

    def candles(self, key: InstrumentKey, timeframe: Timeframe, start: dt.datetime, end: dt.datetime) -> list[Candle]:
        """Bars over ``[start, end]``, oldest first.

        Upstox serves a bounded span per request -- minute data is refused
        beyond roughly a month with UDAPI1148 -- so a long backfill is fetched
        as a series of chunks and concatenated. The caller asks for ten years
        and does not have to know that.

        Raises:
            ValueError: For an inverted range. Upstox answers one with an empty
                series, which reads as "no trades".

        """
        unit, interval = to_interval(timeframe)
        if end < start:
            raise ValueError(f"upstox: candle range ends ({end.date()}) before it starts ({start.date()})")
        resolved = self._key_for(key)
        span = dt.timedelta(days=chunk_days(unit, interval))

        out: list[Candle] = []
        window_start = start
        while window_start <= end:
            window_end = min(window_start + span - dt.timedelta(seconds=1), end)
            # The path takes the LATER date first. Both Go and Python donors
            # order it this way; reversing it returns an empty series rather
            # than an error, which is why it is worth a comment.
            path = (
                f"/historical-candle/{urllib.parse.quote(resolved, safe='')}"
                f"/{unit}/{interval}/{window_end.date().isoformat()}/{window_start.date().isoformat()}"
            )
            out.extend(self._candles(path, self._transport.base_url_v3, key, timeframe))
            window_start += span
        return out

    def intraday_candles(self, key: InstrumentKey, timeframe: Timeframe) -> list[Candle]:
        """Today's bars so far.

        The historical endpoint serves only completed sessions, so a screener
        running at 09:46 cannot use it: the session it needs is the one still
        running. This is the separate path for that.
        """
        unit, interval = to_interval(timeframe)
        path = f"/historical-candle/intraday/{urllib.parse.quote(self._key_for(key), safe='')}/{unit}/{interval}"
        return self._candles(path, self._transport.base_url_v3, key, timeframe)

    def expired_expiries(self, underlying_key: str) -> list[str]:
        """The expiry dates available for an underlying, as ISO date strings.

        Upstox documents this as covering roughly the last six months, which
        bounds any option-level backtest built on it.
        """
        payload = self._transport.request(
            "GET",
            "/expired-instruments/expiries",
            base=self._transport.base_url_v3,
            params={"instrument_key": underlying_key},
            retry=True,
        )
        data = payload.get("data")
        return [str(value) for value in data] if isinstance(data, list) else []

    def expired_option_contracts(self, underlying_key: str, expiry: str) -> list[dict[str, Any]]:
        """The contracts that expired on a date, with strikes and lot sizes."""
        payload = self._transport.request(
            "GET",
            "/expired-instruments/option/contract",
            base=self._transport.base_url_v3,
            params={"instrument_key": underlying_key, "expiry_date": expiry},
            retry=True,
        )
        data = payload.get("data")
        return [row for row in data if isinstance(row, dict)] if isinstance(data, list) else []

    def expired_historical_candles(
        self, expired_key: str, timeframe: Timeframe, start: dt.datetime, end: dt.datetime
    ) -> list[Candle]:
        """Bars for an already-settled contract.

        Keyed by the ``NSE_FO|47983|17-04-2025`` form
        :meth:`expired_option_contracts` returns. This is the only route in the
        whole codebase to backtesting options that have already expired.

        Unlike the live historical endpoint it needs both a valid token and an
        Upstox Plus entitlement; without one the call fails with an
        authorisation error rather than returning an empty series, so a caller
        can tell "not entitled" from "no trades that day".

        It is served from the v2 root while its sibling listing endpoints are
        v3 -- verified in zerobha, not assumed.
        """
        unit, interval = to_interval(timeframe)
        if end < start:
            raise ValueError(f"upstox: candle range ends ({end.date()}) before it starts ({start.date()})")
        segment, instrument_id = parse_instrument_key(expired_key)
        key = InstrumentKey(exchange=exchange_for(segment), symbol=instrument_id)
        path = (
            f"/expired-instruments/historical-candle/{urllib.parse.quote(expired_key, safe='')}"
            f"/{unit}/{interval}/{end.date().isoformat()}/{start.date().isoformat()}"
        )
        return self._candles(path, self._transport.base_url, key, timeframe)

    def _candles(self, path: str, base: str, key: InstrumentKey, timeframe: Timeframe) -> list[Candle]:
        """Perform one candle request and convert the positional arrays.

        Upstox returns bars newest first; they come back oldest first, which is
        the order a backtest replays and every indicator assumes.

        A malformed row is skipped rather than failing the batch: one
        unparseable bar in a five-year backfill is a gap, and refusing the whole
        range over it means no history at all.
        """
        payload = self._transport.request("GET", path, base=base, retry=True)
        rows = (payload.get("data") or {}).get("candles") or []
        out: list[Candle] = []
        for row in reversed(rows):
            if not isinstance(row, list) or len(row) < 6:
                continue
            try:
                start = dt.datetime.fromisoformat(str(row[0]))
            except (ValueError, TypeError):
                continue
            out.append(
                Candle(
                    key=key,
                    timeframe=timeframe,
                    start=start,
                    open=paise(_number(row, 1)),
                    high=paise(_number(row, 2)),
                    low=paise(_number(row, 3)),
                    close=paise(_number(row, 4)),
                    volume=int(_number(row, 5)),
                    open_interest=int(_number(row, 6)),
                )
            )
        return out

    def instruments(self, exchange: str = "NSE") -> list[Instrument]:
        """The instrument master for an exchange.

        The master is a gzipped CSV on the assets host and needs no token, so it
        is fetched directly rather than through the transport. It is measured in
        megabytes, so it belongs in a daily sync and not in a request path.
        """
        source = self._instruments_url
        if exchange and exchange.upper() != "NSE" and source == INSTRUMENTS_URL:
            source = source.replace("/NSE.csv.gz", f"/{exchange.upper()}.csv.gz")
        response = self._http.get(source)
        response.raise_for_status()
        return parse_instrument_csv(response.content)

    # --- helpers ------------------------------------------------------

    def _key_for(self, key: InstrumentKey) -> str:
        """Resolve the ``SEGMENT|ID`` key every endpoint addresses by."""
        resolved = self._instrument_key(key)
        if not resolved:
            raise RuntimeError(f"upstox: no instrument key known for {key}")
        return resolved

    def _resolve_keys(self, keys: list[InstrumentKey]) -> tuple[dict[str, InstrumentKey], list[str]]:
        """Map each requested instrument to its Upstox key and back again."""
        by_key: dict[str, InstrumentKey] = {}
        order: list[str] = []
        for key in keys:
            resolved = self._key_for(key)
            if resolved in by_key:
                continue
            by_key[resolved] = key
            order.append(resolved)
        return by_key, order

    def close(self) -> None:
        """Close the underlying HTTP clients."""
        self._transport.close()
        self._http.close()


def _stop_direction(side: Side) -> str:
    """The stop leg's trigger direction for a protective order's side.

    A sell protects a long and stops below it.
    """
    return TRIGGER_BELOW if side == Side.SELL else TRIGGER_ABOVE


def _target_direction(side: Side) -> str:
    """The mirror of :func:`_stop_direction`."""
    return TRIGGER_ABOVE if side == Side.SELL else TRIGGER_BELOW


def _gtt_id(row: dict[str, Any]) -> str:
    """Whichever spelling of the trigger id the row carried."""
    return _first(str(row.get("gtt_order_id") or ""), str(row.get("order_id") or ""))


def _gtt_is_live(row: dict[str, Any]) -> bool:
    """Whether a trigger is still protecting a position.

    A row with no rules to judge on is treated as live, and so is a rule whose
    status is one this adapter has not seen. An unknown status raising a false
    "stop is missing" on every position every morning would bury the real
    alerts, which is the failure this distinction exists to avoid.
    """
    rules = row.get("rules")
    if not isinstance(rules, list) or not rules:
        return True
    for rule in rules:
        if not isinstance(rule, dict):
            return True
        status = str(rule.get("status") or "").strip().upper()
        if not status or status not in TERMINAL_GTT_STATUSES:
            return True
    return False


def _entry_key(entry: dict[str, Any]) -> str:
    """The instrument key a quote entry names, under either spelling."""
    return _first(str(entry.get("instrument_token") or ""), str(entry.get("instrument_key") or ""))


def _number(row: list[Any], index: int) -> float:
    """Read a positional candle field as a float.

    Open interest is absent for cash equity, which is a zero and not an error.
    """
    if index >= len(row):
        return 0.0
    try:
        return float(row[index])
    except (TypeError, ValueError):
        return 0.0


def parse_instrument_csv(raw: bytes) -> list[Instrument]:
    """Parse the instrument master, gzipped or not.

    Whether the body arrives compressed depends on the hop, so the gzip magic
    number is sniffed rather than the URL suffix or the headers trusted.

    A malformed row is skipped rather than failing the batch. The master carries
    tens of thousands of rows and one bad line -- a field the exchange added
    this morning -- must not cost a sync its whole universe.
    """
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    reader = csv.DictReader(io.StringIO(raw.decode("utf-8", errors="replace")))

    out: list[Instrument] = []
    for row in reader:
        cells = {(k or "").strip().lower(): (v or "").strip() for k, v in row.items()}
        symbol = _first(cells.get("tradingsymbol", ""), cells.get("trading_symbol", ""))
        raw_key = cells.get("instrument_key", "")
        if not symbol or not raw_key:
            continue
        try:
            segment, _ = parse_instrument_key(raw_key)
        except ValueError:
            continue

        instrument_type = cells.get("instrument_type", "")
        lot_size = _int_or(cells.get("lot_size", ""), 1)
        out.append(
            Instrument(
                key=InstrumentKey(exchange=exchange_for(segment), symbol=symbol),
                name=cells.get("name", ""),
                isin=cells.get("isin", ""),
                segment=segment_of(instrument_type, segment),
                # The master reports 0 for cash equity; the domain uses 1 so
                # sizing can multiply by it unconditionally.
                lot_size=lot_size if lot_size >= 1 else 1,
                tick_size=paise(_float_or(cells.get("tick_size", ""), 0.0)),
                expiry=_parse_expiry(_first(cells.get("expiry", ""))),
                strike=paise(_float_or(_first(cells.get("strike", ""), cells.get("strike_price", "")), 0.0)),
                option_type=option_type_of(instrument_type),
                active=True,
            )
        )
    return out


def segment_of(instrument_type: str, segment: str) -> str:
    """Classify an instrument type into the domain's vocabulary.

    The CSV master spells cash equity ``"EQUITY"`` and the JSON master spells it
    ``"EQ"``; both are accepted, because a project switching masters must not
    silently reclassify its whole universe.
    """
    up = instrument_type.upper()
    if up in ("CE", "PE", "OPTIDX", "OPTSTK"):
        return "options"
    if up in ("FUT", "FUTIDX", "FUTSTK", "FUTCUR"):
        return "futures"
    if segment.upper().endswith("_INDEX"):
        return "index"
    return "equity"


def option_type_of(instrument_type: str) -> str:
    """The domain's option marker, or an empty string for a non-option."""
    up = instrument_type.upper()
    return up.lower() if up in ("CE", "PE") else ""


def _parse_expiry(value: str) -> dt.date | None:
    """Read the master's expiry column.

    Epoch milliseconds in the JSON feed, a date string in the CSV one. An
    unparseable or absent value returns ``None``, which is what a cash
    instrument carries.
    """
    if not value or value == "0":
        return None
    try:
        return dt.datetime.fromtimestamp(int(value) / 1000, tz=dt.UTC).date()
    except (ValueError, OverflowError, OSError):
        pass
    try:
        return dt.datetime.fromisoformat(value).date()
    except ValueError:
        return None


def _int_or(value: str, fallback: int) -> int:
    """Parse an integer, falling back when the field is absent or malformed."""
    try:
        return int(float(value))
    except (TypeError, ValueError):
        return fallback


def _float_or(value: str, fallback: float) -> float:
    """Parse a float, falling back when the field is absent or malformed."""
    try:
        return float(value)
    except (TypeError, ValueError):
        return fallback
