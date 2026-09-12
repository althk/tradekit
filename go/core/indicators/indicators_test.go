package indicators

import (
	"math"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

const tol = 1e-9

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// bars builds candles from parallel high/low/close series, in minor units.
func bars(high, low, close []float64) []domain.Candle {
	cs := make([]domain.Candle, len(close))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range close {
		cs[i] = domain.Candle{
			Start:  base.AddDate(0, 0, i),
			High:   money.Money(high[i]),
			Low:    money.Money(low[i]),
			Close:  money.Money(close[i]),
			Volume: 100,
		}
	}
	return cs
}

func TestSMA(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	got := SMA(v, 3)
	for i := 0; i < 2; i++ {
		if IsValid(got[i]) {
			t.Errorf("SMA warm-up index %d should be NaN, got %v", i, got[i])
		}
	}
	approx(t, "SMA[2]", got[2], 2)
	approx(t, "SMA[3]", got[3], 3)
	approx(t, "SMA[4]", got[4], 4)
}

func TestSMAShortInput(t *testing.T) {
	got := SMA([]float64{1, 2}, 5)
	for i, v := range got {
		if IsValid(v) {
			t.Errorf("index %d should be NaN when the series is shorter than the period, got %v", i, v)
		}
	}
}

func TestEMASeedsWithSimpleAverage(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	got := EMA(v, 3)
	if IsValid(got[1]) {
		t.Error("EMA must be NaN before the seed window completes")
	}
	// Seed is the mean of the first three values.
	approx(t, "EMA[2]", got[2], 2)
	// k = 2/(3+1) = 0.5; next = (4-2)*0.5 + 2 = 3.
	approx(t, "EMA[3]", got[3], 3)
	// next = (5-3)*0.5 + 3 = 4.
	approx(t, "EMA[4]", got[4], 4)
}

func TestTrueRangeUsesPreviousClose(t *testing.T) {
	cs := bars(
		[]float64{110, 130, 120},
		[]float64{90, 115, 100},
		[]float64{100, 120, 110},
	)
	tr := TrueRange(cs)
	if IsValid(tr[0]) {
		t.Error("the first bar has no previous close and must be NaN")
	}
	// Bar 1 gaps up: high-low is 15, but high - prevClose is 30.
	approx(t, "TR[1]", tr[1], 30)
	// Bar 2: high-low 20, |high-prevClose| 0, |low-prevClose| 20 -> 20.
	approx(t, "TR[2]", tr[2], 20)
}

func TestATRWilderSmoothing(t *testing.T) {
	// Every true range is exactly 10, so ATR must be 10 throughout.
	n := 20
	high, low, cl := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		cl[i] = 100
		high[i] = 105
		low[i] = 95
	}
	cs := bars(high, low, cl)
	atr := ATR(cs, 5)
	if IsValid(atr[4]) {
		t.Error("ATR needs period+1 bars: index 4 must still be NaN for period 5")
	}
	for i := 5; i < n; i++ {
		approx(t, "ATR", atr[i], 10)
	}
}

func TestATRIsNotAnEMA(t *testing.T) {
	// A single spike must decay more slowly under Wilder smoothing than
	// under an EMA of the same period. This is the substitution that
	// silently tightens every ATR-derived stop.
	n := 30
	high, low, cl := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		cl[i], high[i], low[i] = 100, 101, 99
	}
	high[10], low[10] = 150, 50 // one very wide bar
	cs := bars(high, low, cl)

	atr := ATR(cs, 14)
	ema := EMA(TrueRange(cs)[1:], 14)
	if !IsValid(atr[n-1]) || !IsValid(ema[len(ema)-1]) {
		t.Fatal("both series should have warmed up by the end")
	}
	if atr[n-1] <= ema[len(ema)-1] {
		t.Errorf("Wilder ATR (%v) should decay more slowly than EMA (%v)", atr[n-1], ema[len(ema)-1])
	}
}

func TestRSIBounds(t *testing.T) {
	rising := make([]float64, 30)
	for i := range rising {
		rising[i] = float64(100 + i)
	}
	got := RSI(rising, 14)
	approx(t, "RSI of a monotonically rising series", got[len(got)-1], 100)

	falling := make([]float64, 30)
	for i := range falling {
		falling[i] = float64(200 - i)
	}
	got = RSI(falling, 14)
	approx(t, "RSI of a monotonically falling series", got[len(got)-1], 0)
}

