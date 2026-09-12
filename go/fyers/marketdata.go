package fyers

import (
	"context"
	"encoding/json"
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

// quoteChunkSize is the per-call symbol limit on the quotes endpoint.
const quoteChunkSize = 50

// quoteEntry is one symbol's payload in the quotes response. S is "ok" or
// "error" per symbol, so one unknown symbol fails only itself.
type quoteEntry struct {
	N string `json:"n"`
	S string `json:"s"`
	V struct {
		LP             float64 `json:"lp"`
		OpenPrice      float64 `json:"open_price"`
		HighPrice      float64 `json:"high_price"`
		LowPrice       float64 `json:"low_price"`
		PrevClosePrice float64 `json:"prev_close_price"`
		Bid            float64 `json:"bid"`
		Ask            float64 `json:"ask"`
		// TT is the quote's timestamp as epoch seconds, sent as a string
		// in the reference's own sample and as a number by the SDK's
		// model; both are accepted.
		TT json.Number `json:"tt"`
	} `json:"v"`
}

// LTP returns the last traded price for each instrument.
//
// Instruments FYERS does not recognise are omitted rather than erroring, so
// one delisted symbol cannot fail a 500-symbol scan. A caller that needs to
// know which were missing compares the result's length against its request.
func (c *Client) LTP(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]money.Money, error) {
	quotes, err := c.Quote(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[domain.InstrumentKey]money.Money, len(quotes))
	for key, q := range quotes {
		out[key] = q.Last
	}
	return out, nil
}

