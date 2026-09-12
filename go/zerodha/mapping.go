package zerodha

import (
	"fmt"
	"math"
	"strings"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// Kite Connect's own string constants, restated here.
//
// They are restated rather than imported so that this file — which is all the
// translation between tradekit's domain and Kite's wire vocabulary — carries no
// dependency on the SDK and can be tested anywhere. constants_test.go, which
// does import the SDK, asserts every one of these still equals the SDK's own
// constant, so a rename upstream fails the build rather than silently sending
// an unrecognised value to the exchange.
const (
	varietyRegular = "regular"

	productCNC  = "CNC"
	productMIS  = "MIS"
	productNRML = "NRML"

	orderTypeMarket = "MARKET"
	orderTypeLimit  = "LIMIT"
	orderTypeSL     = "SL"
	orderTypeSLM    = "SL-M"

	validityDay = "DAY"
	validityIOC = "IOC"

	transactionTypeBuy  = "BUY"
	transactionTypeSell = "SELL"

	statusComplete  = "COMPLETE"
	statusRejected  = "REJECTED"
	statusCancelled = "CANCELLED"
)

// rupees converts a minor-unit amount to the whole-currency float the SDK
// expects on the wire.
//
// The conversion is confined to this pair of functions. Kite speaks rupees as
// float64 and tradekit speaks paise as int64, and every place that boundary is
// crossed ad hoc is a place a rounding error can enter the P&L.
func rupees(m money.Money) float64 { return float64(m) / 100 }

// paise converts a rupee amount from the SDK back to minor units, rounding half
// away from zero to match core/money.
func paise(v float64) money.Money {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return money.Money(math.Round(v * 100))
}

// quoteKey renders an instrument in the "EXCHANGE:TRADINGSYMBOL" form the
// quote, LTP and OHLC endpoints key their responses by.
func quoteKey(k domain.InstrumentKey) string { return k.Exchange + ":" + k.Symbol }

// parseQuoteKey reads an "EXCHANGE:TRADINGSYMBOL" key back into a key.
//
// A symbol may itself contain no colon, so a key with more than one is
// malformed rather than something to split leniently.
func parseQuoteKey(s string) (domain.InstrumentKey, error) {
	exchange, symbol, found := strings.Cut(s, ":")
	if !found || exchange == "" || symbol == "" || strings.Contains(symbol, ":") {
		return domain.InstrumentKey{}, fmt.Errorf("zerodha: malformed quote key %q", s)
	}
	return domain.InstrumentKey{Exchange: exchange, Symbol: symbol}, nil
}

// toProduct maps a domain product to Kite's code.
//
// Margin has no Kite equivalent: it is the US margin-account bucket, and
// silently substituting MIS or NRML would change the settlement of a real
// order. An unsupported product is an error at wiring time instead.
func toProduct(p domain.Product) (string, error) {
	switch p {
	case domain.CNC:
		return productCNC, nil
	case domain.MIS:
		return productMIS, nil
	case domain.NRML:
		return productNRML, nil
	default:
		return "", fmt.Errorf("zerodha: no Kite product for %q", p)
	}
}

// fromProduct maps Kite's product code back to the domain.
func fromProduct(s string) domain.Product {
	switch s {
	case productCNC:
		return domain.CNC
	case productMIS:
		return domain.MIS
	case productNRML:
		return domain.NRML
	default:
		return domain.Product(strings.ToLower(s))
	}
}

// toOrderType maps a domain order type to Kite's.
//
// Kite distinguishes SL (stop with a limit price) from SL-M (stop that becomes
// a market order); tradekit's Stop is the market-on-trigger form, so it maps to
// SL-M and StopLimit to SL.
func toOrderType(t domain.OrderType) (string, error) {
	switch t {
	case domain.Market:
		return orderTypeMarket, nil
	case domain.Limit:
		return orderTypeLimit, nil
	case domain.Stop:
		return orderTypeSLM, nil
	case domain.StopLimit:
		return orderTypeSL, nil
	default:
		return "", fmt.Errorf("zerodha: no Kite order type for %q", t)
	}
}

// fromOrderType maps Kite's order type back to the domain.
func fromOrderType(s string) domain.OrderType {
	switch s {
	case orderTypeMarket:
		return domain.Market
	case orderTypeLimit:
		return domain.Limit
	case orderTypeSLM:
		return domain.Stop
	case orderTypeSL:
		return domain.StopLimit
	default:
		return domain.OrderType(strings.ToLower(s))
	}
}

// toValidity maps a domain time-in-force to Kite's validity.
//
// GTT is not a validity at Kite: it is a separate trigger API, reached through
// the ProtectiveOrders capability. An order asking for it here is a wiring
// mistake worth catching rather than quietly downgrading to DAY.
func toValidity(t domain.TimeInForce) (string, error) {
	switch t {
	case domain.Day, "":
		return validityDay, nil
	case domain.IOC:
		return validityIOC, nil
	case domain.GTT:
		return "", fmt.Errorf("zerodha: GTT is a separate trigger API, not an order validity; use PlaceProtective")
	default:
		return "", fmt.Errorf("zerodha: no Kite validity for %q", t)
	}
}

// fromValidity maps Kite's validity back to the domain.
//
// An unrecognised validity falls back to Day rather than to an empty value,
// because Day is what Kite applies when it is not told otherwise, and an empty
// TimeInForce read back from a broker order would fail toValidity on any
// round trip through it.
func fromValidity(s string) domain.TimeInForce {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case validityIOC:
		return domain.IOC
	default:
		return domain.Day
	}
}

