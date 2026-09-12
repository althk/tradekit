package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/althk/tradekit/go/store"
)

// Task is one unit of sync work: fetch something, then persist it.
//
// The split is the whole point. Fetch runs on a worker goroutine and must touch
// nothing but the network; Persist runs on the runner's own goroutine and is
// the only place that writes. SQLite has one writer, and the donor's structure
// — a pool fetching, results persisted on the caller's thread — is what keeps a
// concurrent sync from serialising into lock contention or, worse, failing
// intermittently under "database is locked".
type Task struct {
	// ID identifies the task across runs. It is the key progress and
	// last-success are recorded under, so it must be stable: a task whose id
	// changes between runs looks like a new task that has never succeeded,
	// and is refetched every pass forever.
	ID string
	// Cadence gates how often the task is refetched.
	Cadence Cadence
	// Fetch does the network work. It must not touch the store.
	Fetch func(ctx context.Context) (any, error)
	// Persist writes the fetched value. It is called on the runner's own
	// goroutine, one task at a time.
	Persist func(ctx context.Context, fetched any) error
}

// Runner drives tasks with cadence gating and resume.
type Runner struct {
	// DB holds the progress state. Required.
	DB *store.DB
	// Workers bounds concurrent fetches. Zero or one fetches serially.
	Workers int
	// Force refetches every task regardless of cadence.
	Force bool
	// Now is injectable for tests. Zero means time.Now().
	Now func() time.Time
	// RunName distinguishes one sync's progress from another's in the state
	// table, so a candle sync and a fundamentals sync do not resume into
	// each other.
	RunName string
}

// RunReport is what one pass did.
type RunReport struct {
	// RunID is the runs-table row this pass recorded itself under.
	RunID int64
	// Succeeded, Skipped and Failed count tasks, not bars. Skipped means the
	// cadence gate held, or the task was already done earlier in a run this
	// one resumed.
	Succeeded int
	Skipped   int
	Failed    int
	// Errors holds one error per failed task.
	Errors []error
}

// progress is the resume record for one run, stored as JSON under a kv_state
// key.
//
// It lives in kv_state rather than in a table of its own because it is
// transient: it exists only between a crash and the resumed run that consumes
// it. Giving it a table would mean a migration, and a schema change to hold
// state that is meaningless a day later is not worth one.
type progress struct {
	RunID int64 `json:"run_id"`
	// Done maps task id to the outcome recorded for it. A task present here
	// is not attempted again by a resumed run.
	Done map[string]string `json:"done"`
}

// Run executes the tasks, resuming an interrupted pass rather than repeating it.
//
// A pass records progress after each task, so a run killed halfway through 500
// symbols resumes at symbol 251 rather than at symbol 1 — which, against a
// metered endpoint, is the difference between finishing the day's sync and
// burning the day's quota on work already done.
//
// One task's failure does not stop the pass; a cancelled context does.
func (r *Runner) Run(ctx context.Context, tasks []Task) (RunReport, error) {
	if r.DB == nil {
		return RunReport{}, fmt.Errorf("sync: Runner.DB is required")
	}
	now := r.now()

	state, err := r.resume(ctx)
	if err != nil {
		return RunReport{}, err
	}
	report := RunReport{RunID: state.RunID}

	pending := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if _, done := state.Done[task.ID]; done {
			// Handled earlier in the run this one is resuming.
			report.Skipped++
			continue
		}
		due, err := r.due(ctx, task, now)
		if err != nil {
			return report, err
		}
		if !due {
			report.Skipped++
			state.Done[task.ID] = "skipped"
			if err := r.save(ctx, state); err != nil {
				return report, err
			}
			continue
		}
		pending = append(pending, task)
	}

	if err := r.drive(ctx, pending, state, &report, now); err != nil {
		return report, err
	}

	// The pass finished, so its resume record is spent. Clearing it means the
	// next run starts fresh rather than believing every task is already done.
	if err := r.DB.DeleteState(ctx, r.stateKey()); err != nil {
		return report, err
	}
	status, message := "ok", ""
	if report.Failed > 0 {
		status = "partial"
		message = fmt.Sprintf("%d of %d tasks failed", report.Failed, len(tasks))
	}
	if err := r.DB.FinishRun(ctx, state.RunID, status, message); err != nil {
		return report, err
	}
	return report, nil
}

