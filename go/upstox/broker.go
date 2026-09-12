package upstox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// envelope is the shape every Upstox response shares: a status and a payload.
type envelope[T any] struct {
	Status string `json:"status"`
	Data   T      `json:"data"`
}

// idPayload covers both spellings of an order acknowledgement. The v3 place
// endpoint answers with order_ids as a list and the v2 one with a single
// order_id; breakout500's live path accepts either, having met both.
type idPayload struct {
	OrderID  string   `json:"order_id"`
	OrderIDs []string `json:"order_ids"`
}

// id returns the acknowledged order id, or an empty string when the response
// carried none.
func (p idPayload) id() string {
	if p.OrderID != "" {
		return p.OrderID
	}
	if len(p.OrderIDs) > 0 {
		return p.OrderIDs[0]
	}
	return ""
}

// fundsSegment is one segment's funds view.
type fundsSegment struct {
	AvailableMargin float64 `json:"available_margin"`
	UsedMargin      float64 `json:"used_margin"`
}

// Account returns the equity segment's funds.
//
// Commodity margins are deliberately not folded in, as in the Kite adapter: an
// equity strategy sizing against a combined figure would size against capital
// it cannot use for the instrument it is about to trade.
//
// Upstox reports what is available and what is used but no explicit equity
// figure, so Equity is their sum — the capital the segment holds, whether or
// not it is currently committed. dhaara reads only AvailableMargin and leaves
// the rest zero, which makes any risk check that reasons about deployed capital
// silently wrong.
func (c *Client) Account(ctx context.Context) (domain.Account, error) {
	q := url.Values{}
	// SEC is the securities segment: equity and equity derivatives.
	q.Set("segment", "SEC")

	var resp envelope[map[string]fundsSegment]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/user/get-funds-and-margin",
		query:  q,
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("upstox: fetching funds: %w", err)
	}

	seg, ok := resp.Data["equity"]
	if !ok {
		// Upstox keys the response by segment name. Falling back to the
		// only segment present is dhaara's behaviour and is right when
		// the account trades one segment; erroring on an empty response
		// is better than reporting zero equity, which reads as a wiped
		// account and would stop every strategy at once.
		if len(resp.Data) == 0 {
			return domain.Account{}, fmt.Errorf("upstox: funds response carried no segment data")
		}
		for _, only := range resp.Data {
			seg = only
			break
		}
	}
	available, used := paise(seg.AvailableMargin), paise(seg.UsedMargin)
	return domain.Account{
		Equity:    available + used,
		Available: available,
		Used:      used,
	}, nil
}

// placeOrderRequest is the v3 order payload.
//
// InstrumentToken carries the instrument *key* despite the name; that is what
// Upstox calls the field, and both Go and Python donors send the key in it.
type placeOrderRequest struct {
	Quantity          int     `json:"quantity"`
	Product           string  `json:"product"`
	Validity          string  `json:"validity"`
	Price             float64 `json:"price"`
	InstrumentToken   string  `json:"instrument_token"`
	OrderType         string  `json:"order_type"`
	TransactionType   string  `json:"transaction_type"`
	DisclosedQuantity int     `json:"disclosed_quantity"`
	TriggerPrice      float64 `json:"trigger_price"`
	IsAMO             bool    `json:"is_amo"`
	Tag               string  `json:"tag,omitempty"`
}

// PlaceOrder submits an order.
//
// The returned Order carries Upstox's id and a pending status: an
// acknowledgement is not a fill, and treating it as one is how a reconciler
// comes to believe in positions that do not exist. Call OrderStatus to learn
// what actually happened.
//
// The call is not retried. Retrying a POST that placed an order places a second
// one, and a transport error gives no way to tell which happened.
func (c *Client) PlaceOrder(ctx context.Context, req domain.OrderRequest) (domain.Order, error) {
	body, err := c.orderBody(req)
	if err != nil {
		return domain.Order{}, err
	}

	var resp envelope[idPayload]
	err = c.doJSON(ctx, request{
		method: http.MethodPost,
		base:   c.baseV3,
		path:   "/order/place",
		body:   body,
		out:    &resp,
		retry:  false,
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("upstox: placing %s %d %s: %w", req.Side, req.Quantity, req.Key, err)
	}
	id := resp.Data.id()
	if id == "" {
		return domain.Order{}, fmt.Errorf("upstox: placing %s %d %s: response carried no order id (status %q)",
			req.Side, req.Quantity, req.Key, resp.Status)
	}

	now := time.Now()
	return domain.Order{
		ID:        id,
		Request:   req,
		Status:    domain.StatusPending,
		PlacedAt:  now,
		UpdatedAt: now,
	}, nil
}

// orderBody translates a request into Upstox's payload.
func (c *Client) orderBody(req domain.OrderRequest) (placeOrderRequest, error) {
	if req.Quantity <= 0 {
		return placeOrderRequest{}, fmt.Errorf("upstox: quantity must be positive, got %d", req.Quantity)
	}
	key, err := c.instrumentKeyFor(req.Key)
	if err != nil {
		return placeOrderRequest{}, err
	}
	product, err := toProduct(req.Product)
	if err != nil {
		return placeOrderRequest{}, err
	}
	orderType, err := toOrderType(req.Type)
	if err != nil {
		return placeOrderRequest{}, err
	}
	validity, err := toValidity(req.TimeInForce)
	if err != nil {
		return placeOrderRequest{}, err
	}

	tag := req.Tag
	if tag == "" {
		tag = c.opts.Tag
	}

	body := placeOrderRequest{
		Quantity:        req.Quantity,
		Product:         product,
		Validity:        validity,
		InstrumentToken: key,
		OrderType:       orderType,
		TransactionType: toTransactionType(req.Side),
		Tag:             tag,
		// is_amo is documented but dead: Upstox answers UDAPI1162 to any
		// order placed while the market is closed whatever the flag says,
		// verified against a live account. It is sent false rather than
		// omitted because the field is required.
		IsAMO: false,
	}
	switch req.Type {
	case domain.Limit:
		if req.LimitPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("upstox: a limit order needs a limit price")
		}
		body.Price = rupees(req.LimitPrice)
	case domain.Stop:
		if req.TriggerPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("upstox: a stop order needs a trigger price")
		}
		body.TriggerPrice = rupees(req.TriggerPrice)
	case domain.StopLimit:
		if req.TriggerPrice <= 0 || req.LimitPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("upstox: a stop-limit order needs both a trigger and a limit price")
		}
		body.TriggerPrice = rupees(req.TriggerPrice)
		body.Price = rupees(req.LimitPrice)
	}
	return body, nil
}

