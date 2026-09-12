package upstox

import (
	"fmt"
	"math"
	"strings"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// Upstox's wire vocabulary.
//
// Unlike the Kite adapter's constants these are not restated from an SDK —
// there is no Go SDK for Upstox and every donor hand-rolls the REST calls — so
// there is nothing for a constants_test.go to pin them against. They are traced
// instead to donor code known to work against the live API; upstox-api.txt
// records the file and line for each.
const (
	productIntraday = "I" // MIS
	productDelivery = "D" // CNC and NRML both land here; Upstox has two buckets

	orderTypeMarket = "MARKET"
	orderTypeLimit  = "LIMIT"
	orderTypeSL     = "SL"   // stop with a limit price
	orderTypeSLM    = "SL-M" // stop that becomes a market order

	validityDay = "DAY"
	validityIOC = "IOC"

	transactionTypeBuy  = "BUY"
	transactionTypeSell = "SELL"

	triggerBelow = "BELOW"
	triggerAbove = "ABOVE"

	gttSingle   = "SINGLE"
	gttMultiple = "MULTIPLE"
)

// Segment prefixes of an instrument key. Exported because a caller resolving
// keys out of the instrument master needs to recognise them.
const (
	SegmentNSEEquity = "NSE_EQ"
	SegmentBSEEquity = "BSE_EQ"
	SegmentNSEFO     = "NSE_FO"
	SegmentNSEIndex  = "NSE_INDEX"
	SegmentBSEIndex  = "BSE_INDEX"
)

// instrumentKey renders an instrument in Upstox's "SEGMENT|ID" form.
//
// The id is an ISIN for cash equity ("NSE_EQ|INE002A01018") and an exchange
// token for derivatives ("NSE_FO|54321"), so it cannot be derived from an
// exchange and a symbol. This function only formats a key once the id is
// known; resolving the id is Options.InstrumentKey's job, wired to the store.
func instrumentKey(k domain.InstrumentKey, id string) string {
	return segmentFor(k.Exchange) + "|" + id
}

// segmentFor maps an exchange name to the segment prefix of an instrument key.
//
// An exchange that already looks like a segment ("NSE_EQ") is passed through,
// so a caller holding keys in Upstox's own vocabulary is not forced to
// round-trip them through a name this function happens not to know.
func segmentFor(exchange string) string {
	up := strings.ToUpper(strings.TrimSpace(exchange))
	if strings.Contains(up, "_") {
		return up
	}
	switch up {
	case "NSE":
		return SegmentNSEEquity
	case "BSE":
		return SegmentBSEEquity
	case "NFO":
		return SegmentNSEFO
	case "INDICES", "NSE_INDICES":
		return SegmentNSEIndex
	default:
		return up
	}
}

// parseInstrumentKey splits "SEGMENT|ID" into its parts.
//
// An expired contract's key carries a third segment — "NSE_FO|47983|17-04-2025"
// — and the whole remainder after the first separator is returned as the id, so
// such a key survives the round trip instead of being truncated to something
// the expired-candle endpoint would reject.
func parseInstrumentKey(s string) (segment, id string, err error) {
	segment, id, found := strings.Cut(strings.TrimSpace(s), "|")
	if !found || segment == "" || id == "" {
		return "", "", fmt.Errorf("upstox: malformed instrument key %q, want SEGMENT|ID", s)
	}
	return segment, id, nil
}

// exchangeFor maps a segment prefix back to the domain's exchange name, so a
// key read out of a response reconstructs the instrument it names.
func exchangeFor(segment string) string {
	switch strings.ToUpper(segment) {
	case SegmentNSEEquity, SegmentNSEIndex:
		return "NSE"
	case SegmentBSEEquity, SegmentBSEIndex:
		return "BSE"
	case SegmentNSEFO:
		return "NFO"
	default:
		return segment
	}
}

// rupees converts a minor-unit amount to the whole-currency float Upstox
// expects on the wire.
//
// The conversion is confined to this pair of functions, as in the Kite adapter:
// Upstox speaks rupees as JSON numbers and tradekit speaks paise as int64, and
// every place that boundary is crossed ad hoc is a place a rounding error can
// enter the P&L.
func rupees(m money.Money) float64 { return float64(m) / 100 }

// paise converts a rupee amount from a response back to minor units, rounding
// half away from zero to match core/money.
func paise(v float64) money.Money {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return money.Money(math.Round(v * 100))
}

// toProduct maps a domain product to Upstox's code.
//
// Upstox has only two buckets, so CNC and NRML both become D. That is not a
// loss: Upstox itself does not distinguish delivery from overnight-derivative
// margining in this field, and dhaara's mapProduct makes the same collapse.
// Margin is the US margin-account bucket and has no Upstox equivalent, so it
// is an error at wiring time rather than a silent substitution that would
// change a real order's settlement.
func toProduct(p domain.Product) (string, error) {
	switch p {
	case domain.MIS:
		return productIntraday, nil
	case domain.CNC, domain.NRML:
		return productDelivery, nil
	default:
		return "", fmt.Errorf("upstox: no Upstox product for %q", p)
	}
}

// fromProduct maps Upstox's product code back to the domain.
//
// D becomes NRML rather than CNC, matching dhaara's unmapProduct. The mapping
// is genuinely lossy in this direction — Upstox cannot tell the two apart — and
// NRML is the safer guess: it never claims a position is a delivery holding
// when it is a carried derivative.
func fromProduct(s string) domain.Product {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case productIntraday:
		return domain.MIS
	case productDelivery:
		return domain.NRML
	default:
		return domain.Product(strings.ToLower(s))
	}
}

