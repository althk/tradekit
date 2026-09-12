package zerodha

import (
	"context"
	"fmt"
	"strconv"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// PlaceProtective rests a stop, or a stop and target as a one-cancels-other
// pair, as a Kite GTT.
//
// A GTT rests at Zerodha rather than at the exchange, and survives the process
// exiting — which is the whole reason a swing strategy uses one instead of
// watching prices itself. The returned id is Kite's trigger id rendered as a
// string, so the ports interface stays free of Kite's numeric ids.
//
// LastPrice is required by Kite and must be the instrument's current price; it
// is fetched here rather than asked of the caller, because a stale value is
// rejected and a caller passing one is the likeliest source of that.
func (c *Client) PlaceProtective(ctx context.Context, p domain.Protective) (string, error) {
	params, err := c.gttParams(ctx, p)
	if err != nil {
		return "", err
	}

	var resp kiteconnect.GTTResponse
	err = c.call(ctx, func() error {
		var err error
		resp, err = c.kite.PlaceGTT(params)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("zerodha: placing GTT for %s: %w", p.Key, err)
	}
	return strconv.Itoa(resp.TriggerID), nil
}

// gttParams builds Kite's GTT parameters from a Protective, fetching the last
// price Kite requires alongside them.
func (c *Client) gttParams(ctx context.Context, p domain.Protective) (kiteconnect.GTTParams, error) {
	if p.Quantity <= 0 {
		return kiteconnect.GTTParams{}, fmt.Errorf("zerodha: protective quantity must be positive, got %d", p.Quantity)
	}
	if p.Stop <= 0 && p.Target <= 0 {
		return kiteconnect.GTTParams{}, fmt.Errorf("zerodha: a protective order needs a stop or a target")
	}
	last, err := c.lastPrice(ctx, p.Key)
	if err != nil {
		return kiteconnect.GTTParams{}, err
	}
	return buildGTTParams(p, last)
}

// buildGTTParams is the pure half of gttParams, so the leg assignment can be
// tested without a Kite session.
func buildGTTParams(p domain.Protective, last money.Money) (kiteconnect.GTTParams, error) {
	product, err := toProduct(p.Product)
	if err != nil {
		return kiteconnect.GTTParams{}, err
	}

	params := kiteconnect.GTTParams{
		Tradingsymbol:   p.Key.Symbol,
		Exchange:        p.Key.Exchange,
		LastPrice:       rupees(last),
		TransactionType: toTransactionType(p.Side),
		Product:         product,
	}

	qty := float64(p.Quantity)
	stopLimit := p.Stop
	if p.StopLimit > 0 {
		stopLimit = p.StopLimit
	}
	if p.OCO() {
		// Kite orders the legs by price, not by role: Lower is the
		// numerically smaller trigger. For a long's protective sell that
		// is the stop and Upper is the target; for a short's protective
		// buy it is the reverse. Assigning by role rather than by price
		// is the mistake that turns a stop into a target.
		lower, upper := p.Stop, p.Target
		lowerLimit, upperLimit := stopLimit, p.Target
		if lower > upper {
			lower, upper = upper, lower
			lowerLimit, upperLimit = upperLimit, lowerLimit
		}
		params.Trigger = &kiteconnect.GTTOneCancelsOtherTrigger{
			Lower: kiteconnect.TriggerParams{
				TriggerValue: rupees(lower),
				LimitPrice:   rupees(lowerLimit),
				Quantity:     qty,
			},
			Upper: kiteconnect.TriggerParams{
				TriggerValue: rupees(upper),
				LimitPrice:   rupees(upperLimit),
				Quantity:     qty,
			},
		}
		return params, nil
	}

	level, limit := p.Stop, stopLimit
	if level <= 0 {
		level, limit = p.Target, p.Target
	}
	params.Trigger = &kiteconnect.GTTSingleLegTrigger{
		TriggerParams: kiteconnect.TriggerParams{
			TriggerValue: rupees(level),
			LimitPrice:   rupees(limit),
			Quantity:     qty,
		},
	}
	return params, nil
}

// lastPrice fetches one instrument's last traded price.
func (c *Client) lastPrice(ctx context.Context, key domain.InstrumentKey) (money.Money, error) {
	prices, err := c.LTP(ctx, []domain.InstrumentKey{key})
	if err != nil {
		return 0, err
	}
	last, ok := prices[key]
	if !ok || last <= 0 {
		return 0, fmt.Errorf("zerodha: no last price for %s, which a GTT requires", key)
	}
	return last, nil
}

// ModifyStop moves a resting GTT's stop, as a breakeven or trailing adjustment
// does.
//
// Kite has no partial modify: the whole trigger is replaced. The existing GTT
// is read first so the target leg, quantity and product survive the change,
// which is what makes this safe to call repeatedly from a trailing loop.
func (c *Client) ModifyStop(ctx context.Context, id string, stop money.Money) error {
	triggerID, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("zerodha: %q is not a Kite trigger id: %w", id, err)
	}

	var existing kiteconnect.GTT
	err = c.call(ctx, func() error {
		var err error
		existing, err = c.kite.GetGTT(triggerID)
		return err
	})
	if err != nil {
		return fmt.Errorf("zerodha: reading GTT %s before modifying it: %w", id, err)
	}

	current, err := c.fromGTT(existing)
	if err != nil {
		return err
	}
	current.Stop = stop

	params, err := c.gttParams(ctx, current)
	if err != nil {
		return err
	}
	err = c.call(ctx, func() error {
		_, err := c.kite.ModifyGTT(triggerID, params)
		return err
	})
	if err != nil {
		return fmt.Errorf("zerodha: modifying GTT %s: %w", id, err)
	}
	return nil
}

// CancelProtective deletes a resting GTT.
func (c *Client) CancelProtective(ctx context.Context, id string) error {
	triggerID, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("zerodha: %q is not a Kite trigger id: %w", id, err)
	}
	err = c.call(ctx, func() error {
		_, err := c.kite.DeleteGTT(triggerID)
		return err
	})
	if err != nil {
		return fmt.Errorf("zerodha: deleting GTT %s: %w", id, err)
	}
	return nil
}

