package backtest

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSimClockAdvancesForward(t *testing.T) {
	start := time.Date(2025, 4, 17, 9, 15, 0, 0, time.UTC)
	c := NewSimClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("a new clock reads its start time, got %v", c.Now())
	}
	c.Advance(start.Add(5 * time.Minute))
	if !c.Now().Equal(start.Add(5 * time.Minute)) {
		t.Errorf("Advance must move the clock, got %v", c.Now())
	}
	c.Advance(c.Now())
	if !c.Now().Equal(start.Add(5 * time.Minute)) {
		t.Errorf("advancing to the same instant is a no-op, not an error; got %v", c.Now())
	}
}

func TestSimClockPanicsOnGoingBackwards(t *testing.T) {
	start := time.Date(2025, 4, 17, 9, 15, 0, 0, time.UTC)
	c := NewSimClock(start)
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("a clock asked to go backwards must panic: an out-of-order replay produces plausible results that are wrong in a way nothing downstream can detect")
		}
		if msg, ok := p.(string); !ok || !strings.Contains(msg, "backwards") {
			t.Errorf("the panic must say what happened, got %v", p)
		}
	}()
	c.Advance(start.Add(-time.Second))
}

func TestSimClockIsSafeForConcurrentReads(t *testing.T) {
	start := time.Date(2025, 4, 17, 9, 15, 0, 0, time.UTC)
	c := NewSimClock(start)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if c.Now().Before(start) {
					t.Error("the clock must never read before its start")
				}
			}
		}()
	}
	for j := 1; j <= 1000; j++ {
		c.Advance(start.Add(time.Duration(j) * time.Second))
	}
	wg.Wait()
}

func TestRealClockReadsInItsLocation(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	now := RealClock{Loc: loc}.Now()
	if now.Location() != loc {
		t.Errorf("a real clock must report time in the location it was built with, got %v", now.Location())
	}
	defer func() {
		if recover() == nil {
			t.Error("a RealClock with no location must panic rather than trade the wrong hours")
		}
	}()
	RealClock{}.Now()
}
