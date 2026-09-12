"""Connection handling, pragmas and the conversion helpers the repos share."""

from __future__ import annotations

import datetime as dt
import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager

__all__ = [
    "Base",
    "NotFoundError",
    "StateNotFoundError",
    "format_date",
    "format_time",
    "parse_date",
    "parse_time",
]


class NotFoundError(LookupError):
    """A row asked for by key does not exist.

    Distinct from a query failure so that a cold start reads as normal rather
    than as an error to be logged.
    """


class StateNotFoundError(NotFoundError):
    """A key-value state key has never been written.

    One of the stores this replaces returned ``None`` for a missing key, which
    meant a caller could not tell "no state yet" from "state loaded into an
    untouched object" -- and a risk manager that cannot tell those apart
    silently starts the day with zeroed counters after a failed read.
    """


def format_time(t: dt.datetime) -> str:
    """Render a timestamp for storage: ISO-8601 with an explicit offset.

    Stored timestamps sort lexicographically and carry their zone, which is what
    lets range queries work without a conversion function in the SQL.
    """
    if t.tzinfo is None:
        raise ValueError("store: refusing to store a naive datetime; attach a timezone")
    return t.isoformat()


def parse_time(s: str) -> dt.datetime:
    """Read a stored timestamp."""
    return dt.datetime.fromisoformat(s)


def format_date(d: dt.date) -> str:
    """Render a calendar date for storage."""
    return d.isoformat()


def parse_date(s: str) -> dt.date:
    """Read a stored calendar date."""
    return dt.date.fromisoformat(s)


def to_int(flag: bool) -> int:
    """Convert a flag to the INTEGER SQLite stores it as."""
    return 1 if flag else 0


class Base:
    """Owns the connection and the pragmas.

    ``Database`` composes this with the repository mixins, so the connection is
    opened and configured in exactly one place.
    """

    def __init__(
        self,
        path: str,
        *,
        wal: bool = True,
        busy_timeout_ms: int = 5000,
        foreign_keys: bool = True,
    ) -> None:
        """Open ``path`` and apply the pragmas.

        Migrations are not run here, so a read-only tool can open a database
        without being able to change its shape. Call :meth:`migrate` for that.
        """
        self.conn = sqlite3.connect(path, isolation_level=None)
        self.conn.row_factory = sqlite3.Row
        if wal:
            self.conn.execute("PRAGMA journal_mode=WAL")
        if busy_timeout_ms > 0:
            self.conn.execute(f"PRAGMA busy_timeout={busy_timeout_ms}")
        if foreign_keys:
            self.conn.execute("PRAGMA foreign_keys=ON")

    def close(self) -> None:
        """Close the connection."""
        self.conn.close()

    def __enter__(self) -> Base:
        """Enter a context that closes the connection on exit."""
        return self

    def __exit__(self, *exc: object) -> None:
        """Close the connection."""
        self.close()

    @contextmanager
    def tx(self) -> Iterator[sqlite3.Connection]:
        """Run a block in a transaction, rolling back on any exception.

        ``isolation_level=None`` puts the connection in autocommit, so
        transactions are explicit here rather than implied by the driver's own
        heuristics about which statements begin one.
        """
        self.conn.execute("BEGIN")
        try:
            yield self.conn
        except BaseException:
            self.conn.execute("ROLLBACK")
            raise
        self.conn.execute("COMMIT")
