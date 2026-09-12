"""SQLite persistence shared by every tradekit project.

The schema lives in ``contracts/sqlite`` and is applied identically by this
package and by ``go/store``, which is what lets a Python screener write signals
that a Go executor reads out of the same file.

    from tradekit.store import connect

    db = connect("breakout500.db")
    db.migrate()
    last = db.last_candle_start(key, Timeframe.D1)

Conventions:

* Money is stored as INTEGER minor units, matching ``tradekit.core.money``.
* Timestamps are TEXT in ISO-8601 with an explicit offset, so they sort
  lexicographically and carry their zone. A naive datetime is rejected.
* Dates are TEXT ``"YYYY-MM-DD"``.
"""

from ._base import NotFoundError, StateNotFoundError
from .database import Database, connect
from .migrations import (
    LIBRARY_MAX_VERSION,
    PROJECT_MIN_VERSION,
    Migration,
    UnknownMigrationError,
    load_migrations,
)

__all__ = [
    "LIBRARY_MAX_VERSION",
    "PROJECT_MIN_VERSION",
    "Database",
    "Migration",
    "NotFoundError",
    "StateNotFoundError",
    "UnknownMigrationError",
    "connect",
    "load_migrations",
]
