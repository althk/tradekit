// Package indicators computes the technical series the strategies share.
//
// It replaces four separate implementations of the same handful of functions —
// ATR alone existed in four projects, two of them subtly different in how they
// seeded the first value.
//
// # Representation
//
// Every function returns a []float64 the same length as its input, with
// math.NaN() in the positions before the indicator has warmed up. NaN is used
// rather than zero because zero is a legitimate indicator value and a
// zero-filled warm-up silently biases any average taken over the series.
//
// Price-valued outputs (SMA, EMA, ATR, Donchian, VWAP, pivots) are in the same
// minor units as their money.Money input. float64 holds those integers exactly
// up to 2^53, which is far beyond any realistic price, so no precision is lost;
// use ToMoney to convert a value back.
package indicators

import (
	"math"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// ToMoney converts an indicator value back to minor units, rounding half away
// from zero. A NaN input returns 0, which callers must therefore guard against
// with IsValid rather than relying on the zero.
func ToMoney(v float64) money.Money {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return money.Money(math.Round(v))
}

// IsValid reports whether an indicator value has warmed up.
func IsValid(v float64) bool { return !math.IsNaN(v) }

// Closes extracts the closing prices of a candle series as indicator input.
func Closes(cs []domain.Candle) []float64 {
	return field(cs, func(c domain.Candle) money.Money { return c.Close })
}

// Highs extracts the high prices of a candle series.
func Highs(cs []domain.Candle) []float64 {
	return field(cs, func(c domain.Candle) money.Money { return c.High })
}

// Lows extracts the low prices of a candle series.
func Lows(cs []domain.Candle) []float64 {
	return field(cs, func(c domain.Candle) money.Money { return c.Low })
}

func field(cs []domain.Candle, get func(domain.Candle) money.Money) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = float64(get(c))
	}
	return out
}

