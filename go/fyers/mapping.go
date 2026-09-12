package fyers

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// FYERS's wire vocabulary.
//
// FYERS encodes most enumerations as small integers rather than strings, so a
// typo here is not a misspelt word the broker rejects but a different, valid
// value the broker accepts: 1 is a limit order and 2 a market order, 1 is buy
// and -1 sell. These are restated from the API reference and pinned against the
// official Go SDK by constants_test.go, so a renumbering upstream fails the
// build rather than reaching the exchange.
const (
	productCNC      = "CNC"      // delivery
	productIntraday = "INTRADAY" // MIS
	productMargin   = "MARGIN"   // overnight derivatives, NRML
	productMTF      = "MTF"      // margin trading facility; no domain equivalent

	orderTypeLimit     = 1
	orderTypeMarket    = 2
	orderTypeStop      = 3 // SL-M: a stop that becomes a market order
	orderTypeStopLimit = 4 // SL-L: a stop with a limit price

	sideBuy  = 1
	sideSell = -1

	validityDay = "DAY"
	validityIOC = "IOC"

	statusCancelled = 1
	statusTraded    = 2
	statusTransit   = 4
	statusRejected  = 5
	statusPending   = 6
	statusExpired   = 7

	// gttSingle and gttOCO are the values of gtt_oco_ind in the GTT book.
	gttSingle = 1
	gttOCO    = 2

	// responseOK and responseError are the two values of the "s" field
	// every response carries.
	responseOK    = "ok"
	responseError = "error"
)

// Segment codes, as the order book, positions and symbol master report them.
const (
	segmentCapitalMarket = 10
	segmentEquityDeriv   = 11
	segmentCurrencyDeriv = 12
	segmentCommodity     = 20
)

// defaultSeries is the NSE series appended to a bare cash-equity symbol.
//
// FYERS addresses cash equity as "NSE:SBIN-EQ" and the domain as {NSE, SBIN}.
// EQ is the rolling-settlement series nearly every listed company trades in;
// a symbol in another series (BE, BZ, SM) carries it in the domain symbol, as
// "MODIRUBBER-BE", and is passed through untouched.
const defaultSeries = "EQ"

// symbolFor renders an instrument in FYERS's "EX:SYMBOL-SERIES" form.
//
// Unlike Upstox, FYERS addresses instruments by a symbol the adapter can
// derive: the exchange, the trading symbol, and for cash equity a series. That
// is why this adapter needs no key-resolver option. A symbol already carrying a
// colon is taken to be in FYERS's form and passed through, so a caller holding
// keys in FYERS's own vocabulary is not forced to round-trip them.
//
// Derivatives on NSE trade on FYERS under the NSE prefix rather than a separate
// NFO one, so the domain's NFO and CDS exchanges map to NSE and BFO to BSE.
func symbolFor(k domain.InstrumentKey) string {
	symbol := strings.TrimSpace(k.Symbol)
	if strings.Contains(symbol, ":") {
		return symbol
	}
	switch strings.ToUpper(strings.TrimSpace(k.Exchange)) {
	case "NSE", "BSE":
		if strings.Contains(symbol, "-") {
			return strings.ToUpper(k.Exchange) + ":" + symbol
		}
		return strings.ToUpper(k.Exchange) + ":" + symbol + "-" + defaultSeries
	case "NFO", "CDS":
		return "NSE:" + symbol
	case "BFO", "BCD":
		return "BSE:" + symbol
	default:
		return strings.ToUpper(strings.TrimSpace(k.Exchange)) + ":" + symbol
	}
}

