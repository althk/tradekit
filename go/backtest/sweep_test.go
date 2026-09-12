package backtest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
)

func cases(n int) []Case {
	out := make([]Case, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Case{
			Label: fmt.Sprintf("case-%02d", i),
			From:  time.Date(2025, 1, 1, 0, 0, 0, 0, ist), To: time.Date(2025, 12, 31, 0, 0, 0, 0, ist),
			Params: i,
		})
	}
	return out
}

func TestResultsComeBackInCasesOrderHoweverThePoolCompletes(t *testing.T) {
	all := cases(12)
	var inFlight, peak atomic.Int32
	s := &Sweep{Workers: 8}

	results, err := s.Run(t.Context(), all, func(_ context.Context, c Case) ([]domain.Trade, []stats.Point, error) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		// Earlier cases sleep longer, so completion order is the reverse of
		// case order.
		time.Sleep(time.Duration(12-c.Params.(int)) * time.Millisecond)
		inFlight.Add(-1)
		return nil, []stats.Point{{Equity: money.Money(c.Params.(int))}}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != len(all) {
		t.Fatalf("one result per case, got %d", len(results))
	}
	for i, r := range results {
		if r.Case.Label != all[i].Label || r.Curve[0].Equity != money.Money(i) {
			t.Errorf("result %d is for %q; results must come back in cases order, or the 'best parameters' line flip-flops between runs when two cases tie", i, r.Case.Label)
		}
	}
	if peak.Load() < 2 {
		t.Errorf("cases must actually run concurrently, peak in flight was %d", peak.Load())
	}
}

func TestAFailingCaseDoesNotAbortTheSweep(t *testing.T) {
	all := cases(5)
	boom := errors.New("no data for symbol")
	results, err := s8().Run(t.Context(), all, func(_ context.Context, c Case) ([]domain.Trade, []stats.Point, error) {
		switch c.Params.(int) {
		case 2:
			return nil, nil, boom
		case 3:
			panic("nil map in a bad parameter set")
		}
		return sampleTrades(), sampleCurve(), nil
	})
	if err != nil {
		t.Fatalf("a case failing must not fail the sweep: %v", err)
	}
	if !errors.Is(results[2].Err, boom) {
		t.Errorf("case 2's error must be carried in its result, got %v", results[2].Err)
	}
	if results[3].Err == nil {
		t.Error("a panicking case must become that case's error, not take the sweep down")
	}
	for _, i := range []int{0, 1, 4} {
		if results[i].Err != nil || len(results[i].Trades) != 4 {
			t.Errorf("case %d must complete despite its neighbours failing: %+v", i, results[i].Err)
		}
	}
}

func s8() *Sweep { return &Sweep{Workers: 8} }

func TestASweepRecordsEachSuccessfulCase(t *testing.T) {
	db := openTest(t)
	s := &Sweep{Workers: 2, Recorder: &Recorder{DB: db}, Strategy: "test", Opening: money.MustParse("1000.00")}
	results, err := s.Run(t.Context(), cases(3), func(_ context.Context, c Case) ([]domain.Trade, []stats.Point, error) {
		if c.Params.(int) == 1 {
			return nil, nil, errors.New("skip")
		}
		return sampleTrades(), sampleCurve(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE kind = 'backtest'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("two successful cases must be recorded as two runs, found %d", runs)
	}
	if results[0].RunID == 0 || results[2].RunID == 0 || results[1].RunID != 0 {
		t.Errorf("successful cases carry their run id and the failed one does not: %+v", results)
	}
}

func TestACancelledContextStopsTheSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var started atomic.Int32
	_, err := (&Sweep{Workers: 1}).Run(ctx, cases(10), func(context.Context, Case) ([]domain.Trade, []stats.Point, error) {
		if started.Add(1) == 2 {
			cancel()
		}
		return nil, nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Err(), got %v", err)
	}
	if started.Load() >= 10 {
		t.Error("a cancelled sweep must stop dispatching cases")
	}
}

func TestWindowsRollWithoutOverlapAndAnchorToDates(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, ist)
	to := time.Date(2025, 1, 1, 0, 0, 0, 0, ist)
	windows := Windows(from, to, 180, 30, 30)

	// 366 days (2024 is a leap year): the first test window ends at day 210,
	// then every 30 days while day 210+30k <= 366, k = 0..5.
	if len(windows) != 6 {
		t.Fatalf("expected 6 windows, got %d", len(windows))
	}
	for i, w := range windows {
		if !w.TestFrom.Equal(w.TrainTo) {
			t.Errorf("window %d: the test period must start where training ends, got train to %v, test from %v", i, w.TrainTo, w.TestFrom)
		}
		if w.TrainTo.Sub(w.TrainFrom) != 180*24*time.Hour || w.TestTo.Sub(w.TestFrom) != 30*24*time.Hour {
			t.Errorf("window %d has the wrong spans: %v train, %v test", i, w.TrainTo.Sub(w.TrainFrom), w.TestTo.Sub(w.TestFrom))
		}
		if w.TestTo.After(to) {
			t.Errorf("window %d runs past the end of the range: %v", i, w.TestTo)
		}
		if i > 0 && !w.TrainFrom.Equal(windows[i-1].TrainFrom.AddDate(0, 0, 30)) {
			t.Errorf("window %d must step 30 calendar days from the previous, got %v after %v", i, w.TrainFrom, windows[i-1].TrainFrom)
		}
		if w.TrainFrom.Hour() != 0 {
			t.Errorf("window %d drifted off midnight: %v; windows are anchored to dates, not durations", i, w.TrainFrom)
		}
	}
	if Windows(from, to, 400, 30, 30) != nil {
		t.Error("a training window longer than the range yields nothing")
	}
	if Windows(from, to, 0, 30, 30) != nil || Windows(to, from, 30, 30, 30) != nil {
		t.Error("degenerate inputs yield nothing rather than looping")
	}
}
