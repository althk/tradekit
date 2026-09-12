package fyers

import (
	"encoding/json"
	"strings"
	"testing"

	fyerssdk "github.com/FyersDev/fyers-go-sdk"
)

// TestConstantsMatchTheSDK is the reason the adapter may restate FYERS's
// endpoints and product codes without importing the SDK anywhere else.
//
// The SDK is not used at runtime — it returns undecoded strings and swallows
// transport errors — but it is the vendor's own statement of the wire
// vocabulary, and pinning the literals here means a renamed endpoint or a
// renumbered product upstream fails the build instead of reaching the
// exchange as an unrecognised request.
func TestConstantsMatchTheSDK(t *testing.T) {
	cases := []struct {
		name  string
		local string
		sdk   string
	}{
		{"BaseURL", BaseURL, fyerssdk.BaseURL},
		{"DataURL", DataURL, fyerssdk.BaseDataURL},
		{"ProductCNC", productCNC, fyerssdk.ProductCNC},
		{"ProductIntraday", productIntraday, fyerssdk.ProductIntraday},
		{"ProductMargin", productMargin, fyerssdk.ProductMargin},
		{"ProductMTF", productMTF, fyerssdk.ProductMTF},
		{"ValidateAuthCode", BaseURL + "/validate-authcode", fyerssdk.ValidateAuthCodeURL},
		{"Funds", BaseURL + "/funds", fyerssdk.FundURL},
		{"Positions", BaseURL + "/positions", fyerssdk.PositionURL},
		{"Orders", BaseURL + "/orders/sync", fyerssdk.SingleOrderActionURL},
		{"OrderBook", BaseURL + "/orders", fyerssdk.OrderBookURL},
		{"GTT", BaseURL + "/gtt/orders/sync", fyerssdk.GTTOrderURL},
		{"GTTBook", BaseURL + "/gtt/orders", fyerssdk.GTTOrderBookURL},
		{"Margin", BaseURL + "/multiorder/margin", fyerssdk.OrderCheckMarginURL},
		{"Quotes", DataURL + "/quotes", strings.TrimSuffix(fyerssdk.StockQuotesURL, "?symbols=")},
		{"History", DataURL + "/history", strings.TrimSuffix(fyerssdk.StockHistoryURL, "?")},
	}
	for _, c := range cases {
		if c.local != c.sdk {
			t.Errorf("%s: the adapter restates %q but the SDK says %q; the wire vocabulary has moved", c.name, c.local, c.sdk)
		}
	}
}

// TestPayloadFieldsMatchTheSDK pins the JSON field names the adapter sends
// against the SDK's request models, for the same reason.
func TestPayloadFieldsMatchTheSDK(t *testing.T) {
	fields := func(v any) map[string]bool {
		raw, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}

	sdkOrder := fields(fyerssdk.OrderRequest{OrderTag: "x", StopLoss: 1, TakeProfit: 1})
	for k := range fields(placeOrderRequest{OrderTag: "x"}) {
		if !sdkOrder[k] {
			t.Errorf("order field %q is not in the SDK's OrderRequest", k)
		}
	}

	sdkGTT := fields(fyerssdk.GTTOrderRequest{})
	for k := range fields(gttRequest{}) {
		if k != "orderTag" && !sdkGTT[k] {
			t.Errorf("GTT field %q is not in the SDK's GTTOrderRequest", k)
		}
	}
	sdkLeg := fields(fyerssdk.Leg1{})
	for k := range fields(gttLeg{}) {
		if !sdkLeg[k] {
			t.Errorf("GTT leg field %q is not in the SDK's Leg1", k)
		}
	}

	sdkRow := fields(fyerssdk.OrderBookItem{})
	for k := range fields(orderRow{}) {
		if !sdkRow[k] {
			t.Errorf("order book field %q is not in the SDK's OrderBookItem", k)
		}
	}
	sdkPos := fields(fyerssdk.NetPosition{})
	for k := range fields(positionRow{}) {
		if !sdkPos[k] {
			t.Errorf("position field %q is not in the SDK's NetPosition", k)
		}
	}
	sdkGTTRow := fields(fyerssdk.GTTOrderBookItem{})
	for k := range fields(gttRow{}) {
		if !sdkGTTRow[k] {
			t.Errorf("GTT book field %q is not in the SDK's GTTOrderBookItem", k)
		}
	}
}
