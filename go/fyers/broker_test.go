package fyers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

const fundsBody = `{"code":200,"message":"","s":"ok","fund_limit":[
	{"id":1,"title":"Total Balance","equityAmount":150000.25,"commodityAmount":9999},
	{"id":2,"title":"Utilized Amount","equityAmount":25000.00,"commodityAmount":0},
	{"id":3,"title":"Clear Balance","equityAmount":149000.00,"commodityAmount":0},
	{"id":10,"title":"Available Balance","equityAmount":125000.25,"commodityAmount":0}]}`

func TestAccountReadsTheAvailableBalanceRow(t *testing.T) {
	c, _ := newRouted(t, routes{"/funds": fundsBody})

	acct, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Available != money.MustParse("125000.25") {
		t.Errorf("available must be the Available Balance row (id 10), not Clear Balance; got %s", acct.Available)
	}
	if acct.Used != money.MustParse("25000.00") {
		t.Errorf("used must be the Utilized Amount row, got %s", acct.Used)
	}
	if acct.Equity != money.MustParse("150000.25") {
		t.Errorf("equity must be the Total Balance row, got %s", acct.Equity)
	}
	if acct.Equity == money.MustParse("9999.00") {
		t.Error("commodity ledger must never be read for an equity account")
	}
}

func TestAccountFallsBackToTitlesWhenIDsAreRenumbered(t *testing.T) {
	c, _ := newRouted(t, routes{"/funds": `{"s":"ok","code":200,"fund_limit":[
		{"id":99,"title":"Available Balance","equityAmount":10.5},
		{"id":98,"title":"Utilized Amount","equityAmount":1}]}`})
	acct, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Available != money.MustParse("10.50") {
		t.Errorf("a renumbered ledger must still be read by title, got %s", acct.Available)
	}
	if acct.Equity != acct.Available+acct.Used {
		t.Errorf("without a total row, equity is what the ledger holds committed or not; got %s", acct.Equity)
	}
}

func TestAccountRefusesAnEmptyLedger(t *testing.T) {
	c, _ := newRouted(t, routes{"/funds": `{"s":"ok","code":200,"fund_limit":[]}`})
	if _, err := c.Account(context.Background()); err == nil {
		t.Error("reporting zero equity reads as a wiped account and would stop every strategy; an empty ledger must error")
	}
}

func TestPlaceOrderReturnsPendingWithTheBrokersID(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/orders/sync": `{"s":"ok","code":1101,"message":"Order submitted successfully. Your Order Ref. No.808058117761","id":"808058117761"}`,
	})
	order, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:         domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"},
		Side:        domain.Buy,
		Quantity:    10,
		Type:        domain.Limit,
		Product:     domain.CNC,
		LimitPrice:  money.MustParse("612.35"),
		TimeInForce: domain.Day,
		Tag:         "breakout",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.ID != "808058117761" {
		t.Errorf("the broker's id must be carried, got %q", order.ID)
	}
	if order.Status != domain.StatusPending {
		t.Errorf("an acknowledgement is not a fill; status must be pending, got %s", order.Status)
	}

	var body map[string]any
	if err := json.Unmarshal(sent["/orders/sync"], &body); err != nil {
		t.Fatalf("decoding sent body: %v", err)
	}
	checks := map[string]any{
		"symbol": "NSE:SBIN-EQ", "qty": 10.0, "type": 1.0, "side": 1.0, "productType": "CNC",
		"limitPrice": 612.35, "stopPrice": 0.0, "validity": "DAY", "offlineOrder": false,
		"disclosedQty": 0.0, "orderTag": "breakout",
	}
	for k, want := range checks {
		if body[k] != want {
			t.Errorf("payload %s: want %v got %v", k, want, body[k])
		}
	}
}

func TestPlaceOrderRefusesAnAcknowledgementWithNoID(t *testing.T) {
	c, _ := newRouted(t, routes{"/orders/sync": `{"s":"ok","code":201,"message":"Order in transit"}`})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Buy, Quantity: 1,
		Type: domain.Market, Product: domain.MIS,
	})
	if err == nil {
		t.Error("code 201 carries no id to poll; an empty order would be polled forever")
	}
}

