package upstox

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// InstrumentsURL is the NSE instrument master. It is served from the assets
// host without authentication, as a gzipped CSV measured in megabytes, so it
// belongs in a daily sync and not in a request path.
const InstrumentsURL = "https://assets.upstox.com/market-quote/instruments/exchange/NSE.csv.gz"

// Per-call key limits on the quote endpoints.
//
// The full-quote endpoint's 500-key ceiling is what lets a morning screener
// prefilter a whole universe in one request; the LTP endpoint's is far lower,
// which is why the two are chunked separately rather than sharing a constant.
const (
	ltpChunkSize   = 30
	quoteChunkSize = 500
)

// LTP returns the last traded price for each instrument.
//
// Instruments Upstox does not recognise are omitted rather than erroring, so
// one delisted symbol cannot fail a 500-symbol scan. A caller that needs to
// know which were missing compares the result's length against its request.
func (c *Client) LTP(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]money.Money, error) {
	out := make(map[domain.InstrumentKey]money.Money, len(keys))
	byKey, order, err := c.resolveKeys(keys)
	if err != nil {
		return nil, err
	}

	for _, chunk := range chunks(order, ltpChunkSize) {
		var resp envelope[map[string]struct {
			LastPrice       float64 `json:"last_price"`
			InstrumentToken string  `json:"instrument_token"`
			InstrumentKey   string  `json:"instrument_key"`
		}]
		q := url.Values{}
		q.Set("instrument_key", strings.Join(chunk, ","))
		err := c.doJSON(ctx, request{
			method: http.MethodGet,
			path:   "/market-quote/ltp",
			query:  q,
			out:    &resp,
			retry:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("upstox: fetching last prices for %d instruments: %w", len(chunk), err)
		}
		for _, entry := range resp.Data {
			key, ok := byKey[firstNonEmpty(entry.InstrumentToken, entry.InstrumentKey)]
			if !ok {
				continue
			}
			out[key] = paise(entry.LastPrice)
		}
	}
	return out, nil
}

// Quote returns the still-forming session's OHLC and last price.
func (c *Client) Quote(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]domain.Quote, error) {
	out := make(map[domain.InstrumentKey]domain.Quote, len(keys))
	byKey, order, err := c.resolveKeys(keys)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	for _, chunk := range chunks(order, quoteChunkSize) {
		var resp envelope[map[string]quoteEntry]
		q := url.Values{}
		q.Set("instrument_key", strings.Join(chunk, ","))
		err := c.doJSON(ctx, request{
			method: http.MethodGet,
			path:   "/market-quote/quotes",
			query:  q,
			out:    &resp,
			retry:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("upstox: fetching quotes for %d instruments: %w", len(chunk), err)
		}
		// The response is keyed by "NSE_EQ:RELIANCE", not by the
		// instrument key that was asked for, so entries are re-keyed from
		// the field each one carries. An entry naming no key at all is
		// dropped: a quote that cannot be attributed is worse than a
		// missing one, since a screener would size a spike against
		// another company's history.
		for _, entry := range resp.Data {
			key, ok := byKey[firstNonEmpty(entry.InstrumentToken, entry.InstrumentKey)]
			if !ok {
				continue
			}
			quote := domain.Quote{
				Key:  key,
				At:   now,
				Last: paise(entry.LastPrice),
				Open: paise(entry.OHLC.Open),
				High: paise(entry.OHLC.High),
				Low:  paise(entry.OHLC.Low),
				// OHLC.Close is the previous session's close, which is
				// what domain.Quote.Close means. Conflating it with the
				// last price makes every gap calculation silently zero.
				Close: paise(entry.OHLC.Close),
			}
			if len(entry.Depth.Buy) > 0 {
				quote.Bid = paise(entry.Depth.Buy[0].Price)
			}
			if len(entry.Depth.Sell) > 0 {
				quote.Ask = paise(entry.Depth.Sell[0].Price)
			}
			if at := parseUpstoxTime(entry.LastTradeTime); !at.IsZero() {
				quote.At = at
			}
			out[key] = quote
		}
	}
	return out, nil
}

// quoteEntry is one instrument's payload in the full-quote response.
type quoteEntry struct {
	LastPrice       float64 `json:"last_price"`
	Volume          float64 `json:"volume"`
	OI              float64 `json:"oi"`
	InstrumentToken string  `json:"instrument_token"`
	InstrumentKey   string  `json:"instrument_key"`
	LastTradeTime   string  `json:"last_trade_time"`
	OHLC            struct {
		Open  float64 `json:"open"`
		High  float64 `json:"high"`
		Low   float64 `json:"low"`
		Close float64 `json:"close"`
	} `json:"ohlc"`
	Depth struct {
		Buy  []depthLevel `json:"buy"`
		Sell []depthLevel `json:"sell"`
	} `json:"depth"`
}

type depthLevel struct {
	Quantity int     `json:"quantity"`
	Price    float64 `json:"price"`
	Orders   int     `json:"orders"`
}

// resolveKeys maps each requested instrument to its Upstox key, returning the
// reverse index the responses are decoded through and the keys in request
// order.
func (c *Client) resolveKeys(keys []domain.InstrumentKey) (map[string]domain.InstrumentKey, []string, error) {
	byKey := make(map[string]domain.InstrumentKey, len(keys))
	order := make([]string, 0, len(keys))
	for _, k := range keys {
		resolved, err := c.instrumentKeyFor(k)
		if err != nil {
			return nil, nil, err
		}
		if _, seen := byKey[resolved]; seen {
			continue
		}
		byKey[resolved] = k
		order = append(order, resolved)
	}
	return byKey, order, nil
}

// chunks splits a slice into batches of at most n.
func chunks(items []string, n int) [][]string {
	var out [][]string
	for start := 0; start < len(items); start += n {
		end := start + n
		if end > len(items) {
			end = len(items)
		}
		out = append(out, items[start:end])
	}
	return out
}

// firstNonEmpty returns the first non-empty string, which is how the two
// spellings of the instrument-key field are read without preferring a
// missing one.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// candleResponse is the shape every candle endpoint returns: a positional array
// per bar, newest first.
//
//	[timestamp, open, high, low, close, volume, open_interest]
type candleResponse struct {
	Status string `json:"status"`
	Data   struct {
		Candles [][]any `json:"candles"`
	} `json:"data"`
}

// Candles returns bars over [from, to], oldest first.
//
// Upstox serves a bounded span per request — minute data is refused beyond
// roughly a month with UDAPI1148 — so a long backfill is fetched as a series of
// chunks and concatenated. The caller asks for ten years and does not have to
// know that.
func (c *Client) Candles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	unit, interval, err := toInterval(tf)
	if err != nil {
		return nil, err
	}
	resolved, err := c.instrumentKeyFor(key)
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("upstox: candle range ends (%s) before it starts (%s)",
			to.Format(time.DateOnly), from.Format(time.DateOnly))
	}

	span := time.Duration(chunkDays(unit, interval)) * 24 * time.Hour
	var out []domain.Candle
	for start := from; !start.After(to); start = start.Add(span) {
		end := start.Add(span - time.Second)
		if end.After(to) {
			end = to
		}
		// The path takes the LATER date first. Both Go and Python donors
		// order it this way; reversing it returns an empty series rather
		// than an error, which is why it is worth a comment.
		path := fmt.Sprintf("/historical-candle/%s/%s/%d/%s/%s",
			url.PathEscape(resolved), unit, interval,
			end.Format(time.DateOnly), start.Format(time.DateOnly))

		chunk, err := c.candles(ctx, c.baseV3, path, key, tf)
		if err != nil {
			return nil, fmt.Errorf("upstox: fetching %s candles for %s from %s to %s: %w",
				tf, key, start.Format(time.DateOnly), end.Format(time.DateOnly), err)
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// IntradayCandles returns today's bars so far.
//
// The historical endpoint serves only completed sessions, so a screener running
// at 09:46 cannot use it: the session it needs is the one still running. This is
// the separate path for that, and it is the only call here whose answer changes
// minute to minute.
func (c *Client) IntradayCandles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe) ([]domain.Candle, error) {
	unit, interval, err := toInterval(tf)
	if err != nil {
		return nil, err
	}
	resolved, err := c.instrumentKeyFor(key)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/historical-candle/intraday/%s/%s/%d", url.PathEscape(resolved), unit, interval)
	out, err := c.candles(ctx, c.baseV3, path, key, tf)
	if err != nil {
		return nil, fmt.Errorf("upstox: fetching today's %s candles for %s: %w", tf, key, err)
	}
	return out, nil
}

// candles performs one candle request and converts the positional arrays into
// candles, oldest first — the order a backtest replays and every indicator
// assumes.
//
// A malformed row is skipped rather than failing the batch: one unparseable bar
// in a five-year backfill is a gap, and refusing the whole range over it means
// no history at all.
func (c *Client) candles(ctx context.Context, base, path string, key domain.InstrumentKey, tf domain.Timeframe) ([]domain.Candle, error) {
	var resp candleResponse
	err := c.doJSON(ctx, request{
		method:  http.MethodGet,
		base:    base,
		path:    path,
		out:     &resp,
		limiter: c.historical,
		retry:   true,
	})
	if err != nil {
		return nil, err
	}

	rows := resp.Data.Candles
	out := make([]domain.Candle, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if len(row) < 6 {
			continue
		}
		stamp, ok := row[0].(string)
		if !ok {
			continue
		}
		start, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			continue
		}
		out = append(out, domain.Candle{
			Key:          key,
			Timeframe:    tf,
			Start:        start,
			Open:         paise(number(row, 1)),
			High:         paise(number(row, 2)),
			Low:          paise(number(row, 3)),
			Close:        paise(number(row, 4)),
			Volume:       int64(number(row, 5)),
			OpenInterest: int64(number(row, 6)),
		})
	}
	return out, nil
}

