package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeClock drives the limiter without real sleeping, so the tests assert the
// pacing arithmetic rather than the scheduler's accuracy.
type fakeClock struct {
	now    time.Time
	slept  []time.Duration
	cancel error
}

func (f *fakeClock) Now() time.Time { return f.now }

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	if f.cancel != nil {
		return f.cancel
	}
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	return nil
}

func newFakeLimiter(perSecond float64) (*Limiter, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 15, 9, 15, 0, 0, time.UTC)}
	l := New(perSecond)
	l.now = clock.Now
	l.sleep = clock.Sleep
	return l, clock
}

func TestLimiterSpacesRequestsEvenly(t *testing.T) {
	l, clock := newFakeLimiter(3) // 3/s means one every ~333ms
	ctx := context.Background()

	for range 4 {
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The first call goes through immediately; the next three each wait one
	// interval. A burst of four arriving at once must not all pass.
	if len(clock.slept) != 3 {
		t.Fatalf("slept %d times, want 3: the first request is free, the rest are paced", len(clock.slept))
	}
	want := time.Second / 3
	for i, d := range clock.slept {
		if d != want {
			t.Errorf("sleep %d = %v, want %v", i, d, want)
		}
	}
}

func TestLimiterDoesNotDelayWhenIdle(t *testing.T) {
	l, clock := newFakeLimiter(3)
	ctx := context.Background()

	if err := l.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// Enough time passes that the allowance has refilled.
	clock.now = clock.now.Add(2 * time.Second)
	if err := l.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(clock.slept) != 0 {
		t.Errorf("slept %v, want no delay after an idle gap", clock.slept)
	}
}

func TestLimiterRespectsCancellation(t *testing.T) {
	l, clock := newFakeLimiter(1)
	clock.cancel = context.Canceled
	ctx := context.Background()

	if err := l.Wait(ctx); err != nil {
		t.Fatalf("the first request should pass: %v", err)
	}
	// A cancelled scan must stop making requests, not work through its
	// backlog at one per second.
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLimiterReturnsContextErrorWhenAlreadyDone(t *testing.T) {
	l, _ := newFakeLimiter(3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled even on the free first request", err)
	}
}

func TestZeroRateDisablesPacing(t *testing.T) {
	l, clock := newFakeLimiter(0)
	ctx := context.Background()
	for range 10 {
		if err := l.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(clock.slept) != 0 {
		t.Errorf("a non-positive rate must disable pacing, slept %v", clock.slept)
	}
}

func TestNilLimiterIsUsable(t *testing.T) {
	// A zero-valued client should not panic before its options are applied.
	var l *Limiter
	if err := l.Wait(context.Background()); err != nil {
		t.Errorf("a nil limiter must be a no-op, got %v", err)
	}
}

func TestConcurrentCallersQueueRatherThanCollide(t *testing.T) {
	// Real clock here: the point is that N goroutines take at least N-1
	// intervals in total, rather than each sleeping one interval and then
	// all firing together.
	l := New(200) // 5ms apart, so the test stays fast
	ctx := context.Background()

	const callers = 5
	start := time.Now()
	done := make(chan struct{}, callers)
	for range callers {
		go func() {
			_ = l.Wait(ctx)
			done <- struct{}{}
		}()
	}
	for range callers {
		<-done
	}

	if elapsed := time.Since(start); elapsed < 4*5*time.Millisecond {
		t.Errorf("5 concurrent callers finished in %v; they must queue, not all sleep one interval", elapsed)
	}
}
