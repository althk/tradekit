package marketdata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func drain(t *testing.T, feed BarFeed) []domain.Candle {
	t.Helper()
	var out []domain.Candle
	for {
		c, ok, err := feed.Next()
		if err != nil {
			t.Fatalf("feed returned an error: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, c)
	}
}

// Compile-time proof that all three constructors produce the one interface. A
// source that drifted out of it would force every consumer to special-case it,
// which is the state this package exists to end.
var (
	_                                                                                                                 = FromSlice
	_ func(string, domain.InstrumentKey, domain.Timeframe) (BarFeed, error)                                           = FromCSV
	_ func(context.Context, *store.DB, domain.InstrumentKey, domain.Timeframe, time.Time, time.Time) (BarFeed, error) = FromStore
)

func TestSliceAndCSVFeedsAgreeOnTheSameBars(t *testing.T) {
	start := time.Date(2025, 4, 17, 0, 0, 0, 0, IST)
	want := []domain.Candle{{
		Key: reliance, Timeframe: domain.D1, Start: start,
		Open:   money.MustParse("100.00"),
		High:   money.MustParse("101.00"),
		Low:    money.MustParse("99.00"),
		Close:  money.MustParse("100.50"),
		Volume: 1000,
	}}

	path := CSVPath(t.TempDir(), "1d", "RELIANCE")
	writeFixture(t, path, "timestamp,open,high,low,close,volume\n"+
		"2025-04-17T00:00:00+05:30,100.00,101.00,99.00,100.50,1000\n")
	csvFeed, err := FromCSV(path, reliance, domain.D1)
	if err != nil {
		t.Fatalf("building a CSV feed: %v", err)
	}

	fromCSV := drain(t, csvFeed)
	fromSlice := drain(t, FromSlice(want))
	if len(fromCSV) != 1 || len(fromSlice) != 1 {
		t.Fatalf("expected one bar from each feed, got %d and %d", len(fromCSV), len(fromSlice))
	}
	if !fromCSV[0].Start.Equal(fromSlice[0].Start) || fromCSV[0] != fromSlice[0] {
		t.Errorf("the two feeds must yield the same bar for the same data: csv %+v, slice %+v",
			fromCSV[0], fromSlice[0])
	}
}

func TestReadCSVLocatesColumnsByName(t *testing.T) {
	// The two orderings that coexist in zerobha's tree today. A positional
	// reader gets the second one exactly backwards, and a backtest cannot
	// detect it from its own results.
	histdl := "timestamp,open,high,low,close,volume\n" +
		"2025-04-17T09:15:00+05:30,100.00,101.00,99.00,100.50,1000\n"
	older := "timestamp,close,high,low,open,volume\n" +
		"2025-04-17T09:15:00+05:30,100.50,101.00,99.00,100.00,1000\n"

	for name, body := range map[string]string{"histdl order": histdl, "older order": older} {
		candles, err := ReadCSV(strings.NewReader(body), reliance, domain.M5)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(candles) != 1 {
			t.Fatalf("%s: expected one bar, got %d", name, len(candles))
		}
		c := candles[0]
		if c.Open != money.MustParse("100.00") || c.Close != money.MustParse("100.50") {
			t.Errorf("%s: open and close are transposed; got open=%s close=%s", name, c.Open, c.Close)
		}
		if c.High != money.MustParse("101.00") || c.Low != money.MustParse("99.00") {
			t.Errorf("%s: high/low wrong: %s/%s", name, c.High, c.Low)
		}
		if c.Volume != 1000 {
			t.Errorf("%s: volume wrong: %d", name, c.Volume)
		}
	}
}

func TestReadCSVRejectsAFileMissingAPriceColumn(t *testing.T) {
	body := "timestamp,open,high,low,volume\n2025-04-17T09:15:00+05:30,1,2,3,4\n"
	if _, err := ReadCSV(strings.NewReader(body), reliance, domain.M5); err == nil {
		t.Error("a file with no close column cannot be a bar series and must be refused, not read as zero closes")
	}
}

func TestReadCSVFailsOnAMalformedRow(t *testing.T) {
	// Unlike the instrument master, a bar file is this series and nothing
	// else: dropping a bar changes every indicator computed over it.
	body := "timestamp,open,high,low,close,volume\n" +
		"2025-04-17T09:15:00+05:30,100.00,101.00,99.00,100.50,1000\n" +
		"2025-04-17T09:20:00+05:30,not-a-price,101.00,99.00,100.50,1000\n"
	if _, err := ReadCSV(strings.NewReader(body), reliance, domain.M5); err == nil {
		t.Error("a malformed bar must fail the read rather than silently vanish from the series")
	}
}

func TestParseCSVTimeAcceptsEveryLayoutInTheTree(t *testing.T) {
	cases := map[string]struct {
		hour, minute int
		offset       int // seconds east
	}{
		"2025-04-17T09:15:00+05:30": {9, 15, 5*3600 + 1800},
		"2026-06-24T03:45:00+0000":  {3, 45, 0},
		"2023-06-12T00:00:00":       {0, 0, 5*3600 + 1800},
		"2023-06-12":                {0, 0, 5*3600 + 1800},
	}
	for input, want := range cases {
		got, err := ParseCSVTime(input)
		if err != nil {
			t.Errorf("%q is a layout the existing tree carries and must parse: %v", input, err)
			continue
		}
		if got.Hour() != want.hour || got.Minute() != want.minute {
			t.Errorf("%q parsed to %v, want %02d:%02d", input, got, want.hour, want.minute)
		}
		if _, offset := got.Zone(); offset != want.offset {
			t.Errorf("%q parsed with offset %d, want %d", input, offset, want.offset)
		}
	}
}

func TestParseCSVTimeReadsANaiveStampAsIST(t *testing.T) {
	// These are NSE bars. Reading a naive stamp as UTC shifts every bar five
	// and a half hours and puts the whole session in the wrong day.
	got, err := ParseCSVTime("2025-04-17T09:15:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	utc := got.UTC()
	if utc.Hour() != 3 || utc.Minute() != 45 {
		t.Errorf("09:15 IST is 03:45 UTC; got %v", utc)
	}
}

func TestParseCSVTimeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "yesterday", "17/04/2025"} {
		if _, err := ParseCSVTime(bad); err == nil {
			t.Errorf("%q is not a timestamp and must be refused", bad)
		}
	}
}

func TestFromSliceYieldsInOrderThenStops(t *testing.T) {
	start := time.Date(2025, 4, 17, 9, 15, 0, 0, IST)
	in := []domain.Candle{
		{Key: reliance, Timeframe: domain.M5, Start: start, Close: 100},
		{Key: reliance, Timeframe: domain.M5, Start: start.Add(5 * time.Minute), Close: 101},
	}
	feed := FromSlice(in)
	got := drain(t, feed)
	if len(got) != 2 || !got[0].Start.Before(got[1].Start) {
		t.Fatalf("a feed must yield bars in ascending time order, got %+v", got)
	}
	if _, ok, err := feed.Next(); ok || err != nil {
		t.Error("an exhausted feed must keep reporting false, not restart or error")
	}
}

func TestCSVPathMatchesTheExistingTree(t *testing.T) {
	got := CSVPath(filepath.Join("test", "data"), "5minute", "RELIANCE")
	want := filepath.Join("test", "data", "5minute", "reliance_real.csv")
	if got != want {
		t.Errorf("zerobha's backtester reads %s; got %s", want, got)
	}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
}
