package zerodha

import (
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

func gtt(side string, last float64, triggers []float64, prices ...float64) kiteconnect.GTT {
	g := kiteconnect.GTT{
		ID: 7,
		Condition: kiteconnect.GTTCondition{
			Exchange: "NSE", Tradingsymbol: "INFY", LastPrice: last, TriggerValues: triggers,
		},
	}
	for _, p := range prices {
		g.Orders = append(g.Orders, kiteconnect.Order{
			TransactionType: side, Quantity: 10, Price: p, Product: kiteconnect.ProductCNC,
		})
	}
	return g
}

func TestFromGTTKeepsTheStopLimitBuffer(t *testing.T) {
	c := &Client{}
	p, err := c.fromGTT(gtt(kiteconnect.TransactionTypeSell, 1500, []float64{1400, 1600}, 1395, 1600))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stop != money.MustParse("1400.00") || p.StopLimit != money.MustParse("1395.00") {
		t.Errorf("stop %s limit %s, want 1400.00 / 1395.00: a trail that drops the buffer leaves a stop that does not stop",
			p.Stop, p.StopLimit)
	}

	short, _ := c.fromGTT(gtt(kiteconnect.TransactionTypeBuy, 1500, []float64{1400, 1600}, 1400, 1605))
	if short.Stop != money.MustParse("1600.00") || short.StopLimit != money.MustParse("1605.00") {
		t.Errorf("short stop %s limit %s, want 1600.00 / 1605.00", short.Stop, short.StopLimit)
	}

	flat, _ := c.fromGTT(gtt(kiteconnect.TransactionTypeSell, 1500, []float64{1400}, 1400))
	if flat.StopLimit != 0 {
		t.Errorf("limit at the trigger should read as StopLimit 0, got %s", flat.StopLimit)
	}
}

func TestFromGTTReadsATargetOnlyLegAsATarget(t *testing.T) {
	c := &Client{}
	p, err := c.fromGTT(gtt(kiteconnect.TransactionTypeSell, 1500, []float64{1650}, 1650))
	if err != nil {
		t.Fatal(err)
	}
	if p.Stop != 0 || p.Target != money.MustParse("1650.00") {
		t.Errorf("got stop %s target %s; a sell above the market is a target, and reporting it as a stop hides an unprotected long",
			p.Stop, p.Target)
	}
	short, _ := c.fromGTT(gtt(kiteconnect.TransactionTypeBuy, 1500, []float64{1350}, 1350))
	if short.Side != domain.Buy || short.Target != money.MustParse("1350.00") {
		t.Errorf("a protective buy below the market is a short's target, got %+v", short)
	}
}
