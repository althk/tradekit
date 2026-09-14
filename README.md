# tradekit

Shared trading primitives, built once and held to a single contract: a Go
library and a Python library, side by side, backed by the same domain model,
schema and test data.

## Why

Trading bots tend to reinvent the same handful of things per project: a
broker adapter, a risk manager, a SQLite store, an ATR, a charge calculator, a
rate limiter, a paper broker. `tradekit` factors those out once, in both
languages, so a new project pulls in a tested implementation instead of
writing (or copy-pasting) its own.

## Layout

```text
contracts/          the source of parity between the two libraries
  domain.md         canonical type and field names
  sqlite/           the shared schema, applied by both
  testdata/         golden cases both test suites run

go/
  core/             domain, money, ports, risk, costs, calendar, indicators, stats, paper, options, ratelimit
  store/            SQLite open/migrate/repos (no driver imported)
  marketdata/       feed, sync, universe, reference data, bar construction, CSV export
  backtest/         clock, replay driver, equity snapshots, run recording, HTML report, sweep
  zerodha/          Kite Connect adapter
  upstox/           HTTP-direct Upstox adapter
  fyers/            one FYERS API v3 adapter, same contract as the other two
  alpaca/           not started
  harness/          config + Secret, log helpers, decision journal, notifiers, browser login

py/                 one distribution, vendor SDKs as extras
  src/tradekit/core/        mirrors go/core module for module, paper included
  src/tradekit/store/       applies the same contracts/sqlite schema
  src/tradekit/upstox/      mirrors go/upstox
  src/tradekit/fyers/       mirrors go/fyers
  src/tradekit/marketdata/  mirrors go/marketdata (pandas optional)
  src/tradekit/harness/     mirrors go/harness
```

Go modules are split only where a heavy third-party dependency justifies it.
`core` has no dependency beyond the standard library, so a screener that needs
indicators and charge maths pulls in no SQLite driver and no broker SDK.

## How to use this repo

Each directory under `go/` is its own Go module (its own `go.mod`, its own
`github.com/althk/tradekit/go/<name>` import path), and everything under `py/`
is one Python distribution. That split exists so a consumer only pulls in
what it needs — a screener that wants indicators and cost maths doesn't drag
in a SQLite driver or a broker SDK.

### Examples

