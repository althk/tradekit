package universe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/money"
)

const nifty500CSV = `Company Name,Industry,Symbol,Series,ISIN Code
Reliance Industries Ltd.,Oil Gas & Consumable Fuels,RELIANCE,EQ,INE002A01018
Tata Consultancy Services Ltd.,Information Technology,TCS,EQ,INE467B01029
Infosys Ltd.,Information Technology,INFY,EQ,INE009A01021
,,,,
`

// A watchlist export: lowercase headers, a ".NS" suffix, no sector column.
const watchlistCSV = `symbol,name
RELIANCE.NS,Reliance Industries
tcs.ns,Tata Consultancy Services
`

const bhavcopyCSV = ` SYMBOL, SERIES, DATE1, PREV_CLOSE, OPEN_PRICE, HIGH_PRICE, LOW_PRICE, LAST_PRICE, CLOSE_PRICE, AVG_PRICE, TTL_TRD_QNTY, TURNOVER_LACS, NO_OF_TRADES, DELIV_QTY, DELIV_PER
RELIANCE,EQ,17-Apr-2025,1038.00,1040.00,1060.00,1035.00,1057.00,1057.60,1048.00,5000000,52400.00,120000,2500000,50.00
TCS,BE,17-Apr-2025,3480.00,3500.00,3520.00,3490.00,3510.00,3505.05,3505.00,800000,28040.00,40000,-,-
GOVSEC01,GS,17-Apr-2025,100.00,100.00,100.10,99.90,100.00,100.00,100.00,1000,1.00,10,-,-
BROKEN,EQ,17-Apr-2025,-,-,-,-,-,-,-,-,-,-,-
`

func TestParseIndexCSVReadsThePublishedList(t *testing.T) {
	instruments, err := ParseIndexCSV([]byte(nifty500CSV))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instruments) != 3 {
		t.Fatalf("the trailing blank line must be skipped, not fail the batch; got %d instruments", len(instruments))
	}
	first := instruments[0]
	if first.Key.Symbol != "RELIANCE" || first.Key.Exchange != "NSE" {
		t.Errorf("instrument key wrong: %+v", first.Key)
	}
	if first.Name != "Reliance Industries Ltd." {
		t.Errorf("the Company Name column must be read, got %q", first.Name)
	}
	if first.ISIN != "INE002A01018" {
		t.Errorf("the ISIN column must be read -- Upstox keys cash equity by it -- got %q", first.ISIN)
	}
	if first.Segment != "equity" {
		t.Errorf("everything in an equity index is equity; the Industry column is a sector and the domain has no field for it. got %q", first.Segment)
	}
	if first.LotSize != 1 || first.TickSize <= 0 {
		t.Errorf("an index list carries no lot or tick, so sane defaults are needed: %+v", first)
	}
}

func TestParseIndexCSVReadsAWatchlistExportToo(t *testing.T) {
	// One parser for both formats is what let neev's universe loader survive
	// several changes to the published file.
	instruments, err := ParseIndexCSV([]byte(watchlistCSV))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instruments) != 2 {
		t.Fatalf("expected two instruments, got %d", len(instruments))
	}
	for i, want := range []string{"RELIANCE", "TCS"} {
		if instruments[i].Key.Symbol != want {
			t.Errorf("the .NS trading suffix must be stripped and the symbol uppercased; got %q want %q",
				instruments[i].Key.Symbol, want)
		}
	}
}

func TestParseIndexCSVRejectsAFileWithNoSymbolColumn(t *testing.T) {
	if _, err := ParseIndexCSV([]byte("name,industry\nReliance,Oil\n")); err == nil {
		t.Error("a file with no symbol column names no instruments and must be refused, not read as an empty universe")
	}
}

func TestParseIndexCSVStripsAByteOrderMark(t *testing.T) {
	body := append([]byte(byteOrderMark), []byte(nifty500CSV)...)
	instruments, err := ParseIndexCSV(body)
	if err != nil {
		t.Fatalf("a Windows-authored CSV begins with a BOM and must still parse: %v", err)
	}
	if len(instruments) != 3 {
		t.Errorf("the BOM makes the first header unmatchable unless stripped; got %d instruments", len(instruments))
	}
}

func TestParseBhavcopyKeepsEquitySeriesAndSkipsBadRows(t *testing.T) {
	day := time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC)
	rows, err := ParseBhavcopy([]byte(bhavcopyCSV), day)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("EQ and BE are the tradable series; GS is not, and the all-dashes row is unparseable. got %d rows: %+v", len(rows), rows)
	}

	reliance := rows[0]
	if reliance.Key.Symbol != "RELIANCE" || reliance.Series != "EQ" {
		t.Errorf("row identity wrong: %+v", reliance)
	}
	if reliance.Close != money.MustParse("1057.60") {
		t.Errorf("prices must convert to paise, got %s", reliance.Close)
	}
	if reliance.PrevClose != money.MustParse("1038.00") {
		t.Errorf("prev close is what a gap is measured against, got %s", reliance.PrevClose)
	}
	if reliance.Volume != 5000000 {
		t.Errorf("volume wrong: %d", reliance.Volume)
	}
	if reliance.DeliverableQty != 2500000 || reliance.DeliverablePct != 50.0 {
		t.Errorf("the delivery figures are why a screener reads the bhavcopy at all rather than a candle feed; got %d, %v",
			reliance.DeliverableQty, reliance.DeliverablePct)
	}

	tcs := rows[1]
	if tcs.Series != "BE" {
		t.Errorf("BE is tradable equity and must be kept, got series %q", tcs.Series)
	}
	if tcs.DeliverableQty != 0 {
		t.Errorf(`the file writes "-" for an absent delivery figure; it must read as zero, not fail the row. got %d`, tcs.DeliverableQty)
	}
}

