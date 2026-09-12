"""Upstox's public market-holiday calendar as a ``HolidaySource``.

Mirrors ``go/upstox/holidays.go``. ``GET /v2/market/holidays`` needs no
authentication, which is why the calendar is available before the daily
login and why a project that trades elsewhere can use it too.

This replaces three copies of the same fetch (chartinkbot, breakout500,
fanse), which between them had two readings of what "closed" means. This is
the strict one: an exchange is closed on a day when it appears in
``closed_exchanges`` and not in ``open_exchanges``. A ``SPECIAL_TIMING`` entry
such as the muhurat session lists NSE under both -- a trading day with
unusual hours, not a holiday.

It does not cache. Wrap it in :class:`tradekit.marketdata.reference.Holidays`
for the disk cache and the fail-open behaviour a live process wants.
"""

from __future__ import annotations

import datetime as dt
from typing import Any

import httpx

DEFAULT_HOLIDAYS_URL = "https://api.upstox.com/v2/market/holidays"


def _exchange_names(items: Any) -> set[str]:
    """Read an exchange list in either shape Upstox has used: strings, or objects with an ``exchange`` field."""
    names: set[str] = set()
    if not isinstance(items, list):
        return names
    for item in items:
        if isinstance(item, str):
            names.add(item)
        elif isinstance(item, dict) and item.get("exchange"):
            names.add(str(item["exchange"]))
    return names


def parse_holidays(payload: dict[str, Any], exchange: str) -> dict[str, str]:
    """Extract the days on which ``exchange`` is closed, keyed ``"YYYY-MM-DD"``.

    Raises:
        ValueError: If the calendar is empty. The endpoint has never
            legitimately returned one, and treating it as "no holidays" would
            silently make every holiday a trading day.

    """
    entries = payload.get("data") or []
    if not entries:
        raise ValueError("upstox: holiday calendar is empty")
    closed: dict[str, str] = {}
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        day = str(entry.get("date") or "")[:10]
        if len(day) < 10:
            continue
        if exchange in _exchange_names(entry.get("closed_exchanges")) and exchange not in _exchange_names(
            entry.get("open_exchanges")
        ):
            closed[day] = str(entry.get("description") or "")
    return closed


class Holidays:
    """A ``HolidaySource`` that fetches Upstox's calendar on every call."""

    def __init__(self, url: str = DEFAULT_HOLIDAYS_URL, client: httpx.Client | None = None) -> None:
        """Point at the endpoint; ``client`` lets a test inject a mock transport."""
        self.url = url
        self._client = client

    def closed(self, exchange: str, start: dt.date, end: dt.date) -> dict[str, str]:
        """Days in ``[start, end]`` on which ``exchange`` does not trade."""
        owns = self._client is None
        client = self._client or httpx.Client(timeout=30.0)
        try:
            response = client.get(self.url, headers={"accept": "application/json"})
            response.raise_for_status()
            payload = response.json()
        finally:
            if owns:
                client.close()
        lo, hi = start.isoformat(), end.isoformat()
        return {day: desc for day, desc in parse_holidays(payload, exchange).items() if lo <= day <= hi}