func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// SMA is the simple moving average over the trailing period values.
func SMA(values []float64, period int) []float64 {
	out := nanSlice(len(values))
	if period <= 0 || len(values) < period {
		return out
	}
	var sum float64
	for i, v := range values {
		sum += v
		if i >= period {
			sum -= values[i-period]
		}
		if i >= period-1 {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// EMA is the exponentially weighted moving average, seeded with the simple
// average of the first period values.
//
// Seeding matters and is the difference between two implementations this
// replaces: seeding with the first value instead makes the early series
// depend on how much history the caller happened to load.
func EMA(values []float64, period int) []float64 {
	out := nanSlice(len(values))
	if period <= 0 || len(values) < period {
		return out
	}
	k := 2.0 / float64(period+1)

	var seed float64
	for i := 0; i < period; i++ {
		seed += values[i]
	}
	prev := seed / float64(period)
	out[period-1] = prev

	for i := period; i < len(values); i++ {
		prev = (values[i]-prev)*k + prev
		out[i] = prev
	}
	return out
}

// TrueRange is the per-bar true range: the greatest of the bar's own span, the
// gap up from the previous close, and the gap down to it. The first bar has no
// previous close and is therefore NaN, not its own high-low span.
func TrueRange(cs []domain.Candle) []float64 {
	out := nanSlice(len(cs))
	for i := 1; i < len(cs); i++ {
		prevClose := float64(cs[i-1].Close)
		hl := float64(cs[i].High - cs[i].Low)
		hc := math.Abs(float64(cs[i].High) - prevClose)
		lc := math.Abs(float64(cs[i].Low) - prevClose)
		out[i] = math.Max(hl, math.Max(hc, lc))
	}
	return out
}

// ATR is Wilder's average true range: a simple average of the first period
// true ranges, then Wilder smoothing.
//
// Wilder smoothing, not an EMA with the same period, is what "ATR(14)" means
// everywhere it is quoted. Substituting an EMA gives a series roughly 7%
// tighter, which silently moves every ATR-derived stop.
func ATR(cs []domain.Candle, period int) []float64 {
	out := nanSlice(len(cs))
	tr := TrueRange(cs)
	// tr[0] is NaN, so the first period true ranges are tr[1..period].
	if period <= 0 || len(cs) < period+1 {
		return out
	}

	var seed float64
	for i := 1; i <= period; i++ {
		seed += tr[i]
	}
	prev := seed / float64(period)
	out[period] = prev

	for i := period + 1; i < len(cs); i++ {
		prev = (prev*float64(period-1) + tr[i]) / float64(period)
		out[i] = prev
	}
	return out
}

// RSI is Wilder's relative strength index, on a 0-100 scale.
func RSI(values []float64, period int) []float64 {
	out := nanSlice(len(values))
	if period <= 0 || len(values) < period+1 {
		return out
	}

	var gain, loss float64
	for i := 1; i <= period; i++ {
		switch d := values[i] - values[i-1]; {
		case d > 0:
			gain += d
		default:
			loss -= d
		}
	}
	avgGain, avgLoss := gain/float64(period), loss/float64(period)
	out[period] = rsiFrom(avgGain, avgLoss)

	for i := period + 1; i < len(values); i++ {
		d := values[i] - values[i-1]
		var g, l float64
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		avgGain = (avgGain*float64(period-1) + g) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + l) / float64(period)
		out[i] = rsiFrom(avgGain, avgLoss)
	}
	return out
}

func rsiFrom(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		if avgGain == 0 {
			return 50 // flat series: neither overbought nor oversold
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}

// Channel is a Donchian channel: the highest high and lowest low over a
// trailing window, and the midpoint between them.
type Channel struct {
	Upper  []float64
	Lower  []float64
	Middle []float64
}

// Donchian computes the channel over the trailing period bars.
//
// The window excludes the current bar. A breakout strategy that includes it can
// never fire: the current bar's own high is by definition not greater than the
// highest high of a window containing it.
func Donchian(cs []domain.Candle, period int) Channel {
	ch := Channel{Upper: nanSlice(len(cs)), Lower: nanSlice(len(cs)), Middle: nanSlice(len(cs))}
	if period <= 0 || len(cs) <= period {
		return ch
	}
	for i := period; i < len(cs); i++ {
		hi, lo := float64(cs[i-period].High), float64(cs[i-period].Low)
		for j := i - period + 1; j < i; j++ {
			hi = math.Max(hi, float64(cs[j].High))
			lo = math.Min(lo, float64(cs[j].Low))
		}
		ch.Upper[i], ch.Lower[i] = hi, lo
		ch.Middle[i] = (hi + lo) / 2
	}
	return ch
}

// VWAP is the volume-weighted average price, accumulated from the start of
// each session.
//
// Sessions are delimited by newSession, which reports whether the bar at index
// i opens a new session. Passing a function rather than a calendar keeps this
// package free of a calendar dependency and lets a backtest group bars however
// its data is shaped.
func VWAP(cs []domain.Candle, newSession func(i int) bool) []float64 {
	out := nanSlice(len(cs))
	var pv, vol float64
	for i, c := range cs {
		if i == 0 || newSession(i) {
			pv, vol = 0, 0
		}
		typical := float64(c.High+c.Low+c.Close) / 3
		v := float64(c.Volume)
		pv += typical * v
		vol += v
		if vol > 0 {
			out[i] = pv / vol
		} else {
			// A zero-volume session has no volume-weighted price. Fall
			// back to the typical price rather than dividing by zero,
			// which is what an illiquid first bar would otherwise do.
			out[i] = typical
		}
	}
	return out
}

// Pivots is the central pivot range and the classic support and resistance
// levels derived from one completed session.
type Pivots struct {
	Pivot      money.Money
	BC, TC     money.Money // central pivot range bottom and top
	R1, R2, R3 money.Money
	S1, S2, S3 money.Money
}

// CPR computes the pivot levels for the session following the given candle.
//
// BC and TC are ordered so that BC is always the lower of the two; the raw
// formulas produce them in either order depending on where the close sat, and
// a caller comparing price to "the top of the range" needs them sorted.
func CPR(prev domain.Candle) Pivots {
	h, l, c := float64(prev.High), float64(prev.Low), float64(prev.Close)
	pivot := (h + l + c) / 3
	bc := (h + l) / 2
	tc := pivot - bc + pivot
	if bc > tc {
		bc, tc = tc, bc
	}
	return Pivots{
		Pivot: ToMoney(pivot),
		BC:    ToMoney(bc),
		TC:    ToMoney(tc),
		R1:    ToMoney(2*pivot - l),
		S1:    ToMoney(2*pivot - h),
		R2:    ToMoney(pivot + (h - l)),
		S2:    ToMoney(pivot - (h - l)),
		R3:    ToMoney(h + 2*(pivot-l)),
		S3:    ToMoney(l - 2*(h-pivot)),
	}
}

// ADX is Wilder's average directional index together with the directional
// indicators it is built from, all on a 0-100 scale.
type ADX struct {
	ADX     []float64
	PlusDI  []float64
	MinusDI []float64
}

// ComputeADX returns the ADX and directional indicators over the given period.
func ComputeADX(cs []domain.Candle, period int) ADX {
	n := len(cs)
	res := ADX{ADX: nanSlice(n), PlusDI: nanSlice(n), MinusDI: nanSlice(n)}
	if period <= 0 || n < 2*period+1 {
		return res
	}

	tr := TrueRange(cs)
	plusDM, minusDM := make([]float64, n), make([]float64, n)
	for i := 1; i < n; i++ {
		up := float64(cs[i].High - cs[i-1].High)
		down := float64(cs[i-1].Low - cs[i].Low)
		if up > down && up > 0 {
			plusDM[i] = up
		}
		if down > up && down > 0 {
			minusDM[i] = down
		}
	}

	var sTR, sPlus, sMinus float64
	for i := 1; i <= period; i++ {
		sTR += tr[i]
		sPlus += plusDM[i]
		sMinus += minusDM[i]
	}

	dx := nanSlice(n)
	setDI := func(i int) {
		if sTR == 0 {
			res.PlusDI[i], res.MinusDI[i], dx[i] = 0, 0, 0
			return
		}
		p := 100 * sPlus / sTR
		m := 100 * sMinus / sTR
		res.PlusDI[i], res.MinusDI[i] = p, m
		if p+m == 0 {
			dx[i] = 0
		} else {
			dx[i] = 100 * math.Abs(p-m) / (p + m)
		}
	}
	setDI(period)

	for i := period + 1; i < n; i++ {
		sTR = sTR - sTR/float64(period) + tr[i]
		sPlus = sPlus - sPlus/float64(period) + plusDM[i]
		sMinus = sMinus - sMinus/float64(period) + minusDM[i]
		setDI(i)
	}

	// ADX is Wilder's average of DX, first available a further period on.
	first := 2 * period
	var seed float64
	for i := period; i <= first; i++ {
		seed += dx[i]
	}
	prev := seed / float64(period+1)
	res.ADX[first] = prev
	for i := first + 1; i < n; i++ {
		prev = (prev*float64(period-1) + dx[i]) / float64(period)
		res.ADX[i] = prev
	}
	return res
}
