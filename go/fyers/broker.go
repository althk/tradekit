package fyers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// fund_limit row ids.
//
// The funds response is a ledger of titled rows rather than named fields, and
// the ids are what identify them; the titles are display text. These three are
// the ones a risk check needs. They are matched by id first and by title as a
// fallback, so a renumbering is survived if the wording is kept.
const (
	fundTotalBalance     = 1
	fundUtilizedAmount   = 2
	fundAvailableBalance = 10

	fundTitleTotal     = "Total Balance"
	fundTitleUtilized  = "Utilized Amount"
	fundTitleAvailable = "Available Balance"
)

// fundRow is one row of the funds ledger.
type fundRow struct {
	ID              int     `json:"id"`
	Title           string  `json:"title"`
	EquityAmount    float64 `json:"equityAmount"`
	CommodityAmount float64 `json:"commodityAmount"`
}

// Account returns the equity ledger's funds.
//
// Each row carries a separate figure for the commodity ledger, and it is
// deliberately not folded in, as in the other adapters: an equity strategy
// sizing against a combined figure would size against capital it cannot use
// for the instrument it is about to trade.
//
// "Available Balance" (id 10) is what can be committed to a new order right
// now, after utilisation, collateral and the day's realised P&L; "Clear
// Balance" (id 3) is the ledger figure before collateral and is not what a
// margin check should size against.
func (c *Client) Account(ctx context.Context) (domain.Account, error) {
	var resp struct {
		envelope
		FundLimit []fundRow `json:"fund_limit"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/funds",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("fyers: fetching funds: %w", err)
	}
	if len(resp.FundLimit) == 0 {
		// Erroring on an empty ledger is better than reporting zero
		// equity, which reads as a wiped account and would stop every
		// strategy at once.
		return domain.Account{}, fmt.Errorf("fyers: funds response carried no fund_limit rows")
	}

	total, okTotal := fundAmount(resp.FundLimit, fundTotalBalance, fundTitleTotal)
	used, _ := fundAmount(resp.FundLimit, fundUtilizedAmount, fundTitleUtilized)
	available, okAvailable := fundAmount(resp.FundLimit, fundAvailableBalance, fundTitleAvailable)
	if !okAvailable {
		return domain.Account{}, fmt.Errorf("fyers: funds response carried no %q row", fundTitleAvailable)
	}
	if !okTotal {
		total = available + used
	}
	return domain.Account{
		Equity:    total,
		Available: available,
		Used:      used,
	}, nil
}

// fundAmount finds a ledger row by id, or by title when no row carries the id,
// and returns its equity figure in paise.
func fundAmount(rows []fundRow, id int, title string) (money.Money, bool) {
	for _, r := range rows {
		if r.ID == id {
			return paise(r.EquityAmount), true
		}
	}
	for _, r := range rows {
		if strings.EqualFold(strings.TrimSpace(r.Title), title) {
			return paise(r.EquityAmount), true
		}
	}
	return 0, false
}

// placeOrderRequest is the order payload.
//
// Every price field is sent, zero when unused, because the reference marks
// them mandatory. TakeProfit and StopLoss are offsets FYERS would attach as
// bracket legs; they are never set here, since a resting stop is placed through
// PlaceProtective where its lifecycle can be managed.
type placeOrderRequest struct {
	Symbol       string  `json:"symbol"`
	Qty          int     `json:"qty"`
	Type         int     `json:"type"`
	Side         int     `json:"side"`
	ProductType  string  `json:"productType"`
	LimitPrice   float64 `json:"limitPrice"`
	StopPrice    float64 `json:"stopPrice"`
	Validity     string  `json:"validity"`
	DisclosedQty int     `json:"disclosedQty"`
	OfflineOrder bool    `json:"offlineOrder"`
	StopLoss     float64 `json:"stopLoss"`
	TakeProfit   float64 `json:"takeProfit"`
	OrderTag     string  `json:"orderTag,omitempty"`
}

// ackResponse is the acknowledgement every order mutation returns.
type ackResponse struct {
	envelope
	ID string `json:"id"`
}

// PlaceOrder submits an order.
//
// The returned Order carries FYERS's id and a pending status: an
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

	var resp ackResponse
	err = c.doJSON(ctx, request{
		method: http.MethodPost,
		path:   "/orders/sync",
		body:   body,
		out:    &resp,
		retry:  false,
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("fyers: placing %s %d %s: %w", req.Side, req.Quantity, req.Key, err)
	}
	if resp.ID == "" {
		// The reference documents code 201 as "request made, no
		// acknowledgement received; check the order book". There is no id
		// to poll, so the caller is told rather than handed an empty
		// order it would poll forever.
		return domain.Order{}, fmt.Errorf("fyers: placing %s %d %s: response carried no order id (code %d: %s)",
			req.Side, req.Quantity, req.Key, resp.Code, resp.Message)
	}

	now := time.Now()
	return domain.Order{
		ID:        resp.ID,
		Request:   req,
		Status:    domain.StatusPending,
		PlacedAt:  now,
		UpdatedAt: now,
	}, nil
}

// orderBody translates a request into FYERS's payload.
func (c *Client) orderBody(req domain.OrderRequest) (placeOrderRequest, error) {
	if req.Quantity <= 0 {
		return placeOrderRequest{}, fmt.Errorf("fyers: quantity must be positive, got %d", req.Quantity)
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
		Symbol:      symbolFor(req.Key),
		Qty:         req.Quantity,
		Type:        orderType,
		Side:        toSide(req.Side),
		ProductType: product,
		Validity:    validity,
		OrderTag:    tag,
		// offlineOrder is the after-market flag. It is sent false: a
		// strategy that places orders while the market is closed is
		// making a mistake this adapter should surface, not paper over.
		OfflineOrder: false,
	}
	switch req.Type {
	case domain.Limit:
		if req.LimitPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("fyers: a limit order needs a limit price")
		}
		body.LimitPrice = rupees(req.LimitPrice)
	case domain.Stop:
		if req.TriggerPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("fyers: a stop order needs a trigger price")
		}
		body.StopPrice = rupees(req.TriggerPrice)
	case domain.StopLimit:
		if req.TriggerPrice <= 0 || req.LimitPrice <= 0 {
			return placeOrderRequest{}, fmt.Errorf("fyers: a stop-limit order needs both a trigger and a limit price")
		}
		body.StopPrice = rupees(req.TriggerPrice)
		body.LimitPrice = rupees(req.LimitPrice)
	}
	return body, nil
}

// CancelOrder withdraws an open order.
//
// The id goes in a JSON body on a DELETE, which is FYERS's documented form and
// the one the SDKs use. Not retried: a cancel that succeeded but whose
// response was lost would, on a second attempt, be refused for an order that no
// longer exists — and that refusal would be reported to the caller as a failed
// cancel, which is the dangerous direction to be wrong in.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	err := c.doJSON(ctx, request{
		method: http.MethodDelete,
		path:   "/orders/sync",
		body:   map[string]string{"id": id},
		retry:  false,
	})
	if err != nil {
		return fmt.Errorf("fyers: cancelling order %s: %w", id, err)
	}
	return nil
}

// orderRow is one order as the order book reports it.
type orderRow struct {
	ID            string  `json:"id"`
	Symbol        string  `json:"symbol"`
	Segment       int     `json:"segment"`
	Qty           int     `json:"qty"`
	FilledQty     int     `json:"filledQty"`
	LimitPrice    float64 `json:"limitPrice"`
	StopPrice     float64 `json:"stopPrice"`
	TradedPrice   float64 `json:"tradedPrice"`
	Type          int     `json:"type"`
	Side          int     `json:"side"`
	ProductType   string  `json:"productType"`
	Status        int     `json:"status"`
	Message       string  `json:"message"`
	OrderValidity string  `json:"orderValidity"`
	OrderDateTime string  `json:"orderDateTime"`
	OrderTag      string  `json:"orderTag"`
}

// orderBookResponse is the shape of both the full book and the by-id filter.
type orderBookResponse struct {
	envelope
	OrderBook []orderRow `json:"orderBook"`
}

// OrderStatus fetches one order's current state.
//
// FYERS has no single-order endpoint; the book is filtered by id. The filter
// answers an unknown id with an empty book rather than an error, which is
// turned into one here: an empty order would read as one that is pending and
// never settles.
func (c *Client) OrderStatus(ctx context.Context, id string) (domain.Order, error) {
	q := url.Values{}
	q.Set("id", id)

	var resp orderBookResponse
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/orders",
		query:  q,
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("fyers: fetching order %s: %w", id, err)
	}
	for _, row := range resp.OrderBook {
		if row.ID == id {
			return toOrder(row), nil
		}
	}
	if len(resp.OrderBook) == 1 {
		// The filter honoured the id; trust it even if the row's own id
		// is spelled differently, as it is for a sliced order's child.
		return toOrder(resp.OrderBook[0]), nil
	}
	return domain.Order{}, fmt.Errorf("fyers: order %s not found", id)
}

// OpenOrders returns orders that have not reached a terminal state.
func (c *Client) OpenOrders(ctx context.Context) ([]domain.Order, error) {
	var resp orderBookResponse
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/orders",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("fyers: fetching the order book: %w", err)
	}

	var out []domain.Order
	for _, row := range resp.OrderBook {
		order := toOrder(row)
		if !order.Status.Terminal() {
			out = append(out, order)
		}
	}
	return out, nil
}

// toOrder converts a FYERS order row into the domain's.
func toOrder(r orderRow) domain.Order {
	placed := parseOrderTime(r.OrderDateTime)
	return domain.Order{
		ID: r.ID,
		Request: domain.OrderRequest{
			Key:          keyFor(r.Symbol, r.Segment),
			Side:         fromSide(r.Side),
			Quantity:     r.Qty,
			Type:         fromOrderType(r.Type),
			Product:      fromProduct(r.ProductType),
			LimitPrice:   paise(r.LimitPrice),
			TriggerPrice: paise(r.StopPrice),
			TimeInForce:  fromValidity(r.OrderValidity),
			Tag:          stripTagPrefix(r.OrderTag),
		},
		Status:         normalizeStatus(r.Status),
		FilledQuantity: r.FilledQty,
		AveragePrice:   paise(r.TradedPrice),
		PlacedAt:       placed,
		UpdatedAt:      placed,
		Message:        r.Message,
	}
}

// stripTagPrefix removes the "1:" FYERS prepends to a caller's tag, so an
// order round-trips with the tag the strategy gave it. A "2:" prefix marks a
// tag FYERS generated itself ("2:Untagged"); it is returned as an empty tag,
// because the strategy never set one.
func stripTagPrefix(tag string) string {
	switch {
	case strings.HasPrefix(tag, "1:"):
		return tag[2:]
	case strings.HasPrefix(tag, "2:"):
		return ""
	default:
		return tag
	}
}

// positionRow is one row of the net positions response.
type positionRow struct {
	Symbol           string  `json:"symbol"`
	Segment          int     `json:"segment"`
	NetQty           int     `json:"netQty"`
	NetAvg           float64 `json:"netAvg"`
	BuyAvg           float64 `json:"buyAvg"`
	SellAvg          float64 `json:"sellAvg"`
	ProductType      string  `json:"productType"`
	RealizedProfit   float64 `json:"realized_profit"`
	UnrealizedProfit float64 `json:"unrealized_profit"`
}

// Positions returns the open positions.
//
// netPositions, not holdings: the port means positions, and holdings are a
// separate concept. A strategy reading holdings would believe it is flat while
// holding an intraday position.
//
// Zero-quantity rows are dropped. FYERS keeps a closed position's row for the
// rest of the day; it is history, not a holding.
func (c *Client) Positions(ctx context.Context) ([]domain.Position, error) {
	var resp struct {
		envelope
		NetPositions []positionRow `json:"netPositions"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		path:   "/positions",
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("fyers: fetching positions: %w", err)
	}

	out := make([]domain.Position, 0, len(resp.NetPositions))
	for _, p := range resp.NetPositions {
		if p.NetQty == 0 {
			continue
		}
		// netAvg is FYERS's own average for the net position. It is
		// preferred over the buy or sell average because a position that
		// has been partly scaled out has both, and only netAvg says what
		// the remaining quantity cost.
		avg := p.NetAvg
		if avg == 0 {
			if p.NetQty > 0 {
				avg = p.BuyAvg
			} else {
				avg = p.SellAvg
			}
		}
		out = append(out, domain.Position{
			Key:           keyFor(p.Symbol, p.Segment),
			Quantity:      p.NetQty,
			AveragePrice:  paise(avg),
			Product:       fromProduct(p.ProductType),
			RealizedPnL:   paise(p.RealizedProfit),
			UnrealizedPnL: paise(p.UnrealizedProfit),
		})
	}
	return out, nil
}

