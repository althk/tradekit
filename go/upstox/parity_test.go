package upstox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// The shared golden cases for the Upstox mapping, from
// contracts/testdata/parity.json.
//
// py/tests/test_upstox.py runs the same block. A disagreement between the two
// clients about any value here is a failing build rather than a discovery six
// months later: an instrument key one formats and the other cannot parse, or a
// status one calls terminal and the other calls open, is a Python screener and
// a Go executor disagreeing about the same live order.
//
// The core parity suite cannot cover this. It lives in go/core, which must not
// depend on an adapter, so the adapter's cases are run from the adapter.
type upstoxFixture struct {
	Upstox struct {
		InstrumentKey []struct {
			Exchange string `json:"exchange"`
			ID       string `json:"id"`
			Want     string `json:"want"`
		} `json:"instrument_key"`

		ParseInstrumentKey []struct {
			Key     string `json:"key"`
			Segment string `json:"segment"`
			ID      string `json:"id"`
		} `json:"parse_instrument_key"`

		ParseInstrumentKeyInvalid []string `json:"parse_instrument_key_invalid"`

		Status []struct {
			Wire string `json:"wire"`
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
			Wire   string `json:"wire"`
		} `json:"order_type"`

		Validity []struct {
			Domain string `json:"domain"`
			Wire   string `json:"wire"`
		} `json:"validity"`

		ValidityUnsupported []string `json:"validity_unsupported"`

		Interval []struct {
			Timeframe string `json:"timeframe"`
			Unit      string `json:"unit"`
			Interval  int    `json:"interval"`
			ChunkDays int    `json:"chunk_days"`
		} `json:"interval"`

		IntervalUnsupported []string `json:"interval_unsupported"`

		Paise []struct {
			Rupees float64     `json:"rupees"`
			Want   money.Money `json:"want"`
		} `json:"paise"`

		TriggerDirection []struct {
			Side   string `json:"side"`
			Stop   string `json:"stop"`
			Target string `json:"target"`
		} `json:"trigger_direction"`
	} `json:"upstox"`
}

func loadParity(t *testing.T) upstoxFixture {
	t.Helper()
	path := filepath.Join("..", "..", "contracts", "testdata", "parity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shared fixture: %v", err)
	}
	var f upstoxFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding the shared fixture: %v", err)
	}
	return f
}

func TestParityInstrumentKey(t *testing.T) {
	f := loadParity(t)
	if len(f.Upstox.InstrumentKey) == 0 {
		t.Fatal("the fixture carries no instrument-key cases; the upstox block is missing or misnamed")
	}
	for _, c := range f.Upstox.InstrumentKey {
		got := instrumentKey(domain.InstrumentKey{Exchange: c.Exchange, Symbol: "IGNORED"}, c.ID)
		if got != c.Want {
			t.Errorf("instrumentKey(%q, %q) = %q, want %q", c.Exchange, c.ID, got, c.Want)
		}
	}
}

func TestParityParseInstrumentKey(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.ParseInstrumentKey {
		segment, id, err := parseInstrumentKey(c.Key)
		if err != nil {
			t.Errorf("parseInstrumentKey(%q) failed: %v", c.Key, err)
			continue
		}
		if segment != c.Segment || id != c.ID {
			t.Errorf("parseInstrumentKey(%q) = %q, %q; want %q, %q", c.Key, segment, id, c.Segment, c.ID)
		}
	}
	for _, bad := range f.Upstox.ParseInstrumentKeyInvalid {
		if _, _, err := parseInstrumentKey(bad); err == nil {
			t.Errorf("parseInstrumentKey(%q) must fail; both clients reject it", bad)
		}
	}
}

func TestParityStatus(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.Status {
		if got := normalizeStatus(c.Wire); string(got) != c.Want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", c.Wire, got, c.Want)
		}
	}
}

func TestParityProduct(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.Product {
		got, err := toProduct(domain.Product(c.Domain))
		if err != nil {
			t.Errorf("toProduct(%q) failed: %v", c.Domain, err)
			continue
		}
		if got != c.Wire {
			t.Errorf("toProduct(%q) = %q, want %q", c.Domain, got, c.Wire)
		}
	}
	for _, p := range f.Upstox.ProductUnsupported {
		if _, err := toProduct(domain.Product(p)); err == nil {
			t.Errorf("toProduct(%q) must fail; Upstox has no such bucket and both clients refuse it", p)
		}
	}
	for _, c := range f.Upstox.FromProduct {
		if got := fromProduct(c.Wire); string(got) != c.Want {
			t.Errorf("fromProduct(%q) = %q, want %q", c.Wire, got, c.Want)
		}
	}
}

func TestParityOrderTypeAndValidity(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.OrderType {
		got, err := toOrderType(domain.OrderType(c.Domain))
		if err != nil {
			t.Errorf("toOrderType(%q) failed: %v", c.Domain, err)
			continue
		}
		if got != c.Wire {
			t.Errorf("toOrderType(%q) = %q, want %q", c.Domain, got, c.Wire)
		}
	}
	for _, c := range f.Upstox.Validity {
		got, err := toValidity(domain.TimeInForce(c.Domain))
		if err != nil {
			t.Errorf("toValidity(%q) failed: %v", c.Domain, err)
			continue
		}
		if got != c.Wire {
			t.Errorf("toValidity(%q) = %q, want %q", c.Domain, got, c.Wire)
		}
	}
	for _, v := range f.Upstox.ValidityUnsupported {
		if _, err := toValidity(domain.TimeInForce(v)); err == nil {
			t.Errorf("toValidity(%q) must fail; both clients refuse it rather than downgrading to DAY", v)
		}
	}
}

func TestParityInterval(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.Interval {
		unit, interval, err := toInterval(domain.Timeframe(c.Timeframe))
		if err != nil {
			t.Errorf("toInterval(%q) failed: %v", c.Timeframe, err)
			continue
		}
		if unit != c.Unit || interval != c.Interval {
			t.Errorf("toInterval(%q) = %s/%d, want %s/%d", c.Timeframe, unit, interval, c.Unit, c.Interval)
		}
		if got := chunkDays(unit, interval); got != c.ChunkDays {
			t.Errorf("chunkDays(%s, %d) = %d, want %d; the two clients must chunk a backfill identically",
				unit, interval, got, c.ChunkDays)
		}
	}
	for _, tf := range f.Upstox.IntervalUnsupported {
		if _, _, err := toInterval(domain.Timeframe(tf)); err == nil {
			t.Errorf("toInterval(%q) must fail rather than build a path Upstox rejects", tf)
		}
	}
}

func TestParityPaise(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.Paise {
		if got := paise(c.Rupees); got != c.Want {
			t.Errorf("paise(%v) = %d, want %d; a rounding disagreement here is a fill price the two libraries book differently",
				c.Rupees, got, c.Want)
		}
	}
}

func TestParityTriggerDirection(t *testing.T) {
	f := loadParity(t)
	for _, c := range f.Upstox.TriggerDirection {
		side := domain.Side(c.Side)
		if got := stopDirection(side); got != c.Stop {
			t.Errorf("stopDirection(%q) = %q, want %q", c.Side, got, c.Stop)
		}
		if got := targetDirection(side); got != c.Target {
			t.Errorf("targetDirection(%q) = %q, want %q", c.Side, got, c.Target)
		}
	}
}
