package upstox

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

func TestLTPOmitsInstrumentsUpstoxDoesNotKnow(t *testing.T) {
	c, _ := newRouted(t, routes{
		// Only two of the three requested instruments come back, which is
		// what a delisted symbol looks like.
		"/market-quote/ltp": `{"status":"success","data":{
			"NSE_EQ:RELIANCE":{"last_price":1057.60,"instrument_token":"NSE_EQ|RELIANCE"},
			"NSE_EQ:TCS":{"last_price":3500.05,"instrument_token":"NSE_EQ|TCS"}}}`,
	})

	keys := []domain.InstrumentKey{
		{Exchange: "NSE", Symbol: "RELIANCE"},
		{Exchange: "NSE", Symbol: "TCS"},
		{Exchange: "NSE", Symbol: "DELISTED"},
	}
	prices, err := c.LTP(context.Background(), keys)
	if err != nil {
		t.Fatalf("one unknown symbol must not fail a whole scan, got %v", err)
	}
	if len(prices) != 2 {
		t.Fatalf("expected the two known instruments, got %d", len(prices))
	}
	if prices[keys[0]] != money.MustParse("1057.60") {
		t.Errorf("prices must convert to paise, got %s", prices[keys[0]])
	}
	if _, ok := prices[keys[2]]; ok {
		t.Error("an instrument the venue did not answer for must be absent, not zero-priced")
	}
}

func TestQuoteReadsThePreviousCloseAndTheDepthTop(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/market-quote/quotes": `{"status":"success","data":{
			"NSE_EQ:RELIANCE":{
				"instrument_token":"NSE_EQ|RELIANCE","last_price":1057.60,
				"ohlc":{"open":1040.00,"high":1060.00,"low":1035.00,"close":1038.00},
				"depth":{"buy":[{"price":1057.55,"quantity":100},{"price":1057.50,"quantity":50}],
				         "sell":[{"price":1057.65,"quantity":80}]}}}}`,
	})

	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	quotes, err := c.Quote(context.Background(), []domain.InstrumentKey{key})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := quotes[key]
	if q.Close != money.MustParse("1038.00") {
		t.Errorf("OHLC.close is the previous session's close; conflating it with the last price makes every gap calculation zero. got %s", q.Close)
	}
	if q.Last != money.MustParse("1057.60") {
		t.Errorf("last price is wrong, got %s", q.Last)
	}
	if q.Bid != money.MustParse("1057.55") || q.Ask != money.MustParse("1057.65") {
		t.Errorf("bid and ask come from the top of each depth side, got bid=%s ask=%s", q.Bid, q.Ask)
	}
}

func TestQuoteDropsAnEntryThatNamesNoInstrument(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/market-quote/quotes": `{"status":"success","data":{
			"NSE_EQ:MYSTERY":{"last_price":99.00,"ohlc":{"open":1,"high":1,"low":1,"close":1}}}}`,
	})
	quotes, err := c.Quote(context.Background(), []domain.InstrumentKey{{Exchange: "NSE", Symbol: "RELIANCE"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(quotes) != 0 {
		t.Error("a quote that cannot be attributed is worse than a missing one: a screener would size a spike against another company's history")
	}
}

// candleServer answers every historical request with one bar dated at the end
// of the range, and records the paths it was asked for.
func candleServer(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.EscapedPath())
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[
			["2025-04-17T15:25:00+05:30",100.5,101.0,100.0,100.75,1500,0],
			["2025-04-17T15:20:00+05:30",100.0,100.6,99.9,100.5,1200,0]]}}`))
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)
	return c, &paths
}

func TestCandlesReturnsOldestFirst(t *testing.T) {
	c, _ := candleServer(t)
	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	from := time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC)
	candles, err := c.Candles(context.Background(), key, domain.M5, from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candles) != 2 {
		t.Fatalf("expected both bars, got %d", len(candles))
	}
	if !candles[0].Start.Before(candles[1].Start) {
		t.Error("Upstox returns bars newest first; a backtest replays oldest first and every indicator assumes it")
	}
	if candles[0].Open != money.MustParse("100.00") || candles[1].Close != money.MustParse("100.75") {
		t.Errorf("OHLC must convert to paise, got %+v", candles)
	}
	if candles[0].Volume != 1200 {
		t.Errorf("volume must be carried, got %d", candles[0].Volume)
	}
}

