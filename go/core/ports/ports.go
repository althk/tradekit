// Package ports declares the interfaces that separate strategy and engine code
// from any particular broker, data vendor or clock.
//
// The design rule here is that Broker is small and everything else is an
// optional capability a caller type-asserts for. The five broker interfaces
// tradekit replaces ranged from 8 to 14 methods with almost no overlap beyond
// placing an order; merging them into one interface would have forced every
// adapter to stub methods its venue cannot perform, and a stub that returns
// "not supported" at runtime is worse than a compile-time absence.
//
// A consumer therefore asks for what it needs:
//
//	if q, ok := brk.(ports.Quoter); ok {
//	    prices, err := q.LTP(ctx, keys)
//	}
//
// and a strategy that needs a capability the configured broker lacks fails at
// wiring time with a clear message, rather than at 09:15 with a nil result.
package ports

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// Broker is the minimum every execution venue must provide. Anything a paper
// simulator, a live Indian broker and a US broker can all do belongs here;
// anything else is a capability below.
type Broker interface {
	// Account returns the funds view used by risk checks.
	Account(ctx context.Context) (domain.Account, error)

	// PlaceOrder submits an order and returns the broker's acknowledgement.
	// The returned Order carries the broker's ID even when the status is
	// still pending.
	PlaceOrder(ctx context.Context, req domain.OrderRequest) (domain.Order, error)

	// CancelOrder withdraws an unfilled or partially filled order.
	CancelOrder(ctx context.Context, id string) error

	// OrderStatus fetches one order's current state.
	OrderStatus(ctx context.Context, id string) (domain.Order, error)

	// Positions returns all open positions.
	Positions(ctx context.Context) ([]domain.Position, error)

	// OpenOrders returns orders that have not reached a terminal state.
	OpenOrders(ctx context.Context) ([]domain.Order, error)
}

// Quoter provides current prices. Separate from Broker because a data-only
// vendor implements it without being able to trade, and a simulator implements
// it from its replay feed.
type Quoter interface {
	// LTP returns the last traded price for each requested instrument.
	// Instruments the venue does not know are omitted rather than erroring,
	// so one bad symbol cannot fail a 500-symbol scan.
	LTP(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]money.Money, error)

	// Quote returns the still-forming session's OHLC and last price.
	Quote(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]domain.Quote, error)
}

// HistoryProvider serves completed bars. It is the port the sync layer and
// every backtest read through.
type HistoryProvider interface {
	// Candles returns bars in ascending time order over [from, to].
	// Implementations handle the venue's per-request span limit internally;
	// a caller asking for ten years must not have to know about chunking.
	Candles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error)
}

// ProtectiveOrders is the capability to rest a stop, or a stop and target as a
// one-cancels-other pair, at the venue. Kite calls it a GTT, Alpaca calls it a
// bracket; the distinction that matters to a strategy is only whether the
// venue will honour the stop when the process is not running.
type ProtectiveOrders interface {
	PlaceProtective(ctx context.Context, p domain.Protective) (id string, err error)
	ModifyStop(ctx context.Context, id string, stop money.Money) error
	CancelProtective(ctx context.Context, id string) error
	ListProtective(ctx context.Context) ([]domain.Protective, error)
}

// MarginEstimator answers what a basket would cost in margin before it is
// placed. Only the Indian brokers offer it, and only derivatives strategies
// need it.
type MarginEstimator interface {
	BasketMargin(ctx context.Context, legs []domain.MarginLeg) (money.Money, error)
}

// Streamer delivers live updates. Handlers are called from a background
// goroutine; cancelling ctx stops the subscription.
type Streamer interface {
	SubscribeTicks(ctx context.Context, keys []domain.InstrumentKey, handle func(domain.Tick)) error
	SubscribeOrders(ctx context.Context, handle func(domain.Order)) error
}

// PositionCloser closes one position in a single call, at a known price.
//
// A simulator must implement it: its positions live inside its own book with
// the P&L bookkeeping attached, and a generic counter order would open a
// second position rather than close the first. A live broker may omit it, and
// the caller falls back to a counter order built from Positions.
type PositionCloser interface {
	// ClosePosition closes any open position in key on the given side at
	// price, tagging the resulting trade with reason. `at` is market time,
	// not wall clock: a simulator has no other way to stamp the trade, and
	// an unstamped trade drops out of every time-ordered analysis.
	// It reports whether a position was found and closed.
	ClosePosition(ctx context.Context, key domain.InstrumentKey, side domain.Side, price money.Money, at time.Time, reason domain.ExitReason) (bool, error)
}

