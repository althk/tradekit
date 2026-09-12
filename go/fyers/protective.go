package fyers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// gttLeg is one leg of a GTT. Price is the limit the exit order is placed at
// once TriggerPrice is touched.
type gttLeg struct {
	Price        float64 `json:"price"`
	TriggerPrice float64 `json:"triggerPrice"`
	Qty          int     `json:"qty"`
}

// gttOrderInfo carries the legs. FYERS's rule for an OCO is positional: leg1
// must trigger above the last traded price and leg2 below it, whichever of
// them is the stop.
type gttOrderInfo struct {
	Leg1 gttLeg  `json:"leg1"`
	Leg2 *gttLeg `json:"leg2,omitempty"`
}

// gttRequest is the place payload. The modify endpoint takes only the id and
// the legs, so it uses a separate, smaller struct below.
type gttRequest struct {
	Side        int          `json:"side"`
	Symbol      string       `json:"symbol"`
	ProductType string       `json:"productType"`
	OrderInfo   gttOrderInfo `json:"orderInfo"`
	OrderTag    string       `json:"orderTag,omitempty"`
}

// gttModifyRequest is the modify payload.
type gttModifyRequest struct {
	ID        string       `json:"id"`
	OrderInfo gttOrderInfo `json:"orderInfo"`
}

// gttRow is one trigger as the GTT book reports it. The book uses snake_case
// where the rest of the API uses camelCase; the names are FYERS's.
type gttRow struct {
	ID            string  `json:"id"`
	Symbol        string  `json:"symbol"`
	Segment       int     `json:"segment"`
	ProductType   string  `json:"product_type"`
	TranSide      int     `json:"tran_side"`
	Qty           int     `json:"qty"`
	Qty2          int     `json:"qty2"`
	PriceLimit    float64 `json:"price_limit"`
	PriceTrigger  float64 `json:"price_trigger"`
	Price2Limit   float64 `json:"price2_limit"`
	Price2Trigger float64 `json:"price2_trigger"`
	OCOInd        int     `json:"gtt_oco_ind"`
	OrdStatus     int     `json:"ord_status"`
	LTP           float64 `json:"ltp"`
}

// live reports whether the trigger is still protecting a position.
//
// The book returns cancelled and triggered rows alongside live ones — the
// reference's own sample is two cancelled triggers — and reading it as a set of
// live stops is wrong in the silent direction: a dead trigger still looks like
// protection, so a missing stop is never noticed. A status this adapter has not
// seen reads as live: an unknown status raising a false "stop is missing" on
// every position every morning would bury the real alerts.
func (r gttRow) live() bool {
	switch r.OrdStatus {
	case statusCancelled, statusTraded, statusRejected, statusExpired:
		return false
	default:
		return true
	}
}

