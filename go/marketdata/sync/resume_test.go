package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeDetector records whether two persists ever overlap.
//
// SQLite has one writer. The runner's contract is that fetches run
// concurrently and persists do not, and this is what proves the second half:
// TryLock fails only if another persist is already inside.
type writeDetector struct {
	mu         sync.Mutex
	concurrent atomic.Bool
	order      []string
}

func (w *writeDetector) persist(id string) error {
	if !w.mu.TryLock() {
		w.concurrent.Store(true)
		return fmt.Errorf("concurrent write")
	}
	defer w.mu.Unlock()
	// Long enough that a genuinely concurrent persist would collide.
	time.Sleep(time.Millisecond)
	w.order = append(w.order, id)
	return nil
}

func task(id string, fetch func(context.Context) (any, error), persist func(context.Context, any) error) Task {
	return Task{ID: id, Cadence: Always, Fetch: fetch, Persist: persist}
}

func TestRunnerFetchesConcurrentlyAndWritesSerially(t *testing.T) {
	db := openTest(t)
	detector := &writeDetector{}

	var inFlight, maxInFlight atomic.Int32
	tasks := make([]Task, 0, 12)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("task-%02d", i)
		tasks = append(tasks, task(id,
			func(context.Context) (any, error) {
				n := inFlight.Add(1)
				for {
					peak := maxInFlight.Load()
					if n <= peak || maxInFlight.CompareAndSwap(peak, n) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				inFlight.Add(-1)
				return id, nil
			},
			func(_ context.Context, fetched any) error { return detector.persist(fetched.(string)) },
		))
	}

	r := &Runner{DB: db, Workers: 4, RunName: "concurrency"}
	report, err := r.Run(t.Context(), tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Succeeded != len(tasks) {
		t.Fatalf("every task must succeed, got %d of %d (%v)", report.Succeeded, len(tasks), report.Errors)
	}
	if detector.concurrent.Load() {
		t.Error("two persists overlapped; SQLite has one writer and concurrent writes surface as intermittent 'database is locked'")
	}
	if maxInFlight.Load() < 2 {
		t.Errorf("fetches must actually run concurrently, peak in flight was %d", maxInFlight.Load())
	}
	if len(detector.order) != len(tasks) {
		t.Fatalf("expected %d persists, got %d", len(tasks), len(detector.order))
	}
	for i := 1; i < len(detector.order); i++ {
		if detector.order[i] <= detector.order[i-1] {
			t.Errorf("persists must land in task order however the pool completes; got %v", detector.order)
			break
		}
	}
}

func TestATaskInsideItsFreshnessWindowIsSkipped(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 17, 9, 0, 0, 0, time.UTC)
	var fetches atomic.Int32

	daily := Task{
		ID:      "fundamentals",
		Cadence: Daily,
		Fetch:   func(context.Context) (any, error) { fetches.Add(1); return "x", nil },
		Persist: func(context.Context, any) error { return nil },
	}

	r := &Runner{DB: db, RunName: "cadence", Now: func() time.Time { return now }}
	if _, err := r.Run(t.Context(), []Task{daily}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("the first pass must fetch, got %d fetches", fetches.Load())
	}

	// An hour later, well inside the daily window.
	r.Now = func() time.Time { return now.Add(time.Hour) }
	report, err := r.Run(t.Context(), []Task{daily})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if fetches.Load() != 1 {
		t.Errorf("a task inside its freshness window must not be refetched; got %d fetches", fetches.Load())
	}
	if report.Skipped != 1 {
		t.Errorf("the skip must be reported, got %+v", report)
	}
}

func TestForceOverridesTheFreshnessGate(t *testing.T) {
	db := openTest(t)
	now := time.Date(2025, 4, 17, 9, 0, 0, 0, time.UTC)
	var fetches atomic.Int32

	daily := Task{
		ID:      "fundamentals",
		Cadence: Daily,
		Fetch:   func(context.Context) (any, error) { fetches.Add(1); return "x", nil },
		Persist: func(context.Context, any) error { return nil },
	}

	r := &Runner{DB: db, RunName: "cadence", Now: func() time.Time { return now }}
	if _, err := r.Run(t.Context(), []Task{daily}); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	r.Force = true
	r.Now = func() time.Time { return now.Add(time.Hour) }
	if _, err := r.Run(t.Context(), []Task{daily}); err != nil {
		t.Fatalf("forced pass: %v", err)
	}
	if fetches.Load() != 2 {
		t.Error("without force overriding the gate, an operator re-running a failed sync gets a run that does nothing")
	}
}

