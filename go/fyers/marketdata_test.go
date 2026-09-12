package fyers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

var (
	sbin = domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}
	idea = domain.InstrumentKey{Exchange: "NSE", Symbol: "IDEA"}
)

const quotesBody = `{"s":"ok","code":200,"d":[
	{"n":"NSE:SBIN-EQ","s":"ok","v":{"ch":1.7,"chp":0.4,"lp":426.9,"spread":0.05,"ask":426.9,"bid":426.85,
	 "open_price":430.5,"high_price":433.65,"low_price":423.6,"prev_close_price":425.2,"atp":428.07,
	 "volume":38977242,"short_name":"SBIN-EQ","exchange":"NSE","symbol":"NSE:SBIN-EQ","tt":"1622160000"}},
	{"n":"NSE:IDEA-EQ","s":"error","code":-300,"message":"Invalid symbol"},
	{"n":"NSE:NOTASKED-EQ","s":"ok","v":{"lp":1}}]}`

func TestLTPOmitsInstrumentsFYERSDoesNotKnow(t *testing.T) {
	c, _ := newRouted(t, routes{"/quotes": quotesBody})
	got, err := c.LTP(context.Background(), []domain.InstrumentKey{sbin, idea})
	if err != nil {
		t.Fatalf("one bad symbol must not fail the scan: %v", err)
	}
	if got[sbin] != money.MustParse("426.90") {
		t.Errorf("last price must be read from lp, got %s", got[sbin])
	}
	if _, present := got[idea]; present {
		t.Error("a symbol FYERS marks failed must be omitted, not reported as zero")
	}
	if len(got) != 1 {
		t.Errorf("an entry for a symbol that was not asked for cannot be attributed and must be dropped; got %v", got)
	}
}

func TestQuoteReadsThePreviousCloseAndTheTimestamp(t *testing.T) {
	c, _ := newRouted(t, routes{"/quotes": quotesBody})
	got, err := c.Quote(context.Background(), []domain.InstrumentKey{sbin})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := got[sbin]
	if q.Close != money.MustParse("425.20") {
		t.Errorf("Close is the previous session's close, not the last price; got %s", q.Close)
	}
	if q.Open != money.MustParse("430.50") || q.High != money.MustParse("433.65") || q.Low != money.MustParse("423.60") {
		t.Errorf("OHLC must be mapped, got %+v", q)
	}
	if q.Bid != money.MustParse("426.85") || q.Ask != money.MustParse("426.90") {
		t.Errorf("bid and ask must be mapped, got %s/%s", q.Bid, q.Ask)
	}
	if !q.At.Equal(time.Unix(1622160000, 0)) {
		t.Errorf("tt is the quote's epoch timestamp, even when sent as a string; got %v", q.At)
	}
}

func TestQuoteChunksAtTheDocumentedLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		symbols := strings.Split(req.URL.Query().Get("symbols"), ",")
		if len(symbols) > quoteChunkSize {
			t.Errorf("a request carried %d symbols; the endpoint refuses more than %d", len(symbols), quoteChunkSize)
		}
		_, _ = w.Write([]byte(`{"s":"ok","code":200,"d":[]}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	keys := make([]domain.InstrumentKey, 0, 120)
	for i := 0; i < 120; i++ {
		keys = append(keys, domain.InstrumentKey{Exchange: "NSE", Symbol: fmt.Sprintf("S%d", i)})
	}
	if _, err := c.Quote(context.Background(), keys); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("120 symbols is three requests of at most 50, got %d", calls)
	}
}

// historyServer answers the history endpoint with one bar per request at the
// requested range's start, and records the ranges asked for.
func historyServer(t *testing.T) (*Client, *[][2]int64) {
	t.Helper()
	var ranges [][2]int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/history" {
			t.Errorf("unexpected request to %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := req.URL.Query()
		if q.Get("date_format") != "0" {
			t.Errorf("the range is sent as epoch seconds so an intraday caller can end one bar before now; got date_format %q", q.Get("date_format"))
		}
		from, _ := strconv.ParseInt(q.Get("range_from"), 10, 64)
		to, _ := strconv.ParseInt(q.Get("range_to"), 10, 64)
		ranges = append(ranges, [2]int64{from, to})
		_, _ = fmt.Fprintf(w, `{"s":"ok","candles":[[%d,417.0,419.2,405.3,412.05,142964052]]}`, from)
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)
	return c, &ranges
}

func TestCandlesMakeOneRequestForAShortRange(t *testing.T) {
	c, ranges := historyServer(t)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, ist)
	to := time.Date(2024, 1, 31, 0, 0, 0, 0, ist)
	got, err := c.Candles(context.Background(), sbin, domain.M5, from, to)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*ranges) != 1 {
		t.Errorf("a month of 5m bars fits in one request, got %d", len(*ranges))
	}
	if len(got) != 1 || !got[0].Start.Equal(from) {
		t.Errorf("the bar's epoch must become its start, got %v", got)
	}
	if got[0].Start.Location().String() != ist.String() {
		t.Errorf("bars are stamped in IST so a date-keyed store lands them on the right session, got %v", got[0].Start.Location())
	}
	if got[0].Open != money.MustParse("417.00") || got[0].Volume != 142964052 {
		t.Errorf("OHLCV must be mapped, got %+v", got[0])
	}
}

func TestCandlesChunkContiguouslyWithNoGapOrOverlap(t *testing.T) {
	c, ranges := historyServer(t)
	from := time.Date(2023, 1, 1, 0, 0, 0, 0, ist)
	to := time.Date(2023, 12, 31, 0, 0, 0, 0, ist)
	if _, err := c.Candles(context.Background(), sbin, domain.M1, from, to); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*ranges) < 4 {
		t.Fatalf("a year of minute bars must be chunked under the 100-day ceiling, got %d requests", len(*ranges))
	}
	if (*ranges)[0][0] != from.Unix() {
		t.Errorf("the first chunk must start at from, got %d", (*ranges)[0][0])
	}
	for i := 1; i < len(*ranges); i++ {
		prevEnd, start := (*ranges)[i-1][1], (*ranges)[i][0]
		if start != prevEnd+1 {
			t.Errorf("chunk %d starts at %d but the previous ended at %d: a gap loses bars and an overlap duplicates them", i, start, prevEnd)
		}
		if span := (*ranges)[i][1] - (*ranges)[i][0]; span > 100*24*3600 {
			t.Errorf("chunk %d spans %d seconds, over the 100-day ceiling", i, span)
		}
	}
	last := (*ranges)[len(*ranges)-1]
	if last[1] != to.Unix() {
		t.Errorf("the last chunk must end at to, got %d want %d", last[1], to.Unix())
	}
}

func TestCandlesRejectAnInvertedRange(t *testing.T) {
	c, ranges := historyServer(t)
	_, err := c.Candles(context.Background(), sbin, domain.D1, time.Now(), time.Now().Add(-time.Hour))
	if err == nil {
		t.Error("an inverted range must be refused, not sent and answered with an empty series")
	}
	if len(*ranges) != 0 {
		t.Error("no request should be made for an inverted range")
	}
}

func TestCandlesSkipAMalformedRowWithoutFailingTheBatch(t *testing.T) {
	c, _ := newRouted(t, routes{"/history": `{"s":"ok","candles":[
		[1622160000,417.0,419.2,405.3,412.05,100],
		["bad",1,2,3,4,5],
		[1622160300,1,2],
		[1622160600,418.0,420.0,417.0,419.0,200]]}`})
	got, err := c.Candles(context.Background(), sbin, domain.M5, time.Unix(1622160000, 0), time.Unix(1622160600, 0))
	if err != nil {
		t.Fatalf("one bad bar is a gap, not a failed backfill: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected the two well-formed bars, got %d", len(got))
	}
}

const symbolMaster = `{
	"NSE:SBIN-EQ":{"fyToken":"10100000003045","isin":"INE062A01020","exSymbol":"SBIN","symDetails":"STATE BANK OF INDIA",
		"symTicker":"NSE:SBIN-EQ","exchange":10,"segment":10,"exSeries":"EQ","optType":"XX","exInstType":0,
		"minLotSize":1,"tickSize":0.05,"expiryDate":"","strikePrice":-1,"tradeStatus":1},
	"NSE:NIFTY24JAN22000CE":{"fyToken":"101124012522000","exSymbol":"NIFTY","symDetails":"NIFTY 24 JAN 22000 CE",
		"symTicker":"NSE:NIFTY24JAN22000CE","exchange":10,"segment":11,"optType":"CE","exInstType":14,
		"minLotSize":50,"tickSize":"0.05","expiryDate":"1706178600","strikePrice":22000,"tradeStatus":1,"qtyFreeze":""},
	"NSE:SUSPENDED-EQ":{"symTicker":"NSE:SUSPENDED-EQ","segment":10,"exInstType":0,"minLotSize":0,"tickSize":0.05,"tradeStatus":0},
	"NSE:NIFTY50-INDEX":{"symTicker":"NSE:NIFTY50-INDEX","segment":10,"exInstType":10,"minLotSize":1,"tickSize":0}
}`

func TestParseSymbolMasterToleratesMixedTypesAndEmptyStrings(t *testing.T) {
	got, err := parseSymbolMaster(strings.NewReader(symbolMaster))
	if err != nil {
		t.Fatalf("the master quotes numbers inconsistently and uses \"\" for inapplicable fields; both must decode: %v", err)
	}
	byKey := map[domain.InstrumentKey]domain.Instrument{}
	for _, in := range got {
		byKey[in.Key] = in
	}

	eq := byKey[domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}]
	if eq.ISIN != "INE062A01020" || eq.Segment != "equity" || eq.LotSize != 1 || eq.TickSize != 5 || !eq.Active {
		t.Errorf("cash equity must be mapped with the EQ series stripped, got %+v", eq)
	}
	if eq.Expiry != nil {
		t.Error("an empty expiryDate is no expiry, not the epoch")
	}

	opt := byKey[domain.InstrumentKey{Exchange: "NFO", Symbol: "NIFTY24JAN22000CE"}]
	if opt.Segment != "options" || opt.OptionType != "ce" || opt.LotSize != 50 || opt.Strike != money.MustParse("22000.00") {
		t.Errorf("an option must carry its type, lot and strike, got %+v", opt)
	}
	if opt.Expiry == nil || !opt.Expiry.Equal(time.Unix(1706178600, 0)) {
		t.Errorf("expiryDate is epoch seconds as a string, got %v", opt.Expiry)
	}
	if opt.TickSize != 5 {
		t.Errorf("a quoted tickSize must still be read, got %d", opt.TickSize)
	}

	sus := byKey[domain.InstrumentKey{Exchange: "NSE", Symbol: "SUSPENDED"}]
	if sus.Active {
		t.Error("tradeStatus 0 is suspended")
	}
	if sus.LotSize != 1 {
		t.Errorf("a zero lot size becomes 1 so sizing can multiply unconditionally, got %d", sus.LotSize)
	}

	idx := byKey[domain.InstrumentKey{Exchange: "NSE", Symbol: "NIFTY50-INDEX"}]
	if idx.Segment != "index" {
		t.Errorf("exInstType 10 is an index, got %q", idx.Segment)
	}
}

func TestInstrumentsFetchesTheFileForTheExchange(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path = req.URL.Path
		if req.Header.Get("Authorization") != "" {
			t.Error("the symbol master is public and must not be sent the token")
		}
		_, _ = w.Write([]byte(symbolMaster))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	if _, err := c.Instruments(context.Background(), "NFO"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/NSE_FO_sym_master.json" {
		t.Errorf("NFO is FYERS's NSE_FO file, got %s", path)
	}
	if _, err := c.Instruments(context.Background(), "NASDAQ"); err == nil {
		t.Error("an exchange with no master must be refused, not fetched as a 404")
	}
}