// TickObserver is implemented by a broker that must fill its own resting
// protective orders and therefore needs to see traded prices.
//
// A live adapter does not implement it: its stops rest at the exchange and
// trigger without the engine's help. A simulator does, because nothing else
// will ever trigger them — without a price feed a simulated position has no
// stop at all, and a backtest that silently loses its stops reports fictional
// results.
type TickObserver interface {
	OnTick(key domain.InstrumentKey, price money.Money, at time.Time)
}

// InstrumentSource lists the instruments a venue trades, for the universe and
// reference-data sync.
type InstrumentSource interface {
	Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error)
}

// TokenState reports whether the adapter's credentials are usable right now.
// Indian brokers issue access tokens that expire daily, and a scheduler needs
// to know before the open rather than on the first rejected order.
type TokenState interface {
	// TokenFresh reports whether the current access token is valid for the
	// current trading day.
	TokenFresh(ctx context.Context) bool
	// SetAccessToken installs a token obtained earlier -- from the store
	// after a restart, typically -- and records when it was issued, which is
	// what TokenFresh judges by.
	SetAccessToken(token string, issuedAt time.Time)
}

// ErrTokenExpired is the venue-neutral form of "log in again". Each adapter
// wraps its own ErrTokenExpired around this one, so code written against the
// interfaces here -- session reuse in harness, a scheduler's pre-open check --
// can errors.Is for it without importing every adapter.
var ErrTokenExpired = errors.New("access token expired")

// BrowserLogin is the redirect-based login every Indian broker uses to issue
// the day's access token: the user visits LoginURL, the broker sends the
// browser back to a registered redirect URL with a single-use code in the
// query string, and the adapter exchanges the code for a token.
//
// The callback hands the adapter the whole query rather than a code because
// the brokers disagree on everything about it: Kite sends request_token,
// Upstox code, FYERS auth_code plus a state the adapter must verify, and each
// reports a refused login in its own way. Keeping that inside the adapter is
// what lets one callback server in harness serve all of them.
type BrowserLogin interface {
	// LoginURL is where the user starts. Calling it may begin a login attempt
	// (FYERS generates the state it later checks), so call it once per attempt.
	LoginURL() string
	// LoginCallback takes the query string the broker redirected back with,
	// exchanges the code in it for an access token, installs the token on the
	// adapter and returns it so the caller can persist it. A refused login
	// arrives here too, as an error naming the broker's reason.
	LoginCallback(ctx context.Context, query url.Values) (string, error)
}

// Clock is the source of "now". Live code uses the system clock; a backtest
// supplies simulated time, which is what lets the same strategy run in both
// without knowing which it is in.
type Clock interface {
	Now() time.Time
}

// HolidaySource reports the days an exchange is closed. Implementations are
// expected to cache: a backtest runs offline and must not need the network.
type HolidaySource interface {
	// Closed returns the closed dates for an exchange within [from, to],
	// keyed by "2006-01-02" in the exchange's own timezone and valued by
	// the holiday's description.
	//
	// The key is a string rather than a time.Time deliberately. Two
	// time.Time values for the same instant compare unequal as map keys
	// when their *Location pointers differ, and time.LoadLocation returns a
	// fresh pointer on every call — so a time-keyed map would look correct,
	// compile, and silently never match a single holiday. A date string
	// also matches how the date is stored in SQLite and how the Python
	// implementation keys the same map.
	Closed(ctx context.Context, exchange string, from, to time.Time) (map[string]string, error)
}

// Notifier delivers an operator-facing message. Failures are reported but must
// never abort trading: an unsent Telegram alert is not a reason to skip a stop
// loss.
type Notifier interface {
	Notify(ctx context.Context, subject, body string) error
}

// NewsGate lets a strategy ask whether a price move is explained by genuinely
// bad information before trading against it.
//
// asOf is the market timestamp of the decision, not wall-clock time, so an
// implementation can bound "recent news" against the bar being evaluated. A
// backtest injects nil (no gate) or a recorded one.
type NewsGate interface {
	Assess(ctx context.Context, key domain.InstrumentKey, asOf time.Time) (GateVerdict, error)
}

// GateVerdict is the outcome of a NewsGate consultation. Reason is always
// populated: it is journalled with the signal so a blocked or allowed entry
// can be audited afterwards.
type GateVerdict struct {
	Allow  bool
	Reason string
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current system time.
func (SystemClock) Now() time.Time { return time.Now() }
