package zerodha

import (
	"context"
	"strings"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
)

// The download itself needs Kite; these seed the cache and check what the
// client does around it.

func TestInstrumentTokenPrefersTheWiredResolver(t *testing.T) {
	c, err := New(Options{APIKey: "k", InstrumentToken: func(domain.InstrumentKey) (int, error) { return 42, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c.tokens = map[string]map[domain.InstrumentKey]int{"NSE": {{Exchange: "NSE", Symbol: "RELIANCE"}: 738561}}
	got, err := c.instrumentToken(context.Background(), domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"})
	if err != nil || got != 42 {
		t.Errorf("a resolver the caller wired must win over the built-in cache, got %d %v", got, err)
	}
}

func TestInstrumentTokenReadsTheCacheWithoutAnotherDownload(t *testing.T) {
	c, err := New(Options{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	c.tokens = map[string]map[domain.InstrumentKey]int{"NSE": {{Exchange: "NSE", Symbol: "RELIANCE"}: 738561}}

	got, err := c.instrumentToken(context.Background(), domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"})
	if err != nil || got != 738561 {
		t.Errorf("a cached exchange must resolve locally, got %d %v", got, err)
	}
	// A symbol missing from an exchange already downloaded is an answer, not
	// a reason to download again -- which, with no Kite behind this test,
	// would surface as a network error rather than this message.
	_, err = c.instrumentToken(context.Background(), domain.InstrumentKey{Exchange: "NSE", Symbol: "NOPE"})
	if err == nil || !strings.Contains(err.Error(), "no instrument token known") {
		t.Errorf("an unknown symbol on a cached exchange must say so without refetching, got %v", err)
	}
}
