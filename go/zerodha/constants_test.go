package zerodha

import (
	"testing"

	"github.com/althk/tradekit/go/core/ports"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// TestConstantsMatchTheSDK is the reason mapping.go may restate Kite's strings.
//
// mapping.go deliberately imports nothing from the SDK so that all the
// translation logic is testable without a broker account or a network. That
// only stays safe if the literals it restates are checked against the real
// ones, which is what this does: if Zerodha ever changes a value, the build
// fails here instead of the system silently sending an unrecognised product or
// validity to the exchange.
func TestConstantsMatchTheSDK(t *testing.T) {
	cases := []struct {
		name  string
		local string
		sdk   string
	}{
		{"VarietyRegular", varietyRegular, kiteconnect.VarietyRegular},
		{"ProductCNC", productCNC, kiteconnect.ProductCNC},
		{"ProductMIS", productMIS, kiteconnect.ProductMIS},
		{"ProductNRML", productNRML, kiteconnect.ProductNRML},
		{"OrderTypeMarket", orderTypeMarket, kiteconnect.OrderTypeMarket},
		{"OrderTypeLimit", orderTypeLimit, kiteconnect.OrderTypeLimit},
		{"OrderTypeSL", orderTypeSL, kiteconnect.OrderTypeSL},
		{"OrderTypeSLM", orderTypeSLM, kiteconnect.OrderTypeSLM},
		{"ValidityDay", validityDay, kiteconnect.ValidityDay},
		{"ValidityIOC", validityIOC, kiteconnect.ValidityIOC},
		{"TransactionTypeBuy", transactionTypeBuy, kiteconnect.TransactionTypeBuy},
		{"TransactionTypeSell", transactionTypeSell, kiteconnect.TransactionTypeSell},
		{"OrderStatusComplete", statusComplete, kiteconnect.OrderStatusComplete},
		{"OrderStatusRejected", statusRejected, kiteconnect.OrderStatusRejected},
		{"OrderStatusCancelled", statusCancelled, kiteconnect.OrderStatusCancelled},
	}
	for _, c := range cases {
		if c.local != c.sdk {
			t.Errorf("%s: mapping.go has %q, the SDK has %q", c.name, c.local, c.sdk)
		}
	}
}

// TestSatisfiesThePorts pins which capabilities this adapter provides.
//
// The interfaces are asserted at compile time so that a change to a port, or a
// signature drifting here, is a build failure rather than a type assertion that
// quietly returns false at wiring time.
func TestSatisfiesThePorts(t *testing.T) {
	var c any = &Client{}

	required := map[string]bool{
		"Broker":           implements[ports.Broker](c),
		"Quoter":           implements[ports.Quoter](c),
		"HistoryProvider":  implements[ports.HistoryProvider](c),
		"ProtectiveOrders": implements[ports.ProtectiveOrders](c),
		"MarginEstimator":  implements[ports.MarginEstimator](c),
		"Streamer":         implements[ports.Streamer](c),
		"InstrumentSource": implements[ports.InstrumentSource](c),
		"TokenState":       implements[ports.TokenState](c),
	}
	for name, ok := range required {
		if !ok {
			t.Errorf("must implement ports.%s", name)
		}
	}

	// Kite's stops rest at the exchange as GTTs and trigger without the
	// engine's help, so this adapter must not implement TickObserver: doing
	// so would tell the engine to feed it prices it has no use for, and
	// suggest a fill responsibility it does not have.
	if implements[ports.TickObserver](c) {
		t.Error("a live adapter must not implement TickObserver; its stops rest at the exchange")
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}
