// Package store is the SQLite persistence layer shared by every tradekit
// project.
//
// It replaces eight hand-rolled stores, two of which were a verbatim fork of
// each other and none of which agreed on table names. The schema itself lives
// in contracts/sqlite and is applied identically by this package and by the
// Python one, which is what allows a Python screener to write signals that a Go
// executor reads out of the same file.
//
// # No driver is imported
//
// This package depends on database/sql and nothing else. Pinning a SQLite
// driver in a shared library would force that choice on every consumer forever,
// and the projects here already split between drivers. Import the driver you
// want for its side effects and pass its name:
//
//	import _ "modernc.org/sqlite"
//
//	db, err := store.Open("sqlite", "neev.db")
//
// # Conventions
//
//   - Money is stored as INTEGER minor units, matching core/money.
//   - Timestamps are TEXT in RFC3339 with an explicit offset, so they sort
//     lexicographically and carry their zone. Use the package's own conversion
//     helpers rather than formatting them at call sites.
//   - Dates (holidays, effective-from) are TEXT "2006-01-02".
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound reports that a row a caller asked for by key does not exist.
// It is distinct from a query failure so that a cold start reads as normal
// rather than as an error to be logged.
var ErrNotFound = errors.New("store: not found")

// DB is a database handle with tradekit's pragmas and migrations applied.
type DB struct {
	db *sql.DB
}

// Option configures a DB at open time.
type Option func(*config)

type config struct {
	maxOpenConns int
	busyTimeout  time.Duration
	foreignKeys  bool
	walMode      bool
}

func defaults() config {
	return config{
		maxOpenConns: 1,
		busyTimeout:  5 * time.Second,
		foreignKeys:  true,
		walMode:      true,
	}
}

// WithMaxOpenConns sets the connection pool size.
//
// The default is 1, deliberately. busy_timeout and foreign_keys are
// per-connection pragmas, so with a larger pool a connection opened later may
// not carry them, and the resulting "database is locked" or silently unenforced
// foreign key appears only under concurrency. One connection makes the pragmas
// reliable and is ample for these workloads; raise it only if you have measured
// a need and understand that trade-off.
func WithMaxOpenConns(n int) Option {
	return func(c *config) { c.maxOpenConns = n }
}

// WithBusyTimeout sets how long a writer waits for a lock before failing.
// Zero disables the pragma.
func WithBusyTimeout(d time.Duration) Option {
	return func(c *config) { c.busyTimeout = d }
}

// WithoutWAL leaves the journal mode alone.
//
// WAL is the default because it lets a dashboard read while the trading loop
// writes; one of the stores this package replaces omitted it and had a latent
// reader-versus-writer lock as a result. Turn it off only for a database on a
// filesystem that cannot support it, such as some network mounts.
func WithoutWAL() Option {
	return func(c *config) { c.walMode = false }
}

// WithoutForeignKeys leaves foreign key enforcement off.
func WithoutForeignKeys() Option {
	return func(c *config) { c.foreignKeys = false }
}

// Open opens path with an already-registered driver and applies the pragmas.
//
// It does not run migrations; call Migrate for that, so that a read-only tool
// can open a database without being able to change its shape.
func Open(driverName, path string, opts ...Option) (*DB, error) {
	sqlDB, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("store: open %q with driver %q: %w", path, driverName, err)
	}
	db, err := New(sqlDB, opts...)
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// OpenMigrated is Open followed by Migrate, for the process that owns the
// database. It is the first thing every bot's main does; a read-only tool
// keeps using Open so it cannot change the file's shape.
func OpenMigrated(ctx context.Context, driverName, path string, opts ...Option) (*DB, error) {
	db, err := Open(driverName, path, opts...)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// New wraps an already-open *sql.DB and applies the pragmas.
//
// Use it when the caller needs control over the DSN, or is supplying a
// connection from elsewhere such as a test harness.
func New(sqlDB *sql.DB, opts ...Option) (*DB, error) {
	cfg := defaults()
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.maxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.maxOpenConns)
	}

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	var pragmas []string
	if cfg.walMode {
		pragmas = append(pragmas, "PRAGMA journal_mode=WAL")
	}
	if cfg.busyTimeout > 0 {
		pragmas = append(pragmas, fmt.Sprintf("PRAGMA busy_timeout=%d", cfg.busyTimeout.Milliseconds()))
	}
	if cfg.foreignKeys {
		pragmas = append(pragmas, "PRAGMA foreign_keys=ON")
	}
	for _, p := range pragmas {
		// journal_mode returns a row, so Query rather than Exec: some
		// drivers error on Exec for a statement that produces output.
		rows, err := sqlDB.Query(p)
		if err != nil {
			return nil, fmt.Errorf("store: %s: %w", p, err)
		}
		rows.Close()
	}

	return &DB{db: sqlDB}, nil
}

// SQL exposes the underlying handle for queries this package does not cover.
// Project-specific tables are expected to be queried through it.
func (d *DB) SQL() *sql.DB { return d.db }

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// formatTime renders a timestamp for storage: RFC3339 with an offset, at
// nanosecond precision so a round trip is lossless.
func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

// parseTime reads a stored timestamp.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parsing timestamp %q: %w", s, err)
	}
	return t, nil
}

// formatDate renders a calendar date for storage.
func formatDate(t time.Time) string { return t.Format(time.DateOnly) }

// parseDate reads a stored calendar date.
func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parsing date %q: %w", s, err)
	}
	return t, nil
}

// boolToInt converts a flag to the INTEGER SQLite stores it as.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// inTx runs fn inside a transaction, rolling back on error.
func (d *DB) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		// The rollback error is deliberately discarded: the caller needs
		// to see why the work failed, not why the cleanup did.
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