// marginLegRequest is one leg of a basket margin estimate.
type marginLegRequest struct {
	Symbol      string  `json:"symbol"`
	Qty         int     `json:"qty"`
	Side        int     `json:"side"`
	Type        int     `json:"type"`
	ProductType string  `json:"productType"`
	LimitPrice  float64 `json:"limitPrice"`
	StopPrice   float64 `json:"stopPrice"`
	StopLoss    float64 `json:"stopLoss"`
	TakeProfit  float64 `json:"takeProfit"`
}

// BasketMargin estimates what a basket would block.
//
// margin_total is the figure for the basket as a whole, after any hedge
// benefit between legs, and is what a risk check must size against;
// margin_new_order includes the account's existing positions and would refuse
// a spread the account can afford.
//
// FYERS applies the hedge benefit only when the legs arrive in an order it
// recognises — a buy before the sell it hedges — so legs are sent in the order
// given and a caller building a spread should list the long leg first.
func (c *Client) BasketMargin(ctx context.Context, legs []domain.MarginLeg) (money.Money, error) {
	if len(legs) == 0 {
		return 0, nil
	}
	data := make([]marginLegRequest, 0, len(legs))
	for _, l := range legs {
		product, err := toProduct(l.Product)
		if err != nil {
			return 0, err
		}
		leg := marginLegRequest{
			Symbol:      symbolFor(l.Key),
			Qty:         l.Quantity,
			Side:        toSide(l.Side),
			Type:        orderTypeMarket,
			ProductType: product,
		}
		if l.Price > 0 {
			leg.Type = orderTypeLimit
			leg.LimitPrice = rupees(l.Price)
		}
		data = append(data, leg)
	}

	var resp struct {
		envelope
		Data struct {
			MarginTotal    float64 `json:"margin_total"`
			MarginNewOrder float64 `json:"margin_new_order"`
			MarginAvail    float64 `json:"margin_avail"`
		} `json:"data"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodPost,
		path:   "/multiorder/margin",
		body:   map[string]any{"data": data},
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return 0, fmt.Errorf("fyers: estimating basket margin for %d legs: %w", len(legs), err)
	}
	return paise(resp.Data.MarginTotal), nil
}
