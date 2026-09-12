package zerodha

import (
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func TestRupeesAndPaiseRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "0.05", "1.00", "1234.56", "-0.05", "-1234.56", "23000.00"} {
		m := money.MustParse(s)
		if got := paise(rupees(m)); got != m {
			t.Errorf("round trip of %s = %s", s, got)
		}
	}
}

func TestPaiseRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		in   float64
		want money.Money
	}{
		{100.005, 10001}, // .5 of a paisa rounds away from zero, matching core/money
		{100.004, 10000},
		{-100.005, -10001},
		{0, 0},
	}
	for _, c := range cases {
		if got := paise(c.in); got != c.want {
			t.Errorf("paise(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPaiseGuardsAgainstNonFinite(t *testing.T) {
	// A vendor sending NaN must not become a wild integer price.
	for _, v := range []float64{mathNaN(), mathInf(1), mathInf(-1)} {
		if got := paise(v); got != 0 {
			t.Errorf("paise(%v) = %d, want 0", v, got)
		}
	}
}

func mathNaN() float64 { var z float64; return z / z }
func mathInf(s int) float64 {
	var z float64
	if s < 0 {
		return -1 / z
	}
	return 1 / z
}

func TestQuoteKeyRoundTrip(t *testing.T) {
	keys := []domain.InstrumentKey{
		{Exchange: "NSE", Symbol: "RELIANCE"},
		{Exchange: "NFO", Symbol: "NIFTY26JAN23000CE"},
		{Exchange: "BSE", Symbol: "500325"},
	}
	for _, k := range keys {
		s := quoteKey(k)
		got, err := parseQuoteKey(s)
		if err != nil {
			t.Errorf("parseQuoteKey(%q): %v", s, err)
			continue
		}
		if got != k {
			t.Errorf("round trip of %v = %v", k, got)
		}
	}
	if got := quoteKey(keys[0]); got != "NSE:RELIANCE" {
		t.Errorf("quoteKey = %q, want NSE:RELIANCE", got)
	}
}

func TestParseQuoteKeyRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "RELIANCE", ":RELIANCE", "NSE:", "NSE:A:B"} {
		if _, err := parseQuoteKey(s); err == nil {
			t.Errorf("parseQuoteKey(%q) accepted a malformed key", s)
		}
	}
}

func TestProductMapping(t *testing.T) {
	cases := []struct {
		in   domain.Product
		want string
	}{
		{domain.CNC, "CNC"},
		{domain.MIS, "MIS"},
		{domain.NRML, "NRML"},
	}
	for _, c := range cases {
		got, err := toProduct(c.in)
		if err != nil || got != c.want {
			t.Errorf("toProduct(%s) = (%q, %v), want %q", c.in, got, err, c.want)
		}
		if back := fromProduct(got); back != c.in {
			t.Errorf("fromProduct(%q) = %s, want %s", got, back, c.in)
		}
	}
	// Margin is the US bucket; substituting MIS or NRML would change the
	// settlement of a real order, so it must fail loudly.
	if _, err := toProduct(domain.Margin); err == nil {
		t.Error("Margin has no Kite equivalent and must be rejected, not substituted")
	}
}

func TestOrderTypeMapping(t *testing.T) {
	cases := []struct {
		in   domain.OrderType
		want string
	}{
		{domain.Market, "MARKET"},
		{domain.Limit, "LIMIT"},
		// Stop is market-on-trigger, which Kite calls SL-M; StopLimit
		// carries a limit price, which Kite calls SL. Swapping these
		// turns a guaranteed exit into one that may never fill.
		{domain.Stop, "SL-M"},
		{domain.StopLimit, "SL"},
	}
	for _, c := range cases {
		got, err := toOrderType(c.in)
		if err != nil || got != c.want {
			t.Errorf("toOrderType(%s) = (%q, %v), want %q", c.in, got, err, c.want)
		}
		if back := fromOrderType(got); back != c.in {
			t.Errorf("fromOrderType(%q) = %s, want %s", got, back, c.in)
		}
	}
	if _, err := toOrderType(domain.OrderType("trailing")); err == nil {
		t.Error("an unknown order type must be rejected")
	}
}

func TestValidityMapping(t *testing.T) {
	if got, err := toValidity(domain.Day); err != nil || got != "DAY" {
		t.Errorf("toValidity(day) = (%q, %v)", got, err)
	}
	if got, err := toValidity(domain.IOC); err != nil || got != "IOC" {
		t.Errorf("toValidity(ioc) = (%q, %v)", got, err)
	}
	// An unset validity is the common case and must default rather than fail.
	if got, err := toValidity(""); err != nil || got != "DAY" {
		t.Errorf("toValidity(\"\") = (%q, %v), want DAY", got, err)
	}
	// GTT is a separate API. Downgrading it to DAY would place an order
	// that expires at the close instead of resting as a trigger.
	if _, err := toValidity(domain.GTT); err == nil {
		t.Error("GTT must be rejected as an order validity")
	}
}

