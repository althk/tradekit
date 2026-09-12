// Package ratelimit paces requests to a fixed rate.
//
// It replaces five hand-rolled limiters across the projects tradekit
// consolidates, each of which paced a different subset of calls and none of
// which respected cancellation. It lives in core because every adapter and the
// notifier need it, and a second copy is how the inventory came to find five.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter paces requests to a fixed rate.
//
// The implementation is a token bucket with a burst of one: requests are spaced
// by a fixed interval rather than allowed to arrive in a clump and then stall.
// Kite counts requests per second, and a burst that empties the allowance in the
// first 50ms is rejected just as surely as a sustained overload.
//
// A nil *Limiter is valid and never waits, so a caller can leave pacing off by
// leaving the field nil.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	now      func() time.Time // injectable for tests
	sleep    func(context.Context, time.Duration) error
}

// New returns a limiter allowing perSecond requests per second.
// A non-positive rate disables pacing.
func New(perSecond float64) *Limiter {
	l := &Limiter{now: time.Now, sleep: Sleep}
	if perSecond > 0 {
		l.interval = time.Duration(float64(time.Second) / perSecond)
	}
	return l
}

// Wait blocks until the next request may be issued, or until ctx is done.
//
// It returns ctx.Err() on cancellation rather than proceeding: a scan that has
// been cancelled must stop making requests, not work through its backlog.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil || l.interval <= 0 {
		return ctx.Err()
	}

	l.mu.Lock()
	now := l.now()
	wait := time.Duration(0)
	if l.next.After(now) {
		wait = l.next.Sub(now)
	}
	// Reserve this caller's slot before unlocking, so concurrent callers
	// queue behind each other instead of all sleeping the same interval and
	// then firing at once.
	l.next = now.Add(wait + l.interval)
	l.mu.Unlock()

	if wait <= 0 {
		return ctx.Err()
	}
	return l.sleep(ctx, wait)
}

// Sleep waits for d, or returns early with ctx.Err() if ctx is done.
//
// Exported because the adapters' retry back-off needs the same cancellable
// wait, and a plain time.Sleep in a retry loop is a cancelled sync that keeps
// going for another fourteen seconds.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
