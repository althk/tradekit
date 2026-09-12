# fyers

The FYERS API v3 adapter. Built to the same contract as `zerodha` and
`upstox`, so a project can be pointed at any of the three by changing its
wiring and nothing else.

## What it implements

`ports.Broker`, and separately `Quoter`, `HistoryProvider`, `ProtectiveOrders`,
`MarginEstimator`, `InstrumentSource` and `TokenState`. Ask for a capability by
type assertion; never widen `Broker`.

`ports.Streamer` is **not** implemented: FYERS's market-data socket speaks a
proprietary binary protocol that cannot be verified without a live session.
`ports.TickObserver` is not implemented either, for the reason the other live
adapters omit it: a FYERS stop rests at the broker and triggers without the
engine's help.

## Wiring

FYERS addresses instruments by a symbol the adapter can derive —
`NSE:SBIN-EQ`, `NSE:NIFTY24JANFUT`, `MCX:GOLD24DECFUT` — so unlike Upstox no
key-resolver is needed. A bare cash-equity symbol gets the `EQ` series; a
symbol in another series carries it in the domain symbol (`MODIRUBBER-BE`).
NSE derivatives (`NFO`, `CDS`) map to the `NSE:` prefix and BSE's (`BFO`) to
`BSE:`.

```go
c, err := fyers.New(fyers.Options{
    AppID:         "XXXXXXXXXX-100",
    AppSecret:     secret,
    RedirectURI:   "https://localhost:8080/cb",
    AccessToken:   token,
    TokenIssuedAt: issuedAt,
    Tag:           "my-strategy",
})
```

Authentication is `Authorization: appId:accessToken` on every request, not a
bearer token; `Login` exchanges an auth code using the SHA-256 of
`appId:appSecret`, so the secret itself never leaves the process.

## The SDK is imported by one test only

FYERS publishes a Go SDK, but it returns every response as an undecoded string
and its HTTP layer swallows transport errors, so the adapter speaks HTTP
directly. Every call is exercised end to end against an `httptest.Server`,
offline, using the sample responses from the API reference.

`constants_test.go` imports the SDK and asserts that the endpoint paths,
product codes and payload field names restated here still match it, so a
rename upstream fails the build rather than reaching the exchange.

## Things worth knowing

- **Order status codes are integers.** 6 is "pending at the exchange", which
  is the domain's `open`; 4 is "in transit", which is `pending`. 7 (expired)
  maps to `cancelled`. An unknown code is `pending`, never terminal.
- **A refusal can arrive under HTTP 200.** The envelope's `s:"error"` and
  negative `code` are inspected on every response; `-8/-15/-16/-17` are token
  failures whose remedy is a login, `-429` is a rate limit.
- **Rate limits are 10/s, 200/min, 100k/day**, and three per-minute breaches
  block the account for the day. Defaults pace at 8/s general and 3/s for
  history. `Retry-After` and `X-Retry-After-Ms` are honoured.
- **GTT legs are positional.** `leg1` must trigger above the market and `leg2`
  below, whichever is the stop. The adapter orders them from the protective
  order's side. GTT is refused for `INTRADAY`, which FYERS does not accept.
- **History is chunked** at 90 days for intraday resolutions and 360 for daily
  (the ceilings are 100 and 366), sent as epoch seconds so an intraday caller
  can end the range one bar before now and avoid a partial candle.

## Verifying

```sh
cd go/fyers && GOWORK=off go test ./...
```

Offline. No account, no network — the SDK is fetched from the module proxy for
`constants_test.go` only.
