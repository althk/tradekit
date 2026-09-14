# harness

Built. `Load`/`Overlay` (TOML plus an `env:"..."` overlay and
`validate:"required"`), `Secret` (redacted through fmt, slog and JSON; `Reveal`
is the only way to read it), `Redacted` (the only supported way to print a
config), the slog attribute helpers, the decision `Journal` over the shared
`decisions` table (migration 003), and `Notifier` with `Telegram` (bounded
queue, drop-oldest, one delivery goroutine, paced by `core/ratelimit`),
`Multi` and `Discard`, and `BrowserLogin` â€” the one callback server for the
redirect-based broker login, over `ports.BrowserLogin`, which `zerodha`,
`upstox` and `fyers` implement (`LoginURL` + `LoginCallback(ctx, query)`; the
adapter owns its parameter names, its refusal format and, for FYERS, the
state check), and `EnsureSession` — the store-backed wrapper around it that
reuses yesterday's token when the venue still accepts it and persists a new
one otherwise, over `ports.TokenState` + `ports.ErrTokenExpired`.

The Python mirror is `py/src/tradekit/harness/`. Both run the `config` and
`journal` blocks of `contracts/testdata/parity.json`.

## Verifying

```sh
cd go/harness && GOWORK=off go test -tags sqlitedriver ./...
```