// CancelOrder withdraws an open order.
//
// Not retried: a cancel that succeeded but whose response was lost would, on a
// second attempt, be refused for an order that no longer exists — and that
// refusal would be reported to the caller as a failed cancel, which is the
// dangerous direction to be wrong in.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	q := url.Values{}
	q.Set("order_id", id)
	err := c.doJSON(ctx, request{
		method: http.MethodDelete,
		base:   c.baseV3,
		path:   "/order/cancel",
		query:  q,
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("upstox: cancelling order %s: %w", id, err)
	}
	return nil
}

// orderRow is one order as the details and order-book endpoints report it.
//
// The tradingsymbol is spelled both ways across endpoints, so both are read;
// dhaara's positions handling does the same for the same reason.
type orderRow struct {
	OrderID         string  `json:"order_id"`
	Exchange        string  `json:"exchange"`
	Tradingsymbol   string  `json:"tradingsymbol"`
	TradingSymbol   string  `json:"trading_symbol"`
	InstrumentToken string  `json:"instrument_token"`
	TransactionType string  `json:"transaction_type"`
	Quantity        int     `json:"quantity"`
	OrderType       string  `json:"order_type"`
	Product         string  `json:"product"`
	Validity        string  `json:"validity"`
	Price           float64 `json:"price"`
	TriggerPrice    float64 `json:"trigger_price"`
	Status          string  `json:"status"`
	FilledQuantity  int     `json:"filled_quantity"`
	AveragePrice    float64 `json:"average_price"`
	StatusMessage   string  `json:"status_message"`
	OrderTimestamp  string  `json:"order_timestamp"`
	Tag             string  `json:"tag"`
}

// symbol returns whichever spelling of the tradingsymbol the row carried.
func (r orderRow) symbol() string {
	if r.TradingSymbol != "" {
		return r.TradingSymbol
	}
	return r.Tradingsymbol
}

// OrderStatus fetches one order's current state.
func (c *Client) OrderStatus(ctx context.Context, id string) (domain.Order, error) {
	q := url.Values{}
	q.Set("order_id", id)

	var resp envelope[orderRow]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/order/details",
		query:  q,
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("upstox: fetching order %s: %w", id, err)
	}
	if resp.Data.OrderID == "" && resp.Data.Status == "" {
		return domain.Order{}, fmt.Errorf("upstox: order %s not found", id)
	}
	order := c.toOrder(resp.Data)
	if order.ID == "" {
		order.ID = id
	}
	return order, nil
}

// OpenOrders returns orders that have not reached a terminal state.
func (c *Client) OpenOrders(ctx context.Context) ([]domain.Order, error) {
	var resp envelope[[]orderRow]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/order/retrieve-all",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("upstox: fetching the order book: %w", err)
	}

	var out []domain.Order
	for _, row := range resp.Data {
		order := c.toOrder(row)
		if !order.Status.Terminal() {
			out = append(out, order)
		}
	}
	return out, nil
}

// toOrder converts an Upstox order row into the domain's.
func (c *Client) toOrder(r orderRow) domain.Order {
	placed := parseUpstoxTime(r.OrderTimestamp)
	return domain.Order{
		ID: r.OrderID,
		Request: domain.OrderRequest{
			Key:          c.keyFromRow(r.Exchange, r.symbol(), r.InstrumentToken),
			Side:         fromTransactionType(r.TransactionType),
			Quantity:     r.Quantity,
			Type:         fromOrderType(r.OrderType),
			Product:      fromProduct(r.Product),
			LimitPrice:   paise(r.Price),
			TriggerPrice: paise(r.TriggerPrice),
			TimeInForce:  fromValidity(r.Validity),
			Tag:          r.Tag,
		},
		Status:         normalizeStatus(r.Status),
		FilledQuantity: r.FilledQuantity,
		AveragePrice:   paise(r.AveragePrice),
		PlacedAt:       placed,
		UpdatedAt:      placed,
		Message:        r.StatusMessage,
	}
}

