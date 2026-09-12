package fyers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

const gttAck = `{"code":1101,"message":"Successfully placed order","s":"ok","id":"25012400002074"}`

func sentGTT(t *testing.T, raw []byte) gttRequest {
	t.Helper()
	var body gttRequest
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding sent GTT: %v (%s)", err, raw)
	}
	return body
}

func TestPlaceProtectiveOCOPutsTheUpperLegFirstForALong(t *testing.T) {
	c, sent := newRouted(t, routes{"/gtt/orders/sync": gttAck})
	id, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Sell, Quantity: 10,
		Stop: money.MustParse("590.00"), Target: money.MustParse("650.00"), Product: domain.CNC,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "25012400002074" {
		t.Errorf("the trigger id must be returned, got %q", id)
	}
	body := sentGTT(t, sent["/gtt/orders/sync"])
	if body.Side != -1 || body.Symbol != "NSE:SBIN-EQ" || body.ProductType != "CNC" {
		t.Errorf("side, symbol and product must be mapped, got %+v", body)
	}
	if body.OrderInfo.Leg1.TriggerPrice != 650 {
		t.Errorf("FYERS requires leg1 to trigger above the market; for a long that is the target. Got %v", body.OrderInfo.Leg1.TriggerPrice)
	}
	if body.OrderInfo.Leg2 == nil || body.OrderInfo.Leg2.TriggerPrice != 590 {
		t.Errorf("leg2 triggers below the market; for a long that is the stop. Got %+v", body.OrderInfo.Leg2)
	}
	if body.OrderInfo.Leg1.Qty != 10 || body.OrderInfo.Leg2.Qty != 10 {
		t.Error("both legs carry the full quantity")
	}
}

func TestPlaceProtectiveOnAShortMirrorsTheLegs(t *testing.T) {
	c, sent := newRouted(t, routes{"/gtt/orders/sync": gttAck})
	_, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key: domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JANFUT"}, Side: domain.Buy, Quantity: 25,
		Stop: money.MustParse("22500.00"), Target: money.MustParse("21500.00"), Product: domain.NRML,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := sentGTT(t, sent["/gtt/orders/sync"])
	if body.OrderInfo.Leg1.TriggerPrice != 22500 {
		t.Errorf("a short's stop is above the market and must be leg1, got %v", body.OrderInfo.Leg1.TriggerPrice)
	}
	if body.OrderInfo.Leg2 == nil || body.OrderInfo.Leg2.TriggerPrice != 21500 {
		t.Errorf("a short's target is below the market and must be leg2, got %+v", body.OrderInfo.Leg2)
	}
	if body.ProductType != "MARGIN" {
		t.Errorf("NRML is MARGIN at FYERS, got %q", body.ProductType)
	}
}

func TestPlaceProtectiveSingleLegSendsNoLeg2(t *testing.T) {
	c, sent := newRouted(t, routes{"/gtt/orders/sync": gttAck})
	_, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Sell, Quantity: 5,
		Stop: money.MustParse("590.00"), Product: domain.CNC,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(sent["/gtt/orders/sync"], &raw)
	info, _ := raw["orderInfo"].(map[string]any)
	if _, has := info["leg2"]; has {
		t.Error("a single-leg GTT must not send an empty leg2, which FYERS would reject or read as a zero-price leg")
	}
}

func TestPlaceProtectiveRefusesAnIntradayPosition(t *testing.T) {
	c, _ := newRouted(t, routes{})
	_, err := c.PlaceProtective(context.Background(), domain.Protective{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Sell, Quantity: 5,
		Stop: money.MustParse("590.00"), Product: domain.MIS,
	})
	if err == nil {
		t.Error("FYERS accepts no GTT for INTRADAY; the mistake must be caught at wiring time, not at the broker")
	}
}

const gttBookBody = `{"s":"ok","code":200,"message":"","orderBook":[
	{"id":"25012400002074","symbol":"NSE:SBIN-EQ","segment":10,"product_type":"CNC","tran_side":-1,
	 "qty":10,"qty2":10,"price_limit":650,"price_trigger":650,"price2_limit":590,"price2_trigger":590,
	 "gtt_oco_ind":2,"ord_status":6,"ltp":612.5},
	{"id":"25012400002099","symbol":"NSE:SBIN-EQ","segment":10,"product_type":"CNC","tran_side":-1,
	 "qty":3,"qty2":0,"price_limit":600,"price_trigger":600,"price2_limit":0,"price2_trigger":0,
	 "gtt_oco_ind":1,"ord_status":1,"ltp":612.5},
	{"id":"25012400002100","symbol":"NSE:NIFTY24JANFUT","segment":11,"product_type":"MARGIN","tran_side":1,
	 "qty":25,"qty2":0,"price_limit":22500,"price_trigger":22500,"price2_limit":0,"price2_trigger":0,
	 "gtt_oco_ind":1,"ord_status":6,"ltp":22000},
	{"id":"25012400002101","symbol":"NSE:SBIN-EQ","segment":10,"product_type":"CNC","tran_side":-1,
	 "qty":4,"qty2":0,"price_limit":700,"price_trigger":700,"price2_limit":0,"price2_trigger":0,
	 "gtt_oco_ind":1,"ord_status":6,"ltp":612.5}]}`