func TestCandlesChunksContiguouslyWithNoGapOrOverlap(t *testing.T) {
	c, paths := candleServer(t)
	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 100)
	if _, err := c.Candles(context.Background(), key, domain.M1, from, to); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	span := chunkDays("minutes", 1)
	windows := make([][2]string, 0, len(*paths))
	for _, p := range *paths {
		parts := strings.Split(strings.Trim(p, "/"), "/")
		// .../{unit}/{interval}/{to}/{from}
		windows = append(windows, [2]string{parts[len(parts)-1], parts[len(parts)-2]})
	}
	if len(windows) < 2 {
		t.Fatalf("a 100-day minute range needs several chunks of %d days, got %d requests", span, len(windows))
	}
	for i, w := range windows {
		wantFrom := from.AddDate(0, 0, i*span).Format(time.DateOnly)
		if w[0] != wantFrom {
			t.Errorf("chunk %d starts at %s, want %s — a gap or an overlap here silently loses or duplicates bars", i, w[0], wantFrom)
		}
	}
	last := windows[len(windows)-1]
	if last[1] != to.Format(time.DateOnly) {
		t.Errorf("the final chunk must end exactly at the requested end, got %s want %s", last[1], to.Format(time.DateOnly))
	}
}

func TestCandlesMakesOneRequestForAShortRange(t *testing.T) {
	c, paths := candleServer(t)
	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	from := time.Date(2025, 4, 14, 0, 0, 0, 0, time.UTC)
	if _, err := c.Candles(context.Background(), key, domain.M5, from, from.AddDate(0, 0, 2)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*paths) != 1 {
		t.Errorf("a range shorter than one chunk must cost one request, got %d", len(*paths))
	}
}

func TestCandlesRejectsAnInvertedRange(t *testing.T) {
	c, paths := candleServer(t)
	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	to := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, 10)
	if _, err := c.Candles(context.Background(), key, domain.M5, from, to); err == nil {
		t.Error("an inverted range must be refused; Upstox answers it with an empty series, which reads as 'no trades'")
	}
	if len(*paths) != 0 {
		t.Error("an inverted range must be caught before any request is made")
	}
}

func TestCandlesSendsTheLaterDateFirst(t *testing.T) {
	c, paths := candleServer(t)
	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}
	from := time.Date(2025, 4, 14, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 2)
	if _, err := c.Candles(context.Background(), key, domain.M5, from, to); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/historical-candle/NSE_EQ%7CRELIANCE/minutes/5/" + to.Format(time.DateOnly) + "/" + from.Format(time.DateOnly)
	if (*paths)[0] != want {
		t.Errorf("the path takes the later date first; reversing it returns an empty series rather than an error.\n got %s\nwant %s", (*paths)[0], want)
	}
}

func TestCandlesSkipsAMalformedRowWithoutFailingTheBatch(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/historical-candle/NSE_EQ|RELIANCE/days/1/2025-04-17/2025-04-16": "",
	})
	_ = c
	// Served through a dedicated server: the routed helper keys on a decoded
	// path, and this one carries an escaped separator.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[
			["2025-04-17T00:00:00+05:30",100.0,101.0,99.0,100.5,1000,0],
			["not-a-timestamp",1,2,3,4,5,6],
			[1,2],
			["2025-04-16T00:00:00+05:30",99.0,100.0,98.0,99.5,900,0]]}}`))
	}))
	defer srv.Close()
	client, _ := newTestClient(t, srv)

	from := time.Date(2025, 4, 16, 0, 0, 0, 0, time.UTC)
	candles, err := client.Candles(context.Background(),
		domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}, domain.D1, from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("one unparseable bar in a five-year backfill is a gap, not a reason to return no history: %v", err)
	}
	if len(candles) != 2 {
		t.Errorf("expected the two good bars, got %d", len(candles))
	}
}

func TestExpiredHistoricalCandlesUsesTheV2RootAndKeepsTheExpiryInTheKey(t *testing.T) {
	var path, base string
	v2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		base, path = "v2", req.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[
			["2025-04-17T15:25:00+05:30",12.5,14.0,11.0,13.25,75000,120000]]}}`))
	}))
	defer v2.Close()
	v3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		base = "v3"
		w.WriteHeader(http.StatusNotFound)
	}))
	defer v3.Close()

	c, _ := newTestClient(t, v2)
	c.baseV3 = v3.URL

	candles, err := c.ExpiredHistoricalCandles(context.Background(), "NSE_FO|47983|17-04-2025",
		domain.M5, time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC), time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "v2" {
		t.Error("the expired-candle endpoint is served from the v2 root while its sibling listings are v3; this was verified, not assumed")
	}
	if !strings.Contains(path, "47983%7C17-04-2025") {
		t.Errorf("the expiry is the key's third field and must survive escaping, got %s", path)
	}
	if len(candles) != 1 || candles[0].OpenInterest != 120000 {
		t.Errorf("open interest matters for an option series and must be carried, got %+v", candles)
	}
}