[tradekit-example](https://github.com/althk/tradekit-example) has two small
golden-cross bots — one in Go against the `zerodha` adapter, one in Python
against `upstox` — showing the pieces above wired together: risk sizing and
gates, cost estimation, SQLite persistence, the decision journal, config via
`harness`, and (Go only) a backtest mode through `core/paper` and
`go/backtest`.

### Go

```sh
go get github.com/althk/tradekit/go/core
go get github.com/althk/tradekit/go/zerodha   # add adapters as needed
```

```go
import (
    "github.com/althk/tradekit/go/core/money"
    "github.com/althk/tradekit/go/core/ports"
    "github.com/althk/tradekit/go/zerodha"
)
```

Once the repo has tagged releases, each module is versioned independently
with a tag prefixed by its path, e.g. `go/core/v0.1.0` — `go get
.../go/core@v0.1.0` resolves to that tag. Until then, `go get` without a
version pulls the module at the branch head via a pseudo-version.

`go/store` deliberately imports no SQLite driver; register one yourself
(`modernc.org/sqlite` or `mattn/go-sqlite3`) — see `go/store/README.md`.

### Python

Not published to PyPI — install straight from the repo:

```sh
pip install "tradekit @ git+https://github.com/althk/tradekit.git#subdirectory=py"
```

Vendor SDKs are optional extras (`zerodha`, `upstox`, `fyers`, `alpaca`,
`pandas`), so installing the base package pulls in nothing beyond the
standard library:

```sh
pip install "tradekit[upstox] @ git+https://github.com/althk/tradekit.git#subdirectory=py"
```

See `py/README.md` for the full extras list and package layout.

### Module-by-module details

- [go/core](go/core/README.md)
- [go/store](go/store/README.md)
- [go/zerodha](go/zerodha/README.md)
- [go/upstox](go/upstox/README.md)
- [go/fyers](go/fyers/README.md)
- [go/marketdata](go/marketdata/README.md)
- [go/backtest](go/backtest/README.md)
- [go/harness](go/harness/README.md)
- [py](py/README.md)

## The two rules that make this one system

**One representation of money.** Every price, amount and P&L is an integer
count of minor units — paise or cents. Exact, identical in Go and Python,
stores as a SQLite `INTEGER`, no decimal library, no float drift in
accumulated P&L.

**One contract, enforced by CI.** `contracts/testdata/parity.json` holds
golden cases that *both* test suites run. When the Go and Python
implementations disagree about rounding, position sizing, charges or trade
statistics, the build fails. Drift is caught on the commit that causes it.

## Design notes worth knowing before using it

- **Brokers are capability interfaces, not one large interface.** `Broker` has
  six methods every venue can perform. `Quoter`, `ProtectiveOrders`,
  `MarginEstimator`, `Streamer`, `HistoryProvider`, `PositionCloser` and
  `TickObserver` are asked for by type assertion. A strategy needing a
  capability the configured broker lacks fails at wiring time, not at 09:15.
- **Indicators return NaN during warm-up**, never zero. Zero is a legitimate
  indicator value, and a zero-filled warm-up silently biases every average
  taken over the series.
- **No rate card ships here.** `costs` is driven by a table the caller
  populates, keyed by broker, segment and date. Rates change with each budget;
  a constant would go stale without anyone noticing.
- **Risk gates are composed, not hardcoded.** Order is visible at the call
  site, and it matters: a daily-loss kill switch belongs first, so a day that
  has blown its budget reports that rather than whichever secondary limit
  also tripped.
- **`stats.Summarize` refuses to mix paper and live trades.** Blending
  simulated fills with real ones produces a number that looks entirely
  plausible and is meaningless.
- **Dates are keyed by `"YYYY-MM-DD"` strings, never by date objects.** Two Go
  `time.Time` values for the same instant compare unequal as map keys when
  their `*Location` pointers differ, and `time.LoadLocation` returns a fresh
  pointer every call — a time-keyed holiday map compiles, looks correct, and
  silently never matches.
- **The store imports no SQLite driver.** Consumers already split between
  `modernc.org/sqlite` and `mattn/go-sqlite3`; pinning one in a shared library
  would make that choice for every project forever.
- **Candles are `INSERT OR IGNORE`, not `REPLACE`.** Re-fetching a window is
  the normal way to deepen history, and a vendor occasionally returns a
  different volume for an old bar. Keeping the first value read is what makes
  a backtest reproducible.
- **Migrations are banded.** The library owns 1–499, a project owns 500+, and
  applying a migration the database has but this build does not know about is
  an error rather than a shrug.
- **An adapter's translation layer imports no SDK.** `mapping.go` deals only
  in strings and numbers, so it is testable without a broker account, and
  `constants_test.go` asserts its restated literals still equal the SDK's, so
  a rename upstream fails the build instead of reaching the exchange.
- **The simulator is deliberately pessimistic.** A gap through a stop fills at
  the gap price, not the stop. A bar whose range contains both the stop and
  the target resolves as the stop, because no daily or 5-minute bar says which
  came first. Slippage and tick rounding always work against the position.
  These rules are pinned in `contracts/testdata/parity.json`, so an ambiguous
  bar cannot resolve one way in Go and the other way in Python.

## Running the tests

```sh
cd go/core       && go test ./...                     # 11 packages
cd go/store      && go test -tags sqlitedriver ./...  # SQL tests skip without a driver
cd go/zerodha    && go test ./...                      # offline: mapping, limiter, errors, ticker, SDK constants
cd go/upstox     && go test ./...                      # offline: httptest.Server end to end
cd go/fyers      && go test ./...                      # offline: httptest.Server end to end + SDK constants
cd go/marketdata && go test -tags sqlitedriver ./...
cd go/backtest   && go test -tags sqlitedriver ./...   # -run Golden -update regenerates the report golden file
cd go/harness    && go test -tags sqlitedriver ./...
cd py            && ruff check src tests && ruff format --check src tests && pytest -q
./scripts/sync-schema.sh --check                       # schema copies match contracts/sqlite
```

All must pass; between them they run the same `contracts/testdata/parity.json`.

## Current state

`alpaca` is not started; every other module listed above is built and green.
See each module's own README for what it covers and what's left.