// toTransactionType maps a side to Kite's transaction type.
func toTransactionType(s domain.Side) string {
	if s == domain.Sell {
		return transactionTypeSell
	}
	return transactionTypeBuy
}

// fromTransactionType maps Kite's transaction type back to a side.
func fromTransactionType(s string) domain.Side {
	if strings.EqualFold(s, transactionTypeSell) {
		return domain.Sell
	}
	return domain.Buy
}

// normalizeStatus maps Kite's order status onto the domain's six.
//
// Kite reports around a dozen states, most of which are stages of "the order is
// on its way": PUT ORDER REQ RECEIVED, VALIDATION PENDING, OPEN PENDING,
// MODIFY PENDING, and so on. Collapsing them to Pending and Open is what lets a
// reconciler ask a single question — is this terminal — instead of carrying a
// list of broker-specific strings. An unrecognised status maps to Pending
// rather than to a terminal state, so a reconciler keeps watching an order it
// does not understand instead of abandoning it.
func normalizeStatus(s string) domain.OrderStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case statusComplete:
		return domain.StatusComplete
	case statusRejected:
		return domain.StatusRejected
	case statusCancelled, "CANCELLED AMO":
		return domain.StatusCancelled
	case "OPEN":
		return domain.StatusOpen
	case "TRIGGER PENDING":
		return domain.StatusTriggered
	default:
		return domain.StatusPending
	}
}

// toInterval maps a timeframe to Kite's historical-data interval.
func toInterval(tf domain.Timeframe) (string, error) {
	switch tf {
	case domain.M1:
		return "minute", nil
	case domain.M3:
		return "3minute", nil
	case domain.M5:
		return "5minute", nil
	case domain.M15:
		return "15minute", nil
	case domain.M30:
		return "30minute", nil
	case domain.M60:
		return "60minute", nil
	case domain.D1:
		return "day", nil
	default:
		return "", fmt.Errorf("zerodha: no Kite interval for timeframe %q", tf)
	}
}

// chunkDays is the largest span, in days, that the historical endpoint will
// serve in one request for an interval.
//
// The values are deliberately under Kite's documented ceilings — 55 against 60,
// 90 against 100, 180 against 200, 1800 against 2000. Requesting exactly the
// documented maximum fails intermittently around holidays and daylight
// boundaries, and a sync that dies two years into a backfill is worse than one
// that makes a few more requests.
func chunkDays(interval string) int {
	switch interval {
	case "minute":
		return 55
	case "3minute", "5minute", "10minute":
		return 90
	case "15minute", "30minute", "60minute":
		return 180
	default:
		return 1800
	}
}

// segmentOf classifies a Kite instrument's segment into the domain's vocabulary.
func segmentOf(instrumentType, exchange string) string {
	switch strings.ToUpper(instrumentType) {
	case "CE", "PE":
		return "options"
	case "FUT":
		return "futures"
	}
	if strings.EqualFold(exchange, "INDICES") {
		return "index"
	}
	return "equity"
}

// optionTypeOf normalises an instrument type to the domain's option marker,
// returning an empty string for anything that is not an option.
func optionTypeOf(instrumentType string) string {
	switch strings.ToUpper(instrumentType) {
	case "CE":
		return "ce"
	case "PE":
		return "pe"
	default:
		return ""
	}
}
