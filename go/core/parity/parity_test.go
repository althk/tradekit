// Package parity runs the shared golden cases in contracts/testdata.
//
// The Python package runs the same file. A disagreement between the two
// libraries about any value in it is a failing build rather than a discovery
// six months later, which is the whole reason the contracts directory exists.
package parity

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/costs"
	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/options"
	"github.com/althk/tradekit/go/core/paper"
	"github.com/althk/tradekit/go/core/risk"
	"github.com/althk/tradekit/go/core/stats"
)

type fixture struct {
	MoneyParse []struct {
		Text string      `json:"text"`
		Want money.Money `json:"want"`
	} `json:"money_parse"`

	MoneyParseInvalid []string `json:"money_parse_invalid"`

	MoneyMulFraction []struct {
		Amount   money.Money `json:"amount"`
		Fraction float64     `json:"fraction"`
		Want     money.Money `json:"want"`
	} `json:"money_mul_fraction"`

	MoneyRoundToTick []struct {
		Amount money.Money `json:"amount"`
		Tick   money.Money `json:"tick"`
		Want   money.Money `json:"want"`
	} `json:"money_round_to_tick"`

	MoneyFloorCeilToTick []struct {
		Amount money.Money `json:"amount"`
		Tick   money.Money `json:"tick"`
		Floor  money.Money `json:"floor"`
		Ceil   money.Money `json:"ceil"`
	} `json:"money_floor_ceil_to_tick"`

	Sizing []struct {
		Name         string      `json:"name"`
		Capital      money.Money `json:"capital"`
		RiskFraction float64     `json:"risk_fraction"`
		Entry        money.Money `json:"entry"`
		Stop         money.Money `json:"stop"`
		LotSize      int         `json:"lot_size"`
		MaxNotional  money.Money `json:"max_notional"`
		Leverage     float64     `json:"leverage"`
		WantQuantity int         `json:"want_quantity"`
	} `json:"sizing"`

	Charges struct {
		Broker  string `json:"broker"`
		Segment string `json:"segment"`
		Rates   struct {
			Brokerage float64 `json:"brokerage"`
			STTBuy    float64 `json:"stt_buy"`
			STTSell   float64 `json:"stt_sell"`
			Exchange  float64 `json:"exchange"`
			SEBI      float64 `json:"sebi"`
			Stamp     float64 `json:"stamp"`
			GST       float64 `json:"gst"`
			DPFlat    float64 `json:"dp_flat"`
		} `json:"rates"`
		EffectiveFrom string `json:"effective_from"`
		Cases         []struct {
			Name       string      `json:"name"`
			Quantity   int         `json:"quantity"`
			EntryPrice money.Money `json:"entry_price"`
			ExitPrice  money.Money `json:"exit_price"`
			EntryAt    string      `json:"entry_at"`
			ExitAt     string      `json:"exit_at"`
			Buying     bool        `json:"buying"`
			Want       struct {
				Brokerage money.Money `json:"brokerage"`
				STT       money.Money `json:"stt"`
				Exchange  money.Money `json:"exchange"`
				SEBI      money.Money `json:"sebi"`
				Stamp     money.Money `json:"stamp"`
				DP        money.Money `json:"dp"`
				GST       money.Money `json:"gst"`
				Total     money.Money `json:"total"`
			} `json:"want"`
		} `json:"cases"`
	} `json:"charges"`

	Stats struct {
		NetPnLs []money.Money `json:"net_pnls"`
		Want    struct {
			Trades       int         `json:"trades"`
			Wins         int         `json:"wins"`
			Losses       int         `json:"losses"`
			GrossProfit  money.Money `json:"gross_profit"`
			GrossLoss    money.Money `json:"gross_loss"`
			NetPnL       money.Money `json:"net_pnl"`
			WinRate      float64     `json:"win_rate"`
			ProfitFactor float64     `json:"profit_factor"`
			Expectancy   money.Money `json:"expectancy"`
			AvgWin       money.Money `json:"avg_win"`
			AvgLoss      money.Money `json:"avg_loss"`
			MaxDrawdown  money.Money `json:"max_drawdown"`
		} `json:"want"`
	} `json:"stats"`
}

func load(t *testing.T) fixture {
	t.Helper()
	path := filepath.Join("..", "..", "..", "contracts", "testdata", "parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shared fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing the shared fixture: %v", err)
	}
	return f
}

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("bad date %q in fixture: %v", s, err)
	}
	return d
}

