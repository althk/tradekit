package store

import (
	"context"
	"database/sql"
	"fmt"
)

// LegacySuffix is appended to a table AdoptLegacy moves aside.
const LegacySuffix = "_legacy"

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
// It is a no-op once schema_migrations exists, so calling it on every start
// is safe. A table that is absent is skipped, so a database that never had
// one of the names is fine.
func AdoptLegacy(ctx context.Context, sqlDB *sql.DB, tables ...string) error {
	adopted, err := TableExists(ctx, sqlDB, "schema_migrations")
	if err != nil {
		return err
	}
	if adopted {
		return nil
	}
	for _, name := range tables {
		exists, err := TableExists(ctx, sqlDB, name)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		indexes, err := indexesOf(ctx, sqlDB, name)
		if err != nil {
			return err
		}
		for _, idx := range indexes {
			if _, err := sqlDB.ExecContext(ctx, `DROP INDEX IF EXISTS "`+idx+`"`); err != nil {
				return fmt.Errorf("store: dropping legacy index %s: %w", idx, err)
			}
		}
		if _, err := sqlDB.ExecContext(ctx, `ALTER TABLE "`+name+`" RENAME TO "`+name+LegacySuffix+`"`); err != nil {
			return fmt.Errorf("store: moving legacy table %s aside: %w", name, err)
		}
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
func indexesOf(ctx context.Context, sqlDB *sql.DB, table string) ([]string, error) {
	rows, err := sqlDB.QueryContext(ctx,
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
