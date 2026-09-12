package upstox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func TestPlaceProtectiveOCOSendsBothLegsWithTheRightDirections(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/order/gtt/place": `{"status":"success","data":{"gtt_order_id":"GTT-1"}}`,
	})

	id, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Sell, // protecting a long
		Quantity: 10,
		Stop:     money.MustParse("1000.00"),
		Target:   money.MustParse("1150.00"),
		Product:  domain.CNC,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "GTT-1" {
		t.Errorf("the trigger id must be returned so the stop can later be modified, got %q", id)
	}

	var body gttRequest
	if err := json.Unmarshal(sent["/order/gtt/place"], &body); err != nil {
		t.Fatalf("decoding the sent payload: %v", err)
	}
	if body.Type != gttMultiple {
		t.Errorf("a stop and a target are a two-leg trigger, got type %q", body.Type)
	}
	if len(body.Rules) != 2 {
		t.Fatalf("both legs must be sent, got %d rules", len(body.Rules))
	}
	byStrategy := map[string]gttRule{}
	for _, r := range body.Rules {
		byStrategy[r.Strategy] = r
	}
	stop, ok := byStrategy[ruleStoploss]
	if !ok || stop.TriggerType != triggerBelow || stop.TriggerPrice != 1000.00 {
		t.Errorf("a sell that protects a long stops BELOW; got %+v", stop)
	}
	target, ok := byStrategy[ruleTarget]
	if !ok || target.TriggerType != triggerAbove || target.TriggerPrice != 1150.00 {
		t.Errorf("a sell's target is ABOVE; got %+v", target)
	}
}

func TestPlaceProtectiveOnAShortMirrorsTheDirections(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/order/gtt/place": `{"status":"success","data":{"gtt_order_id":"GTT-2"}}`,
	})

	_, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Buy, // protecting a short
		Quantity: 10,
		Stop:     money.MustParse("1150.00"),
		Target:   money.MustParse("1000.00"),
		Product:  domain.MIS,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body gttRequest
	if err := json.Unmarshal(sent["/order/gtt/place"], &body); err != nil {
		t.Fatalf("decoding the sent payload: %v", err)
	}
	for _, r := range body.Rules {
		if r.Strategy == ruleStoploss && r.TriggerType != triggerAbove {
			t.Errorf("a short's stop is ABOVE the entry; assigning by role rather than side turns it into a target. got %+v", r)
		}
		if r.Strategy == ruleTarget && r.TriggerType != triggerBelow {
			t.Errorf("a short's target is BELOW the entry, got %+v", r)
		}
	}
}

func TestPlaceProtectiveSingleLegNeedsALevel(t *testing.T) {
	c, _ := newRouted(t, routes{})
	_, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Sell,
		Quantity: 10,
		Product:  domain.CNC,
	})
	if err == nil {
		t.Error("a trigger with neither a stop nor a target protects nothing and must be refused")
	}
}