func TestExpiredExpiriesAndContracts(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/expired-instruments/expiries": `{"status":"success","data":["2025-04-17","2025-04-24"]}`,
		"/expired-instruments/option/contract": `{"status":"success","data":[
			{"instrument_key":"NSE_FO|47983|17-04-2025","trading_symbol":"NIFTY24500CE",
			 "instrument_type":"CE","strike_price":24500,"lot_size":75,"expiry":"2025-04-17"}]}`,
	})

	expiries, err := c.ExpiredExpiries(context.Background(), "NSE_INDEX|Nifty 50")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(expiries) != 2 || expiries[0] != "2025-04-17" {
		t.Errorf("expiries must come back as dates, got %v", expiries)
	}

	contracts, err := c.ExpiredOptionContracts(context.Background(), "NSE_INDEX|Nifty 50", "2025-04-17")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(contracts) != 1 || contracts[0].LotSize != 75 {
		t.Errorf("a contract's lot size is what sizing needs, got %+v", contracts)
	}
	if contracts[0].InstrumentKey != "NSE_FO|47983|17-04-2025" {
		t.Errorf("the key must carry the expiry so the candle endpoint can be called with it, got %q", contracts[0].InstrumentKey)
	}
}

// instrumentCSV is a fragment of the master, including one row the parser must
// skip and one whose lot size the domain must correct.
const instrumentCSV = `instrument_key,tradingsymbol,name,isin,lot_size,tick_size,instrument_type,expiry,strike
NSE_EQ|INE002A01018,RELIANCE,Reliance Industries,INE002A01018,0,0.05,EQUITY,,
NSE_EQ|INE467B01029,TCS,Tata Consultancy Services,INE467B01029,1,0.05,EQ,,
,BROKEN,No instrument key at all,,1,0.05,EQUITY,,
NSE_FO|54321,NIFTY24500CE,Nifty 50,,75,0.05,CE,2025-04-17,24500
NSE_INDEX|Nifty 50,Nifty 50,Nifty 50,,0,0,INDEX,,
`

func gzipped(s string) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return buf.Bytes()
}

func TestInstrumentsParsesTheGzippedMaster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(gzipped(instrumentCSV))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	c.opts.InstrumentsURL = srv.URL + "/NSE.csv.gz"

	instruments, err := c.Instruments(context.Background(), "NSE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instruments) != 4 {
		t.Fatalf("the row with no instrument key must be skipped and the other four kept, got %d", len(instruments))
	}

	byKey := map[string]domain.Instrument{}
	for _, in := range instruments {
		byKey[in.Key.Symbol] = in
	}

	reliance := byKey["RELIANCE"]
	if reliance.LotSize != 1 {
		t.Errorf("the master reports 0 for cash equity; the domain uses 1 so sizing can multiply unconditionally. got %d", reliance.LotSize)
	}
	if reliance.TickSize != money.MustParse("0.05") {
		t.Errorf("the tick size must convert to paise, got %s", reliance.TickSize)
	}
	if reliance.ISIN != "INE002A01018" || reliance.Segment != "equity" {
		t.Errorf("equity reference data is wrong: %+v", reliance)
	}
	if byKey["TCS"].Segment != "equity" {
		t.Error(`the CSV master spells equity "EQUITY" and the JSON one "EQ"; both must classify as equity or switching masters reclassifies a whole universe`)
	}

	option := byKey["NIFTY24500CE"]
	if option.Segment != "options" || option.OptionType != "ce" || option.LotSize != 75 {
		t.Errorf("option reference data is wrong: %+v", option)
	}
	if option.Expiry == nil || option.Expiry.Format(time.DateOnly) != "2025-04-17" {
		t.Errorf("an option's expiry must be read, got %v", option.Expiry)
	}
	if option.Strike != money.MustParse("24500.00") {
		t.Errorf("the strike must convert to paise, got %s", option.Strike)
	}

	if byKey["Nifty 50"].Segment != "index" {
		t.Errorf("an NSE_INDEX key is an index, not equity; got %q", byKey["Nifty 50"].Segment)
	}
}

func TestInstrumentsReadsAnUncompressedBodyWhenTheServerDecompressed(t *testing.T) {
	// net/http transparently decompresses a Content-Encoding: gzip response
	// and strips the header, so a .gz URL can still yield a plain body by the
	// time the adapter sees it. Sniffing the magic number is what makes both
	// cases work; trusting the URL suffix or the headers does not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(instrumentCSV))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	c.opts.InstrumentsURL = srv.URL + "/NSE.csv.gz"
	instruments, err := c.Instruments(context.Background(), "NSE")
	if err != nil {
		t.Fatalf("a transparently decompressed body must still parse, got %v", err)
	}
	if len(instruments) != 4 {
		t.Errorf("expected 4 instruments, got %d", len(instruments))
	}
}
