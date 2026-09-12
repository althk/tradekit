# upstox

An HTTP-direct Upstox adapter implementing the tradekit ports.

## What it implements

`ports.Broker`, and separately `Quoter`, `HistoryProvider`, `ProtectiveOrders`,
`MarginEstimator`, `InstrumentSource` and `TokenState`. Ask for a capability by
type assertion; never widen `Broker`.

`ports.TickObserver` is deliberately **not** implemented, for the same reason
the Kite adapter omits it: an Upstox stop rests at the broker and triggers
without the engine's help.

## Wiring

Upstox addresses instruments by `SEGMENT|ID` — an ISIN for cash equity
(`NSE_EQ|INE002A01018`), an exchange token for derivatives — which the adapter
cannot derive from an exchange and a symbol. Supply `Options.InstrumentKey`,
wired to the store's `BrokerID` or to a map built from `Instruments`:

```go
c, err := upstox.New(upstox.Options{
    APIKey:        key,
    APISecret:     secret,
    RedirectURI:   "https://localhost:8080/cb",
    AccessToken:   token,
    TokenIssuedAt: issuedAt,
    InstrumentKey: func(k domain.InstrumentKey) (string, error) {
        return db.BrokerID(ctx, k, "upstox")
    },
})
```

## There is no SDK

Upstox publishes no usable Go SDK, so this adapter speaks HTTP directly. That
makes it *more* verifiable than the Kite one: every call is exercised end to end
against an `httptest.Server`, offline, and the suite runs without an account.

Because there is no SDK there is also nothing for a `constants_test.go` to pin
the wire vocabulary against. Every endpoint path and field name is instead
traced to donor code known to work against the live API. `upstox-api.txt`
(gitignored, like `kite-api.txt`) records the donor file and line for each, along
with the places two donors disagreed and which was taken.

## Verifying

```sh
cd go/upstox && GOWORK=off go test ./...
```

Offline. No account, no network.
