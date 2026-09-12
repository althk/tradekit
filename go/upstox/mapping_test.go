package upstox

import (
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func TestInstrumentKeyFormatsSegmentAndID(t *testing.T) {
	got := instrumentKey(domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}, "INE002A01018")
	if want := "NSE_EQ|INE002A01018"; got != want {
		t.Errorf("an NSE equity key must be %q so Upstox recognises it, got %q", want, got)
	}
	got = instrumentKey(domain.InstrumentKey{Exchange: "NSE_FO", Symbol: "NIFTY25000CE"}, "54321")
	if want := "NSE_FO|54321"; got != want {
		t.Errorf("an exchange already in segment form must pass through, want %q got %q", want, got)
	}
}

func TestParseInstrumentKeyKeepsAnExpiredContractsThirdField(t *testing.T) {
	segment, id, err := parseInstrumentKey("NSE_FO|47983|17-04-2025")
	if err != nil {
		t.Fatalf("an expired contract key must parse, got %v", err)
	}
	if segment != "NSE_FO" || id != "47983|17-04-2025" {
		t.Errorf("truncating the expiry would make the key unusable against the expired-candle endpoint; got %q, %q", segment, id)
	}
}

func TestParseInstrumentKeyRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "NSE_EQ", "|INE002A01018", "NSE_EQ|"} {
		if _, _, err := parseInstrumentKey(s); err == nil {
			t.Errorf("%q is not a valid instrument key but parsed without error", s)
		}
	}
}

func TestRupeesAndPaiseRoundTrip(t *testing.T) {
	for _, m := range []money.Money{0, 5, 105760, -105760, 12345678} {
		if got := paise(rupees(m)); got != m {
			t.Errorf("a price must survive the wire round trip: %d -> %v -> %d", m, rupees(m), got)
		}
	}
	if got := paise(1057.605); got != 105761 {
		t.Errorf("paise must round half away from zero to match core/money, got %d want 105761", got)
	}
}

func TestToProductCollapsesDeliveryAndRejectsMargin(t *testing.T) {
	for _, tc := range []struct {
		in   domain.Product
		want string
	}{
		{domain.MIS, productIntraday},
		{domain.CNC, productDelivery},
		{domain.NRML, productDelivery},
	} {
		got, err := toProduct(tc.in)
		if err != nil {
			t.Fatalf("%q is a product Upstox supports, got %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("product %q must map to %q, got %q", tc.in, tc.want, got)
		}
	}
	if _, err := toProduct(domain.Margin); err == nil {
		t.Error("Margin has no Upstox equivalent; substituting one silently would change a real order's settlement")
	}
}

func TestFromProductPrefersNRMLForDelivery(t *testing.T) {
	if got := fromProduct(productDelivery); got != domain.NRML {
		t.Errorf("D is ambiguous between CNC and NRML; NRML is the safer read, got %q", got)
	}
	if got := fromProduct(productIntraday); got != domain.MIS {
		t.Errorf("I must read back as MIS, got %q", got)
	}
}

func TestOrderTypeDistinguishesStopFromStopLimit(t *testing.T) {
	stop, _ := toOrderType(domain.Stop)
	stopLimit, _ := toOrderType(domain.StopLimit)
	if stop != orderTypeSLM {
		t.Errorf("Stop is market-on-trigger and must be SL-M, got %q", stop)
	}
	if stopLimit != orderTypeSL {
		t.Errorf("StopLimit must be SL, got %q", stopLimit)
	}
	if got := fromOrderType(orderTypeSLM); got != domain.Stop {
		t.Errorf("SL-M must read back as Stop, got %q", got)
	}
}

func TestToValidityRefusesGTT(t *testing.T) {
	if _, err := toValidity(domain.GTT); err == nil {
		t.Error("GTT is a separate trigger API; downgrading it to DAY would leave a position with no resting stop")
	}
	if got, _ := toValidity(""); got != validityDay {
		t.Errorf("an unset validity must default to DAY, got %q", got)
	}
}

func TestNormalizeStatusMapsUpstoxVocabulary(t *testing.T) {
	cases := map[string]domain.OrderStatus{
		"complete":               domain.StatusComplete,
		"completed":              domain.StatusComplete,
		"rejected":               domain.StatusRejected,
		"cancelled":              domain.StatusCancelled,
		"canceled":               domain.StatusCancelled,
		"open":                   domain.StatusOpen,
		"open pending":           domain.StatusOpen,
		"modify pending":         domain.StatusOpen,
		"put order req received": domain.StatusOpen,
		"validation pending":     domain.StatusOpen,
		"trigger pending":        domain.StatusTriggered,
		"  COMPLETE  ":           domain.StatusComplete,
	}
	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Errorf("status %q must normalise to %q, got %q", in, want, got)
		}
	}
}

func TestNormalizeStatusMapsUnknownToPending(t *testing.T) {
	got := normalizeStatus("after market order req received")
	if got != domain.StatusPending {
		t.Errorf("an unrecognised status must not read as terminal or a reconciler abandons a live order; got %q", got)
	}
	if got.Terminal() {
		t.Error("pending must not be terminal")
	}
}

func TestToIntervalSplitsUnitAndMultiple(t *testing.T) {
	cases := map[domain.Timeframe]struct {
		unit     string
		interval int
	}{
		domain.M1:  {"minutes", 1},
		domain.M5:  {"minutes", 5},
		domain.M15: {"minutes", 15},
		domain.M60: {"hours", 1},
		domain.D1:  {"days", 1},
		domain.W1:  {"weeks", 1},
	}
	for tf, want := range cases {
		unit, interval, err := toInterval(tf)
		if err != nil {
			t.Fatalf("%q is a timeframe Upstox serves, got %v", tf, err)
		}
		if unit != want.unit || interval != want.interval {
			t.Errorf("timeframe %q must map to %s/%d, got %s/%d", tf, want.unit, want.interval, unit, interval)
		}
	}
	if _, _, err := toInterval(domain.Timeframe("2m")); err == nil {
		t.Error("an unsupported timeframe must error rather than produce a path Upstox rejects")
	}
}

func TestChunkDaysStaysUnderTheMinuteCeiling(t *testing.T) {
	if got := chunkDays("minutes", 1); got >= 30 {
		t.Errorf("minute data is refused beyond about a month; %d days requests at or over the ceiling", got)
	}
	if chunkDays("days", 1) <= chunkDays("hours", 1) {
		t.Error("a coarser timeframe must permit at least as long a span")
	}
}

func TestExchangeForRoundTripsSegments(t *testing.T) {
	for _, tc := range []struct{ segment, want string }{
		{SegmentNSEEquity, "NSE"},
		{SegmentNSEIndex, "NSE"},
		{SegmentBSEEquity, "BSE"},
		{SegmentNSEFO, "NFO"},
	} {
		if got := exchangeFor(tc.segment); got != tc.want {
			t.Errorf("segment %q must read back as exchange %q, got %q", tc.segment, tc.want, got)
		}
	}
}