func TestRSIFlatSeriesIsNeutral(t *testing.T) {
	flat := make([]float64, 30)
	for i := range flat {
		flat[i] = 100
	}
	got := RSI(flat, 14)
	approx(t, "RSI of a flat series", got[len(got)-1], 50)
}

func TestDonchianExcludesCurrentBar(t *testing.T) {
	high := []float64{10, 12, 11, 20}
	low := []float64{5, 6, 4, 3}
	cl := []float64{8, 9, 7, 15}
	ch := Donchian(bars(high, low, cl), 3)

	if IsValid(ch.Upper[2]) {
		t.Error("Donchian must be NaN until period bars precede the current one")
	}
	// Window for index 3 is bars 0..2, so the current bar's own 20 high and
	// 3 low are excluded. Including them would make a breakout impossible.
	approx(t, "Donchian upper", ch.Upper[3], 12)
	approx(t, "Donchian lower", ch.Lower[3], 4)
	approx(t, "Donchian middle", ch.Middle[3], 8)
}

func TestVWAPResetsEachSession(t *testing.T) {
	cs := bars(
		[]float64{100, 100, 200, 200},
		[]float64{100, 100, 200, 200},
		[]float64{100, 100, 200, 200},
	)
	// Bars 0-1 are one session, bars 2-3 another.
	got := VWAP(cs, func(i int) bool { return i == 2 })
	approx(t, "VWAP[1]", got[1], 100)
	// Without a reset the running average at index 2 would be 133.33.
	approx(t, "VWAP[2] after reset", got[2], 200)
	approx(t, "VWAP[3]", got[3], 200)
}

func TestVWAPZeroVolumeFallsBackToTypicalPrice(t *testing.T) {
	cs := bars([]float64{110}, []float64{90}, []float64{100})
	cs[0].Volume = 0
	got := VWAP(cs, func(int) bool { return false })
	approx(t, "VWAP with zero volume", got[0], 100)
}

func TestCPROrdersCentralRange(t *testing.T) {
	// close below the midpoint puts the raw TC below BC; they must come back
	// sorted so "top of the range" means what it says.
	p := CPR(domain.Candle{High: 12000, Low: 10000, Close: 10100})
	if p.BC > p.TC {
		t.Errorf("BC (%s) must not exceed TC (%s)", p.BC, p.TC)
	}
	// pivot = (12000 + 10000 + 10100)/3 = 10700
	if got, want := p.Pivot, money.Money(10700); got != want {
		t.Errorf("Pivot = %d, want %d", got, want)
	}
	// BC is the high-low midpoint, 11000; TC mirrors the pivot across it,
	// giving 10400, so sorting is what puts them in the stated order.
	if p.BC != 10400 || p.TC != 11000 {
		t.Errorf("central range = (%s, %s), want (104.00, 110.00)", p.BC, p.TC)
	}
}

func TestADXRangesAndTrendDetection(t *testing.T) {
	n := 60
	high, low, cl := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		base := float64(100 + i) // a clean uptrend
		cl[i], high[i], low[i] = base, base+1, base-1
	}
	a := ComputeADX(bars(high, low, cl), 14)
	last := a.ADX[n-1]
	if !IsValid(last) {
		t.Fatal("ADX should have warmed up by bar 60")
	}
	if last < 0 || last > 100 {
		t.Errorf("ADX out of range: %v", last)
	}
	if last < 50 {
		t.Errorf("a clean monotonic uptrend should register a strong ADX, got %v", last)
	}
	if a.PlusDI[n-1] <= a.MinusDI[n-1] {
		t.Error("+DI should exceed -DI in an uptrend")
	}
}

func TestToMoneyAndIsValid(t *testing.T) {
	if got := ToMoney(1234.5); got != 1235 {
		t.Errorf("ToMoney(1234.5) = %d, want 1235 (half away from zero)", got)
	}
	if got := ToMoney(math.NaN()); got != 0 {
		t.Errorf("ToMoney(NaN) = %d, want 0", got)
	}
	if IsValid(math.NaN()) {
		t.Error("NaN must not be reported valid")
	}
	if !IsValid(0) {
		t.Error("zero is a legitimate indicator value and must be reported valid")
	}
}