func TestFromValidity(t *testing.T) {
	if got := fromValidity("IOC"); got != domain.IOC {
		t.Errorf("fromValidity(IOC) = %s", got)
	}
	if got := fromValidity("DAY"); got != domain.Day {
		t.Errorf("fromValidity(DAY) = %s", got)
	}
	// An unset or unknown validity must round-trip through toValidity
	// rather than producing a value toValidity would reject.
	for _, in := range []string{"", "TTL", "something new"} {
		got := fromValidity(in)
		if _, err := toValidity(got); err != nil {
			t.Errorf("fromValidity(%q) = %s, which toValidity rejects: %v", in, got, err)
		}
	}
}

func TestTransactionTypeMapping(t *testing.T) {
	if toTransactionType(domain.Buy) != "BUY" || toTransactionType(domain.Sell) != "SELL" {
		t.Error("transaction type mapping is wrong")
	}
	if fromTransactionType("SELL") != domain.Sell || fromTransactionType("BUY") != domain.Buy {
		t.Error("reverse transaction type mapping is wrong")
	}
	// Kite has been known to vary case across endpoints.
	if fromTransactionType("sell") != domain.Sell {
		t.Error("transaction type must be matched case-insensitively")
	}
}

func TestNormalizeStatus(t *testing.T) {
	cases := []struct {
		in   string
		want domain.OrderStatus
	}{
		{"COMPLETE", domain.StatusComplete},
		{"REJECTED", domain.StatusRejected},
		{"CANCELLED", domain.StatusCancelled},
		{"CANCELLED AMO", domain.StatusCancelled},
		{"OPEN", domain.StatusOpen},
		{"TRIGGER PENDING", domain.StatusTriggered},
		// The in-flight states all collapse to pending.
		{"PUT ORDER REQ RECEIVED", domain.StatusPending},
		{"VALIDATION PENDING", domain.StatusPending},
		{"OPEN PENDING", domain.StatusPending},
		{"MODIFY VALIDATION PENDING", domain.StatusPending},
		{"MODIFY PENDING", domain.StatusPending},
		{"CANCEL PENDING", domain.StatusPending},
		{"AMO REQ RECEIVED", domain.StatusPending},
		// Whitespace and case are not meaningful.
		{"  complete  ", domain.StatusComplete},
	}
	for _, c := range cases {
		if got := normalizeStatus(c.in); got != c.want {
			t.Errorf("normalizeStatus(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestUnknownStatusIsNotTerminal(t *testing.T) {
	// A status this build has never seen must keep the reconciler watching
	// the order, not make it abandon one that is still live.
	got := normalizeStatus("SOME NEW STATE ZERODHA ADDED")
	if got.Terminal() {
		t.Errorf("unknown status mapped to terminal %s", got)
	}
	if got != domain.StatusPending {
		t.Errorf("unknown status = %s, want pending", got)
	}
}

func TestIntervalMapping(t *testing.T) {
	cases := map[domain.Timeframe]string{
		domain.M1:  "minute",
		domain.M3:  "3minute",
		domain.M5:  "5minute",
		domain.M15: "15minute",
		domain.M30: "30minute",
		domain.M60: "60minute",
		domain.D1:  "day",
	}
	for tf, want := range cases {
		got, err := toInterval(tf)
		if err != nil || got != want {
			t.Errorf("toInterval(%s) = (%q, %v), want %q", tf, got, err, want)
		}
	}
	// Kite serves no weekly bars; resampling is the caller's job.
	if _, err := toInterval(domain.W1); err == nil {
		t.Error("weekly must be rejected: Kite does not serve it")
	}
}

func TestChunkDaysStaysUnderKitesCeilings(t *testing.T) {
	// Documented ceilings, which these must stay strictly under.
	ceilings := map[string]int{
		"minute": 60, "3minute": 100, "5minute": 100, "10minute": 100,
		"15minute": 200, "30minute": 200, "60minute": 200, "day": 2000,
	}
	for interval, ceiling := range ceilings {
		got := chunkDays(interval)
		if got <= 0 {
			t.Errorf("chunkDays(%q) = %d, must be positive", interval, got)
		}
		if got >= ceiling {
			t.Errorf("chunkDays(%q) = %d, must stay under the documented %d", interval, got, ceiling)
		}
	}
	// An unrecognised interval must fall back to the daily span rather
	// than to zero, which would make the caller loop forever.
	if got := chunkDays("weekly"); got <= 0 {
		t.Errorf("chunkDays of an unknown interval = %d, want a positive fallback", got)
	}
}

func TestSegmentAndOptionType(t *testing.T) {
	cases := []struct {
		instrumentType, exchange string
		wantSegment, wantOption  string
	}{
		{"EQ", "NSE", "equity", ""},
		{"CE", "NFO", "options", "ce"},
		{"PE", "NFO", "options", "pe"},
		{"FUT", "NFO", "futures", ""},
		{"EQ", "INDICES", "index", ""},
		{"ce", "NFO", "options", "ce"},
	}
	for _, c := range cases {
		if got := segmentOf(c.instrumentType, c.exchange); got != c.wantSegment {
			t.Errorf("segmentOf(%q, %q) = %q, want %q", c.instrumentType, c.exchange, got, c.wantSegment)
		}
		if got := optionTypeOf(c.instrumentType); got != c.wantOption {
			t.Errorf("optionTypeOf(%q) = %q, want %q", c.instrumentType, got, c.wantOption)
		}
	}
}
