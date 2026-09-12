package bars

import (
	"math"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/calendar"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

// session returns NSE, and the day's open, for a test that needs both.
func session(t *testing.T) (calendar.Session, time.Time) {
	t.Helper()
	s := calendar.NSE()
	open := time.Date(2025, 4, 17, 9, 15, 0, 0, s.Location)
	return s, open
}

func TestBuilderEmitsOneBarPerBucket(t *testing.T) {
	s, open := session(t)
	var emitted []domain.Candle
	b := NewBuilder(s, domain.M5, func(c domain.Candle) { emitted = append(emitted, c) })

	// Three ticks inside the first bar, one in the second.
	for _, tick := range []domain.Tick{
		{Key: reliance, At: open, Price: money.MustParse("100.00"), Volume: 10},
		{Key: reliance, At: open.Add(2 * time.Minute), Price: money.MustParse("102.00"), Volume: 5},
		{Key: reliance, At: open.Add(4 * time.Minute), Price: money.MustParse("99.00"), Volume: 5},
		{Key: reliance, At: open.Add(5 * time.Minute), Price: money.MustParse("101.00"), Volume: 7},
	} {
		b.Add(tick)
	}

	if len(emitted) != 1 {
		t.Fatalf("crossing into the second bucket must close exactly one bar, got %d", len(emitted))
	}
	first := emitted[0]
	if !first.Start.Equal(open) {
		t.Errorf("the first bar is anchored to the session open %v, got %v", open, first.Start)
	}
	if first.Open != money.MustParse("100.00") || first.Close != money.MustParse("99.00") {
		t.Errorf("open is the first tick and close the last: got %s/%s", first.Open, first.Close)
	}
	if first.High != money.MustParse("102.00") || first.Low != money.MustParse("99.00") {
		t.Errorf("high/low wrong: %s/%s", first.High, first.Low)
	}
	if first.Volume != 20 {
		t.Errorf("volume must sum the ticks in the bar, got %d", first.Volume)
	}
}

func TestPreOpenTickLandsInTheFirstBarNotAPhantomOne(t *testing.T) {
	s, open := session(t)
	var emitted []domain.Candle
	b := NewBuilder(s, domain.M5, func(c domain.Candle) { emitted = append(emitted, c) })

	b.Add(domain.Tick{Key: reliance, At: open.Add(-30 * time.Minute), Price: money.MustParse("98.00"), Volume: 1})
	b.Add(domain.Tick{Key: reliance, At: open.Add(time.Minute), Price: money.MustParse("100.00"), Volume: 1})
	b.Flush()

	if len(emitted) != 1 {
		t.Fatalf("a pre-open print must not create a bar the exchange never had; got %d bars", len(emitted))
	}
	if !emitted[0].Start.Equal(open) {
		t.Errorf("the pre-open tick must clamp to the open, got a bar starting %v", emitted[0].Start)
	}
	if emitted[0].Open != money.MustParse("98.00") {
		t.Errorf("the pre-open print is still the first trade of the bar, got open %s", emitted[0].Open)
	}
}

func TestFlushEmitsThePartialBar(t *testing.T) {
	s, open := session(t)
	var emitted []domain.Candle
	b := NewBuilder(s, domain.M5, func(c domain.Candle) { emitted = append(emitted, c) })

	b.Add(domain.Tick{Key: reliance, At: open, Price: money.MustParse("100.00"), Volume: 1})
	if len(emitted) != 0 {
		t.Fatal("a bar must not be emitted before its bucket closes")
	}
	b.Flush()
	if len(emitted) != 1 {
		t.Fatalf("without Flush the session's last bar is never emitted, and that is the bar an EOD exit is priced from; got %d", len(emitted))
	}
	b.Flush()
	if len(emitted) != 1 {
		t.Error("a second Flush must emit nothing; a duplicated bar double-counts the session's volume")
	}
}

func TestBuilderFoldsAnOutOfOrderTickIntoTheOpenBar(t *testing.T) {
	s, open := session(t)
	var emitted []domain.Candle
	b := NewBuilder(s, domain.M5, func(c domain.Candle) { emitted = append(emitted, c) })

	b.Add(domain.Tick{Key: reliance, At: open.Add(6 * time.Minute), Price: money.MustParse("101.00"), Volume: 1})
	// Arrives late, stamped in the previous bucket.
	b.Add(domain.Tick{Key: reliance, At: open.Add(time.Minute), Price: money.MustParse("105.00"), Volume: 1})
	b.Flush()

	if len(emitted) != 1 {
		t.Fatalf("a reordered feed must not re-open a bucket already emitted; got %d bars", len(emitted))
	}
	if emitted[0].High != money.MustParse("105.00") {
		t.Errorf("the late tick must still be counted, got high %s", emitted[0].High)
	}
}

// series builds five-minute bars from the session open.
func series(open time.Time, closes ...int64) []domain.Candle {
	out := make([]domain.Candle, 0, len(closes))
	for i, c := range closes {
		price := money.Money(c)
		out = append(out, domain.Candle{
			Key:       reliance,
			Timeframe: domain.M5,
			Start:     open.Add(time.Duration(i) * 5 * time.Minute),
			Open:      price,
			High:      price + 100,
			Low:       price - 100,
			Close:     price,
			Volume:    int64(100 * (i + 1)),
		})
	}
	return out
}

func TestResample5mTo15m(t *testing.T) {
	s, open := session(t)
	in := series(open, 10000, 10100, 10200, 10300, 10400, 10500)

	out, err := Resample(in, domain.M15, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("six 5-minute bars are two 15-minute bars, got %d", len(out))
	}

	first := out[0]
	if first.Open != in[0].Open {
		t.Errorf("the group's open is its first bar's open, got %s want %s", first.Open, in[0].Open)
	}
	if first.Close != in[2].Close {
		t.Errorf("the group's close is its last bar's close, got %s want %s", first.Close, in[2].Close)
	}
	if first.High != in[2].High {
		t.Errorf("the group's high is the extreme of its bars, got %s want %s", first.High, in[2].High)
	}
	if first.Low != in[0].Low {
		t.Errorf("the group's low is the extreme of its bars, got %s want %s", first.Low, in[0].Low)
	}
	if want := in[0].Volume + in[1].Volume + in[2].Volume; first.Volume != want {
		t.Errorf("volume must sum, got %d want %d", first.Volume, want)
	}
	if !first.Start.Equal(open) {
		t.Errorf("the group is anchored to the session open, got %v", first.Start)
	}
	if first.Timeframe != domain.M15 {
		t.Errorf("the result must carry the target timeframe, got %q", first.Timeframe)
	}
}

func TestResampleToAFinerTimeframeIsAnError(t *testing.T) {
	s, open := session(t)
	in := series(open, 10000, 10100)
	if _, err := Resample(in, domain.M1, s); err == nil {
		t.Error("a 5-minute bar carries no information about its third minute; interpolating one lets a backtest trade on data that never existed")
	}
}

func TestResampleRefusesTimeframesThatDoNotDivideEvenly(t *testing.T) {
	s, open := session(t)
	in := series(open, 10000, 10100)

	// 3m into 5m is coarser but not a whole multiple, so a target bar would
	// be built from a partial set of source bars — a 5-minute bar whose
	// high is really the high of six minutes.
	for i := range in {
		in[i].Timeframe = domain.M3
	}
	if _, err := Resample(in, domain.M5, s); err == nil {
		t.Error("3m does not divide 5m evenly; resampling it would build a bar from the wrong span")
	}

	for _, tc := range []struct{ from, to domain.Timeframe }{
		{domain.M5, domain.M15},
		{domain.M15, domain.M30},
		{domain.M15, domain.M60},
		{domain.M30, domain.M60},
	} {
		for i := range in {
			in[i].Timeframe = tc.from
		}
		if _, err := Resample(in, tc.to, s); err != nil {
			t.Errorf("%s divides %s evenly and must be allowed: %v", tc.from, tc.to, err)
		}
	}
}

func TestResampleOpenInterestTakesTheLastValueNotTheSum(t *testing.T) {
	s, open := session(t)
	in := series(open, 10000, 10100, 10200)
	for i := range in {
		in[i].OpenInterest = 5000
	}
	out, err := Resample(in, domain.M15, s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out[0].OpenInterest != 5000 {
		t.Errorf("open interest is a level, not a flow; summing it multiplies the outstanding contracts by the bar count. got %d", out[0].OpenInterest)
	}
}

// sessionBars builds one session's worth of 5-minute bars with a fixed volume
// per bar, so a cumulative profile is easy to reason about.
func sessionBars(s calendar.Session, day time.Time, perBar int64, slots int) []domain.Candle {
	open := s.Open(day)
	out := make([]domain.Candle, 0, slots)
	for i := 0; i < slots; i++ {
		out = append(out, domain.Candle{
			Key:       reliance,
			Timeframe: domain.M5,
			Start:     open.Add(time.Duration(i) * 5 * time.Minute),
			Close:     money.MustParse("100.00"),
			Volume:    perBar,
		})
	}
	return out
}

func TestVolumeProfileExcludesTheCurrentSession(t *testing.T) {
	s := calendar.NSE()
	// Four sessions of 100 per bar, then one of 10000 per bar. The busy
	// session must not raise its own benchmark.
	var candles []domain.Candle
	days := []time.Time{
		time.Date(2025, 4, 14, 0, 0, 0, 0, s.Location),
		time.Date(2025, 4, 15, 0, 0, 0, 0, s.Location),
		time.Date(2025, 4, 16, 0, 0, 0, 0, s.Location),
		time.Date(2025, 4, 17, 0, 0, 0, 0, s.Location),
		time.Date(2025, 4, 18, 0, 0, 0, 0, s.Location),
	}
	for i, day := range days {
		perBar := int64(100)
		if i == len(days)-1 {
			perBar = 10000
		}
		candles = append(candles, sessionBars(s, day, perBar, 3)...)
	}

	profile := VolumeProfile(candles, s, domain.M5, 4)

	// The last session's first bar: its benchmark is the median cumulative
	// volume at slot 0 across the four quiet sessions, which is 100.
	last := len(candles) - 3
	if got := profile[last]; got != 100 {
		t.Errorf("the busy session's own volume must not enter its own benchmark — that is the lookahead bug the whole function guards; got %v want 100", got)
	}
	// Its third bar's benchmark is the cumulative 300, not 30000.
	if got := profile[last+2]; got != 300 {
		t.Errorf("the benchmark is cumulative through the slot, from prior sessions only; got %v want 300", got)
	}
}

func TestVolumeProfileIsNaNDuringWarmUp(t *testing.T) {
	s := calendar.NSE()
	candles := sessionBars(s, time.Date(2025, 4, 17, 0, 0, 0, 0, s.Location), 100, 3)

	profile := VolumeProfile(candles, s, domain.M5, 20)
	for i, v := range profile {
		if !math.IsNaN(v) {
			t.Errorf("bar %d has no prior sessions to benchmark against and must be NaN, not %v — a zero benchmark makes relative volume infinite", i, v)
		}
	}
}

func TestRelativeVolumeIsOneAtTheNormalPace(t *testing.T) {
	s := calendar.NSE()
	var candles []domain.Candle
	for day := 14; day <= 18; day++ {
		candles = append(candles, sessionBars(s, time.Date(2025, 4, day, 0, 0, 0, 0, s.Location), 100, 3)...)
	}

	rvol := RelativeVolume(candles, s, domain.M5, 4)
	last := len(candles) - 1
	if got := rvol[last]; math.Abs(got-1.0) > 1e-9 {
		t.Errorf("a session trading at exactly its usual pace has a relative volume of 1.0, got %v", got)
	}
	if !math.IsNaN(rvol[0]) {
		t.Errorf("the first session has no benchmark and must be NaN, not %v", rvol[0])
	}
}

func TestRelativeVolumeDoublesWithTwiceTheVolume(t *testing.T) {
	s := calendar.NSE()
	var candles []domain.Candle
	for day := 14; day <= 17; day++ {
		candles = append(candles, sessionBars(s, time.Date(2025, 4, day, 0, 0, 0, 0, s.Location), 100, 3)...)
	}
	candles = append(candles, sessionBars(s, time.Date(2025, 4, 18, 0, 0, 0, 0, s.Location), 200, 3)...)

	rvol := RelativeVolume(candles, s, domain.M5, 4)
	last := len(candles) - 1
	if got := rvol[last]; math.Abs(got-2.0) > 1e-9 {
		t.Errorf("twice the usual volume by this point in the day is 2.0, got %v", got)
	}
}
