package fyers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// The shared golden cases for the FYERS mapping, from
// contracts/testdata/parity.json.
//
// py/tests/test_fyers.py runs the same block. A disagreement between the two
// clients about any value here is a failing build rather than a discovery six
// months later: a symbol one formats and the other cannot parse, or a status
// one calls terminal and the other calls open, is a Python screener and a Go
// executor disagreeing about the same live order.
type fyersFixture struct {
	Fyers struct {
		Symbol []struct {
			Exchange string `json:"exchange"`
			Symbol   string `json:"symbol"`
			Want     string `json:"want"`
		} `json:"symbol"`

		Key []struct {
			Symbol   string `json:"symbol"`
			Segment  int    `json:"segment"`
			Exchange string `json:"exchange"`
			Want     string `json:"want"`
		} `json:"key"`

		Status []struct {
			Wire int    `json:"wire"`
			Want string `json:"want"`
		} `json:"status"`

		Product []struct {
			Domain string `json:"domain"`
			Wire   string `json:"wire"`
		} `json:"product"`

		ProductUnsupported []string `json:"product_unsupported"`

		FromProduct []struct {
			Wire string `json:"wire"`
			Want string `json:"want"`
		} `json:"from_product"`

		OrderType []struct {
			Domain string `json:"domain"`
			Wire   int    `json:"wire"`
		} `json:"order_type"`

		Side []struct {
			Domain string `json:"domain"`
			Wire   int    `json:"wire"`
		} `json:"side"`

		Validity []struct {
			Domain string `json:"domain"`
			Wire   string `json:"wire"`
		} `json:"validity"`

		ValidityUnsupported []string `json:"validity_unsupported"`

		Resolution []struct {
			Timeframe  string `json:"timeframe"`
			Resolution string `json:"resolution"`
			ChunkDays  int    `json:"chunk_days"`
		} `json:"resolution"`

		ResolutionUnsupported []string `json:"resolution_unsupported"`

		Paise []struct {
			Rupees float64     `json:"rupees"`
			Want   money.Money `json:"want"`
		} `json:"paise"`

		GTTLegs []struct {
			Side   string      `json:"side"`
			Stop   money.Money `json:"stop"`
			Target money.Money `json:"target"`
			Leg1   money.Money `json:"leg1"`
			Leg2   money.Money `json:"leg2"`
		} `json:"gtt_legs"`

		OrderTime []struct {
			Wire string `json:"wire"`
			Want string `json:"want"`
		} `json:"order_time"`
	} `json:"fyers"`
}

func loadParity(t *testing.T) fyersFixture {
	t.Helper()
	path := filepath.Join("..", "..", "contracts", "testdata", "parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shared fixture: %v", err)
	}
	var f fyersFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding the shared fixture: %v", err)
	}
	if len(f.Fyers.Symbol) == 0 {
		t.Fatal("the fixture carries no symbol cases; the fyers block is missing or misnamed")
	}
	return f
}

func TestParitySymbol(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Symbol {
		got := symbolFor(domain.InstrumentKey{Exchange: c.Exchange, Symbol: c.Symbol})
		if got != c.Want {
			t.Errorf("symbolFor(%q, %q) = %q, want %q", c.Exchange, c.Symbol, got, c.Want)
		}
	}
}

func TestParityKey(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Key {
		got := keyFor(c.Symbol, c.Segment)
		if got.Exchange != c.Exchange || got.Symbol != c.Want {
			t.Errorf("keyFor(%q, %d) = %v, want {%s %s}", c.Symbol, c.Segment, got, c.Exchange, c.Want)
		}
	}
}

func TestParityStatus(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Status {
		if got := normalizeStatus(c.Wire); string(got) != c.Want {
			t.Errorf("normalizeStatus(%d) = %q, want %q", c.Wire, got, c.Want)
		}
	}
}

func TestParityProduct(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Product {
		got, err := toProduct(domain.Product(c.Domain))
		if err != nil || got != c.Wire {
			t.Errorf("toProduct(%q) = %q, %v; want %q", c.Domain, got, err, c.Wire)
		}
	}
	for _, p := range f.Fyers.ProductUnsupported {
		if _, err := toProduct(domain.Product(p)); err == nil {
			t.Errorf("toProduct(%q) must fail; both clients refuse it", p)
		}
	}
	for _, c := range f.Fyers.FromProduct {
		if got := fromProduct(c.Wire); string(got) != c.Want {
			t.Errorf("fromProduct(%q) = %q, want %q", c.Wire, got, c.Want)
		}
	}
}

