# core

The dependency-free foundation. Domain types, money, broker ports, risk,
costs, calendar, indicators, stats, a paper broker, options maths and the one
rate limiter — eleven packages, no import beyond the standard library.

```sh
go get github.com/althk/tradekit/go/core
```

```go
import (
    "github.com/althk/tradekit/go/core/money"
    "github.com/althk/tradekit/go/core/ports"
)
```

## Packages

- `domain` — canonical order, position and instrument types shared by every
  adapter.
- `money` — every price, amount and P&L as `int64` minor units. No float,
  no decimal library; rounding is half away from zero.
- `ports` — `Broker`, the six-method capability every venue implements, plus
  the optional capabilities (`Quoter`, `HistoryProvider`, `ProtectiveOrders`,
  `MarginEstimator`, `Streamer`, `TickObserver`, `InstrumentSource`,
  `TokenState`) asked for by type assertion. Never widen `Broker` itself.
- `risk` — composable gates; order at the call site matters (a daily-loss
  kill switch belongs first).
- `costs` — charge calculation driven by a table the caller loads. No rate
  card ships here — rates change with the budget.
- `calendar` — market holidays and sessions, keyed by `"YYYY-MM-DD"` strings.
- `indicators` — return `NaN` during warm-up, never zero.
- `stats` — trade summary statistics; `Summarize` refuses a set that mixes
  paper and live fills.
- `paper` — a pessimistic fill simulator: a gap through a stop fills at the
  gap price, a bar spanning both stop and target resolves as the stop.
- `options` — Black-Scholes pricing, IV inversion, strike-for-delta.
- `ratelimit` — the one token bucket, shared by every adapter and the
  notifier.

## Running the tests

```sh
go test ./...
```

Offline throughout; no network or broker account needed.
