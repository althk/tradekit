package zerodha

import (
	"context"
	"strings"
	"testing"
	"time"

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

func TestNextResetIsTheFollowingSixAMIST(t *testing.T) {
	ist := istLocation()
	cases := []struct{ issued, want time.Time }{
		// Issued before the reset: dies at 06:00 the same morning, so a
		// 05:30 login is stale by the open.
		{time.Date(2026, 1, 15, 5, 30, 0, 0, ist), time.Date(2026, 1, 15, 6, 0, 0, 0, ist)},
		{time.Date(2026, 1, 15, 8, 45, 0, 0, ist), time.Date(2026, 1, 16, 6, 0, 0, 0, ist)},
		{time.Date(2026, 1, 15, 6, 0, 0, 0, ist), time.Date(2026, 1, 16, 6, 0, 0, 0, ist)},
		// A UTC stamp is judged by its IST wall clock: 00:15 UTC is 05:45 IST.
		{time.Date(2026, 1, 15, 0, 15, 0, 0, time.UTC), time.Date(2026, 1, 15, 6, 0, 0, 0, ist)},
	}
	for _, tc := range cases {
		if got := nextReset(tc.issued, tokenResetMinute); !got.Equal(tc.want) {
			t.Errorf("nextReset(%s) = %s, want %s", tc.issued, got, tc.want)
		}
	}
}
