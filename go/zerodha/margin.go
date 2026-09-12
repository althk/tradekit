package zerodha

import (
	"context"
	"fmt"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// BasketMargin returns the margin a set of legs would require if placed
// together.
//
// Basket rather than per-order margin is the point: a hedged pair costs far
// less than the sum of its legs, and sizing an options spread against the sum
// either refuses trades the account can afford or, worse, sizes off a figure
// that has nothing to do with what will actually be blocked.
//
// The Final figure is returned, which is the margin after the whole basket's
// offsets are applied. Initial is what the first leg alone would cost and is
// not what the account will be charged.
func (c *Client) BasketMargin(ctx context.Context, legs []domain.MarginLeg) (money.Money, error) {
	if len(legs) == 0 {
		return 0, nil
	}

	params := make([]kiteconnect.OrderMarginParam, 0, len(legs))
	for _, leg := range legs {
		if leg.Quantity <= 0 {
			return 0, fmt.Errorf("zerodha: margin leg quantity must be positive, got %d for %s", leg.Quantity, leg.Key)
		}
		product, err := toProduct(leg.Product)
		if err != nil {
			return 0, err
		}
		p := kiteconnect.OrderMarginParam{
			Exchange:        leg.Key.Exchange,
			Tradingsymbol:   leg.Key.Symbol,
			TransactionType: toTransactionType(leg.Side),
			Variety:         varietyRegular,
			Product:         product,
			Quantity:        float64(leg.Quantity),
		}
		// A zero price asks Kite to use the last traded price, which is
		// what a caller sizing against the current market wants.
		if leg.Price > 0 {
			p.OrderType = orderTypeLimit
			p.Price = rupees(leg.Price)
		} else {
			p.OrderType = orderTypeMarket
		}
		params = append(params, p)
	}

	var basket kiteconnect.BasketMargins
	err := c.call(ctx, func() error {
		var err error
		basket, err = c.kite.GetBasketMargins(kiteconnect.GetBasketParams{
			OrderParams: params,
			Compact:     true,
			// Existing positions offset the basket. A strategy adding a
			// leg to a live spread is charged the difference, not the
			// standalone cost, and ignoring that overstates the
			// requirement enough to block valid adjustments.
			ConsiderPositions: true,
		})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("zerodha: computing basket margin for %d legs: %w", len(legs), err)
	}
	return paise(basket.Final.Total), nil
}