func TestPlaceOrderRefusesAStopWithNoTrigger(t *testing.T) {
	c, _ := newRouted(t, routes{})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Sell, Quantity: 1,
		Type: domain.Stop, Product: domain.MIS,
	})
	if err == nil {
		t.Error("a stop with no trigger price would be sent with stopPrice 0 and rejected or, worse, filled at market")
	}
}

func TestCancelOrderSendsTheIDInTheBody(t *testing.T) {
	c, sent := newRouted(t, routes{"/orders/sync": `{"s":"ok","code":1103,"message":"Successfully canceled order","id":"808058117761"}`})
	if err := c.CancelOrder(context.Background(), "808058117761"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(sent["/orders/sync"]) != `{"id":"808058117761"}` {
		t.Errorf("the id goes in a JSON body on DELETE, got %s", sent["/orders/sync"])
	}
}

const orderBookBody = `{"s":"ok","code":200,"message":"","orderBook":[
	{"id":"23030900015105","exchOrdId":"1100000001089341","qty":10,"remainingQuantity":4,"filledQty":6,
	 "limitPrice":6.95,"stopPrice":0,"tradedPrice":6.9,"type":1,"exchange":10,"segment":10,"symbol":"NSE:IDEA-EQ",
	 "message":"","offlineOrder":false,"orderDateTime":"09-Mar-2023 09:34:38","orderValidity":"DAY",
	 "productType":"CNC","side":-1,"status":6,"source":"ITS","orderTag":"1:breakout"},
	{"id":"23030900015106","qty":1,"filledQty":1,"limitPrice":0,"stopPrice":0,"tradedPrice":100.5,"type":2,
	 "segment":11,"symbol":"NSE:NIFTY24JANFUT","orderDateTime":"09-Mar-2023 09:35:00","orderValidity":"DAY",
	 "productType":"MARGIN","side":1,"status":2,"orderTag":"2:Untagged"},
	{"id":"23030900015107","qty":1,"filledQty":0,"type":1,"segment":10,"symbol":"NSE:SBIN-EQ",
	 "productType":"INTRADAY","side":1,"status":5,"message":"Insufficient funds"}]}`

func TestOrderStatusMapsTheRowAndCarriesTheMessage(t *testing.T) {
	c, _ := newRouted(t, routes{"/orders": orderBookBody})
	order, err := c.OrderStatus(context.Background(), "23030900015105")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.Status != domain.StatusOpen {
		t.Errorf("status 6 is resting at the exchange, which is open; got %s", order.Status)
	}
	if order.FilledQuantity != 6 || order.AveragePrice != money.MustParse("6.90") {
		t.Errorf("a partial fill must carry its filled quantity and average, got %d @ %s", order.FilledQuantity, order.AveragePrice)
	}
	if order.Request.Key != (domain.InstrumentKey{Exchange: "NSE", Symbol: "IDEA"}) {
		t.Errorf("the EQ series must be stripped so the key matches the store's, got %v", order.Request.Key)
	}
	if order.Request.Side != domain.Sell || order.Request.Product != domain.CNC || order.Request.Type != domain.Limit {
		t.Errorf("side, product and type must be mapped, got %s %s %s", order.Request.Side, order.Request.Product, order.Request.Type)
	}
	if order.Request.Tag != "breakout" {
		t.Errorf("the 1: prefix FYERS adds must be stripped, got %q", order.Request.Tag)
	}
	if order.PlacedAt.IsZero() {
		t.Error("the order time must be parsed")
	}

	rejected, _ := c.OrderStatus(context.Background(), "23030900015107")
	if rejected.Status != domain.StatusRejected || rejected.Message != "Insufficient funds" {
		t.Errorf("a rejection must carry FYERS's reason, got %s %q", rejected.Status, rejected.Message)
	}
}

func TestOrderStatusReportsAnUnknownOrder(t *testing.T) {
	c, _ := newRouted(t, routes{"/orders": `{"s":"ok","code":200,"orderBook":[]}`})
	if _, err := c.OrderStatus(context.Background(), "nope"); err == nil {
		t.Error("an unknown id answers with an empty book; an empty order would read as pending forever")
	}
}

func TestOpenOrdersDropsTerminalOnes(t *testing.T) {
	c, _ := newRouted(t, routes{"/orders": orderBookBody})
	orders, err := c.OpenOrders(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orders) != 1 || orders[0].ID != "23030900015105" {
		t.Errorf("only the resting order is open; traded and rejected are terminal. Got %v", orders)
	}
}

func TestPositionsFilterZeroQuantityRowsAndMapDerivatives(t *testing.T) {
	c, _ := newRouted(t, routes{"/positions": `{"s":"ok","code":200,"message":"","netPositions":[
		{"netQty":25,"qty":25,"netAvg":22000.5,"buyAvg":22000.5,"sellAvg":0,"side":1,"productType":"MARGIN",
		 "realized_profit":0,"unrealized_profit":1250.25,"segment":11,"symbol":"NSE:NIFTY24JANFUT","id":"NSE:NIFTY24JANFUT-MARGIN"},
		{"netQty":0,"qty":0,"netAvg":0,"buyAvg":610,"sellAvg":615,"side":0,"productType":"INTRADAY",
		 "realized_profit":50,"unrealized_profit":0,"segment":10,"symbol":"NSE:SBIN-EQ"},
		{"netQty":-10,"qty":10,"netAvg":0,"buyAvg":0,"sellAvg":99.5,"side":-1,"productType":"INTRADAY",
		 "realized_profit":0,"unrealized_profit":-5,"segment":10,"symbol":"NSE:IDEA-EQ"}],
		"overall":{"count_total":3,"count_open":2}}`})
	positions, err := c.Positions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("a closed row is history, not a holding; got %d positions", len(positions))
	}
	fut := positions[0]
	if fut.Key != (domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JANFUT"}) {
		t.Errorf("segment 11 under the NSE prefix is NFO, got %v", fut.Key)
	}
	if fut.Product != domain.NRML || fut.AveragePrice != money.MustParse("22000.50") || fut.UnrealizedPnL != money.MustParse("1250.25") {
		t.Errorf("product, average and P&L must be mapped, got %s %s %s", fut.Product, fut.AveragePrice, fut.UnrealizedPnL)
	}
	short := positions[1]
	if short.Quantity != -10 {
		t.Errorf("a short is a negative quantity, got %d", short.Quantity)
	}
	if short.AveragePrice != money.MustParse("99.50") {
		t.Errorf("with no netAvg a short's average is its sell average, got %s", short.AveragePrice)
	}
}

