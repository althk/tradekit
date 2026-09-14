package domain

import "testing"

func TestHeldQuantityReturnsTheSignedQuantityForOneKeyOnly(t *testing.T) {
	rel := InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	tcs := InstrumentKey{Exchange: "NSE", Symbol: "TCS"}
	positions := []Position{{Key: rel, Quantity: 10}, {Key: tcs, Quantity: -5}}

	if got := HeldQuantity(positions, rel); got != 10 {
		t.Errorf("a long position must report its quantity, got %d", got)
	}
	if got := HeldQuantity(positions, tcs); got != -5 {
		t.Errorf("a short position must keep its sign, got %d", got)
	}
	if got := HeldQuantity(positions, InstrumentKey{Exchange: "NSE", Symbol: "INFY"}); got != 0 {
		t.Errorf("an instrument with no position must read as zero, got %d", got)
	}
}
