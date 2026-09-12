package fyers

import (
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func TestSymbolForAppendsTheEQSeriesToCashEquity(t *testing.T) {
	cases := []struct {
		key  domain.InstrumentKey
		want string
	}{
		{domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, "NSE:SBIN-EQ"},
		{domain.InstrumentKey{Exchange: "BSE", Symbol: "SBIN"}, "BSE:SBIN-EQ"},
		{domain.InstrumentKey{Exchange: "NSE", Symbol: "MODIRUBBER-BE"}, "NSE:MODIRUBBER-BE"},
		{domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JANFUT"}, "NSE:NIFTY24JANFUT"},
		{domain.InstrumentKey{Exchange: "BFO", Symbol: "SENSEX24JANFUT"}, "BSE:SENSEX24JANFUT"},
		{domain.InstrumentKey{Exchange: "MCX", Symbol: "GOLD24DECFUT"}, "MCX:GOLD24DECFUT"},
		{domain.InstrumentKey{Exchange: "NSE", Symbol: "NSE:SBIN-EQ"}, "NSE:SBIN-EQ"},
	}
	for _, tc := range cases {
		if got := symbolFor(tc.key); got != tc.want {
			t.Errorf("symbolFor(%v) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestKeyForRoundTripsThroughSymbolFor(t *testing.T) {
	keys := []domain.InstrumentKey{
		{Exchange: "NSE", Symbol: "SBIN"},
		{Exchange: "NSE", Symbol: "MODIRUBBER-BE"},
		{Exchange: "NFO", Symbol: "NIFTY24JANFUT"},
		{Exchange: "NFO", Symbol: "NIFTY2410825000CE"},
		{Exchange: "BFO", Symbol: "SENSEX24JANFUT"},
		{Exchange: "MCX", Symbol: "GOLD24DECFUT"},
	}
	for _, k := range keys {
		if got := keyFor(symbolFor(k), 0); got != k {
			t.Errorf("a key must survive the wire round trip or the store cannot find its own instrument: %v -> %q -> %v", k, symbolFor(k), got)
		}
	}
}

func TestKeyForTrustsTheSegmentOverTheSymbolShape(t *testing.T) {
	if got := keyFor("NSE:USDINR24JANFUT", segmentCurrencyDeriv); got.Exchange != "CDS" {
		t.Errorf("segment 12 is currency derivatives, got exchange %q", got.Exchange)
	}
	if got := keyFor("NSE:NIFTY50-INDEX", 0); got != (domain.InstrumentKey{Exchange: "NSE", Symbol: "NIFTY50-INDEX"}) {
		t.Errorf("-INDEX is not a series and an index is not cash equity, got %v", got)
	}
	if got := keyFor("SBIN", 0); got.Exchange != "" || got.Symbol != "SBIN" {
		t.Errorf("a symbol with no exchange must not be invented one, got %v", got)
	}
}

func TestProductMappingKeepsTheThreeIndianBucketsDistinct(t *testing.T) {
	for _, p := range []domain.Product{domain.CNC, domain.MIS, domain.NRML} {
		wire, err := toProduct(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got := fromProduct(wire); got != p {
			t.Errorf("%s -> %s -> %s: the product must round-trip, or a position's settlement changes on read-back", p, wire, got)
		}
	}
	if _, err := toProduct(domain.Margin); err == nil {
		t.Error("the US margin bucket has no FYERS equivalent and must fail at wiring time, not be substituted")
	}
	if got := fromProduct("MTF"); got != "mtf" {
		t.Errorf("MTF is not a delivery holding and must not be disguised as one, got %q", got)
	}
}

func TestOrderTypeCodesAreTheDocumentedIntegers(t *testing.T) {
	cases := map[domain.OrderType]int{domain.Limit: 1, domain.Market: 2, domain.Stop: 3, domain.StopLimit: 4}
	for typ, code := range cases {
		got, err := toOrderType(typ)
		if err != nil || got != code {
			t.Errorf("%s must be %d (a wrong integer is a different valid order type, not an error), got %d %v", typ, code, got, err)
		}
		if back := fromOrderType(code); back != typ {
			t.Errorf("code %d must read back as %s, got %s", code, typ, back)
		}
	}
	if got := fromOrderType(9); got == domain.Market {
		t.Error("an unknown code must not silently become a market order")
	}
}

func TestStatusCodesDistinguishTransitFromResting(t *testing.T) {
	cases := map[int]domain.OrderStatus{
		1: domain.StatusCancelled,
		2: domain.StatusComplete,
		4: domain.StatusPending,
		5: domain.StatusRejected,
		6: domain.StatusOpen,
		7: domain.StatusCancelled,
	}
	for code, want := range cases {
		if got := normalizeStatus(code); got != want {
			t.Errorf("status %d: want %s got %s", code, want, got)
		}
	}
	if got := normalizeStatus(3); got.Terminal() {
		t.Error("a status the adapter does not understand must never read as terminal, or a reconciler abandons a live order")
	}
}

func TestGTTIsNotAnOrderValidity(t *testing.T) {
	if _, err := toValidity(domain.GTT); err == nil {
		t.Error("downgrading GTT to DAY would leave a position with no resting stop; it must be refused")
	}
	if v, _ := toValidity(""); v != validityDay {
		t.Errorf("an unset validity is DAY, got %q", v)
	}
}

func TestSidesAreSignedIntegers(t *testing.T) {
	if toSide(domain.Buy) != 1 || toSide(domain.Sell) != -1 {
		t.Error("FYERS encodes buy as 1 and sell as -1")
	}
	if fromSide(-1) != domain.Sell || fromSide(1) != domain.Buy || fromSide(0) != domain.Buy {
		t.Error("a negative side is a sell; zero (a closed position) reads as buy and is never acted on")
	}
}

func TestPaiseRoundsHalfAwayFromZero(t *testing.T) {
	if got := paise(1057.605); got != 105761 {
		t.Errorf("1057.605 must round to 105761 paise to match core/money, got %d", got)
	}
	if got := paise(-1057.605); got != -105761 {
		t.Errorf("-1057.605 must round to -105761 paise, got %d", got)
	}
	if got := rupees(money.MustParse("2456.75")); got != 2456.75 {
		t.Errorf("rupees must be the exact float FYERS expects, got %v", got)
	}
}

func TestResolutionAndChunkDaysStayUnderTheDocumentedCeilings(t *testing.T) {
	r, err := toResolution(domain.M5)
	if err != nil || r != "5" {
		t.Errorf("5m is resolution \"5\", got %q %v", r, err)
	}
	r, _ = toResolution(domain.D1)
	if r != "D" {
		t.Errorf("daily is resolution \"D\", got %q", r)
	}
	if _, err := toResolution(domain.Timeframe("2h")); err == nil {
		t.Error("an unsupported timeframe must be refused, not sent as an empty resolution")
	}
	if d := chunkDays(domain.M1); d >= 100 {
		t.Errorf("intraday chunks must sit under the 100-day ceiling, got %d", d)
	}
	if d := chunkDays(domain.D1); d >= 366 || d <= 100 {
		t.Errorf("daily chunks must sit under the 366-day ceiling and above the intraday one, got %d", d)
	}
}

func TestOrderTimeIsReadAsIST(t *testing.T) {
	got := parseOrderTime("09-Mar-2023 09:34:38")
	want := time.Date(2023, 3, 9, 9, 34, 38, 0, ist)
	if !got.Equal(want) {
		t.Errorf("the order book's timestamp is IST with no offset; got %v want %v", got, want)
	}
	if !parseOrderTime("garbage").IsZero() {
		t.Error("an unparseable timestamp must be the zero time, not now: a fabricated time silently reorders a book")
	}
}

func TestStripTagPrefixRestoresTheCallersTag(t *testing.T) {
	if got := stripTagPrefix("1:breakout"); got != "breakout" {
		t.Errorf("FYERS prepends 1: to a caller's tag; it must be removed so the order round-trips, got %q", got)
	}
	if got := stripTagPrefix("2:Untagged"); got != "" {
		t.Errorf("a 2: tag is FYERS's own and the strategy set none, got %q", got)
	}
}