// number reads a positional field as a float, returning 0 for an absent or
// non-numeric one. Open interest is absent for cash equity, which is a zero and
// not an error.
func number(row []any, i int) float64 {
	if i >= len(row) {
		return 0
	}
	v, _ := row[i].(float64)
	return v
}

// ExpiredExpiries lists the expiry dates available for an underlying, as
// "2006-01-02" strings.
//
// Upstox documents this as covering roughly the last six months, which bounds
// any option-level backtest built on it.
func (c *Client) ExpiredExpiries(ctx context.Context, underlyingKey string) ([]string, error) {
	q := url.Values{}
	q.Set("instrument_key", underlyingKey)

	var resp envelope[[]string]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		base:   c.baseV3,
		path:   "/expired-instruments/expiries",
		query:  q,
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("upstox: listing expiries for %s: %w", underlyingKey, err)
	}
	return resp.Data, nil
}

// ExpiredContract is one expired option contract as Upstox lists it.
type ExpiredContract struct {
	Name             string  `json:"name"`
	Segment          string  `json:"segment"`
	Exchange         string  `json:"exchange"`
	Expiry           string  `json:"expiry"`
	InstrumentKey    string  `json:"instrument_key"`
	TradingSymbol    string  `json:"trading_symbol"`
	InstrumentType   string  `json:"instrument_type"` // CE or PE
	StrikePrice      float64 `json:"strike_price"`
	LotSize          int     `json:"lot_size"`
	TickSize         float64 `json:"tick_size"`
	UnderlyingKey    string  `json:"underlying_key"`
	UnderlyingSymbol string  `json:"underlying_symbol"`
	Weekly           bool    `json:"weekly"`
}