func TestListProtectiveDropsDeadTriggersAndReadsLegsBySide(t *testing.T) {
	c, _ := newRouted(t, routes{"/gtt/orders": gttBookBody})
	got, err := c.ListProtective(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("a cancelled trigger still looks like protection and must be dropped; got %d", len(got))
	}
	oco := got[0]
	if oco.Stop != money.MustParse("590.00") || oco.Target != money.MustParse("650.00") {
		t.Errorf("for a sell the lower level is the stop, got stop %s target %s", oco.Stop, oco.Target)
	}
	if oco.Key != (domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}) || oco.Side != domain.Sell || oco.Quantity != 10 {
		t.Errorf("key, side and quantity must be mapped, got %+v", oco)
	}
	shortStop := got[1]
	if shortStop.Stop != money.MustParse("22500.00") || shortStop.Target != 0 {
		t.Errorf("a buy leg above the market is a short's stop, got %+v", shortStop)
	}
	if shortStop.Key.Exchange != "NFO" || shortStop.Product != domain.NRML {
		t.Errorf("segment 11 MARGIN is an NFO NRML position, got %+v", shortStop)
	}
	target := got[2]
	if target.Target != money.MustParse("700.00") || target.Stop != 0 {
		t.Errorf("a sell leg above the market is a target, not a stop, got %+v", target)
	}
}

func TestASingleLegWithNoLTPReadsAsAStop(t *testing.T) {
	p := fromGTT(gttRow{ID: "x", Symbol: "NSE:SBIN-EQ", TranSide: -1, Qty: 1, PriceTrigger: 700, OCOInd: gttSingle, OrdStatus: 6})
	if p.Stop != money.MustParse("700.00") || p.Target != 0 {
		t.Errorf("outside market hours the book reports no LTP; a lone leg is a stop by default, got %+v", p)
	}
}

func TestAnUnknownGTTStatusReadsAsLive(t *testing.T) {
	if !(gttRow{OrdStatus: 42}).live() {
		t.Error("an unknown status raising a false 'stop is missing' every morning would bury the real alerts")
	}
	if (gttRow{OrdStatus: statusTraded}).live() {
		t.Error("a triggered GTT is no longer protecting anything")
	}
}

func TestModifyStopPreservesTheTargetLeg(t *testing.T) {
	var patched []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/gtt/orders" && req.Method == http.MethodGet:
			_, _ = w.Write([]byte(gttBookBody))
		case req.URL.Path == "/gtt/orders/sync" && req.Method == http.MethodPatch:
			patched, _ = io.ReadAll(req.Body)
			_, _ = w.Write([]byte(`{"code":1102,"message":"Successfully modified order","s":"ok","id":"25012400002074"}`))
		default:
			t.Errorf("unexpected %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	if err := c.ModifyStop(context.Background(), "25012400002074", money.MustParse("600.00")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var body gttModifyRequest
	if err := json.Unmarshal(patched, &body); err != nil {
		t.Fatalf("decoding patch: %v (%s)", err, patched)
	}
	if body.ID != "25012400002074" {
		t.Errorf("the modify must name the trigger, got %q", body.ID)
	}
	if body.OrderInfo.Leg1.TriggerPrice != 650 {
		t.Errorf("FYERS replaces the whole leg set; the target must be carried across or the OCO silently loses it. Got leg1 %v", body.OrderInfo.Leg1.TriggerPrice)
	}
	if body.OrderInfo.Leg2 == nil || body.OrderInfo.Leg2.TriggerPrice != 600 {
		t.Errorf("the stop must move to the new level, got %+v", body.OrderInfo.Leg2)
	}
}

func TestModifyStopReportsATriggerThatIsGone(t *testing.T) {
	c, _ := newRouted(t, routes{"/gtt/orders": `{"s":"ok","code":200,"orderBook":[]}`})
	if err := c.ModifyStop(context.Background(), "missing", money.MustParse("1.00")); err == nil {
		t.Error("modifying a trigger that is not in the book must fail loudly; a silent no-op leaves the position on its old stop")
	}
}

func TestCancelProtectiveSendsTheIDInTheBody(t *testing.T) {
	c, sent := newRouted(t, routes{"/gtt/orders/sync": `{"code":1103,"message":"Successfully cancelled order","s":"ok","id":"25012400002099"}`})
	if err := c.CancelProtective(context.Background(), "25012400002099"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(sent["/gtt/orders/sync"]) != `{"id":"25012400002099"}` {
		t.Errorf("the id goes in a JSON body on DELETE, got %s", sent["/gtt/orders/sync"])
	}
}
