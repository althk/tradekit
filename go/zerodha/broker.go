package zerodha

import (
	"context"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// Account returns the equity segment's funds.
//
// Commodity margins are deliberately not folded in: an equity strategy sizing
// against a combined figure would size against capital it cannot use for the
// instrument it is about to trade. A commodity strategy should read
// AllMargins through Kite directly.
func (c *Client) Account(ctx context.Context) (domain.Account, error) {
	var margins kiteconnect.AllMargins
	err := c.call(ctx, func() error {
		var err error
		margins, err = c.kite.GetUserMargins()
		return err
	})
	if err != nil {
		return domain.Account{}, fmt.Errorf("zerodha: fetching margins: %w", err)
	}

	equity := margins.Equity
	return domain.Account{
		Equity:    paise(equity.Net),
		Available: paise(equity.Available.LiveBalance),
		Used:      paise(equity.Used.Debits),
	}, nil
}

// PlaceOrder submits a regular order.
//
// The returned Order carries Kite's id and a pending status: Kite acknowledges
// an order before the exchange has accepted it, so treating the acknowledgement
// as a fill is how a reconciler comes to believe in positions that do not
// exist. Call OrderStatus to learn what actually happened.
func (c *Client) PlaceOrder(ctx context.Context, req domain.OrderRequest) (domain.Order, error) {
	params, err := c.orderParams(req)
	if err != nil {
		return domain.Order{}, err
	}

	var resp kiteconnect.OrderResponse
	err = c.call(ctx, func() error {
		var err error
		resp, err = c.kite.PlaceOrder(varietyRegular, params)
		return err
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("zerodha: placing %s %d %s: %w", req.Side, req.Quantity, req.Key, err)
	}

	now := time.Now()
	return domain.Order{
		ID:        resp.OrderID,
		Request:   req,
		Status:    domain.StatusPending,
		PlacedAt:  now,
		UpdatedAt: now,
	}, nil
}

// orderParams translates a request into Kite's parameters.
func (c *Client) orderParams(req domain.OrderRequest) (kiteconnect.OrderParams, error) {
	if req.Quantity <= 0 {
		return kiteconnect.OrderParams{}, fmt.Errorf("zerodha: quantity must be positive, got %d", req.Quantity)
	}
	product, err := toProduct(req.Product)
	if err != nil {
		return kiteconnect.OrderParams{}, err
	}
	orderType, err := toOrderType(req.Type)
	if err != nil {
		return kiteconnect.OrderParams{}, err
	}
	validity, err := toValidity(req.TimeInForce)
	if err != nil {
		return kiteconnect.OrderParams{}, err
	}

	tag := req.Tag
	if tag == "" {
		tag = c.opts.Tag
	}

	params := kiteconnect.OrderParams{
		Exchange:        req.Key.Exchange,
		Tradingsymbol:   req.Key.Symbol,
		TransactionType: toTransactionType(req.Side),
		Product:         product,
		OrderType:       orderType,
		Validity:        validity,
		Quantity:        req.Quantity,
		Tag:             tag,
	}
	// Kite's url tags are omitempty, so a zero price is simply absent —
	// which is what a market order requires and what a limit order must not
	// have, hence the checks below rather than blind assignment.
	switch req.Type {
	case domain.Limit:
		if req.LimitPrice <= 0 {
			return kiteconnect.OrderParams{}, fmt.Errorf("zerodha: a limit order needs a limit price")
		}
		params.Price = rupees(req.LimitPrice)
	case domain.Stop:
		if req.TriggerPrice <= 0 {
			return kiteconnect.OrderParams{}, fmt.Errorf("zerodha: a stop order needs a trigger price")
		}
		params.TriggerPrice = rupees(req.TriggerPrice)
	case domain.StopLimit:
		if req.TriggerPrice <= 0 || req.LimitPrice <= 0 {
			return kiteconnect.OrderParams{}, fmt.Errorf("zerodha: a stop-limit order needs both a trigger and a limit price")
		}
		params.TriggerPrice = rupees(req.TriggerPrice)
		params.Price = rupees(req.LimitPrice)
	}
	return params, nil
}

// CancelOrder withdraws an open order.
func (c *Client) CancelOrder(ctx context.Context, id string) error {
	err := c.call(ctx, func() error {
		_, err := c.kite.CancelOrder(varietyRegular, id, nil)
		return err
	})
	if err != nil {
		return fmt.Errorf("zerodha: cancelling order %s: %w", id, err)
	}
	return nil
}

// OrderStatus fetches an order's current state.
//
// Kite returns the order's whole history, oldest first, so the last entry is
// the current state. Reading the first — which is the "request received"
// record — would report every order as pending forever.
func (c *Client) OrderStatus(ctx context.Context, id string) (domain.Order, error) {
	var history []kiteconnect.Order
	err := c.call(ctx, func() error {
		var err error
		history, err = c.kite.GetOrderHistory(id)
		return err
	})
	if err != nil {
		return domain.Order{}, fmt.Errorf("zerodha: fetching order %s: %w", id, err)
	}
	if len(history) == 0 {
		return domain.Order{}, fmt.Errorf("zerodha: order %s has no history", id)
	}
	return c.toOrder(history[len(history)-1]), nil
}

// Positions returns the net open positions.
//
// Net, not Day: a position carried overnight appears in Net and not in Day, and
// a strategy reading Day would believe it is flat while holding stock.
func (c *Client) Positions(ctx context.Context) ([]domain.Position, error) {
	var positions kiteconnect.Positions
	err := c.call(ctx, func() error {
		var err error
		positions, err = c.kite.GetPositions()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching positions: %w", err)
	}

	out := make([]domain.Position, 0, len(positions.Net))
	for _, p := range positions.Net {
		if p.Quantity == 0 {
			// Kite keeps a zero-quantity row for the rest of the day
			// after a position is closed; it is history, not a holding.
			continue
		}
		out = append(out, domain.Position{
			Key:           domain.InstrumentKey{Exchange: p.Exchange, Symbol: p.Tradingsymbol},
			Quantity:      p.Quantity,
			AveragePrice:  paise(p.AveragePrice),
			Product:       fromProduct(p.Product),
			RealizedPnL:   paise(p.Realised),
			UnrealizedPnL: paise(p.Unrealised),
		})
	}
	return out, nil
}

// OpenOrders returns orders that have not reached a terminal state.
func (c *Client) OpenOrders(ctx context.Context) ([]domain.Order, error) {
	var orders kiteconnect.Orders
	err := c.call(ctx, func() error {
		var err error
		orders, err = c.kite.GetOrders()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching orders: %w", err)
	}

	var out []domain.Order
	for _, o := range orders {
		converted := c.toOrder(o)
		if !converted.Status.Terminal() {
			out = append(out, converted)
		}
	}
	return out, nil
}

// toOrder converts a Kite order into the domain's.
func (c *Client) toOrder(o kiteconnect.Order) domain.Order {
	return domain.Order{
		ID: o.OrderID,
		Request: domain.OrderRequest{
			Key:          domain.InstrumentKey{Exchange: o.Exchange, Symbol: o.TradingSymbol},
			Side:         fromTransactionType(o.TransactionType),
			Quantity:     int(o.Quantity),
			Type:         fromOrderType(o.OrderType),
			Product:      fromProduct(o.Product),
			LimitPrice:   paise(o.Price),
			TriggerPrice: paise(o.TriggerPrice),
			TimeInForce:  fromValidity(o.Validity),
			Tag:          o.Tag,
		},
		Status:         normalizeStatus(o.Status),
		FilledQuantity: int(o.FilledQuantity),
		AveragePrice:   paise(o.AveragePrice),
		PlacedAt:       o.OrderTimestamp.Time,
		UpdatedAt:      o.ExchangeUpdateTimestamp.Time,
		Message:        o.StatusMessage,
	}
}

// Holding is a delivery holding: stock the account owns, as distinct from a
// day position. It is a Zerodha type rather than a port because only the
// Indian brokers separate the two books, and a CNC bot needs both.
type Holding struct {
	Key InstrumentKey
	// Quantity is settled stock plus T1 -- bought yesterday, not yet
	// delivered -- which is still the account's exposure.
	Quantity     int
	AveragePrice money.Money
	LastPrice    money.Money
	// PnL is the unrealised profit against the average price.
	PnL money.Money
	// DayChange is today's move per share, so today's P&L on a holding is
	// DayChange times Quantity.
	DayChange money.Money
}

// InstrumentKey is re-exported so Holding reads without a domain import at
// the call site.
type InstrumentKey = domain.InstrumentKey

// Holdings returns the account's delivery holdings.
//
// A bot that checks day positions alone will buy the same stock again the day
// after it settles out of the positions book and into this one; the caller
// that guards "already holding this" needs both.
func (c *Client) Holdings(ctx context.Context) ([]Holding, error) {
	var holdings kiteconnect.Holdings
	err := c.call(ctx, func() error {
		var err error
		holdings, err = c.kite.GetHoldings()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching holdings: %w", err)
	}

	out := make([]Holding, 0, len(holdings))
	for _, h := range holdings {
		qty := h.Quantity + h.T1Quantity
		if qty == 0 {
			continue
		}
		out = append(out, Holding{
			Key:          domain.InstrumentKey{Exchange: h.Exchange, Symbol: h.Tradingsymbol},
			Quantity:     qty,
			AveragePrice: paise(h.AveragePrice),
			LastPrice:    paise(h.LastPrice),
			PnL:          paise(h.PnL),
			DayChange:    paise(h.DayChange),
		})
	}
	return out, nil
}