// PlaceProtective rests a stop, or a stop and target as a one-cancels-other
// pair, as a FYERS GTT.
//
// A GTT rests at FYERS rather than at the exchange and survives the process
// exiting, which is the whole reason a swing strategy uses one instead of
// watching prices itself.
//
// FYERS accepts a GTT only for delivery and overnight products; an intraday
// position is squared off by the broker before a GTT could matter, and asking
// for one is refused here rather than at the broker so the mistake is caught
// at wiring time.
func (c *Client) PlaceProtective(ctx context.Context, p domain.Protective) (string, error) {
	info, err := gttLegs(p)
	if err != nil {
		return "", err
	}
	product, err := toProduct(p.Product)
	if err != nil {
		return "", err
	}
	if product == productIntraday {
		return "", fmt.Errorf("fyers: a GTT cannot protect an INTRADAY position; use a stop order")
	}

	var resp ackResponse
	err = c.doJSON(ctx, request{
		method: http.MethodPost,
		path:   "/gtt/orders/sync",
		body: gttRequest{
			Side:        toSide(p.Side),
			Symbol:      symbolFor(p.Key),
			ProductType: product,
			OrderInfo:   info,
			OrderTag:    c.opts.Tag,
		},
		out: &resp,
		// Not retried: a retry that lands after a lost acknowledgement
		// rests a second stop on the same position, which sells the
		// position twice when it triggers.
		retry: false,
	})
	if err != nil {
		return "", fmt.Errorf("fyers: placing GTT for %s: %w", p.Key, err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("fyers: placing GTT for %s: response carried no trigger id (code %d: %s)",
			p.Key, resp.Code, resp.Message)
	}
	return resp.ID, nil
}

// gttLegs builds the legs of a trigger from a Protective.
//
// The leg order follows FYERS's positional rule, not the leg's role: leg1 is
// the one that triggers above the market and leg2 the one below. A sell that
// protects a long has its target above and its stop below, so the target is
// leg1; a buy that protects a short is the mirror image, with the stop as
// leg1. Assigning by role would have FYERS reject every short's OCO.
//
// The exit is placed as a limit at the trigger price, which is what FYERS's
// own web client does. A limit exactly at the trigger can be left unfilled by
// a gap; that is the same exposure a Kite GTT has, and the alternative — a
// limit some distance through the trigger — is a policy the strategy, not the
// adapter, should set.
func gttLegs(p domain.Protective) (gttOrderInfo, error) {
	if p.Quantity <= 0 {
		return gttOrderInfo{}, fmt.Errorf("fyers: protective quantity must be positive, got %d", p.Quantity)
	}
	if p.Stop <= 0 && p.Target <= 0 {
		return gttOrderInfo{}, fmt.Errorf("fyers: a protective order needs a stop or a target")
	}

	leg := func(level money.Money) gttLeg {
		return gttLeg{Price: rupees(level), TriggerPrice: rupees(level), Qty: p.Quantity}
	}
	if p.OCO() {
		above, below := p.Target, p.Stop
		if p.Side == domain.Buy {
			above, below = p.Stop, p.Target
		}
		l2 := leg(below)
		return gttOrderInfo{Leg1: leg(above), Leg2: &l2}, nil
	}

	level := p.Stop
	if level <= 0 {
		level = p.Target
	}
	return gttOrderInfo{Leg1: leg(level)}, nil
}

// ModifyStop moves a resting trigger's stop, as a breakeven or trailing
// adjustment does.
//
// FYERS replaces the whole leg set rather than patching one leg, so the
// existing trigger is read first and its target, quantity and side carried
// across. Sending only the stop would silently drop the target leg of an OCO,
// which is the mistake this read-before-write exists to prevent — the same one
// the Kite and Upstox adapters guard against.
func (c *Client) ModifyStop(ctx context.Context, id string, stop money.Money) error {
	existing, err := c.protective(ctx, id)
	if err != nil {
		return err
	}
	existing.Stop = stop

	info, err := gttLegs(existing)
	if err != nil {
		return err
	}
	err = c.doJSON(ctx, request{
		method: http.MethodPatch,
		path:   "/gtt/orders/sync",
		body:   gttModifyRequest{ID: id, OrderInfo: info},
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("fyers: modifying GTT %s: %w", id, err)
	}
	return nil
}

// CancelProtective deletes a resting trigger. The id goes in a JSON body on a
// DELETE, as for an ordinary order.
func (c *Client) CancelProtective(ctx context.Context, id string) error {
	err := c.doJSON(ctx, request{
		method: http.MethodDelete,
		path:   "/gtt/orders/sync",
		body:   map[string]string{"id": id},
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("fyers: cancelling GTT %s: %w", id, err)
	}
	return nil
}

// ListProtective returns the triggers that are still live.
//
// Triggered and cancelled rows stay in FYERS's book; returning them would make
// a reconciler believe a position is protected by a trigger that has already
// fired.
func (c *Client) ListProtective(ctx context.Context) ([]domain.Protective, error) {
	rows, err := c.gttBook(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Protective
	for _, row := range rows {
		if !row.live() {
			continue
		}
		out = append(out, fromGTT(row))
	}
	return out, nil
}

// gttBook fetches the raw trigger book.
func (c *Client) gttBook(ctx context.Context) ([]gttRow, error) {
	var resp struct {
		envelope
		OrderBook []gttRow `json:"orderBook"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/gtt/orders",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("fyers: listing GTTs: %w", err)
	}
	return resp.OrderBook, nil
}

// protective reads one trigger by id.
//
// FYERS has no single-trigger endpoint, so the book is fetched and filtered.
// It is one request either way, and it means ModifyStop's read-before-write
// costs nothing extra.
func (c *Client) protective(ctx context.Context, id string) (domain.Protective, error) {
	rows, err := c.gttBook(ctx)
	if err != nil {
		return domain.Protective{}, fmt.Errorf("fyers: reading GTT %s before modifying it: %w", id, err)
	}
	for _, row := range rows {
		if row.ID == id {
			return fromGTT(row), nil
		}
	}
	return domain.Protective{}, fmt.Errorf("fyers: GTT %s is not in the trigger book", id)
}

// fromGTT converts a trigger row into a Protective.
//
// FYERS does not label a leg as stop or target; the levels are read by their
// position relative to the market. For an OCO the two legs are on opposite
// sides of the price by construction, so the order's own side says which is
// which: a sell's stop is the lower level, a buy's the higher. A single leg is
// judged against the row's last traded price when the book carries one, and
// read as a stop when it does not — outside market hours the book reports an
// LTP of zero, and a stop is what a single-leg protective order nearly always
// is.
func fromGTT(r gttRow) domain.Protective {
	p := domain.Protective{
		ID:       r.ID,
		Key:      keyFor(r.Symbol, r.Segment),
		Side:     fromSide(r.TranSide),
		Quantity: r.Qty,
		Product:  fromProduct(r.ProductType),
	}
	leg1, leg2 := paise(r.PriceTrigger), paise(r.Price2Trigger)

	if r.OCOInd == gttOCO || leg2 > 0 {
		high, low := leg1, leg2
		if low > high {
			high, low = low, high
		}
		if p.Side == domain.Sell {
			p.Stop, p.Target = low, high
		} else {
			p.Stop, p.Target = high, low
		}
		return p
	}

	ltp := paise(r.LTP)
	isTarget := ltp > 0 && ((p.Side == domain.Sell && leg1 > ltp) || (p.Side == domain.Buy && leg1 < ltp))
	if isTarget {
		p.Target = leg1
	} else {
		p.Stop = leg1
	}
	return p
}