// ListProtective returns the GTTs that are still active.
//
// Triggered and cancelled GTTs stay in Kite's list; returning them would make a
// reconciler believe a position is still protected by a trigger that has
// already fired.
func (c *Client) ListProtective(ctx context.Context) ([]domain.Protective, error) {
	var gtts kiteconnect.GTTs
	err := c.call(ctx, func() error {
		var err error
		gtts, err = c.kite.GetGTTs()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: listing GTTs: %w", err)
	}

	var out []domain.Protective
	for _, g := range gtts {
		if g.Status != "active" {
			continue
		}
		p, err := c.fromGTT(g)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// fromGTT converts a Kite GTT into a Protective.
//
// The trigger values carry the price levels and the embedded orders carry the
// side, quantity and product, so both are needed to reconstruct one.
func (c *Client) fromGTT(g kiteconnect.GTT) (domain.Protective, error) {
	if len(g.Orders) == 0 {
		return domain.Protective{}, fmt.Errorf("zerodha: GTT %d has no orders", g.ID)
	}
	first := g.Orders[0]

	p := domain.Protective{
		ID:       strconv.Itoa(g.ID),
		Key:      domain.InstrumentKey{Exchange: g.Condition.Exchange, Symbol: g.Condition.Tradingsymbol},
		Side:     fromTransactionType(first.TransactionType),
		Quantity: int(first.Quantity),
		Product:  fromProduct(first.Product),
	}

	switch len(g.Condition.TriggerValues) {
	case 1:
		p.Stop = paise(g.Condition.TriggerValues[0])
	case 2:
		// Kite lists the values in price order, so which is the stop and
		// which the target depends on the side of the protective order:
		// a sell that protects a long stops below and targets above.
		lower := paise(g.Condition.TriggerValues[0])
		upper := paise(g.Condition.TriggerValues[1])
		if lower > upper {
			lower, upper = upper, lower
		}
		if p.Side == domain.Sell {
			p.Stop, p.Target = lower, upper
		} else {
			p.Stop, p.Target = upper, lower
		}
	default:
		return domain.Protective{}, fmt.Errorf("zerodha: GTT %d has %d trigger values, expected 1 or 2",
			g.ID, len(g.Condition.TriggerValues))
	}
	return p, nil
}
