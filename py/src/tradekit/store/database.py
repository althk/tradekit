"""The ``Database`` handle: one class composed from the repository mixins.

Composing mixins rather than exposing separate repository objects keeps the
method names aligned with the Go library one for one -- Go's
``db.UpsertInstruments`` is ``db.upsert_instruments`` here -- while letting each
group of queries live in its own module.
"""

from __future__ import annotations

from ._base import Base
from .migrations import MigrationMixin
from .repos import CandleMixin, InstrumentMixin, MetaMixin, TradingMixin

__all__ = ["Database", "connect", "connect_migrated"]


class Database(MigrationMixin, InstrumentMixin, CandleMixin, TradingMixin, MetaMixin, Base):
    """A SQLite database with tradekit's pragmas, schema and queries."""


def connect(
    path: str,
    *,
    wal: bool = True,
    busy_timeout_ms: int = 5000,
    foreign_keys: bool = True,
) -> Database:
    """Open a database and apply the pragmas.

    Migrations are not run here, so a read-only tool can open a database without
    being able to change its shape. Call :meth:`Database.migrate` for that.
    """
    return Database(path, wal=wal, busy_timeout_ms=busy_timeout_ms, foreign_keys=foreign_keys)


def connect_migrated(
    path: str,
    *,
    wal: bool = True,
    busy_timeout_ms: int = 5000,
    foreign_keys: bool = True,
) -> Database:
    """:func:`connect` followed by :meth:`Database.migrate`, for the process that owns the database.

    It is the first thing every bot's main does; a read-only tool keeps using
    :func:`connect` so it cannot change the file's shape.
    """
    db = connect(path, wal=wal, busy_timeout_ms=busy_timeout_ms, foreign_keys=foreign_keys)
    try:
        db.migrate()
    except BaseException:
        db.close()
        raise
    return db