// keyFromRow reconstructs an instrument key from whatever a response row
// carried.
//
// The exchange and tradingsymbol fields are preferred; when a row names only
// the instrument key, the segment gives the exchange and the id stands in for
// the symbol. That id is an ISIN for equity, which is not a tradingsymbol —
// but a key that names the right instrument in an unusual way is more useful
// than an empty one, and the caller can resolve it through the store.
func (c *Client) keyFromRow(exchange, symbol, instrumentKey string) domain.InstrumentKey {
	if exchange != "" && symbol != "" {
		return domain.InstrumentKey{Exchange: exchangeFor(exchange), Symbol: symbol}
	}
	segment, id, err := parseInstrumentKey(instrumentKey)
	if err != nil {
		return domain.InstrumentKey{Exchange: exchangeFor(exchange), Symbol: symbol}
	}
	if symbol == "" {
		symbol = id
	}
	return domain.InstrumentKey{Exchange: exchangeFor(segment), Symbol: symbol}
}

// positionRow is one row of the short-term positions response.
type positionRow struct {
	Exchange        string  `json:"exchange"`
	Product         string  `json:"product"`
	Quantity        int     `json:"quantity"`
	AveragePrice    float64 `json:"average_price"`
	Tradingsymbol   string  `json:"tradingsymbol"`
	TradingSymbol   string  `json:"trading_symbol"`
	InstrumentToken string  `json:"instrument_token"`
	Realised        float64 `json:"realised"`
	Unrealised      float64 `json:"unrealised"`
}

// Positions returns the open positions.
//
// short-term-positions, not long-term-holdings: the port means positions, and
// holdings are a separate concept. A strategy reading holdings would believe it
// is flat while holding an intraday position.
//
// Zero-quantity rows are dropped. Upstox keeps a closed position's row for the
// rest of the day; it is history, not a holding.
func (c *Client) Positions(ctx context.Context) ([]domain.Position, error) {
	var resp envelope[[]positionRow]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/portfolio/short-term-positions",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("upstox: fetching positions: %w", err)
	}

	out := make([]domain.Position, 0, len(resp.Data))
	for _, p := range resp.Data {
		if p.Quantity == 0 {
			continue
		}
		symbol := p.TradingSymbol
		if symbol == "" {
			symbol = p.Tradingsymbol
		}
		out = append(out, domain.Position{
			Key:           c.keyFromRow(p.Exchange, symbol, p.InstrumentToken),
			Quantity:      p.Quantity,
			AveragePrice:  paise(p.AveragePrice),
			Product:       fromProduct(p.Product),
			RealizedPnL:   paise(p.Realised),
			UnrealizedPnL: paise(p.Unrealised),
		})
	}
	return out, nil
}

// marginLegRequest is one leg of a basket margin estimate.
type marginLegRequest struct {
	InstrumentKey   string `json:"instrument_key"`
	Quantity        int    `json:"quantity"`
	TransactionType string `json:"transaction_type"`
	Product         string `json:"product"`
}

// BasketMargin estimates what a basket would block.
//
// final_margin is the figure after offsetting benefit — the amount actually
// blocked — and is what a risk check must size against. required_margin is the
// gross number before benefit and would refuse spreads the account can afford.
func (c *Client) BasketMargin(ctx context.Context, legs []domain.MarginLeg) (money.Money, error) {
	if len(legs) == 0 {
		return 0, nil
	}
	instruments := make([]marginLegRequest, 0, len(legs))
	for _, l := range legs {
		key, err := c.instrumentKeyFor(l.Key)
		if err != nil {
			return 0, err
		}
		product, err := toProduct(l.Product)
		if err != nil {
			return 0, err
		}
		instruments = append(instruments, marginLegRequest{
			InstrumentKey:   key,
			Quantity:        l.Quantity,
			TransactionType: toTransactionType(l.Side),
			Product:         product,
		})
	}

	var resp envelope[struct {
		RequiredMargin float64 `json:"required_margin"`
		FinalMargin    float64 `json:"final_margin"`
	}]
	err := c.doJSON(ctx, request{
		method: http.MethodPost,
		path:   "/charges/margin",
		body:   map[string]any{"instruments": instruments},
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return 0, fmt.Errorf("upstox: estimating basket margin for %d legs: %w", len(legs), err)
	}
	return paise(resp.Data.FinalMargin), nil
}

// parseUpstoxTime reads the several timestamp forms Upstox uses.
//
// Quote payloads carry epoch milliseconds as a string; order rows carry a local
// datetime with no offset, which is IST. A value that parses as neither returns
// the zero time rather than time.Now(), which dhaara substitutes: a fabricated
// timestamp is indistinguishable from a real one and would silently reorder a
// time-sorted book.
func parseUpstoxTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}
