package upstox

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

// routes maps a request path to the body served for it, so a test states the
// recorded responses it needs and nothing else.
type routes map[string]string

// newRouted returns a client backed by a server that answers from routes and
// records the bodies it was sent, keyed by path.
func newRouted(t *testing.T, r routes) (*Client, map[string][]byte) {
	t.Helper()
	sent := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil {
			body, _ := io.ReadAll(req.Body)
			sent[req.URL.Path] = body
		}
		body, ok := r[req.URL.Path]
		if !ok {
			t.Errorf("unexpected request to %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)
	return c, sent
}

func TestAccountReadsTheEquitySegment(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/user/get-funds-and-margin": `{"status":"success","data":{
			"equity":{"available_margin":125000.50,"used_margin":25000.00},
			"commodity":{"available_margin":9999.00,"used_margin":0.0}}}`,
	})

	acct, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if acct.Available != money.MustParse("125000.50") {
		t.Errorf("available margin must come from the equity segment, got %s", acct.Available)
	}
	if acct.Used != money.MustParse("25000.00") {
		t.Errorf("used margin must be carried, not left zero, got %s", acct.Used)
	}
	if acct.Equity != acct.Available+acct.Used {
		t.Errorf("equity is what the segment holds, committed or not; got %s", acct.Equity)
	}
	if acct.Available == money.MustParse("9999.00") {
		t.Error("commodity margin must never be read for an equity account")
	}
}

func TestAccountRefusesAnEmptyFundsResponse(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/user/get-funds-and-margin": `{"status":"success","data":{}}`,
	})
	if _, err := c.Account(context.Background()); err == nil {
		t.Error("reporting zero equity reads as a wiped account and would stop every strategy; an empty response must error")
	}
}

func TestPlaceOrderReturnsPendingWithTheBrokersID(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/order/place": `{"status":"success","data":{"order_ids":["250905000123456"]}}`,
	})

	order, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:        domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:       domain.Buy,
		Quantity:   7,
		Type:       domain.Limit,
		Product:    domain.CNC,
		LimitPrice: money.MustParse("1057.60"),
		Tag:        "breakout",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.ID != "250905000123456" {
		t.Errorf("the v3 endpoint answers with order_ids as a list; the id must be read from it, got %q", order.ID)
	}
	if order.Status != domain.StatusPending {
		t.Errorf("an acknowledgement is not a fill; status must be pending, got %q", order.Status)
	}

	var body placeOrderRequest
	if err := json.Unmarshal(sent["/order/place"], &body); err != nil {
		t.Fatalf("decoding the sent payload: %v", err)
	}
	if body.InstrumentToken != "NSE_EQ|RELIANCE" {
		t.Errorf("the instrument key goes in instrument_token despite the name, got %q", body.InstrumentToken)
	}
	if body.Price != 1057.60 {
		t.Errorf("the limit price must be sent in rupees, got %v", body.Price)
	}
	if body.Product != productDelivery || body.OrderType != orderTypeLimit || body.Validity != validityDay {
		t.Errorf("payload vocabulary is wrong: %+v", body)
	}
	if body.Tag != "breakout" {
		t.Errorf("the strategy tag must reach the broker, got %q", body.Tag)
	}
}

func TestPlaceOrderRejectsAMissingPrice(t *testing.T) {
	c, _ := newRouted(t, routes{})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Buy,
		Quantity: 1,
		Type:     domain.Limit,
		Product:  domain.CNC,
	})
	if err == nil {
		t.Error("a limit order with no limit price must fail locally, not be sent for the exchange to refuse")
	}
}

func TestPlaceOrderRejectsANonPositiveQuantity(t *testing.T) {
	c, _ := newRouted(t, routes{})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Buy,
		Quantity: 0,
		Type:     domain.Market,
		Product:  domain.CNC,
	})
	if err == nil {
		t.Error("a zero-quantity order must be refused before it reaches the wire")
	}
}

func TestPlaceOrderFailsWhenNoIDComesBack(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/order/place": `{"status":"success","data":{}}`,
	})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Buy,
		Quantity: 1,
		Type:     domain.Market,
		Product:  domain.CNC,
	})
	if err == nil {
		t.Error("an order with no id cannot be tracked or cancelled; that must be an error, not a zero-valued Order")
	}
}

func TestOrderStatusMapsEachStatusAndCarriesTheMessage(t *testing.T) {
	cases := map[string]domain.OrderStatus{
		"complete":        domain.StatusComplete,
		"rejected":        domain.StatusRejected,
		"cancelled":       domain.StatusCancelled,
		"open":            domain.StatusOpen,
		"trigger pending": domain.StatusTriggered,
		"something new":   domain.StatusPending,
	}
	for wire, want := range cases {
		c, _ := newRouted(t, routes{
			"/order/details": `{"status":"success","data":{
				"order_id":"1","exchange":"NSE","trading_symbol":"RELIANCE",
				"transaction_type":"BUY","quantity":10,"order_type":"LIMIT",
				"product":"D","validity":"DAY","price":1057.60,
				"status":"` + wire + `","filled_quantity":10,
				"average_price":1057.55,"status_message":"RMS rule check failed"}}`,
		})
		order, err := c.OrderStatus(context.Background(), "1")
		if err != nil {
			t.Fatalf("unexpected error for status %q: %v", wire, err)
		}
		if order.Status != want {
			t.Errorf("Upstox status %q must map to %q, got %q", wire, want, order.Status)
		}
		if order.Message != "RMS rule check failed" {
			t.Errorf("the broker's own reason must reach the caller, got %q", order.Message)
		}
		if order.AveragePrice != money.MustParse("1057.55") {
			t.Errorf("the fill price must convert to paise, got %s", order.AveragePrice)
		}
	}
}

func TestOrderStatusReportsAnUnknownOrder(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/order/details": `{"status":"success","data":{}}`,
	})
	if _, err := c.OrderStatus(context.Background(), "missing"); err == nil {
		t.Error("an empty payload means the order is unknown; returning a zero Order would read as a pending order that never settles")
	}
}

func TestOpenOrdersDropsTerminalOnes(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/order/retrieve-all": `{"status":"success","data":[
			{"order_id":"1","status":"open","exchange":"NSE","trading_symbol":"RELIANCE","quantity":1,"transaction_type":"BUY","product":"D"},
			{"order_id":"2","status":"complete","exchange":"NSE","trading_symbol":"TCS","quantity":1,"transaction_type":"BUY","product":"D"},
			{"order_id":"3","status":"cancelled","exchange":"NSE","trading_symbol":"INFY","quantity":1,"transaction_type":"BUY","product":"D"},
			{"order_id":"4","status":"trigger pending","exchange":"NSE","trading_symbol":"WIPRO","quantity":1,"transaction_type":"SELL","product":"D"}]}`,
	})

	orders, err := c.OpenOrders(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("only the open and trigger-pending orders are still live; got %d", len(orders))
	}
	for _, o := range orders {
		if o.Status.Terminal() {
			t.Errorf("order %s is terminal and must not appear in OpenOrders", o.ID)
		}
	}
}

func TestPositionsFiltersZeroQuantityRows(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/portfolio/short-term-positions": `{"status":"success","data":[
			{"exchange":"NSE","trading_symbol":"RELIANCE","product":"I","quantity":10,
			 "average_price":1057.60,"realised":0.0,"unrealised":125.50,
			 "instrument_token":"NSE_EQ|INE002A01018"},
			{"exchange":"NSE","tradingsymbol":"TCS","product":"D","quantity":0,
			 "average_price":3500.00,"realised":250.00,"unrealised":0.0,
			 "instrument_token":"NSE_EQ|INE467B01029"},
			{"exchange":"NSE","trading_symbol":"INFY","product":"D","quantity":-5,
			 "average_price":1400.00,"realised":0.0,"unrealised":-40.00,
			 "instrument_token":"NSE_EQ|INE009A01021"}]}`,
	})

	positions, err := c.Positions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("a closed position's row is history, not a holding; got %d positions", len(positions))
	}
	for _, p := range positions {
		if p.Key.Symbol == "TCS" {
			t.Error("the zero-quantity row must be dropped")
		}
	}
	if positions[0].Product != domain.MIS {
		t.Errorf("product I must read back as MIS, got %q", positions[0].Product)
	}
	if positions[0].UnrealizedPnL != money.MustParse("125.50") {
		t.Errorf("unrealised P&L must convert to paise, got %s", positions[0].UnrealizedPnL)
	}
	if positions[1].Quantity != -5 {
		t.Errorf("a short position is a negative quantity, got %d", positions[1].Quantity)
	}
}

func TestPositionsReadsEitherSpellingOfTradingsymbol(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/portfolio/short-term-positions": `{"status":"success","data":[
			{"exchange":"NSE","tradingsymbol":"RELIANCE","product":"D","quantity":1,"average_price":1.0}]}`,
	})
	positions, err := c.Positions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(positions) != 1 || positions[0].Key.Symbol != "RELIANCE" {
		t.Errorf("Upstox spells the field both ways across endpoints; both must be read, got %+v", positions)
	}
}

func TestBasketMarginUsesTheMarginAfterBenefit(t *testing.T) {
	c, sent := newRouted(t, routes{
		"/charges/margin": `{"status":"success","data":{"required_margin":250000.00,"final_margin":85000.00}}`,
	})

	got, err := c.BasketMargin(context.Background(), []domain.MarginLeg{
		{Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "NIFTY25000CE"}, Side: domain.Buy, Quantity: 75, Product: domain.NRML},
		{Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "NIFTY25500CE"}, Side: domain.Sell, Quantity: 75, Product: domain.NRML},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != money.MustParse("85000.00") {
		t.Errorf("final_margin is what is actually blocked; required_margin would refuse spreads the account can afford. got %s", got)
	}

	var body struct {
		Instruments []marginLegRequest `json:"instruments"`
	}
	if err := json.Unmarshal(sent["/charges/margin"], &body); err != nil {
		t.Fatalf("decoding the sent payload: %v", err)
	}
	if len(body.Instruments) != 2 || body.Instruments[1].TransactionType != transactionTypeSell {
		t.Errorf("both legs must be sent with their own sides, got %+v", body.Instruments)
	}
}

func TestBasketMarginOfNoLegsCostsNothingAndMakesNoCall(t *testing.T) {
	c, _ := newRouted(t, routes{})
	got, err := c.BasketMargin(context.Background(), nil)
	if err != nil || got != 0 {
		t.Errorf("an empty basket blocks nothing and needs no request; got %s, %v", got, err)
	}
}

func TestParseUpstoxTimeReturnsZeroForAnUnreadableStamp(t *testing.T) {
	if got := parseUpstoxTime("not a time"); !got.IsZero() {
		t.Errorf("a fabricated timestamp is indistinguishable from a real one and would reorder a time-sorted book; got %v", got)
	}
	if got := parseUpstoxTime("1757000000000"); got.IsZero() {
		t.Error("epoch milliseconds are the quote endpoints' form and must parse")
	}
}
