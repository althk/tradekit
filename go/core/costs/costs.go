// Package costs computes the transaction charges on a trade.
//
// Four implementations of this existed across the projects tradekit replaces,
// and two of them disagreed on the DP charge — correctly, because one priced
// Zerodha and the other Upstox, and neither could express which. That is the
// design constraint here: a rate is never a constant. It is looked up by
// broker, segment and trade date, so a backtest over 2024 prices its trades
// with the schedule that was in force in 2024 rather than with today's.
//
// # What this package does not do
//
// It ships no real rate card. Statutory rates change with each budget and
// broker plans change more often; a hardcoded table would be wrong silently.
// Rates come from a Table the caller populates — from the charge_rates SQLite
// table, from config, or from a literal in a test. DefaultIndiaEquityDelivery
// exists only as a documented shape to copy, with every value zero.
package costs

import (
	"fmt"
	"sort"
	"time"

	"github.com/althk/tradekit/go/core/money"
)

// Segment is the charge schedule a trade falls under. The schedules differ in
// which components apply at all, not merely in their rates: delivery pays a DP
// charge and stamp duty on the buy leg, intraday pays neither.
type Segment string

// Supported charge segments.
const (
	EquityDelivery Segment = "equity_delivery"
	EquityIntraday Segment = "equity_intraday"
	Futures        Segment = "futures"
	Options        Segment = "options"
)

// Kind identifies one component of a charge schedule.
type Kind string

// The components of a charge schedule. Each is either a fraction of turnover
// or, when Rate.Flat is set, an absolute amount in minor units.
const (
	Brokerage Kind = "brokerage"
	STTBuy    Kind = "stt_buy"
	STTSell   Kind = "stt_sell"
	Exchange  Kind = "exchange"
	SEBI      Kind = "sebi"
	Stamp     Kind = "stamp"
	DP        Kind = "dp"
	GST       Kind = "gst"
)

// Rate is one component's value, effective from a date.
type Rate struct {
	// Value is a fraction of turnover (0.001 means 0.1%) unless Flat is
	// set, in which case it is an absolute amount in minor units.
	Value float64
	// Flat marks Value as an absolute amount rather than a fraction.
	Flat bool
	// Cap bounds the computed charge per order, in minor units. Zero means
	// uncapped. It models "0.03% or Rs 20, whichever is lower".
	Cap money.Money
	// EffectiveFrom is the first date this rate applies to.
	EffectiveFrom time.Time
}

// Table holds effective-dated rates, keyed by broker, segment and kind.
type Table struct {
	rates map[tableKey][]Rate
}

type tableKey struct {
	broker  string
	segment Segment
	kind    Kind
}

// NewTable returns an empty rate table.
func NewTable() *Table { return &Table{rates: map[tableKey][]Rate{}} }

// Set records a rate. Repeated calls for the same broker, segment and kind
// build the effective-dated history; order does not matter.
func (t *Table) Set(broker string, seg Segment, kind Kind, r Rate) {
	k := tableKey{broker, seg, kind}
	rs := append(t.rates[k], r)
	sort.Slice(rs, func(i, j int) bool { return rs[i].EffectiveFrom.Before(rs[j].EffectiveFrom) })
	t.rates[k] = rs
}

// Lookup returns the rate in force on the given date.
//
// A missing component is not an error: it means the component does not apply
// to that broker and segment, which is exactly how intraday differs from
// delivery. The boolean distinguishes "does not apply" from "zero rate".
func (t *Table) Lookup(broker string, seg Segment, kind Kind, on time.Time) (Rate, bool) {
	rs := t.rates[tableKey{broker, seg, kind}]
	var found Rate
	var ok bool
	for _, r := range rs {
		if r.EffectiveFrom.After(on) {
			break
		}
		found, ok = r, true
	}
	return found, ok
}

// Trade is one round trip to be priced.
type Trade struct {
	Broker     string
	Segment    Segment
	Quantity   int
	EntryPrice money.Money
	ExitPrice  money.Money
	// EntryAt and ExitAt select the effective rate for each leg, so a
	// position held across a rate change is priced correctly on both sides.
	EntryAt time.Time
	ExitAt  time.Time
	// Buying reports whether the entry leg was a buy. A short sale pays
	// stamp duty on its buy leg too, but that leg is the exit.
	Buying bool
}