// ExpiredOptionContracts lists the contracts that expired on a date for an
// underlying, with their strikes and lot sizes.
func (c *Client) ExpiredOptionContracts(ctx context.Context, underlyingKey, expiry string) ([]ExpiredContract, error) {
	q := url.Values{}
	q.Set("instrument_key", underlyingKey)
	q.Set("expiry_date", expiry)

	var resp envelope[[]ExpiredContract]
	err := c.doJSON(ctx, request{
		method: http.MethodGet,
		base:   c.baseV3,
		path:   "/expired-instruments/option/contract",
		query:  q,
		out:    &resp,
		retry:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("upstox: listing contracts expiring %s on %s: %w", expiry, underlyingKey, err)
	}
	return resp.Data, nil
}

// ExpiredHistoricalCandles fetches bars for an already-settled contract, keyed
// by the "NSE_FO|47983|17-04-2025" form ExpiredOptionContracts returns.
//
// This is the only route in the whole codebase to backtesting options that have
// already expired. Unlike the live historical endpoint it needs both a valid
// token and an Upstox Plus entitlement; without one the call fails with an
// authorisation error rather than returning an empty series, so a caller can
// tell "not entitled" from "no trades that day".
//
// It is served from the v2 root while its sibling listing endpoints are v3 —
// verified in zerobha, not assumed.
func (c *Client) ExpiredHistoricalCandles(ctx context.Context, expiredKey string, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	unit, interval, err := toInterval(tf)
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("upstox: candle range ends (%s) before it starts (%s)",
			to.Format(time.DateOnly), from.Format(time.DateOnly))
	}
	segment, id, err := parseInstrumentKey(expiredKey)
	if err != nil {
		return nil, err
	}
	key := domain.InstrumentKey{Exchange: exchangeFor(segment), Symbol: id}

	path := fmt.Sprintf("/expired-instruments/historical-candle/%s/%s/%d/%s/%s",
		url.PathEscape(expiredKey), unit, interval,
		to.Format(time.DateOnly), from.Format(time.DateOnly))
	out, err := c.candles(ctx, c.baseURL, path, key, tf)
	if err != nil {
		return nil, fmt.Errorf("upstox: fetching expired %s candles for %s: %w", tf, expiredKey, err)
	}
	return out, nil
}

