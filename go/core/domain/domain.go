// Package domain holds the types that cross module and language boundaries.
//
// Every type here is defined by contracts/domain.md, which the Python package
// implements identically. Renaming a field in one language only is a defect,
// not a refactor: the two libraries read each other's rows out of the same
// SQLite file.
package domain

import (
	"time"

	"github.com/althk/tradekit/go/core/money"
)

// Side is the direction of an order or a fill.
type Side string

// The two order directions. A short position is expressed by a sell entry, not
// by a negative quantity.
const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Opposite returns the side that closes a position opened with s.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrderType is how the order is priced and triggered.
type OrderType string

// Supported order types. Venue-specific variants (Kite's SL-M, Alpaca's
// trailing stop) are expressed by an adapter in terms of these.
const (
	Market    OrderType = "market"
	Limit     OrderType = "limit"
	Stop      OrderType = "stop"
	StopLimit OrderType = "stop_limit"
)

// Product is the settlement and margin bucket the position sits in.
type Product string

// Product codes, named as the Indian brokers name them because that is where
// the distinction actually bites. Alpaca maps everything to Margin or CNC.
const (
	CNC    Product = "cnc"    // delivery
	MIS    Product = "mis"    // intraday, auto square-off
	NRML   Product = "nrml"   // overnight derivatives
	Margin Product = "margin" // US margin account
)

// TimeInForce is how long an unfilled order stays live.
type TimeInForce string

// Supported validities.
const (
	Day TimeInForce = "day"
	IOC TimeInForce = "ioc"
	GTT TimeInForce = "gtt"
)

// OrderStatus is the lifecycle state of an order, normalised across brokers.
type OrderStatus string

// Order lifecycle states. Terminal returns whether no further change is
// expected.
const (
	StatusPending   OrderStatus = "pending"
	StatusOpen      OrderStatus = "open"
	StatusComplete  OrderStatus = "complete"
	StatusCancelled OrderStatus = "cancelled"
	StatusRejected  OrderStatus = "rejected"
	StatusTriggered OrderStatus = "triggered"
)

// Terminal reports whether the order can no longer change state, which is what
// a reconciler needs in order to stop polling it.
func (s OrderStatus) Terminal() bool {
	switch s {
	case StatusComplete, StatusCancelled, StatusRejected:
		return true
	default:
		return false
	}
}

// SignalKind is what a strategy is asking for.
type SignalKind string

// Signal kinds. Exit kinds exist so a strategy can close a position it cannot
// describe with a price level.
const (
	Long      SignalKind = "long"
	Short     SignalKind = "short"
	ExitLong  SignalKind = "exit_long"
	ExitShort SignalKind = "exit_short"
)

// ExitReason classifies why a position was closed.
//
// The set is deliberately closed. Every backtest and every live journal must
// classify an exit into the same categories, or the statistics they produce
// cannot be compared with each other.
type ExitReason string

// The exhaustive set of exit reasons.
const (
	ExitStop       ExitReason = "stop"
	ExitTarget     ExitReason = "target"
	ExitTrail      ExitReason = "trail"
	ExitTimeStop   ExitReason = "time_stop"
	ExitEOD        ExitReason = "eod"
	ExitSignal     ExitReason = "signal"
	ExitKillSwitch ExitReason = "kill_switch"
	ExitManual     ExitReason = "manual"
)

// Timeframe is a bar size.
type Timeframe string

// Supported bar sizes.
const (
	M1  Timeframe = "1m"
	M3  Timeframe = "3m"
	M5  Timeframe = "5m"
	M15 Timeframe = "15m"
	M30 Timeframe = "30m"
	M60 Timeframe = "60m"
	D1  Timeframe = "1d"
	W1  Timeframe = "1w"
)

// Duration returns the wall-clock span of one bar. Daily and weekly bars
// return 0: their boundaries come from the exchange calendar, not from
// arithmetic, so a caller must ask the calendar rather than add a duration.
func (t Timeframe) Duration() time.Duration {
	switch t {
	case M1:
		return time.Minute
	case M3:
		return 3 * time.Minute
	case M5:
		return 5 * time.Minute
	case M15:
		return 15 * time.Minute
	case M30:
		return 30 * time.Minute
	case M60:
		return time.Hour
	default:
		return 0
	}
}

// Intraday reports whether the bar is smaller than one session.
func (t Timeframe) Intraday() bool { return t.Duration() > 0 }

// InstrumentKey is the venue-neutral identity of a tradable instrument. A
// broker adapter maps it to and from that broker's own encoding.
type InstrumentKey struct {
	Exchange string
	Symbol   string
}

// String renders the key as "EXCHANGE:SYMBOL", the form used in logs and as a
// map key in memory. It is not a broker identifier.
func (k InstrumentKey) String() string { return k.Exchange + ":" + k.Symbol }

// Instrument is a tradable contract and the reference data needed to size and
// price an order in it.
type Instrument struct {
	Key        InstrumentKey
	Name       string
	ISIN       string
	Segment    string // "equity", "futures", "options", "index"
	LotSize    int    // 1 for cash equity
	TickSize   money.Money
	Expiry     *time.Time  // derivatives only
	Strike     money.Money // options only; 0 otherwise
	OptionType string      // "ce", "pe", or ""
	Active     bool
}