func TestAFailedTaskDoesNotAbortThePassAndTheRecordIsCleared(t *testing.T) {
	db := openTest(t)

	// One task's persist fails. The pass must finish the rest and then clear
	// its resume record, so the next run starts fresh.
	var fetched []string
	var mu sync.Mutex
	makeTasks := func(record *[]string, failAt string) []Task {
		var tasks []Task
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("task-%02d", i)
			tasks = append(tasks, Task{
				ID:      id,
				Cadence: Always,
				Fetch: func(context.Context) (any, error) {
					mu.Lock()
					*record = append(*record, id)
					mu.Unlock()
					return id, nil
				},
				Persist: func(_ context.Context, fetched any) error {
					if fetched.(string) == failAt {
						return errors.New("store went away")
					}
					return nil
				},
			})
		}
		return tasks
	}

	r := &Runner{DB: db, RunName: "resume"}
	report, err := r.Run(t.Context(), makeTasks(&fetched, "task-03"))
	if err != nil {
		t.Fatalf("a task-level failure must not abort the pass: %v", err)
	}
	if report.Failed != 1 {
		t.Fatalf("expected exactly one failed task, got %+v", report)
	}
	if report.Succeeded != 5 {
		t.Errorf("the other five tasks must still complete; got %d", report.Succeeded)
	}
	if len(fetched) != 6 {
		t.Errorf("every task must be attempted, got %d", len(fetched))
	}

	// A completed pass clears its resume record, so the next run starts
	// fresh rather than believing everything is already done.
	var state progress
	if err := r.DB.GetState(t.Context(), r.stateKey(), &state); err == nil {
		t.Error("a finished pass must clear its resume record, or the next run skips every task forever")
	}
}

func TestResumeSkipsTasksAlreadyDone(t *testing.T) {
	db := openTest(t)
	r := &Runner{DB: db, RunName: "resume"}

	// Simulate a crash: a resume record naming three of five tasks as done.
	runID, err := db.StartRun(t.Context(), "sync", "resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetState(t.Context(), r.stateKey(), progress{
		RunID: runID,
		Done:  map[string]string{"task-00": "success", "task-01": "success", "task-02": "success"},
	}); err != nil {
		t.Fatal(err)
	}

	var fetched []string
	var mu sync.Mutex
	var tasks []Task
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("task-%02d", i)
		tasks = append(tasks, Task{
			ID:      id,
			Cadence: Always,
			Fetch: func(context.Context) (any, error) {
				mu.Lock()
				fetched = append(fetched, id)
				mu.Unlock()
				return id, nil
			},
			Persist: func(context.Context, any) error { return nil },
		})
	}

	report, err := r.Run(t.Context(), tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fetched) != 2 {
		t.Fatalf("a crashed 500-symbol sync must resume where it stopped, not burn the day's quota redoing work; refetched %v", fetched)
	}
	if fetched[0] != "task-03" || fetched[1] != "task-04" {
		t.Errorf("the resumed pass must pick up at the first unfinished task, got %v", fetched)
	}
	if report.Skipped != 3 {
		t.Errorf("the three already-done tasks must be reported as skipped, got %d", report.Skipped)
	}
	if report.RunID != runID {
		t.Errorf("a resumed pass continues the interrupted run rather than starting a new one; got run %d want %d", report.RunID, runID)
	}
}

func TestAPanicInOneTaskDoesNotTearDownThePool(t *testing.T) {
	db := openTest(t)

	tasks := []Task{
		task("ok-1", func(context.Context) (any, error) { return "a", nil }, func(context.Context, any) error { return nil }),
		task("panics", func(context.Context) (any, error) {
			var m map[string]string
			m["boom"] = "x" // nil map write
			return nil, nil
		}, func(context.Context, any) error { return nil }),
		task("ok-2", func(context.Context) (any, error) { return "b", nil }, func(context.Context, any) error { return nil }),
	}

	r := &Runner{DB: db, Workers: 3, RunName: "panic"}
	report, err := r.Run(t.Context(), tasks)
	if err != nil {
		t.Fatalf("a panicking task must become a failed result, not kill the process: %v", err)
	}
	if report.Succeeded != 2 {
		t.Errorf("the two healthy tasks must still succeed, got %d", report.Succeeded)
	}
	if report.Failed != 1 {
		t.Errorf("the panicking task must be reported as failed, got %d", report.Failed)
	}
}

func TestRunnerRequiresAStore(t *testing.T) {
	r := &Runner{}
	if _, err := r.Run(context.Background(), nil); err == nil {
		t.Error("a runner with no store cannot record progress and must fail at once")
	}
}
