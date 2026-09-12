package sync

import (
	"testing"
	"time"
)

func TestChunksAreContiguousWithNoGapOrOverlap(t *testing.T) {
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 100)
	span := 25 * 24 * time.Hour

	windows := Chunks(from, to, span)
	if len(windows) < 4 {
		t.Fatalf("a 100-day range in 25-day windows needs at least four, got %d", len(windows))
	}
	if !windows[0].From.Equal(from) {
		t.Errorf("the first window must start exactly at the requested start, got %v", windows[0].From)
	}
	if !windows[len(windows)-1].To.Equal(to) {
		t.Errorf("the last window must end exactly at the requested end, got %v", windows[len(windows)-1].To)
	}
	for i := 1; i < len(windows); i++ {
		gap := windows[i].From.Sub(windows[i-1].To)
		if gap != time.Second {
			t.Errorf("window %d begins %v after the previous ends; anything but one second is a gap that loses bars or an overlap that re-requests them",
				i, gap)
		}
	}
	for i, w := range windows {
		if w.To.Before(w.From) {
			t.Errorf("window %d is inverted: %v to %v", i, w.From, w.To)
		}
		if w.To.Sub(w.From) >= span {
			t.Errorf("window %d spans %v, at or beyond the %v request limit", i, w.To.Sub(w.From), span)
		}
	}
}

func TestChunksMakeOneWindowForAShortRange(t *testing.T) {
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 2)

	windows := Chunks(from, to, 25*24*time.Hour)
	if len(windows) != 1 {
		t.Fatalf("a range shorter than one span is one window, got %d", len(windows))
	}
	if !windows[0].From.Equal(from) || !windows[0].To.Equal(to) {
		t.Errorf("the single window must be the requested range exactly, got %v to %v", windows[0].From, windows[0].To)
	}
}

func TestChunksOfAnInvertedRangeAreEmpty(t *testing.T) {
	to := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, 10)
	if got := Chunks(from, to, 24*time.Hour); got != nil {
		t.Errorf("an inverted range is what 'everything since the last bar' looks like on an already-current instrument; it is legitimately empty, got %v", got)
	}
}

func TestChunksOfANonPositiveSpanAreEmpty(t *testing.T) {
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := Chunks(from, from.AddDate(0, 0, 10), 0); got != nil {
		t.Errorf("a zero span would loop forever; it must yield nothing instead, got %v", got)
	}
}

func TestChunksCoverTheWholeRange(t *testing.T) {
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 63)
	windows := Chunks(from, to, 10*24*time.Hour)

	// Every instant in the range falls in exactly one window, which is what
	// "no gap and no overlap" means for the bars inside them.
	for d := from; !d.After(to); d = d.Add(6 * time.Hour) {
		matches := 0
		for _, w := range windows {
			if !d.Before(w.From) && !d.After(w.To) {
				matches++
			}
		}
		if matches != 1 {
			t.Fatalf("%v falls in %d windows, want exactly 1", d, matches)
		}
	}
}

func TestDueGatesOnAge(t *testing.T) {
	now := time.Date(2025, 4, 17, 9, 0, 0, 0, time.UTC)

	if !Due(Daily, time.Time{}, now, false) {
		t.Error("a task that has never succeeded is always due; treating a zero time as 'succeeded at the epoch' leaves a fresh install permanently up to date and permanently empty")
	}
	if Due(Daily, now.Add(-time.Hour), now, false) {
		t.Error("a task that succeeded an hour ago is inside its daily window and must be skipped")
	}
	if !Due(Daily, now.Add(-25*time.Hour), now, false) {
		t.Error("a task that succeeded yesterday is due")
	}
	if !Due(Daily, now.Add(-time.Hour), now, true) {
		t.Error("force must override the gate, or an operator re-running a failed sync gets a run that does nothing")
	}
	if !Due(Always, now, now, false) {
		t.Error("an Always cadence refetches every pass, whatever the last success")
	}
	if Due(Weekly, now.Add(-48*time.Hour), now, false) {
		t.Error("two days is inside the weekly window")
	}
}
