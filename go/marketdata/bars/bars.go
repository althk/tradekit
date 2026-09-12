// Package bars builds bars from ticks, converts them between timeframes, and
// measures a session against the sessions before it.
//
// The aggregator replaces zerobha's, the only tick-to-bar builder in the
// codebase, and the session-slot machinery replaces breakout500's volume
// profile and relative volume — which is where that system's edge actually
// comes from.
//
// Everything here anchors to the session open through core/calendar.BucketStart
// rather than to the wall clock. With a 09:15 open and a 5-minute bar the two
// disagree from the first bar onward, and a series bucketed by wall clock does
// not line up with the exchange's own.
package bars

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/althk/tradekit/go/core/calendar"
	"github.com/althk/tradekit/go/core/domain"
)

// Builder aggregates ticks into bars, anchored to the session open.
//
// It is not safe for concurrent use: a tick feed delivers on one goroutine and
// serialising here would hide a caller fanning ticks out across several, which
// would interleave them and corrupt every bar.
type Builder struct {
	session   calendar.Session
	timeframe domain.Timeframe
	emit      func(domain.Candle)

	current domain.Candle
	open    bool
}

// NewBuilder returns a Builder that calls emit with each completed bar.
//
// emit is called synchronously from Add, so a slow handler back-pressures the
// tick feed. That is deliberate: dropping a bar to keep up would leave a gap
// nothing downstream can detect.
func NewBuilder(s calendar.Session, tf domain.Timeframe, emit func(domain.Candle)) *Builder {
	return &Builder{session: s, timeframe: tf, emit: emit}
}

// Add ingests one tick.
//
// A tick before the session open lands in the first bar rather than in a
// phantom bar of its own — BucketStart clamps it — so a pre-open print does not
// create a bar the exchange never had.
//
// A tick that arrives out of order, stamped earlier than the bar already open,
// is folded into the current bar rather than opening an earlier one. A feed
// that reorders across a bucket boundary would otherwise emit a bar, then
// re-open it, and the consumer would see the same bucket twice.
func (b *Builder) Add(tick domain.Tick) {
	start := b.session.BucketStart(tick.At, b.timeframe)

	if b.open && start.After(b.current.Start) {
		b.close()
	}
	if !b.open {
		b.current = domain.Candle{
			Key:       tick.Key,
			Timeframe: b.timeframe,
			Start:     start,
			Open:      tick.Price,
			High:      tick.Price,
			Low:       tick.Price,
			Close:     tick.Price,
			Volume:    tick.Volume,
		}
		b.open = true
		return
	}

	if tick.Price > b.current.High {
		b.current.High = tick.Price
	}
	if tick.Price < b.current.Low {
		b.current.Low = tick.Price
	}
	b.current.Close = tick.Price
	b.current.Volume += tick.Volume
}

// Flush emits the bar still forming, if any.
//
// It must be called at the close and at shutdown. Without it the session's last
// bar is never emitted, which is the bar an end-of-day exit is priced from.
func (b *Builder) Flush() {
	if b.open {
		b.close()
	}
}

// close emits the current bar and clears it.
func (b *Builder) close() {
	if b.emit != nil {
		b.emit(b.current)
	}
	b.open = false
	b.current = domain.Candle{}
}

// Resample converts a bar series to a coarser timeframe.
//
// Bars are grouped by the coarser timeframe's session-anchored bucket: the
// group's open is its first bar's open, its close its last bar's close, its
// high and low the extremes, and its volume the sum. Open interest takes the
// last value rather than a sum, because it is a level and not a flow — summing
// it would multiply the outstanding contracts by the number of bars.
//
// Resampling to a finer timeframe is an error, not an interpolation. There is
// no information in a 15-minute bar about what happened in its third minute,
// and inventing some produces a backtest that trades on data that never
// existed.
//
// The input must be in ascending time order, which is what every BarFeed
// yields.
func Resample(candles []domain.Candle, to domain.Timeframe, s calendar.Session) ([]domain.Candle, error) {
	if len(candles) == 0 {
		return nil, nil
	}
	from := candles[0].Timeframe
	if err := coarser(from, to); err != nil {
		return nil, err
	}

	var out []domain.Candle
	var bucketStart time.Time
	started := false

	for _, c := range candles {
		start := s.BucketStart(c.Start, to)
		if started && !start.Equal(bucketStart) {
			started = false
		}
		if !started {
			out = append(out, domain.Candle{
				Key:          c.Key,
				Timeframe:    to,
				Start:        start,
				Open:         c.Open,
				High:         c.High,
				Low:          c.Low,
				Close:        c.Close,
				Volume:       c.Volume,
				OpenInterest: c.OpenInterest,
			})
			bucketStart = start
			started = true
			continue
		}

		agg := &out[len(out)-1]
		if c.High > agg.High {
			agg.High = c.High
		}
		if c.Low < agg.Low {
			agg.Low = c.Low
		}
		agg.Close = c.Close
		agg.Volume += c.Volume
		agg.OpenInterest = c.OpenInterest
	}
	return out, nil
}