// Instruments returns the instrument master for an exchange.
//
// The master is a gzipped CSV on the assets host and needs no token, so it is
// fetched directly rather than through doJSON. An exchange other than NSE is
// served by substituting its name into the URL; an empty one means NSE, which
// is the file every donor uses.
func (c *Client) Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error) {
	source := InstrumentsURL
	if exchange != "" && !strings.EqualFold(exchange, "NSE") {
		source = strings.Replace(InstrumentsURL, "/NSE.csv.gz", "/"+strings.ToUpper(exchange)+".csv.gz", 1)
	}
	if c.opts.InstrumentsURL != "" {
		source = c.opts.InstrumentsURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("upstox: building instrument-master request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstox: downloading the instrument master: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstox: downloading the instrument master: HTTP %d", resp.StatusCode)
	}

	// The master is a .gz file, but whether the body arrives compressed
	// depends on the hop: net/http transparently decompresses a
	// Content-Encoding: gzip response and strips the header while doing so,
	// so neither the URL suffix nor the headers can be trusted. The magic
	// number can be, and costs two bytes to check.
	buf := bufio.NewReader(resp.Body)
	var body io.Reader = buf
	if magic, err := buf.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(buf)
		if err != nil {
			return nil, fmt.Errorf("upstox: gunzipping the instrument master: %w", err)
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}
	return parseInstrumentCSV(body)
}

// parseInstrumentCSV reads the instrument master.
//
// A malformed row is skipped rather than failing the batch. The master carries
// tens of thousands of rows and one bad line — an unescaped comma in a company
// name, a field the exchange added this morning — must not cost a sync its
// whole universe.
func parseInstrumentCSV(r io.Reader) ([]domain.Instrument, error) {
	reader := csv.NewReader(r)
	// Rows genuinely vary in width across segments; FieldsPerRecord of -1
	// leaves the per-row check to the column lookup below, which knows
	// which columns it actually needs.
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("upstox: reading the instrument master header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}

	var out []domain.Instrument
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A parse error here is the reader's, not one row's, so it
			// would repeat on every subsequent Read.
			return out, fmt.Errorf("upstox: reading the instrument master: %w", err)
		}
		field := func(name string) string {
			i, ok := col[name]
			if !ok || i >= len(row) {
				return ""
			}
			return strings.TrimSpace(row[i])
		}

		symbol := field("tradingsymbol")
		if symbol == "" {
			symbol = field("trading_symbol")
		}
		instrumentKey := field("instrument_key")
		if symbol == "" || instrumentKey == "" {
			continue
		}
		segment, _, err := parseInstrumentKey(instrumentKey)
		if err != nil {
			continue
		}

		instrumentType := field("instrument_type")
		in := domain.Instrument{
			Key:        domain.InstrumentKey{Exchange: exchangeFor(segment), Symbol: symbol},
			Name:       field("name"),
			ISIN:       field("isin"),
			Segment:    segmentOf(instrumentType, segment),
			LotSize:    atoiOr(field("lot_size"), 1),
			TickSize:   paise(atofOr(field("tick_size"), 0)),
			Strike:     paise(atofOr(field("strike"), atofOr(field("strike_price"), 0))),
			OptionType: optionTypeOf(instrumentType),
			Active:     true,
		}
		if in.LotSize < 1 {
			// The master reports 0 for cash equity; the domain uses 1 so
			// sizing can multiply by it unconditionally.
			in.LotSize = 1
		}
		if expiry := parseExpiry(field("expiry")); expiry != nil {
			in.Expiry = expiry
		}
		out = append(out, in)
	}
	return out, nil
}

// segmentOf classifies an instrument type into the domain's vocabulary.
//
// The CSV master spells cash equity "EQUITY" and the JSON master spells it
// "EQ"; both are accepted, because a project switching masters must not
// silently reclassify its whole universe.
func segmentOf(instrumentType, segment string) string {
	switch strings.ToUpper(instrumentType) {
	case "CE", "PE", "OPTIDX", "OPTSTK":
		return "options"
	case "FUT", "FUTIDX", "FUTSTK", "FUTCUR":
		return "futures"
	}
	if strings.HasSuffix(strings.ToUpper(segment), "_INDEX") {
		return "index"
	}
	return "equity"
}

// optionTypeOf normalises an instrument type to the domain's option marker,
// returning an empty string for anything that is not an option.
func optionTypeOf(instrumentType string) string {
	switch strings.ToUpper(instrumentType) {
	case "CE":
		return "ce"
	case "PE":
		return "pe"
	default:
		return ""
	}
}

// parseExpiry reads the master's expiry column, which is epoch milliseconds in
// the JSON feed and a date string in the CSV one. An unparseable or absent
// value returns nil, which is what a cash instrument carries.
func parseExpiry(s string) *time.Time {
	if s == "" || s == "0" {
		return nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms > 0 {
		t := time.UnixMilli(ms)
		return &t
	}
	for _, layout := range []string{time.DateOnly, "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

func atoiOr(s string, fallback int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return fallback
}

func atofOr(s string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return fallback
}