// keyFor reconstructs an instrument key from a FYERS symbol.
//
// The segment code, when a response carries one, decides between cash and
// derivatives; when it is absent (zero) the symbol's shape decides, because a
// cash symbol always carries a "-SERIES" suffix and a derivative never does.
// The EQ series is stripped so the key round-trips through symbolFor; any other
// series is kept in the symbol, since dropping it would make "SBIN-BE" and
// "SBIN-EQ" the same instrument.
func keyFor(symbol string, segment int) domain.InstrumentKey {
	symbol = strings.TrimSpace(symbol)
	exchange, rest, found := strings.Cut(symbol, ":")
	if !found {
		return domain.InstrumentKey{Symbol: symbol}
	}
	exchange = strings.ToUpper(exchange)

	base, series, hasSeries := strings.Cut(rest, "-")
	// "-INDEX" is a suffix FYERS puts on index quotes; it is not a series
	// and an index is not a cash equity.
	if hasSeries && strings.EqualFold(series, "INDEX") {
		return domain.InstrumentKey{Exchange: exchange, Symbol: rest}
	}

	cash := hasSeries
	switch segment {
	case segmentCapitalMarket:
		cash = true
	case segmentEquityDeriv, segmentCurrencyDeriv, segmentCommodity:
		cash = false
	}

	if cash {
		if hasSeries && strings.EqualFold(series, defaultSeries) {
			return domain.InstrumentKey{Exchange: exchange, Symbol: base}
		}
		return domain.InstrumentKey{Exchange: exchange, Symbol: rest}
	}

	switch exchange {
	case "NSE":
		if segment == segmentCurrencyDeriv {
			return domain.InstrumentKey{Exchange: "CDS", Symbol: rest}
		}
		return domain.InstrumentKey{Exchange: "NFO", Symbol: rest}
	case "BSE":
		if segment == segmentCurrencyDeriv {
			return domain.InstrumentKey{Exchange: "BCD", Symbol: rest}
		}
		return domain.InstrumentKey{Exchange: "BFO", Symbol: rest}
	default:
		return domain.InstrumentKey{Exchange: exchange, Symbol: rest}
	}
}

// rupees converts a minor-unit amount to the whole-currency float FYERS
// expects on the wire.
//
// The conversion is confined to this pair of functions, as in the other
// adapters: FYERS speaks rupees as JSON numbers and tradekit speaks paise as
// int64, and every place that boundary is crossed ad hoc is a place a rounding
// error can enter the P&L.
func rupees(m money.Money) float64 { return float64(m) / 100 }

// paise converts a rupee amount from a response back to minor units, rounding
// half away from zero to match core/money.
func paise(v float64) money.Money {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return money.Money(math.Round(v * 100))
}

// toProduct maps a domain product to FYERS's code.
//
// FYERS keeps the three Indian buckets distinct, so unlike Upstox nothing is
// collapsed. Margin is the US margin-account bucket and has no FYERS
// equivalent, so it is an error at wiring time rather than a silent
// substitution that would change a real order's settlement. MTF is not offered
// in the other direction: the domain has no product for it, and a strategy
// that wants margin funding on a delivery position should say so explicitly.
func toProduct(p domain.Product) (string, error) {
	switch p {
	case domain.CNC:
		return productCNC, nil
	case domain.MIS:
		return productIntraday, nil
	case domain.NRML:
		return productMargin, nil
	default:
		return "", fmt.Errorf("fyers: no FYERS product for %q", p)
	}
}

// fromProduct maps FYERS's product code back to the domain. MTF, which the
// domain does not name, is passed through lowercased rather than disguised as
// CNC: a position funded on margin is not a delivery holding.
func fromProduct(s string) domain.Product {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case productCNC:
		return domain.CNC
	case productIntraday:
		return domain.MIS
	case productMargin:
		return domain.NRML
	default:
		return domain.Product(strings.ToLower(strings.TrimSpace(s)))
	}
}

// toOrderType maps a domain order type to FYERS's integer code.
//
// FYERS's 3 is the stop that becomes a market order and 4 the stop with a limit
// price, so tradekit's Stop maps to 3 and StopLimit to 4.
func toOrderType(t domain.OrderType) (int, error) {
	switch t {
	case domain.Market:
		return orderTypeMarket, nil
	case domain.Limit:
		return orderTypeLimit, nil
	case domain.Stop:
		return orderTypeStop, nil
	case domain.StopLimit:
		return orderTypeStopLimit, nil
	default:
		return 0, fmt.Errorf("fyers: no FYERS order type for %q", t)
	}
}

// fromOrderType maps FYERS's integer code back to the domain. An unknown code
// is rendered as a string rather than guessed at, so it shows up in a log
// instead of silently becoming a market order.
func fromOrderType(code int) domain.OrderType {
	switch code {
	case orderTypeMarket:
		return domain.Market
	case orderTypeLimit:
		return domain.Limit
	case orderTypeStop:
		return domain.Stop
	case orderTypeStopLimit:
		return domain.StopLimit
	default:
		return domain.OrderType(fmt.Sprintf("unknown(%d)", code))
	}
}

