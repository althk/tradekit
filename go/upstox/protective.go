package upstox

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// GTT rule strategies. ENTRY is the leg that triggers the exit order; TARGET
// and STOPLOSS are the two legs of a one-cancels-other pair.
const (
	ruleEntry    = "ENTRY"
	ruleTarget   = "TARGET"
	ruleStoploss = "STOPLOSS"
)

// terminalGTTStatuses are the rule states that mean a trigger is no longer
// protecting anything.
//
// breakout500's reconciler records, verified against a live account, that the
// GTT book returns cancelled triggers alongside live ones — four runs that each
// placed and cancelled one trigger left four rows, every one of them CANCELLED.
// Reading the book as a set of live stops is wrong in the silent direction: a
// dead trigger still looks like protection, so a missing stop is never noticed.
var terminalGTTStatuses = map[string]bool{
	"CANCELLED": true,
	"CANCELED":  true,
	"TRIGGERED": true,
	"COMPLETED": true,
	"COMPLETE":  true,
	"EXPIRED":   true,
	"REJECTED":  true,
}

// gttRule is one leg of a trigger.
type gttRule struct {
	Strategy     string  `json:"strategy"`
	TriggerType  string  `json:"trigger_type"`
	TriggerPrice float64 `json:"trigger_price"`
	Status       string  `json:"status,omitempty"`
}

// gttRequest is the place and modify payload. Upstox identifies the trigger to
// modify by GTTOrderID and ignores the instrument on that call, so one struct
// serves both with the irrelevant fields omitted.
type gttRequest struct {
	Type            string    `json:"type"`
	Quantity        int       `json:"quantity"`
	Product         string    `json:"product,omitempty"`
	InstrumentToken string    `json:"instrument_token,omitempty"`
	TransactionType string    `json:"transaction_type,omitempty"`
	GTTOrderID      string    `json:"gtt_order_id,omitempty"`
	Rules           []gttRule `json:"rules"`
}

// gttRow is one trigger as the book reports it.
type gttRow struct {
	GTTOrderID      string    `json:"gtt_order_id"`
	OrderID         string    `json:"order_id"`
	Type            string    `json:"type"`
	Exchange        string    `json:"exchange"`
	Tradingsymbol   string    `json:"tradingsymbol"`
	TradingSymbol   string    `json:"trading_symbol"`
	InstrumentToken string    `json:"instrument_token"`
	TransactionType string    `json:"transaction_type"`
	Product         string    `json:"product"`
	Quantity        int       `json:"quantity"`
	Rules           []gttRule `json:"rules"`
}

// id returns whichever spelling of the trigger id the row carried.
func (r gttRow) id() string {
	if r.GTTOrderID != "" {
		return r.GTTOrderID
	}
	return r.OrderID
}

// live reports whether the trigger is still protecting a position.
//
// A row with no rules to judge on is treated as live, and so is a rule whose
// status is one this adapter has not seen. An unknown status raising a false
// "stop is missing" on every position every morning would bury the real alerts,
// which is the failure this distinction exists to avoid.
func (r gttRow) live() bool {
	if len(r.Rules) == 0 {
		return true
	}
	for _, rule := range r.Rules {
		status := strings.ToUpper(strings.TrimSpace(rule.Status))
		if status == "" || !terminalGTTStatuses[status] {
			return true
		}
	}
	return false
}

// PlaceProtective rests a stop, or a stop and target as a one-cancels-other
// pair, as an Upstox GTT.
//
// A GTT rests at Upstox rather than at the exchange and survives the process
// exiting, which is the whole reason a swing strategy uses one instead of
// watching prices itself.
//
// The trigger direction follows the protective order's own side rather than the
// leg's role: a sell that protects a long stops BELOW and targets ABOVE, and a
// buy that protects a short is the mirror image. Assigning by role would turn a
// short's stop into a target.
func (c *Client) PlaceProtective(ctx context.Context, p domain.Protective) (string, error) {
	body, err := c.gttBody(p)
	if err != nil {
		return "", err
	}
	key, err := c.instrumentKeyFor(p.Key)
	if err != nil {
		return "", err
	}
	product, err := toProduct(p.Product)
	if err != nil {
		return "", err
	}
	body.InstrumentToken = key
	body.TransactionType = toTransactionType(p.Side)
	body.Product = product

	var resp envelope[gttIDPayload]
	err = c.doJSON(ctx, request{
		method: http.MethodPost,
		base:   c.baseV3,
		path:   "/order/gtt/place",
		body:   body,
		out:    &resp,
		// Not retried: a retry that lands after a lost acknowledgement
		// rests a second stop on the same position, which sells the
		// position twice when it triggers.
		retry: false,
	})
	if err != nil {
		return "", fmt.Errorf("upstox: placing GTT for %s: %w", p.Key, err)
	}
	id := resp.Data.id()
	if id == "" {
		return "", fmt.Errorf("upstox: placing GTT for %s: response carried no trigger id", p.Key)
	}
	return id, nil
}

// gttIDPayload covers the spellings a GTT acknowledgement uses.
type gttIDPayload struct {
	GTTOrderID  string   `json:"gtt_order_id"`
	GTTOrderIDs []string `json:"gtt_order_ids"`
	OrderID     string   `json:"order_id"`
}

func (p gttIDPayload) id() string {
	switch {
	case p.GTTOrderID != "":
		return p.GTTOrderID
	case len(p.GTTOrderIDs) > 0:
		return p.GTTOrderIDs[0]
	default:
		return p.OrderID
	}
}

