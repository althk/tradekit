package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LegacySuffix is appended to a table AdoptLegacy moves aside.
const LegacySuffix = "_legacy"

// ErrForeignLedger reports a schema_migrations table that tradekit did not
// create.
var ErrForeignLedger = errors.New("store: foreign migrations ledger")

// AdoptLegacy prepares a database created by a pre-tradekit build for
// Migrate: every named table that exists is renamed to name+LegacySuffix,
// and the indexes attached to it are dropped.
//
// It exists because the contract schema creates its tables with IF NOT
// EXISTS. A project's own `orders` table left in place would be kept with
// the wrong columns, and the first library index on it would fail -- or, if
// the project had an index of the same name, the library's would silently
// never be built. Renaming first lets the library create its own tables,
// after which the project copies its rows across with whatever translation
// its old shape needs and drops the legacy table. That copy is the project's:
// two databases have needed this and their row shapes had nothing in common
// but the name.
//
// It is a no-op once a migration has been recorded, so calling it on every
// start is safe. A schema_migrations table with no rows does not count: that is
// what a first migration that failed and rolled back leaves behind, and
// treating it as adopted would leave the database stuck with its legacy tables
// in the way for good. A table that is absent is skipped, so a database that
// never had one of the names is fine.
//
// The renames run in one transaction on one connection with
// legacy_alter_table on. Without it SQLite 3.26+ rewrites foreign keys in
// every other table to follow the renamed one, so a project's fills table
// would end up referencing orders_legacy and break when that is dropped.
func AdoptLegacy(ctx context.Context, sqlDB *sql.DB, tables ...string) error {
	adopted, err := migrationsRecorded(ctx, sqlDB)
	if err != nil {
		return err
	}
	if adopted {
		return nil
	}

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: reserving a connection to adopt legacy tables: %w", err)
	}
	defer conn.Close()
	// Both pragmas are needed: with legacy_alter_table on, SQLite still
	// rewrites references while foreign_keys is on. foreign_keys cannot
	// change inside a transaction, so it is switched before the BEGIN and
	// restored on this connection afterwards.
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("store: reading foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("store: disabling foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA legacy_alter_table = ON`); err != nil {
		return fmt.Errorf("store: enabling legacy_alter_table: %w", err)
	}
	defer func() {
		restore := context.WithoutCancel(ctx)
		_, _ = conn.ExecContext(restore, `PRAGMA legacy_alter_table = OFF`)
		_, _ = conn.ExecContext(restore, fmt.Sprintf(`PRAGMA foreign_keys = %d`, foreignKeys))
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning legacy adoption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range tables {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
			return fmt.Errorf("store: checking for table %s: %w", name, err)
		}
		if n == 0 {
			continue
		}
		indexes, err := indexesOf(ctx, tx, name)
		if err != nil {
			return err
		}
		for _, idx := range indexes {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS "`+idx+`"`); err != nil {
				return fmt.Errorf("store: dropping legacy index %s: %w", idx, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE "`+name+`" RENAME TO "`+name+LegacySuffix+`"`); err != nil {
			return fmt.Errorf("store: moving legacy table %s aside: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing legacy adoption: %w", err)
	}
	return nil
}

// migrationsRecorded reports whether schema_migrations holds at least one row.
// A table of that name with another tool's columns is an error: it is not
// tradekit's ledger, and migrating on top of it would misread its versions.
func migrationsRecorded(ctx context.Context, sqlDB *sql.DB) (bool, error) {
	exists, err := TableExists(ctx, sqlDB, "schema_migrations")
	if err != nil || !exists {
		return false, err
	}
	if err := checkLedgerShape(ctx, sqlDB); err != nil {
		return false, err
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: counting recorded migrations: %w", err)
	}
	return n > 0, nil
}

// checkLedgerShape refuses a schema_migrations table that another migration
// tool created. golang-migrate, for one, uses the same name with columns
// (version, dirty).
func checkLedgerShape(ctx context.Context, sqlDB *sql.DB) error {
	cols, err := Columns(ctx, sqlDB, "schema_migrations")
	if err != nil {
		return err
	}
	if !cols["version"] || !cols["name"] || !cols["applied_at"] {
		return fmt.Errorf("%w: schema_migrations exists but is not tradekit's ledger (another migration tool's?); rename it before migrating", ErrForeignLedger)
	}
	return nil
}

// TableExists reports whether a table of that name exists.
func TableExists(ctx context.Context, sqlDB *sql.DB, name string) (bool, error) {
	var n int
	err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: checking for table %s: %w", name, err)
	}
	return n > 0, nil
}

// Columns returns the names of a table's columns, for a copy that must
// tolerate a legacy table missing a column a later build added.
func Columns(ctx context.Context, sqlDB *sql.DB, table string) (map[string]bool, error) {
	rows, err := sqlDB.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, fmt.Errorf("store: reading columns of %s: %w", table, err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// indexesOf lists the user-created indexes on a table; SQLite's automatic
// primary-key indexes cannot be dropped and are excluded.
func indexesOf(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL`, table)
	if err != nil {
		return nil, fmt.Errorf("store: listing indexes of %s: %w", table, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
