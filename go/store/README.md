# store

SQLite persistence, shared by every tradekit project.

The schema itself lives in `contracts/sqlite` and is copied here by
`scripts/sync-schema.sh` so `go:embed` can reach it. Never edit `schema/`
directly — CI fails if the copy has drifted from `contracts/sqlite`.

## No driver is imported

This module depends on `database/sql` and nothing else. Pinning a SQLite driver
in a shared library would force that choice on every consumer forever, and the
projects here already split between drivers. Import yours for its side effects
and pass its name:

```go
import _ "modernc.org/sqlite"

db, err := store.Open("sqlite", "app.db")
if err != nil { ... }
defer db.Close()

if err := db.Migrate(ctx); err != nil { ... }
```

`store.OpenMigrated(ctx, "sqlite", "app.db")` is the two steps in one for the
process that owns the file; `Open` alone stays for tools that must not change
its shape.

## Helpers over the tables

Beyond the row-level writers, a few methods cover the sequences every
consumer wrote by hand: `WithRun` brackets work in a `runs` row that is
closed whichever way the work ends, `RecordFill` writes a signal and the
order it became in one transaction, and `LoadDailyState`/`SaveDailyState`
keep `risk.DailyState` in `kv_state` with the day-change check applied on
the way out.

## Running the tests

The driver-independent tests (migration loading, version bands, filename
parsing) run with no setup:

```sh
go test ./...
```

The SQL integration tests need a driver registered, which the `sqlitedriver`
build tag does. It is a test-only dependency and is deliberately not in the
committed `go.mod`, so resolve it once:

```sh
go mod tidy
go test -tags sqlitedriver ./...
```

Without the tag those tests skip and say why, so a checkout with no network is
not a broken checkout.

## Version bands

The library owns migrations 1–499; a project owns 500 and above. `Migrate`
applies the library's, `MigrateProject` applies yours and refuses anything
below 500 — a project numbering its first migration `001` would otherwise
collide with the library's and one of them would silently never run.