// Candle is one completed OHLCV bar. Start is the bar's opening timestamp and
// is always timezone-aware.
type Candle struct {
	Key          InstrumentKey
	Timeframe    Timeframe
	Start        time.Time
	Open         money.Money
	High         money.Money
	Low          money.Money
	Close        money.Money
	Volume       int64
	OpenInterest int64
}

// Tick is a single traded print.
type Tick struct {
	Key    InstrumentKey
	At     time.Time
	Price  money.Money
	Volume int64 // 0 when the feed does not carry per-print size
}

// Quote is the still-forming session's state for an instrument.
type Quote struct {
	Key   InstrumentKey
	At    time.Time
	Last  money.Money
	Open  money.Money
	High  money.Money
	Low   money.Money
	Close money.Money // previous session's close
	Bid   money.Money // 0 when the feed carries no depth
	Ask   money.Money
}

// Account is the funds view a risk check needs.
type Account struct {
	Equity    money.Money
	Available money.Money
	Used      money.Money
}

// Position is an open holding. Quantity is signed: negative is short. It is
// the only place in the domain where a signed quantity appears.
type Position struct {
	Key           InstrumentKey
	Quantity      int
	AveragePrice  money.Money
	Product       Product
	RealizedPnL   money.Money
	UnrealizedPnL money.Money
}

// OrderRequest is an instruction to the broker. Quantity is always positive;
// direction is carried by Side.
type OrderRequest struct {
	Key          InstrumentKey
	Side         Side
	Quantity     int
	Type         OrderType
	Product      Product
	LimitPrice   money.Money // 0 for market
	TriggerPrice money.Money // 0 unless stop or stop_limit
	TimeInForce  TimeInForce
	Tag          string // strategy identifier, echoed back by the broker
}

// Order is a request plus the broker's view of what happened to it.
type Order struct {
	ID             string
	Request        OrderRequest
	Status         OrderStatus
	FilledQuantity int
	AveragePrice   money.Money
	PlacedAt       time.Time
	UpdatedAt      time.Time
	Message        string
	// ProtectiveID is the id of the resting stop (a Kite GTT, an Upstox
	// GTT, an Alpaca bracket leg) that covers this order's fill, or "" when
	// none has been placed. It is what a reconciler reads to decide whether
	// a filled entry still needs protecting, and what a dashboard shows.
	ProtectiveID string
}

// Fill is one execution against an order.
type Fill struct {
	OrderID  string
	Key      InstrumentKey
	Side     Side
	Quantity int
	Price    money.Money
	At       time.Time
}

// Protective is a resting stop, or a stop and target as one cancels-other
// pair. It is the single representation for Kite's GTT, Upstox's GTT and
// Alpaca's bracket legs.
type Protective struct {
	ID       string
	Key      InstrumentKey
	Side     Side // the protective order's own side: sell, for a long position
	Quantity int
	Stop     money.Money
	Target   money.Money // 0 means a single-leg stop
	// StopLimit is the limit price of the order the stop leg fires, for
	// venues whose triggers place limit orders. 0 means at Stop. A long's
	// protective sell limit sits a little below its trigger so that the
	// exit still fills through a fast move rather than resting unfilled
	// above the market -- which is a stop that did not stop.
	StopLimit money.Money
	Product   Product
}

// OCO reports whether both legs are set, which is what decides whether an
// adapter places a one-cancels-other trigger or a single-leg one.
func (p Protective) OCO() bool { return p.Stop > 0 && p.Target > 0 }

// MarginLeg is one leg of a basket for a margin estimate.
type MarginLeg struct {
	Key      InstrumentKey
	Side     Side
	Quantity int
	Product  Product
	Price    money.Money // 0 to let the broker use the last traded price
}

// Signal is a strategy's request to enter or exit.
type Signal struct {
	Key      InstrumentKey
	Kind     SignalKind
	At       time.Time
	Price    money.Money // reference entry price
	Stop     money.Money
	Target   money.Money // 0 when the strategy has no fixed target
	Strategy string
	Metadata map[string]string
}

// Side returns the order side that opens the position this signal describes.
func (s Signal) Side() Side {
	switch s.Kind {
	case Long, ExitShort:
		return Buy
	default:
		return Sell
	}
}

// Trade is a closed round trip. NetPnL is always GrossPnL minus Charges; a
// trade whose charges were never computed carries Charges of 0 rather than an
// estimate.
type Trade struct {
	Key        InstrumentKey
	Strategy   string
	Side       Side // side of the entry
	Quantity   int
	EntryPrice money.Money
	ExitPrice  money.Money
	EntryAt    time.Time
	ExitAt     time.Time
	GrossPnL   money.Money
	Charges    money.Money
	NetPnL     money.Money
	ExitReason ExitReason
	Paper      bool
}

// GrossFor computes the gross profit of a round trip, given the entry side.
// It is the one place the long/short sign convention is written down.
func GrossFor(side Side, quantity int, entry, exit money.Money) money.Money {
	per := exit - entry
	if side == Sell {
		per = -per
	}
	return per.Mul(int64(quantity))
}
