package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Version ranges. The library owns 1-499 and a project owns 500 and above, so
// that adding a table to tradekit can never collide with a migration a project
// wrote last week.
const (
	libraryMaxVersion = 499
	projectMinVersion = 500
)

// ErrUnknownMigration reports that the database has a migration applied that
// the running binary does not know about.
//
// It is an error rather than a warning because it means the binary is older
// than the database: continuing would run queries against a shape this build
// has never seen, and the failure would surface later as a confusing column
// error rather than here as a clear one.
var ErrUnknownMigration = errors.New("store: database has a migration this build does not know about")

// Migration is one numbered schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// LoadMigrations reads NNN_name.sql files from dir in fsys.
//
// Filenames must start with digits followed by an underscore; anything else in
// the directory is ignored rather than guessed at. Duplicate versions are an
// error, since which of the two would win is not something to leave to
// directory ordering.
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: reading migrations in %q: %w", dir, err)
	}

	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, ok := parseMigrationName(e.Name())
		if !ok {
			continue
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migration version %d appears twice: %q and %q", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: reading %q: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: version, Name: name, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName splits "001_init.sql" into 1 and "init".
func parseMigrationName(filename string) (version int, name string, ok bool) {
	base := strings.TrimSuffix(filename, ".sql")
	digits, rest, found := strings.Cut(base, "_")
	if !found || digits == "" {
		return 0, "", false
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return v, rest, true
}

// Migrate applies the library's own migrations, those in contracts/sqlite.
func (d *DB) Migrate(ctx context.Context) error {
	ms, err := LoadMigrations(schemaFS, "schema")
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Version > libraryMaxVersion {
			return fmt.Errorf("store: library migration %d exceeds the reserved range (1-%d)", m.Version, libraryMaxVersion)
		}
	}
	return d.Apply(ctx, ms)
}

// MigrateProject applies a project's own migrations from its own filesystem,
// typically an embed.FS over its migrations directory.
//
// Versions must be at or above 500. That is the whole point of the split: a
// project numbering its first migration 001 would collide with the library's
// and one of them would silently never run.
func (d *DB) MigrateProject(ctx context.Context, fsys fs.FS, dir string) error {
	ms, err := LoadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Version < projectMinVersion {
			return fmt.Errorf(
				"store: project migration %d (%s) is below %d; the library reserves 1-%d",
				m.Version, m.Name, projectMinVersion, libraryMaxVersion)
		}
	}
	return d.Apply(ctx, ms)
}

// Apply runs any migrations not yet recorded, in version order.
//
// Each runs in its own transaction, so a failure part-way leaves the earlier
// ones applied and recorded rather than rolling the whole set back into an
// ambiguous state. Applying an already-recorded version is skipped silently,
// which is what makes calling Migrate on every startup safe.
func (d *DB) Apply(ctx context.Context, ms []Migration) error {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return err
	}

	applied, err := d.AppliedVersions(ctx)
	if err != nil {
		return err
	}

	known := make(map[int]bool, len(ms))
	for _, m := range ms {
		known[m.Version] = true
	}
	for _, v := range applied {
		// Only complain about versions in the range this call owns, so
		// that applying library migrations does not trip over a
		// project's, or the reverse.
		if known[v] || !inRangeOf(v, ms) {
			continue
		}
		return fmt.Errorf("%w: version %d", ErrUnknownMigration, v)
	}

	appliedSet := make(map[int]bool, len(applied))
	for _, v := range applied {
		appliedSet[v] = true
	}

	for _, m := range ms {
		if appliedSet[m.Version] {
			continue
		}
		if err := d.applyOne(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// inRangeOf reports whether v falls in the same reserved band as ms, so that
// Apply only validates versions it is responsible for.
func inRangeOf(v int, ms []Migration) bool {
	if len(ms) == 0 {
		return false
	}
	library := ms[0].Version <= libraryMaxVersion
	if library {
		return v <= libraryMaxVersion
	}
	return v >= projectMinVersion
}

func (d *DB) applyOne(ctx context.Context, m Migration) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("store: applying migration %d (%s): %w", m.Version, m.Name, err)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, formatTime(time.Now().UTC()))
		if err != nil {
			return fmt.Errorf("store: recording migration %d (%s): %w", m.Version, m.Name, err)
		}
		return nil
	})
}

// ensureMigrationsTable creates the ledger itself, which cannot live in a
// migration because it is what records that migrations ran.
func (d *DB) ensureMigrationsTable(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}
	return nil
}

// AppliedVersions returns the recorded migration versions, ascending.
func (d *DB) AppliedVersions(ctx context.Context) ([]int, error) {
	if err := d.ensureMigrationsTable(ctx); err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: reading applied migrations: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scanning applied migration: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