// toValidity maps a domain time-in-force to FYERS's validity.
//
// GTT is not a validity at FYERS any more than it is at Kite or Upstox: it is a
// separate trigger API reached through the ProtectiveOrders capability. Asking
// for it here is a wiring mistake worth catching rather than quietly
// downgrading to DAY, which would leave a position with no resting stop.
func toValidity(t domain.TimeInForce) (string, error) {
	switch t {
	case domain.Day, "":
		return validityDay, nil
	case domain.IOC:
		return validityIOC, nil
	case domain.GTT:
		return "", fmt.Errorf("fyers: GTT is a separate trigger API, not an order validity; use PlaceProtective")
	default:
		return "", fmt.Errorf("fyers: no FYERS validity for %q", t)
	}
}

// fromValidity maps FYERS's validity back to the domain, defaulting to Day —
// which is what FYERS applies when it is not told otherwise, and what keeps a
// broker order round-tripping through toValidity.
func fromValidity(s string) domain.TimeInForce {
	if strings.EqualFold(strings.TrimSpace(s), validityIOC) {
		return domain.IOC
	}
	return domain.Day
}

// toSide maps a side to FYERS's signed integer.
func toSide(s domain.Side) int {
	if s == domain.Sell {
		return sideSell
	}
	return sideBuy
}

// fromSide maps FYERS's signed integer back to a side. Anything non-negative
// is a buy: FYERS also uses 0 for a closed position, and a flat position's side
// is never read by anything that would act on it.
func fromSide(code int) domain.Side {
	if code < 0 {
		return domain.Sell
	}
	return domain.Buy
}

// normalizeStatus maps FYERS's order status code onto the domain's.
//
// FYERS's 6 ("Pending") is an order resting at the exchange, which is the
// domain's Open; its 4 ("Transit") is one the exchange has not yet
// acknowledged, which is the domain's Pending. The two are easy to swap and the
// swap is silent, so the names are written out here. 7 ("Expired") is an
// IOC or day order the exchange let lapse; it ends the order as surely as a
// cancel does and is mapped to Cancelled rather than to something a reconciler
// would keep polling. An unrecognised code — 3 is documented as "for future
// use" — becomes Pending: a status the adapter does not understand must never
// read as terminal, or a reconciler abandons an order that is still live.
func normalizeStatus(code int) domain.OrderStatus {
	switch code {
	case statusTraded:
		return domain.StatusComplete
	case statusRejected:
		return domain.StatusRejected
	case statusCancelled, statusExpired:
		return domain.StatusCancelled
	case statusPending:
		return domain.StatusOpen
	default:
		return domain.StatusPending
	}
}

// toResolution maps a timeframe to FYERS's resolution string.
//
// Intraday resolutions are the bar length in minutes as a bare number; daily
// is "D" and weekly "1W". The daily form is what the SDK's own examples use
// and what the reference lists first.
func toResolution(tf domain.Timeframe) (string, error) {
	switch tf {
	case domain.M1:
		return "1", nil
	case domain.M3:
		return "3", nil
	case domain.M5:
		return "5", nil
	case domain.M15:
		return "15", nil
	case domain.M30:
		return "30", nil
	case domain.M60:
		return "60", nil
	case domain.D1:
		return "D", nil
	case domain.W1:
		return "1W", nil
	default:
		return "", fmt.Errorf("fyers: no FYERS resolution for timeframe %q", tf)
	}
}

// chunkDays is the largest span, in days, the history endpoint is asked for in
// one request.
//
// FYERS documents 100 days per request for intraday resolutions and 366 for
// daily and above. As in the other adapters the values sit under the ceiling
// rather than at it: requesting exactly the documented maximum fails
// intermittently around boundaries, and a sync that dies two years into a
// backfill is worse than one that makes a few more requests.
func chunkDays(tf domain.Timeframe) int {
	if tf.Intraday() {
		return 90
	}
	return 360
}

// ist is the zone every FYERS timestamp is expressed in.
var ist = mustLoadIST()

func mustLoadIST() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// A fixed offset is correct for IST, which has no daylight saving,
		// so a host without a zone database still parses order times
		// correctly rather than not at all.
		return time.FixedZone("IST", 5*3600+30*60)
	}
	return loc
}

// parseOrderTime reads the order book's "02-Jan-2006 15:04:05" timestamp,
// which is IST with no offset.
//
// A value that will not parse returns the zero time rather than time.Now(): a
// fabricated timestamp is indistinguishable from a real one and would silently
// reorder a time-sorted book.
func parseOrderTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("02-Jan-2006 15:04:05", s, ist); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, ist); err == nil {
		return t
	}
	return time.Time{}
}