// toOrderType maps a domain order type to Upstox's.
//
// As at Kite, SL is the stop with a limit price and SL-M the stop that becomes
// a market order, so tradekit's Stop maps to SL-M and StopLimit to SL.
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
		return "", fmt.Errorf("upstox: no Upstox order type for %q", t)
	}
}

// fromOrderType maps Upstox's order type back to the domain.
func fromOrderType(s string) domain.OrderType {
	switch strings.ToUpper(strings.TrimSpace(s)) {
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

// toValidity maps a domain time-in-force to Upstox's validity.
//
// GTT is not a validity at Upstox any more than it is at Kite: it is a separate
// trigger API reached through the ProtectiveOrders capability. Asking for it
// here is a wiring mistake worth catching rather than quietly downgrading to
// DAY, which would leave a position with no resting stop at all.
func toValidity(t domain.TimeInForce) (string, error) {
	switch t {
	case domain.Day, "":
		return validityDay, nil
	case domain.IOC:
		return validityIOC, nil
	case domain.GTT:
		return "", fmt.Errorf("upstox: GTT is a separate trigger API, not an order validity; use PlaceProtective")
	default:
		return "", fmt.Errorf("upstox: no Upstox validity for %q", t)
	}
}

// fromValidity maps Upstox's validity back to the domain, defaulting to Day —
// which is what Upstox applies when it is not told otherwise, and what keeps a
// broker order round-tripping through toValidity.
func fromValidity(s string) domain.TimeInForce {
	if strings.EqualFold(strings.TrimSpace(s), validityIOC) {
		return domain.IOC
	}
	return domain.Day
}

// toTransactionType maps a side to Upstox's transaction type.
func toTransactionType(s domain.Side) string {
	if s == domain.Sell {
		return transactionTypeSell
	}
	return transactionTypeBuy
}

// fromTransactionType maps Upstox's transaction type back to a side.
func fromTransactionType(s string) domain.Side {
	if strings.EqualFold(strings.TrimSpace(s), transactionTypeSell) {
		return domain.Sell
	}
	return domain.Buy
}

// normalizeStatus maps Upstox's order status onto the domain's six.
//
// The vocabulary is dhaara's normalizeStatus verbatim, which collapses the
// several "on its way" states — put order req received, validation pending,
// open pending, modify pending — onto Open. The one deliberate departure is the
// default: dhaara uppercases an unrecognised status and passes it through,
// while here it becomes Pending. A status the adapter does not understand must
// never read as terminal, or a reconciler abandons an order that is still live.
func normalizeStatus(s string) domain.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "complete", "completed":
		return domain.StatusComplete
	case "rejected":
		return domain.StatusRejected
	case "cancelled", "canceled":
		return domain.StatusCancelled
	case "open", "open pending", "modify pending", "put order req received", "validation pending":
		return domain.StatusOpen
	case "trigger pending":
		return domain.StatusTriggered
	default:
		return domain.StatusPending
	}
}

// toInterval maps a timeframe to Upstox's v3 unit and multiple.
//
// The v3 historical path is /{unit}/{interval}/, so a 5-minute bar is
// "minutes" with 5 rather than a single "5minute" token as at Kite.
func toInterval(tf domain.Timeframe) (unit string, interval int, err error) {
	switch tf {
	case domain.M1:
		return "minutes", 1, nil
	case domain.M3:
		return "minutes", 3, nil
	case domain.M5:
		return "minutes", 5, nil
	case domain.M15:
		return "minutes", 15, nil
	case domain.M30:
		return "minutes", 30, nil
	case domain.M60:
		return "hours", 1, nil
	case domain.D1:
		return "days", 1, nil
	case domain.W1:
		return "weeks", 1, nil
	default:
		return "", 0, fmt.Errorf("upstox: no Upstox interval for timeframe %q", tf)
	}
}

// chunkDays is the largest span, in days, the historical endpoint will serve in
// one request for a unit and multiple.
//
// breakout500's client records that minute data is rejected beyond about a
// month with UDAPI1148 and that callers must chunk. As in the Kite adapter the
// values sit under the ceiling rather than at it: requesting exactly the
// documented maximum fails intermittently around holidays, and a sync that dies
// two years into a backfill is worse than one that makes a few more requests.
func chunkDays(unit string, interval int) int {
	switch strings.ToLower(unit) {
	case "minutes":
		// A month is the documented bound for minute data whatever the
		// multiple; 25 days leaves room for the boundary being inclusive.
		if interval <= 1 {
			return 25
		}
		return 60
	case "hours":
		return 180
	default: // days, weeks, months
		return 1800
	}
}
