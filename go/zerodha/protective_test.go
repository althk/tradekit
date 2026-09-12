package zerodha

import (
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func TestSingleLegStopHonoursStopLimit(t *testing.T) {
	p := domain.Protective{
		Key: reliance, Side: domain.Sell, Quantity: 7, Product: domain.CNC,
		Stop: money.MustParse("970.00"), StopLimit: money.MustParse("965.15"),
	}
	params, err := buildGTTParams(p, money.MustParse("1000.00"))
	if err != nil {
		t.Fatal(err)
	}
	leg, ok := params.Trigger.(*kiteconnect.GTTSingleLegTrigger)
	if !ok {
		t.Fatalf("a stop-only protective must be a single-leg trigger, got %T", params.Trigger)
	}
	if leg.TriggerValue != 970 || leg.LimitPrice != 965.15 {
		t.Errorf("trigger/limit = %v/%v, want 970/965.15: a limit at the trigger can rest unfilled through a fast move",
			leg.TriggerValue, leg.LimitPrice)
	}
	if leg.Quantity != 7 || params.TransactionType != "SELL" || params.Product != "CNC" || params.LastPrice != 1000 {
		t.Errorf("order details lost: %+v", params)
	}

	// Without a StopLimit the limit sits at the trigger, as every donor did.
	p.StopLimit = 0
	params, _ = buildGTTParams(p, money.MustParse("1000.00"))
	if leg := params.Trigger.(*kiteconnect.GTTSingleLegTrigger); leg.LimitPrice != 970 {
		t.Errorf("default limit = %v, want the trigger 970", leg.LimitPrice)
	}
}

// Kite orders OCO legs by price, not by role. For a long's protective sell the
// stop is the lower leg; for a short's protective buy it is the upper one, and
// assigning by role instead is the mistake that turns a stop into a target.
func TestOCOLegsAreOrderedByPriceNotRole(t *testing.T) {
	long := domain.Protective{
		Key: reliance, Side: domain.Sell, Quantity: 1, Product: domain.MIS,
		Stop: money.MustParse("95.00"), StopLimit: money.MustParse("94.50"), Target: money.MustParse("110.00"),
	}
	params, err := buildGTTParams(long, money.MustParse("100.00"))
	if err != nil {
		t.Fatal(err)
	}
	oco := params.Trigger.(*kiteconnect.GTTOneCancelsOtherTrigger)
	if oco.Lower.TriggerValue != 95 || oco.Lower.LimitPrice != 94.5 || oco.Upper.TriggerValue != 110 || oco.Upper.LimitPrice != 110 {
		t.Errorf("long: lower=%+v upper=%+v, want stop 95/94.5 below target 110", oco.Lower, oco.Upper)
	}

	short := domain.Protective{
		Key: reliance, Side: domain.Buy, Quantity: 1, Product: domain.MIS,
		Stop: money.MustParse("105.00"), StopLimit: money.MustParse("105.50"), Target: money.MustParse("90.00"),
	}
	params, err = buildGTTParams(short, money.MustParse("100.00"))
	if err != nil {
		t.Fatal(err)
	}
	oco = params.Trigger.(*kiteconnect.GTTOneCancelsOtherTrigger)
	if oco.Lower.TriggerValue != 90 || oco.Lower.LimitPrice != 90 || oco.Upper.TriggerValue != 105 || oco.Upper.LimitPrice != 105.5 {
		t.Errorf("short: lower=%+v upper=%+v, want target 90 below stop 105/105.5", oco.Lower, oco.Upper)
	}
	if params.TransactionType != "BUY" {
		t.Errorf("a short's protective order must buy, got %s", params.TransactionType)
	}
}
