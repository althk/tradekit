package reference

import (
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// daily builds one bar per date, all at the same price and volume, so an
// adjustment's effect is the only thing that varies.
func daily(price money.Money, volume int64, dates ...time.Time) []domain.Candle {
	out := make([]domain.Candle, 0, len(dates))
	for _, d := range dates {
		out = append(out, domain.Candle{
			Key:       reliance,
			Timeframe: domain.D1,
			Start:     d,
			Open:      price,
			High:      price,
			Low:       price,
			Close:     price,
			Volume:    volume,
		})
	}
	return out
}

func TestSplitDividesPricesAndMultipliesVolume(t *testing.T) {
	in := daily(money.MustParse("1000.00"), 100,
		day(2025, 4, 15), day(2025, 4, 16), day(2025, 4, 17))
	actions := []Action{{
		Key: reliance, ExDate: day(2025, 4, 17), Kind: KindSplit, Ratio: 10,
	}}

	out := Adjust(in, actions)

	for i := 0; i < 2; i++ {
		if out[i].Close != money.MustParse("100.00") {
			t.Errorf("a 1:10 split divides pre-ex prices by 10, not halves them; bar %d close is %s", i, out[i].Close)
		}
		if out[i].Volume != 1000 {
			t.Errorf("the share count multiplies by 10, so volume does too; bar %d volume is %d", i, out[i].Volume)
		}
	}
	if out[2].Close != money.MustParse("1000.00") {
		t.Errorf("the ex-date's own bar already reflects the split and must not be touched; got %s", out[2].Close)
	}
	if out[2].Volume != 100 {
		t.Errorf("the ex-date's own volume must not be scaled; got %d", out[2].Volume)
	}
}

func TestBonusAdjustsLikeASplit(t *testing.T) {
	in := daily(money.MustParse("500.00"), 200, day(2025, 4, 16), day(2025, 4, 17))
	// A 1:1 bonus doubles the share count, so the ratio is 2.
	out := Adjust(in, []Action{{
		Key: reliance, ExDate: day(2025, 4, 17), Kind: KindBonus, Ratio: 2,
	}})

	if out[0].Close != money.MustParse("250.00") {
		t.Errorf("a 1:1 bonus halves the pre-ex price, got %s", out[0].Close)
	}
	if out[0].Volume != 400 {
		t.Errorf("a 1:1 bonus doubles the pre-ex volume, got %d", out[0].Volume)
	}
}

func TestDividendAdjustsPricesButNotVolume(t *testing.T) {
	in := daily(money.MustParse("100.00"), 500, day(2025, 4, 16), day(2025, 4, 17))
	out := Adjust(in, []Action{{
		Key: reliance, ExDate: day(2025, 4, 17), Kind: KindDividend,
		Ratio: 1, Amount: money.MustParse("10.00"),
	}})

	// (100 - 10) / 100 = 0.9
	if out[0].Close != money.MustParse("90.00") {
		t.Errorf("a ₹10 dividend against a ₹100 close scales prices by 0.9, got %s", out[0].Close)
	}
	if out[0].Volume != 500 {
		t.Errorf("a dividend creates no shares, so volume is unchanged; got %d", out[0].Volume)
	}
}

func TestActionsApplyOnlyBeforeTheirExDate(t *testing.T) {
	in := daily(money.MustParse("1000.00"), 100,
		day(2025, 4, 15), day(2025, 4, 16), day(2025, 4, 17), day(2025, 4, 18))
	out := Adjust(in, []Action{{
		Key: reliance, ExDate: day(2025, 4, 17), Kind: KindSplit, Ratio: 2,
	}})

	if out[2].Close != money.MustParse("1000.00") || out[3].Close != money.MustParse("1000.00") {
		t.Errorf("bars on and after the ex-date already trade at the new price; got %s and %s", out[2].Close, out[3].Close)
	}
	if out[0].Close != money.MustParse("500.00") || out[1].Close != money.MustParse("500.00") {
		t.Errorf("bars before the ex-date must be restated; got %s and %s", out[0].Close, out[1].Close)
	}
}

func TestMultipleActionsCompoundInDateOrder(t *testing.T) {
	in := daily(money.MustParse("1000.00"), 100,
		day(2025, 1, 10), day(2025, 4, 17), day(2025, 8, 20))
	// Deliberately supplied newest first: the function must order them, not
	// trust the caller.
	actions := []Action{
		{Key: reliance, ExDate: day(2025, 8, 20), Kind: KindSplit, Ratio: 2},
		{Key: reliance, ExDate: day(2025, 4, 17), Kind: KindSplit, Ratio: 2},
	}

	out := Adjust(in, actions)

	if out[0].Close != money.MustParse("250.00") {
		t.Errorf("a bar before both 1:2 splits is divided by four; got %s", out[0].Close)
	}
	if out[0].Volume != 400 {
		t.Errorf("and its volume multiplied by four; got %d", out[0].Volume)
	}
	if out[1].Close != money.MustParse("500.00") {
		t.Errorf("a bar between the two splits is divided by two only; got %s", out[1].Close)
	}
	if out[2].Close != money.MustParse("1000.00") {
		t.Errorf("the last bar keeps its real traded price; got %s", out[2].Close)
	}
}

func TestAdjustWithNoActionsReturnsTheInputUnchanged(t *testing.T) {
	in := daily(money.MustParse("1000.00"), 100, day(2025, 4, 17))
	out := Adjust(in, nil)
	if len(out) != 1 || out[0] != in[0] {
		t.Errorf("an empty action list must leave the series exactly as it was, got %+v", out)
	}
}

func TestAdjustDoesNotModifyItsInput(t *testing.T) {
	in := daily(money.MustParse("1000.00"), 100, day(2025, 4, 16), day(2025, 4, 17))
	before := in[0]
	_ = Adjust(in, []Action{{Key: reliance, ExDate: day(2025, 4, 17), Kind: KindSplit, Ratio: 10}})
	if in[0] != before {
		t.Error("a caller holding the raw series must still hold it; adjusting in place would silently restate every other user of the same slice")
	}
}

func TestAdjustComparesCalendarDatesNotInstants(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	// Intraday bars on the ex-date itself, stamped at 09:15 and 15:25.
	in := []domain.Candle{
		{Key: reliance, Timeframe: domain.M5, Start: time.Date(2025, 4, 16, 15, 25, 0, 0, loc), Close: money.MustParse("1000.00")},
		{Key: reliance, Timeframe: domain.M5, Start: time.Date(2025, 4, 17, 9, 15, 0, 0, loc), Close: money.MustParse("100.00")},
		{Key: reliance, Timeframe: domain.M5, Start: time.Date(2025, 4, 17, 15, 25, 0, 0, loc), Close: money.MustParse("101.00")},
	}
	out := Adjust(in, []Action{{Key: reliance, ExDate: day(2025, 4, 17), Kind: KindSplit, Ratio: 10}})

	if out[1].Close != money.MustParse("100.00") || out[2].Close != money.MustParse("101.00") {
		t.Error("an instant comparison would adjust the ex-date's morning bars and leave its afternoon ones alone, producing a 10x gap inside one session")
	}
	if out[0].Close != money.MustParse("100.00") {
		t.Errorf("the previous session's bars must be restated; got %s", out[0].Close)
	}
}

func TestADividendLargerThanThePriceIsIgnored(t *testing.T) {
	in := daily(money.MustParse("10.00"), 100, day(2025, 4, 16), day(2025, 4, 17))
	out := Adjust(in, []Action{{
		Key: reliance, ExDate: day(2025, 4, 17), Kind: KindDividend,
		Ratio: 1, Amount: money.MustParse("50.00"),
	}})

	if out[0].Close <= 0 {
		t.Errorf("a data error must not produce a zero or negative price, which every indicator downstream would happily compute over; got %s", out[0].Close)
	}
	if out[0].Close != money.MustParse("10.00") {
		t.Errorf("leaving the series unadjusted is the small visible error; got %s", out[0].Close)
	}
}

func TestValidateRejectsActionsThatWouldCorruptASeries(t *testing.T) {
	for name, a := range map[string]Action{
		"zero split ratio":     {Key: reliance, Kind: KindSplit, Ratio: 0},
		"negative bonus ratio": {Key: reliance, Kind: KindBonus, Ratio: -2},
		"dividend of nothing":  {Key: reliance, Kind: KindDividend, Ratio: 1},
		"unknown kind":         {Key: reliance, Kind: "buyback", Ratio: 1},
	} {
		if err := validate(a); err == nil {
			t.Errorf("%s must be refused before it reaches the store", name)
		}
	}
	if err := validate(Action{Key: reliance, Kind: KindSplit, Ratio: 10}); err != nil {
		t.Errorf("a well-formed split must be accepted: %v", err)
	}
}
