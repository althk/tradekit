package marketdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/calendar"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/marketdata/bars"
	"github.com/althk/tradekit/go/marketdata/reference"
	"github.com/althk/tradekit/go/marketdata/sync"
)

// The shared golden cases for market-data arithmetic, from
// contracts/testdata/parity.json.
//
// py/tests/test_marketdata.py runs the same block. Chunk boundaries,
// resampling and the corporate-action adjustment are the places two
// implementations drift silently: a one-second overlap re-requests a bar on
// every run, a bucket anchored to the wall clock shifts every 15-minute bar,
// and a split rounded differently changes every indicator over the history.
//
// The core parity suite cannot cover this: it lives in go/core, which must not
// depend on this module.
type parityBar struct {
	Start        time.Time   `json:"start"`
	Open         money.Money `json:"open"`
	High         money.Money `json:"high"`
	Low          money.Money `json:"low"`
	Close        money.Money `json:"close"`
	Volume       int64       `json:"volume"`
	OpenInterest int64       `json:"open_interest"`
}

type marketdataFixture struct {
	Marketdata struct {
		Chunks []struct {
			Name        string    `json:"name"`
			From        time.Time `json:"from"`
			To          time.Time `json:"to"`
			SpanSeconds int64     `json:"span_seconds"`
			Want        []struct {
				From time.Time `json:"from"`
				To   time.Time `json:"to"`
			} `json:"want"`
		} `json:"chunks"`

		Resample struct {
			Session string      `json:"session"`
			From    string      `json:"from"`
			Input   []parityBar `json:"input"`
			Cases   []struct {
				To   string      `json:"to"`
				Want []parityBar `json:"want"`
			} `json:"cases"`
			Invalid []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"invalid"`
		} `json:"resample"`

		Adjust []struct {
			Name    string      `json:"name"`
			Candles []parityBar `json:"candles"`
			Actions []struct {
				ExDate string      `json:"ex_date"`
				Kind   string      `json:"kind"`
				Ratio  float64     `json:"ratio"`
				Amount money.Money `json:"amount"`
			} `json:"actions"`
			Want []parityBar `json:"want"`
		} `json:"adjust"`
	} `json:"marketdata"`
}

func loadMarketdataFixture(t *testing.T) marketdataFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "testdata", "parity.json"))
	if err != nil {
		t.Fatalf("reading parity fixture: %v", err)
	}
	var f marketdataFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing parity fixture: %v", err)
	}
	if len(f.Marketdata.Chunks) == 0 || len(f.Marketdata.Adjust) == 0 || len(f.Marketdata.Resample.Cases) == 0 {
		t.Fatal("the fixture's marketdata block is empty; the Python suite would be running cases this one is not")
	}
	return f
}

func (b parityBar) candle(tf domain.Timeframe) domain.Candle {
	return domain.Candle{
		Key: reliance, Timeframe: tf, Start: b.Start,
		Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
		Volume: b.Volume, OpenInterest: b.OpenInterest,
	}
}

// sameBar compares a bar with a fixture row by instant rather than by
// time.Time equality, which would also compare the zone pointer.
func sameBar(got domain.Candle, want parityBar) bool {
	return got.Start.Equal(want.Start) &&
		got.Open == want.Open && got.High == want.High &&
		got.Low == want.Low && got.Close == want.Close &&
		got.Volume == want.Volume && got.OpenInterest == want.OpenInterest
}

func TestParityChunks(t *testing.T) {
	f := loadMarketdataFixture(t)
	for _, c := range f.Marketdata.Chunks {
		got := sync.Chunks(c.From, c.To, time.Duration(c.SpanSeconds)*time.Second)
		if len(got) != len(c.Want) {
			t.Errorf("%s: got %d windows, want %d: %v", c.Name, len(got), len(c.Want), got)
			continue
		}
		for i, w := range c.Want {
			if !got[i].From.Equal(w.From) || !got[i].To.Equal(w.To) {
				t.Errorf("%s: window %d is [%v, %v], want [%v, %v]; a boundary Python draws elsewhere is a bar one side re-requests or loses",
					c.Name, i, got[i].From, got[i].To, w.From, w.To)
			}
		}
	}
}

func TestParityResample(t *testing.T) {
	f := loadMarketdataFixture(t)
	r := f.Marketdata.Resample
	if r.Session != "NSE" {
		t.Fatalf("the fixture names session %q; only NSE is wired here", r.Session)
	}
	s := calendar.NSE()
	from := domain.Timeframe(r.From)

	for _, c := range r.Cases {
		in := make([]domain.Candle, 0, len(r.Input))
		for _, b := range r.Input {
			in = append(in, b.candle(from))
		}
		got, err := bars.Resample(in, domain.Timeframe(c.To), s)
		if err != nil {
			t.Errorf("resample %s -> %s: unexpected error: %v", r.From, c.To, err)
			continue
		}
		if len(got) != len(c.Want) {
			t.Errorf("resample %s -> %s: got %d bars, want %d", r.From, c.To, len(got), len(c.Want))
			continue
		}
		for i, w := range c.Want {
			if got[i].Timeframe != domain.Timeframe(c.To) {
				t.Errorf("resample %s -> %s: bar %d carries timeframe %q", r.From, c.To, i, got[i].Timeframe)
			}
			if !sameBar(got[i], w) {
				t.Errorf("resample %s -> %s: bar %d is %+v, want %+v", r.From, c.To, i, got[i], w)
			}
		}
	}

	for _, c := range r.Invalid {
		in := []domain.Candle{r.Input[0].candle(domain.Timeframe(c.From))}
		if _, err := bars.Resample(in, domain.Timeframe(c.To), s); err == nil {
			t.Errorf("resample %s -> %s must be refused; inventing finer bars is trading on data that never existed", c.From, c.To)
		}
	}
}

func TestParityAdjust(t *testing.T) {
	f := loadMarketdataFixture(t)
	for _, c := range f.Marketdata.Adjust {
		in := make([]domain.Candle, 0, len(c.Candles))
		for _, b := range c.Candles {
			in = append(in, b.candle(domain.D1))
		}
		actions := make([]reference.Action, 0, len(c.Actions))
		for _, a := range c.Actions {
			// Parsed in UTC, exactly as reference.Actions reads the column,
			// so the fixture exercises the date-versus-instant comparison
			// the store path actually makes.
			exDate, err := time.Parse(time.DateOnly, a.ExDate)
			if err != nil {
				t.Fatalf("%s: bad ex_date %q: %v", c.Name, a.ExDate, err)
			}
			actions = append(actions, reference.Action{
				Key: reliance, ExDate: exDate, Kind: a.Kind, Ratio: a.Ratio, Amount: a.Amount,
			})
		}

		got := reference.Adjust(in, actions)
		if len(got) != len(c.Want) {
			t.Errorf("%s: got %d bars, want %d", c.Name, len(got), len(c.Want))
			continue
		}
		for i, w := range c.Want {
			if !sameBar(got[i], w) {
				t.Errorf("%s: bar %d is %+v, want %+v", c.Name, i, got[i], w)
			}
		}
	}
}
