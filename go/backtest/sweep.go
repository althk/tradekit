package backtest

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
)

// Case is one run's inputs. Params is opaque to the runner and reaches the
// caller's fn unchanged: what a parameter means is strategy knowledge, and the
// runner shares the scheduling, the parallelism, the determinism and the
// recording without sharing the parameter space.
type Case struct {
	Label    string
	From, To time.Time
	Params   any
}

// RunFn executes one case and returns what it produced.
//
// The caller owns the strategy loop and must create the case's own
// paper.Broker and SimClock inside fn. The runner never hands out a shared one:
// paper.Broker is mutex-guarded and would serialise the sweep, and worse, state
// would leak between parameter sets and make results depend on execution order.
type RunFn func(ctx context.Context, c Case) ([]domain.Trade, []stats.Point, error)

// Result is what one case produced, or why it did not.
type Result struct {
	Case   Case
	Trades []domain.Trade
	Curve  []stats.Point
	// RunID is the recorded run, when a Recorder is set and the case
	// succeeded.
	RunID int64
	Err   error
}

// Sweep runs every case, at most Workers at a time.
type Sweep struct {
	// Workers bounds concurrency. Zero means runtime.NumCPU().
	Workers int
	// Recorder persists each successful case when set.
	Recorder *Recorder
	// Strategy names the recorded runs, so a sweep's rows can be told from
	// another strategy's in the same store.
	Strategy string
	// Opening is the opening balance recorded with each run.
	Opening money.Money
}

// Run executes the cases and returns one Result per case, in cases order.
//
// In cases order, not completion order: a sweep whose output ordering depends
// on scheduling produces a different "best parameters" line on each run when
// two cases tie, and the resulting flip-flopping is blamed on the strategy.
//
// One failing case does not abort the sweep; its error is collected into its
// Result and the rest continue. A three-hour sweep that dies on case 47 of 400
// because one symbol had no data is how people stop running sweeps. A
// cancelled context does stop it, and is returned.
func (s *Sweep) Run(ctx context.Context, cases []Case, fn RunFn) ([]Result, error) {
	if fn == nil {
		return nil, fmt.Errorf("backtest: Sweep.Run needs a RunFn")
	}
	workers := s.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(cases) {
		workers = max(1, len(cases))
	}

	results := make([]Result, len(cases))
	indices := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				results[i] = s.runOne(ctx, cases[i], fn)
			}
		}()
	}
	for i := range cases {
		select {
		case <-ctx.Done():
			close(indices)
			wg.Wait()
			return results, ctx.Err()
		case indices <- i:
		}
	}
	close(indices)
	wg.Wait()
	return results, ctx.Err()
}

// runOne executes a case, converting a panic in fn into that case's error so
// one bad parameter set cannot take the whole sweep down.
func (s *Sweep) runOne(ctx context.Context, c Case, fn RunFn) (res Result) {
	res.Case = c
	defer func() {
		if p := recover(); p != nil {
			res.Err = fmt.Errorf("backtest: case %q panicked: %v", c.Label, p)
		}
	}()
	trades, curve, err := fn(ctx, c)
	if err != nil {
		res.Err = fmt.Errorf("backtest: case %q: %w", c.Label, err)
		return res
	}
	res.Trades, res.Curve = trades, curve
	if s.Recorder != nil {
		id, err := s.Recorder.Save(ctx, RunSpec{
			Name: c.Label, Strategy: s.Strategy, From: c.From, To: c.To, Params: c.Params, Opening: s.Opening,
		}, trades, curve)
		if err != nil {
			res.Err = fmt.Errorf("backtest: recording case %q: %w", c.Label, err)
			return res
		}
		res.RunID = id
	}
	return res
}

// Window is one rolling in-sample/out-of-sample split.
type Window struct {
	TrainFrom, TrainTo time.Time
	TestFrom, TestTo   time.Time
}

// Windows generates rolling walk-forward splits over [from, to].
//
// train, test and step are in calendar days, and the windows are anchored to
// dates rather than to durations added to a timestamp: a DST-free market
// would still drift across a year of stepping if a month were 30×24h. The
// training window is [TrainFrom, TrainTo), the test window [TestFrom, TestTo)
// starting where training ends, so the two never overlap. Windows are produced
// while the whole test period fits inside [from, to].
func Windows(from, to time.Time, trainDays, testDays, stepDays int) []Window {
	if trainDays <= 0 || testDays <= 0 || stepDays <= 0 || !to.After(from) {
		return nil
	}
	var out []Window
	for start := from; ; start = start.AddDate(0, 0, stepDays) {
		trainTo := start.AddDate(0, 0, trainDays)
		testTo := trainTo.AddDate(0, 0, testDays)
		if testTo.After(to) {
			return out
		}
		out = append(out, Window{TrainFrom: start, TrainTo: trainTo, TestFrom: trainTo, TestTo: testTo})
	}
}