// drive fetches concurrently and persists serially.
func (r *Runner) drive(ctx context.Context, tasks []Task, state *progress, report *RunReport, now time.Time) error {
	if len(tasks) == 0 {
		return nil
	}
	workers := r.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	type result struct {
		index   int
		fetched any
		err     error
	}

	// Fetches are dispatched by index and results collected by index, so the
	// persist order matches the task order however the pool completes. A
	// resumed run then stops at a prefix rather than at a scatter of
	// finished tasks, which is what makes the resume record small and its
	// meaning obvious.
	results := make([]result, len(tasks))
	var wg sync.WaitGroup
	indices := make(chan int)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				results[i] = result{index: i}
				results[i].fetched, results[i].err = fetchSafe(ctx, tasks[i])
			}
		}()
	}
	go func() {
		defer close(indices)
		for i := range tasks {
			select {
			case <-ctx.Done():
				return
			case indices <- i:
			}
		}
	}()
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err
	}

	for i, task := range tasks {
		res := results[i]
		outcome := "success"
		if res.err != nil {
			outcome = "error"
			report.Failed++
			report.Errors = append(report.Errors, fmt.Errorf("sync: %s: %w", task.ID, res.err))
		} else if err := task.Persist(ctx, res.fetched); err != nil {
			outcome = "error"
			report.Failed++
			report.Errors = append(report.Errors, fmt.Errorf("sync: %s: persisting: %w", task.ID, err))
		} else {
			report.Succeeded++
			if err := r.markSuccess(ctx, task, now); err != nil {
				return err
			}
		}
		state.Done[task.ID] = outcome
		// Saved per task, not at the end: a record written only on
		// completion is a record that never survives the crash it exists
		// for.
		if err := r.save(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

// fetchSafe runs a task's fetch, converting a panic into an error.
//
// A panic on a worker goroutine tears down the whole process, so one task
// dereferencing a nil field in an unexpected response would kill a 500-symbol
// sync outright. fanse's _fetch_safe makes the same conversion for the same
// reason.
func fetchSafe(ctx context.Context, task Task) (fetched any, err error) {
	defer func() {
		if p := recover(); p != nil {
			fetched, err = nil, fmt.Errorf("fetch panicked: %v", p)
		}
	}()
	return task.Fetch(ctx)
}

// due reports whether a task's cadence gate lets it through.
func (r *Runner) due(ctx context.Context, task Task, now time.Time) (bool, error) {
	if r.Force {
		return true, nil
	}
	var last time.Time
	var stamp string
	err := r.DB.GetState(ctx, r.successKey(task), &stamp)
	if errors.Is(err, store.ErrStateNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if last, err = time.Parse(time.RFC3339, stamp); err != nil {
		// An unreadable timestamp is treated as no record rather than as a
		// failure: refetching once is cheap, and refusing to sync because
		// a state value is malformed is not.
		return true, nil
	}
	return Due(task.Cadence, last, now, false), nil
}

// markSuccess records when a task last succeeded, which is what the cadence
// gate reads on the next pass.
func (r *Runner) markSuccess(ctx context.Context, task Task, now time.Time) error {
	return r.DB.SetState(ctx, r.successKey(task), now.UTC().Format(time.RFC3339))
}

// resume loads an interrupted pass's progress, or starts a new run.
func (r *Runner) resume(ctx context.Context) (*progress, error) {
	var state progress
	err := r.DB.GetState(ctx, r.stateKey(), &state)
	switch {
	case err == nil && state.RunID != 0:
		if state.Done == nil {
			state.Done = map[string]string{}
		}
		return &state, nil
	case err != nil && !errors.Is(err, store.ErrStateNotFound):
		return nil, err
	}

	runID, err := r.DB.StartRun(ctx, "sync", r.RunName, nil)
	if err != nil {
		return nil, err
	}
	return &progress{RunID: runID, Done: map[string]string{}}, nil
}

// save writes the resume record.
func (r *Runner) save(ctx context.Context, state *progress) error {
	return r.DB.SetState(ctx, r.stateKey(), state)
}

// stateKey is where this runner's resume record lives.
func (r *Runner) stateKey() string { return "sync.progress." + r.name() }

// successKey is where one task's last success is recorded. It is scoped by run
// name as well as task id, so two syncs that happen to name a task the same do
// not gate each other.
func (r *Runner) successKey(task Task) string {
	return "sync.success." + r.name() + "." + task.ID
}

func (r *Runner) name() string {
	if r.RunName == "" {
		return "default"
	}
	return r.RunName
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