func TestMoneyParity(t *testing.T) {
	f := load(t)

	for _, c := range f.MoneyParse {
		got, err := money.Parse(c.Text)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.Text, err)
			continue
		}
		if got != c.Want {
			t.Errorf("Parse(%q) = %d, want %d", c.Text, got, c.Want)
		}
	}

	for _, s := range f.MoneyParseInvalid {
		if _, err := money.Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted an invalid amount", s)
		}
	}

	for _, c := range f.MoneyMulFraction {
		if got := c.Amount.MulFraction(c.Fraction); got != c.Want {
			t.Errorf("MulFraction(%d, %v) = %d, want %d", c.Amount, c.Fraction, got, c.Want)
		}
	}

	for _, c := range f.MoneyRoundToTick {
		if got := c.Amount.RoundToTick(c.Tick); got != c.Want {
			t.Errorf("RoundToTick(%d, %d) = %d, want %d", c.Amount, c.Tick, got, c.Want)
		}
	}

	for _, c := range f.MoneyFloorCeilToTick {
		if got := c.Amount.FloorToTick(c.Tick); got != c.Floor {
			t.Errorf("FloorToTick(%d, %d) = %d, want %d", c.Amount, c.Tick, got, c.Floor)
		}
		if got := c.Amount.CeilToTick(c.Tick); got != c.Ceil {
			t.Errorf("CeilToTick(%d, %d) = %d, want %d", c.Amount, c.Tick, got, c.Ceil)
		}
	}
}

func TestSizingParity(t *testing.T) {
	for _, c := range load(t).Sizing {
		got := risk.Size(risk.SizeParams{
			Capital:      c.Capital,
			RiskFraction: c.RiskFraction,
			Entry:        c.Entry,
			Stop:         c.Stop,
			LotSize:      c.LotSize,
			MaxNotional:  c.MaxNotional,
			Leverage:     c.Leverage,
		})
		if got.Quantity != c.WantQuantity {
			t.Errorf("%s: quantity = %d, want %d (%s)", c.Name, got.Quantity, c.WantQuantity, got.Reason)
		}
	}
}

func TestChargesParity(t *testing.T) {
	f := load(t)
	spec := f.Charges
	seg := costs.Segment(spec.Segment)
	from := mustDate(t, spec.EffectiveFrom)

	tbl := costs.NewTable()
	set := func(k costs.Kind, v float64) {
		tbl.Set(spec.Broker, seg, k, costs.Rate{Value: v, EffectiveFrom: from})
	}
	set(costs.Brokerage, spec.Rates.Brokerage)
	set(costs.STTBuy, spec.Rates.STTBuy)
	set(costs.STTSell, spec.Rates.STTSell)
	set(costs.Exchange, spec.Rates.Exchange)
	set(costs.SEBI, spec.Rates.SEBI)
	set(costs.Stamp, spec.Rates.Stamp)
	set(costs.GST, spec.Rates.GST)
	tbl.Set(spec.Broker, seg, costs.DP, costs.Rate{Value: spec.Rates.DPFlat, Flat: true, EffectiveFrom: from})

	for _, c := range spec.Cases {
		got, err := costs.Compute(tbl, costs.Trade{
			Broker:     spec.Broker,
			Segment:    seg,
			Quantity:   c.Quantity,
			EntryPrice: c.EntryPrice,
			ExitPrice:  c.ExitPrice,
			EntryAt:    mustDate(t, c.EntryAt),
			ExitAt:     mustDate(t, c.ExitAt),
			Buying:     c.Buying,
		})
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		want := costs.Charges{
			Brokerage: c.Want.Brokerage, STT: c.Want.STT, Exchange: c.Want.Exchange,
			SEBI: c.Want.SEBI, Stamp: c.Want.Stamp, DP: c.Want.DP, GST: c.Want.GST,
			Total: c.Want.Total,
		}
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.Name, got, want)
		}
	}
}

