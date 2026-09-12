# Domain contract

Canonical names and fields for every type that crosses a module or language
boundary. Both `go/core/domain` and `tradekit.core.domain` implement exactly
this. Nothing here may be renamed in one language only.

## Naming rule

Same nouns, idiomatic casing. Go `PlaceOrder` ↔ Python `place_order`;
Go `Order.AveragePrice` ↔ Python `Order.average_price`. No synonyms:
it is `Quantity`, never `Qty` or `Size`, in both languages.

## Money

All prices, amounts and P&L are **integer minor units** — paise for INR,
cents for USD. Never float.

| | Go | Python |
| --- | --- | --- |
| Type | `money.Money` (`int64`) | `Money` (`int`, `NewType`) |
| From a decimal string | `money.Parse("1234.56")` | `money.parse("1234.56")` |
| From minor units | `money.Money(123456)` | `money.Money(123456)` |
| To display | `m.String()` → `"1234.56"` | `money.format(m)` → `"1234.56"` |

Rationale: exact, identical in both languages, stores as SQLite `INTEGER`,
no third-party decimal library, and no float drift in accumulated P&L.

## Enumerations

Stored and transported as lowercase strings, so a value written by one
language reads identically in the other.

| Enum | Values |
| --- | --- |
| `Side` | `buy`, `sell` |
| `OrderType` | `market`, `limit`, `stop`, `stop_limit` |
| `Product` | `cnc`, `mis`, `nrml`, `margin` |
| `TimeInForce` | `day`, `ioc`, `gtt` |
| `OrderStatus` | `pending`, `open`, `complete`, `cancelled`, `rejected`, `triggered` |
| `SignalKind` | `long`, `short`, `exit_long`, `exit_short` |
| `ExitReason` | `stop`, `target`, `trail`, `time_stop`, `eod`, `signal`, `kill_switch`, `manual` |
| `Timeframe` | `1m`, `3m`, `5m`, `15m`, `30m`, `60m`, `1d`, `1w` |

`ExitReason` is deliberately closed: every backtest and every live journal
must classify an exit into the same set, or their statistics cannot be compared.

## Core types

### InstrumentKey

The venue-neutral identity of a tradable instrument. A broker adapter maps it
to and from its own encoding (Kite's `instrument_token`, Upstox's
`NSE_EQ|INE002A01018`, Alpaca's plain symbol).

```sh
InstrumentKey
  Exchange   string   # "NSE", "NFO", "BSE", "NASDAQ"
  Symbol     string   # "RELIANCE", "NIFTY26JAN23000CE", "AAPL"
```

### Instrument

```sh
Instrument
  Key         InstrumentKey
  Name        string
  ISIN        string        # "" when not applicable
  Segment     string        # "equity", "futures", "options", "index"
  LotSize     int           # 1 for cash equity
  TickSize    Money
  Expiry      date | None   # derivatives only
  Strike      Money | None  # options only
  OptionType  string        # "ce", "pe", or ""
  Active      bool
```

### Candle

`Start` is the bucket's opening timestamp, always timezone-aware and always
the *start* of the interval. A daily candle's `Start` is the session date at
midnight in the exchange's timezone.

```sh
Candle
  Key       InstrumentKey
  Timeframe Timeframe
  Start     datetime  (tz-aware)
  Open      Money
  High      Money
  Low       Money
  Close     Money
  Volume    int
  OpenInterest int    # 0 for cash equity
```

### Tick, Quote, Position, Order, Fill, Signal, Trade

```sh
Tick
  Key   InstrumentKey
  At    datetime
  Price Money
  Volume int          # traded quantity for this print, 0 if unknown

Quote
  Key    InstrumentKey
  At     datetime
  Last   Money
  Open, High, Low, Close  Money   # the still-forming session's OHLC
  Bid, Ask                Money   # 0 when the feed does not carry depth

Position
  Key          InstrumentKey
  Quantity     int      # signed: negative is short
  AveragePrice Money
  Product      Product
  RealizedPnL  Money
  UnrealizedPnL Money

OrderRequest
  Key          InstrumentKey
  Side         Side
  Quantity     int      # always positive; direction lives in Side
  Type         OrderType
  Product      Product
  LimitPrice   Money    # 0 for market
  TriggerPrice Money    # 0 unless stop / stop_limit
  TimeInForce  TimeInForce
  Tag          string   # strategy identifier, echoed back by the broker

Order
  ID           string
  Request      OrderRequest
  Status       OrderStatus
  FilledQuantity int
  AveragePrice Money
  PlacedAt     datetime
  UpdatedAt    datetime
  Message      string   # broker's rejection / status text
  ProtectiveID string   # id of the resting stop covering this order; "" if none

Fill
  OrderID  string
  Key      InstrumentKey
  Side     Side
  Quantity int
  Price    Money
  At       datetime

Signal
  Key        InstrumentKey
  Kind       SignalKind
  At         datetime
  Price      Money    # reference entry price
  Stop       Money
  Target     Money    # 0 when the strategy has no fixed target
  Strategy   string
  Metadata   map[string]string

Trade                  # a closed round trip
  Key         InstrumentKey
  Strategy    string
  Side        Side     # side of the ENTRY
  Quantity    int
  EntryPrice  Money
  ExitPrice   Money
  EntryAt     datetime
  ExitAt      datetime
  GrossPnL    Money
  Charges     Money
  NetPnL      Money    # GrossPnL - Charges
  ExitReason  ExitReason
  Paper       bool
```

### Protective

One representation for Kite's GTT, Alpaca's bracket and Upstox's GTT.
`Target` of 0 means a single-leg stop; both set means OCO.

```sh
Protective
  ID       string
  Key      InstrumentKey
  Side     Side      # side of the PROTECTIVE order (sell, for a long position)
  Quantity int
  Stop     Money
  Target   Money     # 0 = single leg
  StopLimit Money    # limit of the order the stop leg fires; 0 = at Stop
  Product  Product
```

`StopLimit` lets the stop leg's limit sit away from its trigger, so the exit
still fills through a fast move (a long's protective sell limit below the
trigger). Venues whose triggers fire market or trigger-priced orders (Upstox)
ignore it.

## Invariants

1. `Quantity` is always positive on an order or request; direction is `Side`.
   Only `Position.Quantity` is signed.
2. Every `Money` in a struct is in the instrument's currency minor unit. No
   struct mixes currencies.
3. Every `datetime` is timezone-aware. A naive datetime is a bug, not a default.
4. `Trade.NetPnL == Trade.GrossPnL - Trade.Charges`, always. A trade whose
   charges were not computed carries `Charges == 0`, never an estimate.
5. `Paper` is set at creation and never inferred later. Paper and live rows
   share tables and must never be aggregated together.