func TestModifyStopPreservesTheTargetLeg(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/order/gtt": `{"status":"success","data":[{
			"gtt_order_id":"GTT-1","type":"MULTIPLE","exchange":"NSE",
			"trading_symbol":"RELIANCE","transaction_type":"SELL","product":"D","quantity":10,
			"rules":[{"strategy":"STOPLOSS","trigger_type":"BELOW","trigger_price":1000.0,"status":"ACTIVE"},
			         {"strategy":"TARGET","trigger_type":"ABOVE","trigger_price":1150.0,"status":"ACTIVE"}]}]}`,
		"/order/gtt/modify": `{"status":"success","data":{"gtt_order_id":"GTT-1"}}`,
	})

	if err := c.ModifyStop(context.Background(), "GTT-1", money.MustParse("1050.00")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body gttRequest
	if err := json.Unmarshal(sent["/order/gtt/modify"], &body); err != nil {
		t.Fatalf("decoding the sent payload: %v", err)
	}
	if body.GTTOrderID != "GTT-1" {
		t.Errorf("the modify must name the trigger it replaces, got %q", body.GTTOrderID)
	}
	if len(body.Rules) != 2 {
		t.Fatalf("Upstox replaces the whole rule set; sending only the stop drops the target silently. got %d rules", len(body.Rules))
	}
	var stop, target float64
	for _, r := range body.Rules {
		switch r.Strategy {
		case ruleStoploss:
			stop = r.TriggerPrice
		case ruleTarget:
			target = r.TriggerPrice
		}
	}
	if stop != 1050.00 {
		t.Errorf("the stop must move to the new level, got %v", stop)
	}
	if target != 1150.00 {
		t.Errorf("the target must survive a trailing adjustment unchanged, got %v", target)
	}
	if body.Quantity != 10 {
		t.Errorf("the quantity must be carried across, got %d", body.Quantity)
	}
}

func TestModifyStopReportsATriggerThatIsNotInTheBook(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/order/gtt": `{"status":"success","data":[]}`,
	})
	if err := c.ModifyStop(context.Background(), "GTT-gone", money.MustParse("1050.00")); err == nil {
		t.Error("modifying a trigger that no longer exists must be reported; silently succeeding would leave a position unprotected while the caller believes it trailed")
	}
}

func TestCancelProtectiveSendsTheIDInTheBody(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/order/gtt/cancel": `{"status":"success"}`,
	})
	if err := c.CancelProtective(context.Background(), "GTT-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(sent["/order/gtt/cancel"], &body); err != nil {
		t.Fatalf("the id goes in a JSON body, not the query string: %v", err)
	}
	if body["gtt_order_id"] != "GTT-1" {
		t.Errorf("the query form is answered with UDAPI100038 and no indication of what was wrong; got body %v", body)
	}
}

func TestListProtectiveDropsDeadTriggers(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/order/gtt": `{"status":"success","data":[
			{"gtt_order_id":"live","exchange":"NSE","trading_symbol":"RELIANCE","transaction_type":"SELL","product":"D","quantity":10,
			 "rules":[{"strategy":"ENTRY","trigger_type":"BELOW","trigger_price":1000.0,"status":"ACTIVE"}]},
			{"gtt_order_id":"cancelled","exchange":"NSE","trading_symbol":"TCS","transaction_type":"SELL","product":"D","quantity":5,
			 "rules":[{"strategy":"ENTRY","trigger_type":"BELOW","trigger_price":3400.0,"status":"CANCELLED"}]},
			{"gtt_order_id":"triggered","exchange":"NSE","trading_symbol":"INFY","transaction_type":"SELL","product":"D","quantity":5,
			 "rules":[{"strategy":"ENTRY","trigger_type":"BELOW","trigger_price":1400.0,"status":"TRIGGERED"}]}]}`,
	})

	live, err := c.ListProtective(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(live) != 1 || live[0].ID != "live" {
		t.Fatalf("the book returns cancelled and triggered rows alongside live ones; returning them makes a dead stop look like protection. got %+v", live)
	}
	if live[0].Stop != money.MustParse("1000.00") {
		t.Errorf("a single-leg ENTRY rule below a sell's price is the stop, got %s", live[0].Stop)
	}
}

func TestGTTRowWithAnUnknownStatusReadsAsLive(t *testing.T) {
	row := gttRow{Rules: []gttRule{{Status: "SOMETHING NEW"}}}
	if !row.live() {
		t.Error("an unknown status raising a false missing-stop alert on every position every morning would bury the real ones; it must read as live")
	}
	if !(gttRow{}).live() {
		t.Error("a row with no rules to judge on must be assumed live rather than reported as missing protection")
	}
}

func TestFromGTTReadsASingleLegByDirection(t *testing.T) {
	c, _ := newRouted(t, routes{})
	// A single-leg rule is labelled ENTRY whatever it protects, so only the
	// direction relative to the order's side says which level it is.
	short := c.fromGTT(gttRow{
		GTTOrderID:      "1",
		TransactionType: "BUY", // protecting a short
		Rules:           []gttRule{{Strategy: ruleEntry, TriggerType: triggerAbove, TriggerPrice: 1150}},
	})
	if short.Stop != money.MustParse("1150.00") || short.Target != 0 {
		t.Errorf("an ABOVE trigger on a protective buy is the stop, got stop=%s target=%s", short.Stop, short.Target)
	}
	long := c.fromGTT(gttRow{
		GTTOrderID:      "2",
		TransactionType: "SELL",
		Rules:           []gttRule{{Strategy: ruleEntry, TriggerType: triggerAbove, TriggerPrice: 1150}},
	})
	if long.Target != money.MustParse("1150.00") || long.Stop != 0 {
		t.Errorf("an ABOVE trigger on a protective sell is the target, got stop=%s target=%s", long.Stop, long.Target)
	}
}