// Quote returns the still-forming session's OHLC and last price.
//
// FYERS has one quote endpoint serving both, so LTP is Quote with the rest
// discarded; there is no cheaper call to make.
func (c *Client) Quote(ctx context.Context, keys []domain.InstrumentKey) (map[domain.InstrumentKey]domain.Quote, error) {
	out := make(map[domain.InstrumentKey]domain.Quote, len(keys))
	byKey, order := resolveKeys(keys)

	now := time.Now()
	for _, chunk := range chunks(order, quoteChunkSize) {
		var resp struct {
			envelope
			D []quoteEntry `json:"d"`
		}
		q := url.Values{}
		q.Set("symbols", strings.Join(chunk, ","))
		err := c.doJSON(ctx, request{
			method: http.MethodGet,
			base:   c.dataURL,
			path:   "/quotes",
			query:  q,
			out:    &resp,
			retry:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("fyers: fetching quotes for %d instruments: %w", len(chunk), err)
		}
		for _, entry := range resp.D {
			// The entry is keyed by the symbol that was asked for. An
			// entry that FYERS itself marks failed, or that names a
			// symbol not asked for, is dropped: a quote that cannot be
			// attributed is worse than a missing one.
			if !strings.EqualFold(entry.S, responseOK) {
				continue
			}
			key, ok := byKey[entry.N]
			if !ok {
				continue
			}
			quote := domain.Quote{
				Key:  key,
				At:   now,
				Last: paise(entry.V.LP),
				Open: paise(entry.V.OpenPrice),
				High: paise(entry.V.HighPrice),
				Low:  paise(entry.V.LowPrice),
				// prev_close_price is the previous session's close,
				// which is what domain.Quote.Close means. Conflating it
				// with the last price makes every gap calculation
				// silently zero.
				Close: paise(entry.V.PrevClosePrice),
				Bid:   paise(entry.V.Bid),
				Ask:   paise(entry.V.Ask),
			}
			if secs, err := entry.V.TT.Int64(); err == nil && secs > 0 {
				quote.At = time.Unix(secs, 0)
			}
			out[key] = quote
		}
	}
	return out, nil
}

// resolveKeys maps each requested instrument to its FYERS symbol, returning
// the reverse index the responses are decoded through and the symbols in
// request order.
func resolveKeys(keys []domain.InstrumentKey) (map[string]domain.InstrumentKey, []string) {
	byKey := make(map[string]domain.InstrumentKey, len(keys))
	order := make([]string, 0, len(keys))
	for _, k := range keys {
		symbol := symbolFor(k)
		if _, seen := byKey[symbol]; seen {
			continue
		}
		byKey[symbol] = k
		order = append(order, symbol)
	}
	return byKey, order
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

// Candles returns bars over [from, to], oldest first.
//
// FYERS serves a bounded span per request — 100 days for intraday resolutions,
// 366 for daily — so a long backfill is fetched as a series of chunks and
// concatenated. The caller asks for ten years and does not have to know that.
//
// The range is sent as epoch seconds rather than dates, so an intraday caller
// can ask for exactly the completed bars it wants: the reference's own advice
// is to end the range one bar before now, because a bar that includes the
// current minute is returned partial.
func (c *Client) Candles(ctx context.Context, key domain.InstrumentKey, tf domain.Timeframe, from, to time.Time) ([]domain.Candle, error) {
	resolution, err := toResolution(tf)
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("fyers: candle range ends (%s) before it starts (%s)",
			to.Format(time.DateOnly), from.Format(time.DateOnly))
	}
	symbol := symbolFor(key)

	span := time.Duration(chunkDays(tf)) * 24 * time.Hour
	var out []domain.Candle
	for start := from; !start.After(to); start = start.Add(span) {
		end := start.Add(span - time.Second)
		if end.After(to) {
			end = to
		}
		q := url.Values{}
		q.Set("symbol", symbol)
		q.Set("resolution", resolution)
		q.Set("date_format", "0")
		q.Set("range_from", strconv.FormatInt(start.Unix(), 10))
		q.Set("range_to", strconv.FormatInt(end.Unix(), 10))
		q.Set("cont_flag", "")

		chunk, err := c.candles(ctx, q, key, tf)
		if err != nil {
			return nil, fmt.Errorf("fyers: fetching %s candles for %s from %s to %s: %w",
				tf, key, start.Format(time.DateOnly), end.Format(time.DateOnly), err)
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// candles performs one history request and converts the positional arrays
// into candles.
//
// FYERS returns bars oldest first as [epoch, open, high, low, close, volume],
// which is the order a backtest replays and every indicator assumes, so no
// reversal is needed — unlike Upstox, which returns newest first. Timestamps
// are epoch seconds and are rendered in IST, the exchange's zone, so a bar
// keyed by date in the store lands on the right session.
//
// A malformed row is skipped rather than failing the batch: one unparseable
// bar in a five-year backfill is a gap, and refusing the whole range over it
// means no history at all.
func (c *Client) candles(ctx context.Context, q url.Values, key domain.InstrumentKey, tf domain.Timeframe) ([]domain.Candle, error) {
	var resp struct {
		envelope
		Candles [][]any `json:"candles"`
	}
	err := c.doJSON(ctx, request{
		method:  http.MethodGet,
		base:    c.dataURL,
		path:    "/history",
		query:   q,
		out:     &resp,
		limiter: c.historical,
		retry:   true,
	})
	if err != nil {
		return nil, err
	}

	out := make([]domain.Candle, 0, len(resp.Candles))
	for _, row := range resp.Candles {
		if len(row) < 6 {
			continue
		}
		epoch, ok := row[0].(float64)
		if !ok || epoch <= 0 {
			continue
		}
		out = append(out, domain.Candle{
			Key:       key,
			Timeframe: tf,
			Start:     time.Unix(int64(epoch), 0).In(ist),
			Open:      paise(number(row, 1)),
			High:      paise(number(row, 2)),
			Low:       paise(number(row, 3)),
			Close:     paise(number(row, 4)),
			Volume:    int64(number(row, 5)),
			// Open interest is a seventh column only when oi_flag is
			// set, which this adapter does not do; the field is left
			// zero rather than read from a column that is not there.
		})
	}
	return out, nil
}

// number reads a positional field as a float, returning 0 for an absent or
// non-numeric one.
func number(row []any, i int) float64 {
	if i >= len(row) {
		return 0
	}
	v, _ := row[i].(float64)
	return v
}

// masterFile returns the symbol master file name for an exchange.
//
// FYERS splits the master by exchange and segment. The domain's exchange
// names are the ones the Kite adapter established — NFO for NSE derivatives,
// BFO for BSE's, CDS for currency — and each maps to one file. An empty
// exchange means NSE cash, which is the file every project uses.
func masterFile(exchange string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(exchange)) {
	case "", "NSE":
		return "NSE_CM_sym_master.json", nil
	case "BSE":
		return "BSE_CM_sym_master.json", nil
	case "NFO":
		return "NSE_FO_sym_master.json", nil
	case "BFO":
		return "BSE_FO_sym_master.json", nil
	case "CDS":
		return "NSE_CD_sym_master.json", nil
	case "MCX":
		return "MCX_COM_sym_master.json", nil
	default:
		return "", fmt.Errorf("fyers: no symbol master for exchange %q", exchange)
	}
}

// flexNumber is a numeric field that the master may spell as a number, as a
// string of digits, as an empty string, or as null.
//
// The master is not consistent about quoting: minLotSize arrives as a number
// and expiryDate as a string of digits, and inapplicable fields are an empty
// string. json.Number rejects the empty string, and one such row would fail
// the decode of the whole file — so this type accepts every spelling and
// leaves the empty ones empty.
type flexNumber string

// UnmarshalJSON accepts a number, a string, or null.
func (n *flexNumber) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*n = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return err
		}
		*n = flexNumber(strings.TrimSpace(unquoted))
		return nil
	}
	*n = flexNumber(s)
	return nil
}