// coarser reports whether resampling from one timeframe to another is a
// widening.
func coarser(from, to domain.Timeframe) error {
	if from == to {
		return nil
	}
	fromDur, toDur := from.Duration(), to.Duration()
	switch {
	case fromDur > 0 && toDur > 0:
		if toDur < fromDur {
			return fmt.Errorf("bars: cannot resample %s to the finer %s; a coarser bar carries no information about what happened inside it", from, to)
		}
		if toDur%fromDur != 0 {
			return fmt.Errorf("bars: cannot resample %s to %s; the source bars do not divide the target evenly, so a target bar would be built from a partial set", from, to)
		}
	case fromDur == 0 && toDur > 0:
		// Daily or weekly down to intraday.
		return fmt.Errorf("bars: cannot resample %s to the finer %s", from, to)
	case from == domain.W1 && to == domain.D1:
		// Both calendar-sized, so neither has a duration to compare; a week
		// is still coarser than a day.
		return fmt.Errorf("bars: cannot resample %s to the finer %s", from, to)
	}
	return nil
}

// VolumeProfile returns the typical cumulative volume by session slot.
//
// For each bar, the value is the median cumulative volume through that slot
// across the previous lookbackDays sessions. Median rather than mean, following
// breakout500: one frantic session should not set the benchmark for the next
// month.
//
// Only sessions STRICTLY BEFORE the one being evaluated are used. Including the
// current session's own volume in its own benchmark is the classic lookahead
// bug: the ratio it produces is partly a measurement of itself, and a screener
// built on it looks profitable in backtest and is not. This is the line that
// prevents it, and it is why the profile is built session by session rather
// than over the whole series at once.
//
// The result is aligned to candles, with NaN where there is not enough history
// — never zero. A zero benchmark would make relative volume infinite, and a
// zero-filled warm-up biases every average over the series, which is the same
// rule core/indicators follows.
func VolumeProfile(candles []domain.Candle, s calendar.Session, tf domain.Timeframe, lookbackDays int) []float64 {
	out := make([]float64, len(candles))
	for i := range out {
		out[i] = math.NaN()
	}
	if len(candles) == 0 || lookbackDays <= 0 {
		return out
	}

	// Cumulative volume through each slot, per session.
	type slotVolume map[int]int64
	sessions := map[string]slotVolume{}
	var order []string
	cumulative := map[string]int64{}

	slots := make([]int, len(candles))
	dates := make([]string, len(candles))
	cumAt := make([]int64, len(candles))

	for i, c := range candles {
		day := s.DateKey(c.Start)
		slot := s.SlotIndex(c.Start, tf)
		slots[i], dates[i] = slot, day
		if slot < 0 {
			cumAt[i] = cumulative[day]
			continue
		}
		if _, seen := sessions[day]; !seen {
			sessions[day] = slotVolume{}
			order = append(order, day)
		}
		cumulative[day] += c.Volume
		cumAt[i] = cumulative[day]
		sessions[day][slot] = cumulative[day]
	}
	sort.Strings(order)

	// A session's position in the ordered list, so "the sessions before this
	// one" is a slice rather than a scan.
	position := make(map[string]int, len(order))
	for i, day := range order {
		position[day] = i
	}

	// A benchmark from two sessions is noise. Half the lookback, floored at
	// two, is breakout500's minimum and is what keeps the first fortnight of
	// a new instrument from producing confident nonsense.
	minSessions := lookbackDays / 2
	if minSessions < 2 {
		minSessions = 2
	}

	for i := range candles {
		if slots[i] < 0 {
			continue
		}
		at := position[dates[i]]
		lo := at - lookbackDays
		if lo < 0 {
			lo = 0
		}
		var prior []float64
		for j := lo; j < at; j++ { // strictly before: j < at, never j <= at
			if v, ok := sessions[order[j]][slots[i]]; ok {
				prior = append(prior, float64(v))
			}
		}
		if len(prior) < minSessions {
			continue
		}
		out[i] = median(prior)
	}
	return out
}

// RelativeVolume returns each bar's cumulative session volume over the typical
// value for its slot.
//
// 1.0 means the session is tracking its normal pace; 2.0 means twice the usual
// volume has traded by this point in the day. NaN where the profile has no
// value, so a caller cannot mistake "no history" for "no unusual volume".
func RelativeVolume(candles []domain.Candle, s calendar.Session, tf domain.Timeframe, lookbackDays int) []float64 {
	profile := VolumeProfile(candles, s, tf, lookbackDays)
	out := make([]float64, len(candles))

	cumulative := map[string]int64{}
	for i, c := range candles {
		day := s.DateKey(c.Start)
		cumulative[day] += c.Volume
		if math.IsNaN(profile[i]) || profile[i] <= 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = float64(cumulative[day]) / profile[i]
	}
	return out
}

// median returns the middle value, averaging the two middle ones for an even
// count. It sorts a copy: the caller's slice order is not ours to change.
func median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
