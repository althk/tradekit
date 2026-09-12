"""The numbered migration runner.

Mirrors ``go/store/migrate.go``, including the version bands: the library owns
1-499 and a project owns 500 and above, so adding a table to tradekit can never
collide with a migration a project wrote last week.
"""

from __future__ import annotations

import datetime as dt
import re
import sqlite3
from dataclasses import dataclass
from importlib import resources
from pathlib import Path

from ._base import Base, format_time

__all__ = ["Migration", "MigrationMixin", "UnknownMigrationError", "load_migrations"]

LIBRARY_MAX_VERSION = 499
PROJECT_MIN_VERSION = 500

_FILENAME = re.compile(r"^(\d+)_(.+)\.sql$")


class UnknownMigrationError(RuntimeError):
    """The database has a migration applied that this build does not know about.

    An error rather than a warning: it means the code is older than the
    database, and continuing would run queries against a shape this build has
    never seen. The failure would otherwise surface later as a confusing column
    error instead of here as a clear one.
    """


@dataclass(frozen=True, slots=True)
class Migration:
    """One numbered schema change."""

    version: int
    name: str
    sql: str


def _parse_filename(filename: str) -> tuple[int, str] | None:
    """Split ``001_init.sql`` into ``(1, "init")``, or ``None`` if it is not one."""
    match = _FILENAME.match(filename)
    if match is None:
        return None
    version = int(match.group(1))
    if version <= 0:
        return None
    return version, match.group(2)


def load_migrations(directory: Path) -> list[Migration]:
    """Read ``NNN_name.sql`` files from ``directory``, sorted by version.

    Files that do not match the pattern are ignored rather than guessed at.
    Duplicate versions raise: which of the two would win is not something to
    leave to directory ordering.
    """
    found: dict[int, str] = {}
    out: list[Migration] = []
    for path in sorted(directory.iterdir()):
        if not path.is_file():
            continue
        parsed = _parse_filename(path.name)
        if parsed is None:
            continue
        version, name = parsed
        if version in found:
            raise ValueError(f"store: migration version {version} appears twice: {found[version]!r} and {path.name!r}")
        found[version] = path.name
        out.append(Migration(version, name, path.read_text()))
    out.sort(key=lambda m: m.version)
    return out


def _library_migrations() -> list[Migration]:
    """The migrations shipped inside this package."""
    with resources.as_file(resources.files("tradekit.store") / "schema") as schema_dir:
        return load_migrations(schema_dir)


class MigrationMixin(Base):
    """Applies migrations and records them."""

    def migrate(self) -> None:
        """Apply the library's own migrations, those from contracts/sqlite."""
        migrations = _library_migrations()
        for m in migrations:
            if m.version > LIBRARY_MAX_VERSION:
                raise ValueError(
                    f"store: library migration {m.version} exceeds the reserved range (1-{LIBRARY_MAX_VERSION})"
                )
        self.apply(migrations)

    def migrate_project(self, directory: Path) -> None:
        """Apply a project's own migrations from its own directory.

        Versions must be at or above 500. That is the point of the split: a
        project numbering its first migration 001 would collide with the
        library's and one of them would silently never run.
        """
        migrations = load_migrations(directory)
        for m in migrations:
            if m.version < PROJECT_MIN_VERSION:
                raise ValueError(
                    f"store: project migration {m.version} ({m.name}) is below {PROJECT_MIN_VERSION}; "
                    f"the library reserves 1-{LIBRARY_MAX_VERSION}"
                )
        self.apply(migrations)

    def apply(self, migrations: list[Migration]) -> None:
        """Run any migrations not yet recorded, in version order.

        Each runs in its own transaction, so a failure part-way leaves the
        earlier ones applied and recorded rather than rolling the whole set back
        into an ambiguous state. An already-recorded version is skipped, which
        is what makes calling :meth:`migrate` on every startup safe.
        """
        self._ensure_migrations_table()
        applied = set(self.applied_versions())
        known = {m.version for m in migrations}

        for version in sorted(applied):
            if version in known or not _in_range_of(version, migrations):
                continue
            raise UnknownMigrationError(
                f"store: database has migration {version} applied, which this build does not know about"
            )

        for m in migrations:
            if m.version in applied:
                continue
            with self.tx() as conn:
                # Not executescript: it issues an implicit COMMIT before
                # running, which would end the transaction opened above and
                # leave a failed migration half-applied. Splitting the file
                # keeps each migration atomic, as it is on the Go side.
                for statement in _split_statements(m.sql):
                    conn.execute(statement)
                conn.execute(
                    "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
                    (m.version, m.name, format_time(dt.datetime.now(dt.UTC))),
                )

    def applied_versions(self) -> list[int]:
        """The recorded migration versions, ascending."""
        self._ensure_migrations_table()
        rows = self.conn.execute("SELECT version FROM schema_migrations ORDER BY version").fetchall()
        return [r["version"] for r in rows]

    def _ensure_migrations_table(self) -> None:
        """Create the ledger itself.

        It cannot live in a migration, because it is what records that
        migrations ran.
        """
        self.conn.execute(
            """
            CREATE TABLE IF NOT EXISTS schema_migrations (
                version    INTEGER PRIMARY KEY,
                name       TEXT NOT NULL,
                applied_at TEXT NOT NULL
            )
            """
        )


def _split_statements(sql: str) -> list[str]:
    """Split a migration file into individual statements.

    Uses :func:`sqlite3.complete_statement`, which understands string literals
    and comments, rather than splitting on semicolons -- a semicolon inside a
    quoted default or a trigger body would otherwise cut a statement in half.
    """
    statements: list[str] = []
    buffer = ""
    for line in sql.splitlines(keepends=True):
        buffer += line
        if sqlite3.complete_statement(buffer):
            if buffer.strip():
                statements.append(buffer)
            buffer = ""
    if buffer.strip():
        statements.append(buffer)
    return statements


def _in_range_of(version: int, migrations: list[Migration]) -> bool:
    """Whether ``version`` falls in the same reserved band as ``migrations``.

    This keeps :meth:`MigrationMixin.apply` from validating versions it is not
    responsible for, so applying library migrations does not trip over a
    project's, or the reverse.
    """
    if not migrations:
        return False
    if migrations[0].version <= LIBRARY_MAX_VERSION:
        return version <= LIBRARY_MAX_VERSION
    return version >= PROJECT_MIN_VERSION
