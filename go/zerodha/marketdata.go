package zerodha

import (
	"context"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// LTP returns the last traded price for each instrument.
//
// Instruments Kite does not recognise are omitted from the result rather than
// erroring, so one delisted symbol cannot fail a 500-symbol scan. A caller that
// needs to know which were missing compares the result's length against its
// request.
func (c *Client) LTP(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]money.Money, error) {
	if len(keys) == 0 {
		return map[domain.InstrumentKey]money.Money{}, nil
	}
	symbols := make([]string, len(keys))
	for i, k := range keys {
		symbols[i] = quoteKey(k)
	}

	var quotes kiteconnect.QuoteLTP
	err := c.call(ctx, func() error {
		var err error
		quotes, err = c.kite.GetLTP(symbols...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching last prices for %d instruments: %w", len(keys), err)
	}

	out := make(map[domain.InstrumentKey]money.Money, len(quotes))
	for raw, q := range quotes {
		key, err := parseQuoteKey(raw)
		if err != nil {
			continue
		}
		out[key] = paise(q.LastPrice)
	}
	return out, nil
}

// Quote returns the still-forming session's OHLC and last price.
func (c *Client) Quote(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]domain.Quote, error) {
	if len(keys) == 0 {
		return map[domain.InstrumentKey]domain.Quote{}, nil
	}
	symbols := make([]string, len(keys))
	for i, k := range keys {
		symbols[i] = quoteKey(k)
	}

	var quotes kiteconnect.QuoteOHLC
	err := c.call(ctx, func() error {
		var err error
		quotes, err = c.kite.GetOHLC(symbols...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching quotes for %d instruments: %w", len(keys), err)
	}

	now := time.Now()
	out := make(map[domain.InstrumentKey]domain.Quote, len(quotes))
	for raw, q := range quotes {
		key, err := parseQuoteKey(raw)
		if err != nil {
			continue
		}
		out[key] = domain.Quote{
			Key:  key,
			At:   now,
			Last: paise(q.LastPrice),
			Open: paise(q.OHLC.Open),
			High: paise(q.OHLC.High),
			Low:  paise(q.OHLC.Low),
			// Kite's OHLC.Close is the previous session's close, which
			// is what domain.Quote.Close means. The current price is
			// Last, and conflating the two makes every gap calculation
			// silently zero.
			Close: paise(q.OHLC.Close),
		}
	}
	return out, nil
}

// Candles returns bars over [from, to], oldest first.
//
// Kite serves only a bounded span per request, and the bound differs by
// interval, so a long backfill is fetched as a series of chunks and
// concatenated. The caller asks for ten years and does not have to know that.
func (c *Client) Candles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	interval, err := toInterval(tf)
	if err != nil {
		return nil, err
	}
	token, err := c.instrumentToken(ctx, key)
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("zerodha: candle range ends (%s) before it starts (%s)",
			to.Format(time.DateOnly), from.Format(time.DateOnly))
	}

	span := time.Duration(chunkDays(interval)) * 24 * time.Hour
	var out []domain.Candle

	for start := from; !start.After(to); start = start.Add(span) {
		end := start.Add(span - time.Second)
		if end.After(to) {
			end = to
		}

		var chunk []kiteconnect.HistoricalData
		if err := c.historical.Wait(ctx); err != nil {
			return nil, err
		}
		chunk, err = c.kite.GetHistoricalData(token, interval, start, end, false, false)
		if err != nil {
			err = classify(err)
			return nil, fmt.Errorf("zerodha: fetching %s %s candles for %s from %s to %s: %w",
				interval, tf, key, start.Format(time.DateOnly), end.Format(time.DateOnly), err)
		}

		for _, d := range chunk {
			out = append(out, domain.Candle{
				Key:          key,
				Timeframe:    tf,
				Start:        d.Date.Time,
				Open:         paise(d.Open),
				High:         paise(d.High),
				Low:          paise(d.Low),
				Close:        paise(d.Close),
				Volume:       int64(d.Volume),
				OpenInterest: int64(d.OI),
			})
		}
	}
	return out, nil
}

// Instruments returns the instrument master for an exchange.
//
// An empty exchange fetches every exchange at once, which is a single large
// download rather than several; Kite serves this as a CSV and it is measured in
// megabytes, so it belongs in a daily sync and not in a request path.
func (c *Client) Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error) {
	var instruments kiteconnect.Instruments
	err := c.call(ctx, func() error {
		var err error
		if exchange == "" {
			instruments, err = c.kite.GetInstruments()
		} else {
			instruments, err = c.kite.GetInstrumentsByExchange(exchange)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching instruments for %q: %w", exchange, err)
	}

	out := make([]domain.Instrument, 0, len(instruments))
	for _, in := range instruments {
		converted := domain.Instrument{
			Key:        domain.InstrumentKey{Exchange: in.Exchange, Symbol: in.Tradingsymbol},
			Name:       in.Name,
			Segment:    segmentOf(in.InstrumentType, in.Exchange),
			LotSize:    int(in.LotSize),
			TickSize:   paise(in.TickSize),
			Strike:     paise(in.StrikePrice),
			OptionType: optionTypeOf(in.InstrumentType),
			Active:     true,
		}
		if converted.LotSize < 1 {
			// Kite reports 0 for cash equity; the domain uses 1 so
			// that sizing can multiply by it unconditionally.
			converted.LotSize = 1
		}
		if !in.Expiry.Time.IsZero() {
			expiry := in.Expiry.Time
			converted.Expiry = &expiry
		}
		out = append(out, converted)
	}
	return out, nil
}

// InstrumentTokens returns Kite's numeric token for each instrument on an
// exchange, which is what the historical endpoint needs and what a project
// stores through the store's SetBrokerID.
func (c *Client) InstrumentTokens(ctx context.Context, exchange string) (map[domain.InstrumentKey]int, error) {
	var instruments kiteconnect.Instruments
	err := c.call(ctx, func() error {
		var err error
		instruments, err = c.kite.GetInstrumentsByExchange(exchange)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("zerodha: fetching instrument tokens for %q: %w", exchange, err)
	}

	out := make(map[domain.InstrumentKey]int, len(instruments))
	for _, in := range instruments {
		out[domain.InstrumentKey{Exchange: in.Exchange, Symbol: in.Tradingsymbol}] = in.InstrumentToken
	}
	return out, nil
}
