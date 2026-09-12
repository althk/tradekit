// Package sync fills the store from a history provider, resumably and without
// letting one dead symbol kill a 500-symbol pass.
//
// It replaces the downloader each project wrote for itself. Two problems were
// solved separately in every one of them: splitting a long range into
// request-sized windows, and working out which bars are actually missing. Both
// live here.
package sync

import "time"

// Window is one request-sized slice of a date range, inclusive at both ends.
type Window struct {
	From, To time.Time
}

// Chunks splits [from, to] into windows of at most span, oldest first.
//
// Windows are contiguous and never overlap: each begins one second after the
// previous ends. An overlap would re-request bars already held — harmless but
// wasteful against a metered endpoint — while a gap silently loses them, and a
// backtest cannot tell a missing bar from a day the instrument did not trade.
//
// A ports.HistoryProvider chunks internally, so a caller going through one does
// not need this. It is for the callers that drive a raw endpoint themselves —
// an exporter walking a long range, or an adapter being written against a
// provider that has no chunking of its own.
//
// An inverted range yields no windows rather than an error: the caller that
// cares checks it, and the ones that do not are asking for "everything since
// the last bar" on an already-current instrument, which is legitimately empty.
func Chunks(from, to time.Time, span time.Duration) []Window {
	if to.Before(from) || span <= 0 {
		return nil
	}
	var out []Window
	for start := from; !start.After(to); start = start.Add(span) {
		end := start.Add(span - time.Second)
		if end.After(to) {
			end = to
		}
		out = append(out, Window{From: start, To: end})
	}
	return out
}