// Charges is the itemised cost of a round trip, every field in minor units.
type Charges struct {
	Brokerage money.Money
	STT       money.Money
	Exchange  money.Money
	SEBI      money.Money
	Stamp     money.Money
	DP        money.Money
	GST       money.Money
	Total     money.Money
}

// Compute prices a round trip against the table.
//
// The arithmetic is deliberately explicit about which leg each component
// applies to: STT and stamp duty are leg-specific, exchange and SEBI fees
// apply to both legs' turnover, DP is charged once per sell of a delivery
// holding, and GST applies only to the service charges — brokerage, exchange,
// SEBI and DP — never to STT or stamp duty, which are taxes in their own right.
//
// Every component is rounded to the minor unit before being summed, so the
// total matches what a contract note shows rather than differing by a paisa.
func Compute(t *Table, tr Trade) (Charges, error) {
	if tr.Quantity <= 0 {
		return Charges{}, fmt.Errorf("costs: quantity must be positive, got %d", tr.Quantity)
	}

	entryTurnover := tr.EntryPrice.Mul(int64(tr.Quantity))
	exitTurnover := tr.ExitPrice.Mul(int64(tr.Quantity))

	// Identify which leg is the buy and which the sell.
	buyTurnover, buyAt := entryTurnover, tr.EntryAt
	sellTurnover, sellAt := exitTurnover, tr.ExitAt
	if !tr.Buying {
		buyTurnover, buyAt = exitTurnover, tr.ExitAt
		sellTurnover, sellAt = entryTurnover, tr.EntryAt
	}

	var c Charges

	c.Brokerage = t.apply(tr, Brokerage, entryTurnover, tr.EntryAt) +
		t.apply(tr, Brokerage, exitTurnover, tr.ExitAt)

	c.STT = t.apply(tr, STTBuy, buyTurnover, buyAt) +
		t.apply(tr, STTSell, sellTurnover, sellAt)

	c.Exchange = t.apply(tr, Exchange, entryTurnover, tr.EntryAt) +
		t.apply(tr, Exchange, exitTurnover, tr.ExitAt)

	c.SEBI = t.apply(tr, SEBI, entryTurnover, tr.EntryAt) +
		t.apply(tr, SEBI, exitTurnover, tr.ExitAt)

	// Stamp duty is charged on the buy leg only.
	c.Stamp = t.apply(tr, Stamp, buyTurnover, buyAt)

	// DP is a flat charge on the sell of a delivery holding. Turnover is
	// irrelevant to its amount, so it is passed zero -- but a sell leg with
	// no turnover is a position still open, and an open position has not
	// paid it yet.
	if sellTurnover > 0 {
		c.DP = t.apply(tr, DP, 0, sellAt)
	}

	if gst, ok := t.Lookup(tr.Broker, tr.Segment, GST, tr.ExitAt); ok {
		taxable := c.Brokerage + c.Exchange + c.SEBI + c.DP
		c.GST = taxable.MulFraction(gst.Value)
	}

	c.Total = c.Brokerage + c.STT + c.Exchange + c.SEBI + c.Stamp + c.DP + c.GST
	return c, nil
}

// apply computes one component for one leg, returning zero when the component
// does not apply to this broker and segment.
func (t *Table) apply(tr Trade, kind Kind, turnover money.Money, on time.Time) money.Money {
	r, ok := t.Lookup(tr.Broker, tr.Segment, kind, on)
	if !ok {
		return 0
	}
	var v money.Money
	if r.Flat {
		v = money.Money(r.Value)
	} else {
		v = turnover.MulFraction(r.Value)
	}
	if r.Cap > 0 && v > r.Cap {
		v = r.Cap
	}
	return v
}

// DefaultIndiaEquityDelivery returns a table with the shape of an Indian
// equity delivery schedule and every rate set to zero.
//
// It is a template, not a rate card. Fill it from the charge_rates table or
// from config before using it to price anything: shipping real rates here
// would mean shipping rates that go stale without anyone noticing, which is
// the failure this package exists to prevent.
func DefaultIndiaEquityDelivery(broker string, from time.Time) *Table {
	t := NewTable()
	for _, k := range []Kind{Brokerage, STTBuy, STTSell, Exchange, SEBI, Stamp, GST} {
		t.Set(broker, EquityDelivery, k, Rate{Value: 0, EffectiveFrom: from})
	}
	t.Set(broker, EquityDelivery, DP, Rate{Value: 0, Flat: true, EffectiveFrom: from})
	return t
}