func TestCandlesConvertsBhavcopyRows(t *testing.T) {
	day := time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC)
	rows, err := ParseBhavcopy([]byte(bhavcopyCSV), day)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	candles := Candles(rows)
	if len(candles) != len(rows) {
		t.Fatalf("expected one candle per row, got %d from %d", len(candles), len(rows))
	}
	if candles[0].Timeframe != "1d" {
		t.Errorf("a bhavcopy row is a daily bar, got timeframe %q", candles[0].Timeframe)
	}
	if !candles[0].Start.Equal(day) {
		t.Errorf("the bar is stamped with the file's day, got %v", candles[0].Start)
	}
}

// nseServer serves the given paths and counts requests, including warm-ups.
func nseServer(t *testing.T, bodies map[string]string) (*Client, *int32, *int32) {
	t.Helper()
	var requests, warmUps int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			atomic.AddInt32(&warmUps, 1)
			_, _ = w.Write([]byte("<html></html>"))
			return
		}
		atomic.AddInt32(&requests, 1)
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return &Client{
		CacheDir: t.TempDir(),
		BaseURL:  srv.URL,
		HomeURL:  srv.URL + "/",
		HTTP:     srv.Client(),
	}, &requests, &warmUps
}

func TestAConstituentListIsCachedAndNotRefetched(t *testing.T) {
	c, requests, _ := nseServer(t, map[string]string{
		"/content/indices/ind_nifty500list.csv": nifty500CSV,
	})

	first, err := c.IndexConstituents(context.Background(), "ind_nifty500list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("expected 3 constituents, got %d", len(first))
	}

	second, err := c.IndexConstituents(context.Background(), "ind_nifty500list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(second) != len(first) {
		t.Errorf("the cached read must return the same list, got %d", len(second))
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("an index list changes monthly; re-downloading it every run is what left five projects with a stale copy checked in. got %d requests", got)
	}
}

func TestACachedFileIsUsedWithNoNetworkCallAtAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ind_nifty500list.csv"), []byte(nifty500CSV), 0o644); err != nil {
		t.Fatal(err)
	}
	// No HTTP client and no server: any request would panic or fail.
	c := &Client{CacheDir: dir, BaseURL: "http://127.0.0.1:1", HomeURL: "http://127.0.0.1:1/"}

	got, err := c.IndexConstituents(context.Background(), "ind_nifty500list")
	if err != nil {
		t.Fatalf("a backtest must run offline against the cache: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 constituents from the cache, got %d", len(got))
	}
}

func TestNSEIsWarmedUpBeforeTheFirstDownload(t *testing.T) {
	c, _, warmUps := nseServer(t, map[string]string{
		"/content/indices/ind_nifty500list.csv": nifty500CSV,
	})
	if _, err := c.IndexConstituents(context.Background(), "ind_nifty500list"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(warmUps) == 0 {
		t.Error("NSE rejects a bare request with 403; fetching the home page first for its cookies is what makes the download work at all")
	}
}

func TestA403IsRetriedAfterAFreshWarmUp(t *testing.T) {
	var attempts, warmUps int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			atomic.AddInt32(&warmUps, 1)
			return
		}
		// Refuse the first archive request, as an expired session does.
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(nifty500CSV))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HomeURL: srv.URL + "/", HTTP: srv.Client()}
	got, err := c.IndexConstituents(context.Background(), "ind_nifty500list")
	if err != nil {
		t.Fatalf("an expired session must be recovered from, not surfaced: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 constituents after the retry, got %d", len(got))
	}
	if atomic.LoadInt32(&warmUps) < 2 {
		t.Errorf("a 403 means the session expired and must trigger a fresh warm-up; saw %d", warmUps)
	}
}

func TestAnUnpublishedDayIsNotAnError(t *testing.T) {
	c, _, _ := nseServer(t, map[string]string{})
	_, err := c.Bhavcopy(context.Background(), time.Date(2025, 4, 18, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrNotPublished) {
		t.Errorf("a holiday, or today before publication, is the normal answer and must be distinguishable so a sync does not log it as a failure; got %v", err)
	}
}

func TestBhavcopyIsFetchedAndCached(t *testing.T) {
	c, requests, _ := nseServer(t, map[string]string{
		"/products/content/sec_bhavdata_full_17042025.csv": bhavcopyCSV,
	})
	day := time.Date(2025, 4, 17, 0, 0, 0, 0, time.UTC)

	rows, err := c.Bhavcopy(context.Background(), day)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 tradable rows, got %d", len(rows))
	}
	if _, err := c.Bhavcopy(context.Background(), day); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Errorf("a published bhavcopy never changes, so it must be fetched once; got %d requests", got)
	}
}