func TestStatsParity(t *testing.T) {
	f := load(t)
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	trades := make([]domain.Trade, 0, len(f.Stats.NetPnLs))
	for i, net := range f.Stats.NetPnLs {
		exit := base.AddDate(0, 0, i)
		trades = append(trades, domain.Trade{
			Key:        domain.InstrumentKey{Exchange: "NSE", Symbol: "X"},
			Side:       domain.Buy,
			Quantity:   1,
			EntryPrice: money.MustParse("100.00"),
			ExitPrice:  money.MustParse("100.00") + net,
			EntryAt:    exit.Add(-2 * time.Hour),
			ExitAt:     exit,
			GrossPnL:   net,
			NetPnL:     net,
			ExitReason: domain.ExitTarget,
		})
	}

	got, err := stats.Summarize(trades)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	w := f.Stats.Want

	checks := []struct {
		name      string
		got, want any
	}{
		{"trades", got.Trades, w.Trades},
		{"wins", got.Wins, w.Wins},
		{"losses", got.Losses, w.Losses},
		{"gross_profit", got.GrossProfit, w.GrossProfit},
		{"gross_loss", got.GrossLoss, w.GrossLoss},
		{"net_pnl", got.NetPnL, w.NetPnL},
		{"expectancy", got.Expectancy, w.Expectancy},
		{"avg_win", got.AvgWin, w.AvgWin},
		{"avg_loss", got.AvgLoss, w.AvgLoss},
		{"max_drawdown", got.MaxDrawdown, w.MaxDrawdown},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if math.Abs(got.WinRate-w.WinRate) > 1e-9 {
		t.Errorf("win_rate = %v, want %v", got.WinRate, w.WinRate)
	}
	if math.Abs(got.ProfitFactor-w.ProfitFactor) > 1e-9 {
		t.Errorf("profit_factor = %v, want %v", got.ProfitFactor, w.ProfitFactor)
	}
}

// fillFixture mirrors the "fills" section of the shared fixture.
type fillFixture struct {
	Cases []struct {
		Name       string      `json:"name"`
		Side       string      `json:"side"`
		Stop       money.Money `json:"stop"`
		Target     money.Money `json:"target"`
		Open       money.Money `json:"open"`
		High       money.Money `json:"high"`
		Low        money.Money `json:"low"`
		Close      money.Money `json:"close"`
		WantPrice  money.Money `json:"want_price"`
		WantReason string      `json:"want_reason"`
	} `json:"cases"`
}

// TestFillModelParity drives the paper broker through the shared bar-replay
// cases. The Python simulator runs the same list, so an ambiguous bar can never
// resolve one way in a Go backtest and the other way in a Python one.
func TestFillModelParity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "testdata", "parity.json"))
	if err != nil {
		t.Fatalf("reading the shared fixture: %v", err)
	}
	var wrapper struct {
		Fills fillFixture `json:"fills"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("parsing the shared fixture: %v", err)
	}
	if len(wrapper.Fills.Cases) == 0 {
		t.Fatal("no fill cases found in the shared fixture")
	}

	key := domain.InstrumentKey{Exchange: "NSE", Symbol: "X"}
	entryAt := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	for _, c := range wrapper.Fills.Cases {
		side := domain.Side(c.Side)
		// Enter at a price that sits between the stop and the target of
		// every case, so the position exists and neither leg is already
		// triggered when the bar under test arrives.
		entry := money.MustParse("100.00")

		b := paper.New(paper.Options{Cash: money.MustParse("10000000.00")})
		b.OnTick(key, entry, entryAt)
		if _, err := b.PlaceOrder(context.Background(), domain.OrderRequest{
			Key: key, Side: side, Quantity: 10, Type: domain.Market, Product: domain.CNC,
		}); err != nil {
			t.Errorf("%s: entry: %v", c.Name, err)
			continue
		}
		if _, err := b.PlaceProtective(context.Background(), domain.Protective{
			Key: key, Side: side.Opposite(), Quantity: 10, Stop: c.Stop, Target: c.Target,
		}); err != nil {
			t.Errorf("%s: protective: %v", c.Name, err)
			continue
		}

		b.OnBar(domain.Candle{
			Key: key, Timeframe: domain.D1, Start: entryAt.AddDate(0, 0, 1),
			Open: c.Open, High: c.High, Low: c.Low, Close: c.Close, Volume: 1000,
		})

		trades := b.Trades()
		if c.WantReason == "" {
			if len(trades) != 0 {
				t.Errorf("%s: got %d trades, want none", c.Name, len(trades))
			}
			continue
		}
		if len(trades) != 1 {
			t.Errorf("%s: got %d trades, want 1", c.Name, len(trades))
			continue
		}
		if got := string(trades[0].ExitReason); got != c.WantReason {
			t.Errorf("%s: reason = %s, want %s", c.Name, got, c.WantReason)
		}
		if trades[0].ExitPrice != c.WantPrice {
			t.Errorf("%s: exit = %s, want %s", c.Name, trades[0].ExitPrice, c.WantPrice)
		}
	}
}

func TestOptionsParity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "testdata", "parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Options struct {
			RiskFree float64 `json:"risk_free"`
			DivYield float64 `json:"div_yield"`
			Cases    []struct {
				Name   string  `json:"name"`
				Op     string  `json:"op"`
				S      float64 `json:"s"`
				K      float64 `json:"k"`
				T      float64 `json:"t"`
				Sigma  float64 `json:"sigma"`
				Price  float64 `json:"price"`
				Target float64 `json:"target"`
				Call   bool    `json:"call"`
				Want   float64 `json:"want"`
			} `json:"cases"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	m := options.Model{RiskFree: f.Options.RiskFree, DivYield: f.Options.DivYield}
	for _, c := range f.Options.Cases {
		var got float64
		switch c.Op {
		case "price":
			got = m.Price(c.S, c.K, c.T, c.Sigma, c.Call)
		case "delta":
			got = m.Delta(c.S, c.K, c.T, c.Sigma, c.Call)
		case "implied_vol":
			iv, ok := m.ImpliedVol(c.Price, c.S, c.K, c.T, c.Call)
			if !ok {
				t.Fatalf("%s: inversion refused", c.Name)
			}
			got = iv
		case "strike_for_delta":
			got = m.StrikeForDelta(c.S, c.T, c.Sigma, c.Target, c.Call)
		default:
			t.Fatalf("%s: unknown op %q", c.Name, c.Op)
		}
		if math.Abs(got-c.Want) > 1e-6*math.Max(1, math.Abs(c.Want)) {
			t.Errorf("%s: got %.10f, want %.10f -- the two libraries would choose different strikes", c.Name, got, c.Want)
		}
	}
}
