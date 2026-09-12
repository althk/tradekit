package costs

import (
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/money"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// table builds a schedule with round rates chosen so every expected figure
// below can be verified by hand.
func table() *Table {
	t := NewTable()
	epoch := date(2020, 1, 1)
	b, seg := "testbroker", EquityDelivery
	t.Set(b, seg, Brokerage, Rate{Value: 0.001, EffectiveFrom: epoch}) // 0.1% per leg
	t.Set(b, seg, STTBuy, Rate{Value: 0.001, EffectiveFrom: epoch})    // 0.1% on buy
	t.Set(b, seg, STTSell, Rate{Value: 0.001, EffectiveFrom: epoch})   // 0.1% on sell
	t.Set(b, seg, Exchange, Rate{Value: 0.0001, EffectiveFrom: epoch}) // 0.01% per leg
	t.Set(b, seg, SEBI, Rate{Value: 0.0001, EffectiveFrom: epoch})
	t.Set(b, seg, Stamp, Rate{Value: 0.0001, EffectiveFrom: epoch}) // buy leg only
	t.Set(b, seg, DP, Rate{Value: 1500, Flat: true, EffectiveFrom: epoch})
	t.Set(b, seg, GST, Rate{Value: 0.18, EffectiveFrom: epoch})
	return t
}

func TestComputeRoundTrip(t *testing.T) {
	tr := Trade{
		Broker:     "testbroker",
		Segment:    EquityDelivery,
		Quantity:   100,
		EntryPrice: money.MustParse("100.00"), // 10_000 paise
		ExitPrice:  money.MustParse("110.00"), // 11_000 paise
		EntryAt:    date(2026, 1, 5),
		ExitAt:     date(2026, 1, 9),
		Buying:     true,
	}
	got, err := Compute(table(), tr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Turnover: buy 1_000_000 paise, sell 1_100_000 paise.
	// Brokerage 0.1% of each leg: 1000 + 1100 = 2100
	// STT 0.1% of each leg:       1000 + 1100 = 2100
	// Exchange 0.01% of each:      100 +  110 =  210
	// SEBI     0.01% of each:      100 +  110 =  210
	// Stamp    0.01% of buy only:               100
	// DP flat:                                 1500
	// GST 18% of (2100+210+210+1500) = 18% of 4020 = 723.6 -> 724
	want := Charges{
		Brokerage: 2100,
		STT:       2100,
		Exchange:  210,
		SEBI:      210,
		Stamp:     100,
		DP:        1500,
		GST:       724,
		Total:     2100 + 2100 + 210 + 210 + 100 + 1500 + 724,
	}
	if got != want {
		t.Errorf("Compute()\n got %+v\nwant %+v", got, want)
	}
}

func TestGSTExcludesSTTAndStamp(t *testing.T) {
	// Raise STT and stamp far above every other component. If GST were
	// applied to them the total would move; it must not.
	tbl := table()
	tbl.Set("testbroker", EquityDelivery, STTBuy, Rate{Value: 0.5, EffectiveFrom: date(2020, 1, 1)})
	tbl.Set("testbroker", EquityDelivery, Stamp, Rate{Value: 0.5, EffectiveFrom: date(2020, 1, 1)})

	tr := Trade{
		Broker: "testbroker", Segment: EquityDelivery, Quantity: 100,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("110.00"),
		EntryAt: date(2026, 1, 5), ExitAt: date(2026, 1, 9), Buying: true,
	}
	got, err := Compute(tbl, tr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.GST != 724 {
		t.Errorf("GST = %d, want 724: it must not be levied on STT or stamp duty", got.GST)
	}
}

func TestStampAppliesToBuyLegOfAShort(t *testing.T) {
	// A short sells first and buys back. Stamp duty follows the buy leg,
	// which here is the exit at the higher price.
	tbl := table()
	short := Trade{
		Broker: "testbroker", Segment: EquityDelivery, Quantity: 100,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("110.00"),
		EntryAt: date(2026, 1, 5), ExitAt: date(2026, 1, 9), Buying: false,
	}
	got, err := Compute(tbl, short)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 0.01% of the 1_100_000 paise buy-back leg.
	if got.Stamp != 110 {
		t.Errorf("Stamp = %d, want 110 (the buy leg of a short is its exit)", got.Stamp)
	}
}

func TestMissingComponentIsNotAnError(t *testing.T) {
	// An intraday schedule with no DP and no stamp: both must come out zero
	// rather than failing or falling back to the delivery rate.
	tbl := NewTable()
	epoch := date(2020, 1, 1)
	tbl.Set("testbroker", EquityIntraday, Brokerage, Rate{Value: 0.0003, EffectiveFrom: epoch})
	tbl.Set("testbroker", EquityIntraday, GST, Rate{Value: 0.18, EffectiveFrom: epoch})

	got, err := Compute(tbl, Trade{
		Broker: "testbroker", Segment: EquityIntraday, Quantity: 100,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("100.00"),
		EntryAt: date(2026, 1, 5), ExitAt: date(2026, 1, 5), Buying: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.DP != 0 || got.Stamp != 0 || got.STT != 0 {
		t.Errorf("absent components must be zero, got DP=%d Stamp=%d STT=%d", got.DP, got.Stamp, got.STT)
	}
	// Brokerage 0.03% of 1_000_000 on each leg = 300 + 300.
	if got.Brokerage != 600 {
		t.Errorf("Brokerage = %d, want 600", got.Brokerage)
	}
}

func TestBrokerageCap(t *testing.T) {
	tbl := NewTable()
	epoch := date(2020, 1, 1)
	// 0.1% capped at Rs 20 per leg.
	tbl.Set("b", EquityIntraday, Brokerage, Rate{Value: 0.001, Cap: money.MustParse("20.00"), EffectiveFrom: epoch})

	got, err := Compute(tbl, Trade{
		Broker: "b", Segment: EquityIntraday, Quantity: 1000,
		EntryPrice: money.MustParse("500.00"), ExitPrice: money.MustParse("500.00"),
		EntryAt: date(2026, 1, 5), ExitAt: date(2026, 1, 5), Buying: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Uncapped each leg would be 0.1% of 50_000_000 paise = 50_000 paise
	// (Rs 500); the cap pins each to Rs 20, so Rs 40 in total.
	if got.Brokerage != money.MustParse("40.00") {
		t.Errorf("Brokerage = %s, want 40.00 (capped per leg)", got.Brokerage)
	}
}

func TestEffectiveDatingPricesEachLegOnItsOwnDate(t *testing.T) {
	tbl := NewTable()
	tbl.Set("b", EquityDelivery, STTBuy, Rate{Value: 0.001, EffectiveFrom: date(2020, 1, 1)})
	tbl.Set("b", EquityDelivery, STTSell, Rate{Value: 0.001, EffectiveFrom: date(2020, 1, 1)})
	// The sell rate doubles partway through the holding period.
	tbl.Set("b", EquityDelivery, STTSell, Rate{Value: 0.002, EffectiveFrom: date(2026, 2, 1)})

	got, err := Compute(tbl, Trade{
		Broker: "b", Segment: EquityDelivery, Quantity: 100,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("100.00"),
		EntryAt: date(2026, 1, 20), // old rate
		ExitAt:  date(2026, 2, 10), // new rate
		Buying:  true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Buy 0.1% of 1_000_000 = 1000; sell 0.2% of 1_000_000 = 2000.
	if got.STT != 3000 {
		t.Errorf("STT = %d, want 3000: each leg must use the rate in force on its own date", got.STT)
	}
}

func TestLookupDistinguishesAbsentFromZero(t *testing.T) {
	tbl := NewTable()
	tbl.Set("b", EquityDelivery, DP, Rate{Value: 0, Flat: true, EffectiveFrom: date(2020, 1, 1)})

	if _, ok := tbl.Lookup("b", EquityDelivery, DP, date(2026, 1, 1)); !ok {
		t.Error("a zero rate that was explicitly set must be reported present")
	}
	if _, ok := tbl.Lookup("b", EquityDelivery, Brokerage, date(2026, 1, 1)); ok {
		t.Error("a component never set must be reported absent")
	}
	if _, ok := tbl.Lookup("b", EquityDelivery, DP, date(2019, 1, 1)); ok {
		t.Error("a date before the first effective_from must be reported absent")
	}
}

func TestComputeRejectsNonPositiveQuantity(t *testing.T) {
	if _, err := Compute(table(), Trade{Broker: "testbroker", Segment: EquityDelivery, Quantity: 0}); err == nil {
		t.Error("a zero quantity must be rejected rather than priced as free")
	}
}

func TestDefaultTemplateIsAllZero(t *testing.T) {
	tbl := DefaultIndiaEquityDelivery("zerodha", date(2020, 1, 1))
	got, err := Compute(tbl, Trade{
		Broker: "zerodha", Segment: EquityDelivery, Quantity: 10,
		EntryPrice: money.MustParse("100.00"), ExitPrice: money.MustParse("110.00"),
		EntryAt: date(2026, 1, 5), ExitAt: date(2026, 1, 9), Buying: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("the template must price nothing until it is filled in, got total %s", got.Total)
	}
}
