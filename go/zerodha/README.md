# zerodha

Kite Connect adapted to the tradekit ports.

## What it implements

`ports.Broker`, plus `Quoter`, `HistoryProvider`, `ProtectiveOrders`
(Kite GTTs), `MarginEstimator` (basket margin), `Streamer` (the Kite ticker),
`InstrumentSource` and `TokenState`. It deliberately does **not** implement
`TickObserver`: Kite's stops rest at the exchange and trigger without the
engine's help.

Beyond the ports, `Holdings` returns the delivery book -- stock that has
settled out of the positions book -- because a CNC bot that checks positions
alone buys the same stock again the next day. It is a Zerodha method, not a
port, because only the Indian brokers keep two books.

```go
c, err := zerodha.New(zerodha.Options{
    APIKey:      key,
    APISecret:   secret,
    AccessToken: token,          // from a previous Login
    TokenIssuedAt: issuedAt,     // drives TokenFresh
    Tag:         "donchian",     // Kite caps this at 20 chars
    InstrumentToken: store.KiteToken,   // only historical data needs it
})
```

## Two layers, on purpose

`mapping.go` holds every conversion and imports **nothing** from the SDK — it
deals only in strings and numbers. That is what makes it testable without a
broker account, a network, or the SDK itself; `mapping_test.go` runs anywhere.
Request pacing comes from `core/ratelimit`, shared with the other adapters and
the notifier.

The catch with restating Kite's constants is that they could drift.
`constants_test.go` closes that: it imports the SDK and asserts every restated
literal still equals the real one, so a rename upstream fails the build instead
of sending an unrecognised product code to the exchange.

## Running the tests

```sh
go test ./...
```

Everything here is offline — there are no live-API tests. The suite covers the
mapping, the SDK constant check and the port assertions; the limiter's tests
live with it in `core/ratelimit`.

## Notes worth knowing

- **`Stop` maps to `SL-M`, `StopLimit` to `SL`.** Kite's SL carries a limit
  price and may never fill; SL-M becomes a market order on trigger. Swapping
  them turns a guaranteed exit into an optional one.
- **GTT legs are ordered by price, not by role.** Kite's `Lower` is the smaller
  trigger, which is the stop for a long's protective sell and the target for a
  short's protective buy. `protective.go` assigns by price and derives the role
  from the side.
- **`ModifyStop` reads the GTT first.** Kite replaces the whole trigger, so
  moving a stop without re-sending the target would delete the target.
- **`Account` reports the equity segment only.** Folding in commodity margins
  would let an equity strategy size against capital it cannot use.
- **`PlaceOrder` returns `pending`, never `complete`.** Kite acknowledges before
  the exchange accepts; call `OrderStatus` for what actually happened.
- **An unknown order status maps to `pending`, never to a terminal state**, so a
  reconciler keeps watching an order it does not recognise.
- **Errors carry one of three sentinels.** `errors.Is(err, ErrTokenExpired)`
  means re-authenticate, `ErrTransient` means retry, `ErrRejected` means stop.
  HTTP 429 is transient whatever `ErrorType` Kite attaches, and an error type
  the SDK adds later is treated as a rejection — one missed retry costs a call,
  a retried rejection sends the same bad order repeatedly. Kite's own error is
  still reachable with `errors.As`.
- **`SubscribeTicks` subscribes in full mode and re-subscribes on reconnect.**
  LTP mode carries no exchange timestamp, and a ticker that reconnects without
  re-subscribing stays open and silent — which looks exactly like a quiet
  market. Cancelling the context closes the socket.
- **Rate limits default to 3/s.** Conservative on purpose — a scan that trips
  the limiter loses the whole pass. Tune through `Options`.