// gttBody builds the type, quantity and rules of a trigger from a Protective.
// The instrument, side and product are filled in by the caller, because the
// modify endpoint takes none of them.
func (c *Client) gttBody(p domain.Protective) (gttRequest, error) {
	if p.Quantity <= 0 {
		return gttRequest{}, fmt.Errorf("upstox: protective quantity must be positive, got %d", p.Quantity)
	}
	if p.Stop <= 0 && p.Target <= 0 {
		return gttRequest{}, fmt.Errorf("upstox: a protective order needs a stop or a target")
	}

	body := gttRequest{Quantity: p.Quantity}
	if p.OCO() {
		body.Type = gttMultiple
		body.Rules = []gttRule{
			{Strategy: ruleStoploss, TriggerType: stopDirection(p.Side), TriggerPrice: rupees(p.Stop)},
			{Strategy: ruleTarget, TriggerType: targetDirection(p.Side), TriggerPrice: rupees(p.Target)},
		}
		return body, nil
	}

	body.Type = gttSingle
	level, direction := p.Stop, stopDirection(p.Side)
	if level <= 0 {
		level, direction = p.Target, targetDirection(p.Side)
	}
	body.Rules = []gttRule{
		{Strategy: ruleEntry, TriggerType: direction, TriggerPrice: rupees(level)},
	}
	return body, nil
}

// stopDirection is the trigger direction of the stop leg for a protective order
// on the given side. A sell protects a long and stops below it.
func stopDirection(side domain.Side) string {
	if side == domain.Sell {
		return triggerBelow
	}
	return triggerAbove
}

// targetDirection is the mirror of stopDirection.
func targetDirection(side domain.Side) string {
	if side == domain.Sell {
		return triggerAbove
	}
	return triggerBelow
}

// ModifyStop moves a resting trigger's stop, as a breakeven or trailing
// adjustment does.
//
// Upstox replaces the whole rule set rather than patching one leg, so the
// existing trigger is read first and its target, quantity and side carried
// across. Sending only the stop would silently drop the target leg of an OCO,
// which is the mistake this read-before-write exists to prevent — the same one
// the Kite adapter's ModifyStop guards against.
func (c *Client) ModifyStop(ctx context.Context, id string, stop money.Money) error {
	existing, err := c.protective(ctx, id)
	if err != nil {
		return err
	}
	existing.Stop = stop

	body, err := c.gttBody(existing)
	if err != nil {
		return err
	}
	body.GTTOrderID = id

	err = c.doJSON(ctx, request{
		method: http.MethodPut,
		base:   c.baseV3,
		path:   "/order/gtt/modify",
		body:   body,
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("upstox: modifying GTT %s: %w", id, err)
	}
	return nil
}

// CancelProtective deletes a resting trigger.
//
// The id goes in a JSON body, not the query string. breakout500's client
// records that the query form — which looks obviously right — is answered with
// UDAPI100038 and no indication of which input was wrong. The cost of getting
// this wrong is quiet: trailing raises a stop by cancelling and re-placing, so
// a cancel that always fails leaves every position on its original stop while
// the book reports a trailed one.
func (c *Client) CancelProtective(ctx context.Context, id string) error {
	err := c.doJSON(ctx, request{
		method: http.MethodDelete,
		base:   c.baseV3,
		path:   "/order/gtt/cancel",
		body:   map[string]string{"gtt_order_id": id},
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("upstox: cancelling GTT %s: %w", id, err)
	}
	return nil
}

// ListProtective returns the triggers that are still live.
//
// Triggered and cancelled rows stay in Upstox's book; returning them would make
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
		out = append(out, c.fromGTT(row))
	}
	return out, nil
}

// gttBook fetches the raw trigger book.
func (c *Client) gttBook(ctx context.Context) ([]gttRow, error) {
	var resp envelope[[]gttRow]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		base:   c.baseV3,
		path:   "/order/gtt",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("upstox: listing GTTs: %w", err)
	}
	return resp.Data, nil
}

// protective reads one trigger by id.
//
// Upstox has no single-trigger endpoint, so the book is fetched and filtered.
// It is one request either way, and it means ModifyStop's read-before-write
// costs nothing extra.
func (c *Client) protective(ctx context.Context, id string) (domain.Protective, error) {
	rows, err := c.gttBook(ctx)
	if err != nil {
		return domain.Protective{}, fmt.Errorf("upstox: reading GTT %s before modifying it: %w", id, err)
	}
	for _, row := range rows {
		if row.id() == id {
			return c.fromGTT(row), nil
		}
	}
	return domain.Protective{}, fmt.Errorf("upstox: GTT %s is not in the trigger book", id)
}

// fromGTT converts a trigger row into a Protective.
//
// The rules are read by strategy where Upstox names one, and by trigger
// direction where it does not: a single-leg rule is labelled ENTRY whatever it
// protects, so the direction relative to the order's own side is what says
// whether it is the stop or the target.
func (c *Client) fromGTT(r gttRow) domain.Protective {
	symbol := r.TradingSymbol
	if symbol == "" {
		symbol = r.Tradingsymbol
	}
	p := domain.Protective{
		ID:       r.id(),
		Key:      c.keyFromRow(r.Exchange, symbol, r.InstrumentToken),
		Side:     fromTransactionType(r.TransactionType),
		Quantity: r.Quantity,
		Product:  fromProduct(r.Product),
	}

	for _, rule := range r.Rules {
		price := paise(rule.TriggerPrice)
		switch strings.ToUpper(strings.TrimSpace(rule.Strategy)) {
		case ruleStoploss:
			p.Stop = price
		case ruleTarget:
			p.Target = price
		default:
			if strings.EqualFold(rule.TriggerType, stopDirection(p.Side)) {
				p.Stop = price
			} else {
				p.Target = price
			}
		}
	}
	return p
}