func TestBasketMarginUsesTheBasketTotal(t *testing.T) {
	c, sent := newRouted(t, routes{"/multiorder/margin": `{"code":200,"message":"","s":"ok",
		"data":{"margin_avail":1999.9,"margin_total":147738.05,"margin_new_order":247738.05}}`})
	got, err := c.BasketMargin(context.Background(), []domain.MarginLeg{
		{Key: domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JANFUT"}, Side: domain.Buy, Quantity: 25, Product: domain.NRML},
		{Key: domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JAN22000CE"}, Side: domain.Sell, Quantity: 25, Product: domain.NRML, Price: money.MustParse("150.00")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != money.MustParse("147738.05") {
		t.Errorf("margin_total is the basket after hedge benefit; margin_new_order includes existing positions. Got %s", got)
	}
	var body struct {
		Data []marginLegRequest `json:"data"`
	}
	if err := json.Unmarshal(sent["/multiorder/margin"], &body); err != nil || len(body.Data) != 2 {
		t.Fatalf("expected two legs under data, got %s", sent["/multiorder/margin"])
	}
	if body.Data[0].Symbol != "NSE:NIFTY24JANFUT" || body.Data[0].Type != orderTypeMarket || body.Data[0].Side != 1 {
		t.Errorf("a leg with no price is a market leg, got %+v", body.Data[0])
	}
	if body.Data[1].Type != orderTypeLimit || body.Data[1].LimitPrice != 150 || body.Data[1].Side != -1 {
		t.Errorf("a leg with a price is a limit leg, got %+v", body.Data[1])
	}
}

func TestBasketMarginOfNothingIsZero(t *testing.T) {
	c, _ := newRouted(t, routes{})
	if got, err := c.BasketMargin(context.Background(), nil); err != nil || got != 0 {
		t.Errorf("an empty basket blocks nothing and needs no request, got %s %v", got, err)
	}
}