func TestParityOrderTypeSideAndValidity(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.OrderType {
		got, err := toOrderType(domain.OrderType(c.Domain))
		if err != nil || got != c.Wire {
			t.Errorf("toOrderType(%q) = %d, %v; want %d", c.Domain, got, err, c.Wire)
		}
		if back := fromOrderType(c.Wire); string(back) != c.Domain {
			t.Errorf("fromOrderType(%d) = %q, want %q", c.Wire, back, c.Domain)
		}
	}
	for _, c := range f.Fyers.Side {
		if got := toSide(domain.Side(c.Domain)); got != c.Wire {
			t.Errorf("toSide(%q) = %d, want %d", c.Domain, got, c.Wire)
		}
		if back := fromSide(c.Wire); string(back) != c.Domain {
			t.Errorf("fromSide(%d) = %q, want %q", c.Wire, back, c.Domain)
		}
	}
	for _, c := range f.Fyers.Validity {
		got, err := toValidity(domain.TimeInForce(c.Domain))
		if err != nil || got != c.Wire {
			t.Errorf("toValidity(%q) = %q, %v; want %q", c.Domain, got, err, c.Wire)
		}
	}
	for _, v := range f.Fyers.ValidityUnsupported {
		if _, err := toValidity(domain.TimeInForce(v)); err == nil {
			t.Errorf("toValidity(%q) must fail; both clients refuse it", v)
		}
	}
}

func TestParityResolution(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Resolution {
		tf := domain.Timeframe(c.Timeframe)
		got, err := toResolution(tf)
		if err != nil || got != c.Resolution {
			t.Errorf("toResolution(%q) = %q, %v; want %q", c.Timeframe, got, err, c.Resolution)
		}
		if days := chunkDays(tf); days != c.ChunkDays {
			t.Errorf("chunkDays(%q) = %d, want %d", c.Timeframe, days, c.ChunkDays)
		}
	}
	for _, tf := range f.Fyers.ResolutionUnsupported {
		if _, err := toResolution(domain.Timeframe(tf)); err == nil {
			t.Errorf("toResolution(%q) must fail; both clients refuse it", tf)
		}
	}
}

func TestParityPaise(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.Paise {
		if got := paise(c.Rupees); got != c.Want {
			t.Errorf("paise(%v) = %d, want %d", c.Rupees, got, c.Want)
		}
	}
}

func TestParityGTTLegs(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.GTTLegs {
		info, err := gttLegs(domain.Protective{Side: domain.Side(c.Side), Quantity: 1, Stop: c.Stop, Target: c.Target})
		if err != nil {
			t.Errorf("gttLegs(%s stop=%d target=%d) failed: %v", c.Side, c.Stop, c.Target, err)
			continue
		}
		if paise(info.Leg1.TriggerPrice) != c.Leg1 {
			t.Errorf("gttLegs(%s stop=%d target=%d) leg1 = %v, want %d", c.Side, c.Stop, c.Target, info.Leg1.TriggerPrice, c.Leg1)
		}
		var leg2 money.Money
		if info.Leg2 != nil {
			leg2 = paise(info.Leg2.TriggerPrice)
		}
		if leg2 != c.Leg2 {
			t.Errorf("gttLegs(%s stop=%d target=%d) leg2 = %d, want %d", c.Side, c.Stop, c.Target, leg2, c.Leg2)
		}
	}
}

func TestParityOrderTime(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Fyers.OrderTime {
		got := parseOrderTime(c.Wire)
		if c.Want == "" {
			if !got.IsZero() {
				t.Errorf("parseOrderTime(%q) = %v, want the zero time", c.Wire, got)
			}
			continue
		}
		want, err := time.Parse(time.RFC3339, c.Want)
		if err != nil {
			t.Fatalf("fixture carries an unparseable want %q: %v", c.Want, err)
		}
		if !got.Equal(want) {
			t.Errorf("parseOrderTime(%q) = %v, want %v", c.Wire, got, want)
		}
	}
}