// masterRow is one entry of the symbol master JSON, keyed by symTicker.
type masterRow struct {
	FyToken     string     `json:"fyToken"`
	ISIN        string     `json:"isin"`
	ExSymbol    string     `json:"exSymbol"`
	SymDetails  string     `json:"symDetails"`
	SymTicker   string     `json:"symTicker"`
	Segment     flexNumber `json:"segment"`
	ExSeries    string     `json:"exSeries"`
	OptType     string     `json:"optType"`
	ExInstType  flexNumber `json:"exInstType"`
	MinLotSize  flexNumber `json:"minLotSize"`
	TickSize    flexNumber `json:"tickSize"`
	ExpiryDate  flexNumber `json:"expiryDate"`
	StrikePrice flexNumber `json:"strikePrice"`
	TradeStatus flexNumber `json:"tradeStatus"`
}

// Instruments returns the symbol master for an exchange.
//
// The master is a JSON file on the public host and needs no token, so it is
// fetched directly rather than through doJSON. It is measured in megabytes, so
// it belongs in a daily sync and not in a request path.
func (c *Client) Instruments(ctx context.Context, exchange string) ([]domain.Instrument, error) {
	file, err := masterFile(exchange)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.masterURL+"/"+file, nil)
	if err != nil {
		return nil, fmt.Errorf("fyers: building symbol-master request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fyers: downloading the symbol master: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fyers: downloading the symbol master: HTTP %d", resp.StatusCode)
	}
	return parseSymbolMaster(resp.Body)
}

// parseSymbolMaster reads the symbol master JSON.
//
// A malformed row is skipped rather than failing the batch. The master carries
// tens of thousands of rows and one bad entry must not cost a sync its whole
// universe. The whole file is decoded rather than streamed: it is a single
// object keyed by symbol, and the decoder must read to the closing brace to
// know it is well-formed either way.
func parseSymbolMaster(r io.Reader) ([]domain.Instrument, error) {
	var rows map[string]masterRow
	dec := json.NewDecoder(r)
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("fyers: decoding the symbol master: %w", err)
	}

	out := make([]domain.Instrument, 0, len(rows))
	for ticker, row := range rows {
		symbol := row.SymTicker
		if symbol == "" {
			symbol = ticker
		}
		if symbol == "" {
			continue
		}
		segment := intOr(row.Segment, 0)
		instType := intOr(row.ExInstType, -1)

		in := domain.Instrument{
			Key:        keyFor(symbol, segment),
			Name:       row.SymDetails,
			ISIN:       row.ISIN,
			Segment:    segmentOf(instType),
			LotSize:    intOr(row.MinLotSize, 1),
			TickSize:   paise(floatOr(row.TickSize, 0)),
			Strike:     paise(floatOr(row.StrikePrice, 0)),
			OptionType: optionTypeOf(row.OptType),
			// tradeStatus is 1 for active and 0 for suspended. A row
			// with no flag is read as active, since the master lists
			// what is tradable today.
			Active: intOr(row.TradeStatus, 1) != 0,
		}
		if in.LotSize < 1 {
			// The domain uses 1 for cash equity so sizing can multiply
			// by it unconditionally.
			in.LotSize = 1
		}
		if expiry := parseExpiry(row.ExpiryDate); expiry != nil {
			in.Expiry = expiry
		}
		out = append(out, in)
	}
	return out, nil
}

// segmentOf classifies an exchange instrument type into the domain's
// vocabulary. The codes are FYERS's appendix table; anything unlisted is
// equity, which is what the cash-market codes for preference shares,
// debentures and ETFs amount to for sizing purposes.
func segmentOf(instType int) string {
	switch instType {
	case 10:
		return "index"
	case 11, 12, 13, 16, 17, 18, 25, 30, 33, 34, 35:
		return "futures"
	case 14, 15, 19, 31, 32, 36, 37:
		return "options"
	default:
		return "equity"
	}
}

// optionTypeOf normalises the master's option type to the domain's marker.
// "XX" is FYERS's spelling of "not an option".
func optionTypeOf(optType string) string {
	switch strings.ToUpper(strings.TrimSpace(optType)) {
	case "CE":
		return "ce"
	case "PE":
		return "pe"
	default:
		return ""
	}
}

// parseExpiry reads the master's expiry, which is epoch seconds. An absent or
// zero value returns nil, which is what a cash instrument carries.
func parseExpiry(n flexNumber) *time.Time {
	secs := int64(floatOr(n, 0))
	if secs <= 0 {
		return nil
	}
	t := time.Unix(secs, 0).In(ist)
	return &t
}

func intOr(n flexNumber, fallback int) int {
	if n == "" {
		return fallback
	}
	if v, err := strconv.ParseInt(string(n), 10, 64); err == nil {
		return int(v)
	}
	if v, err := strconv.ParseFloat(string(n), 64); err == nil {
		return int(v)
	}
	return fallback
}

func floatOr(n flexNumber, fallback float64) float64 {
	if n == "" {
		return fallback
	}
	if v, err := strconv.ParseFloat(string(n), 64); err == nil {
		return v
	}
	return fallback
}
