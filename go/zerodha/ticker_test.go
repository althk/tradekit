package zerodha

import (
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/zerodha/gokiteconnect/v4/models"
)

// The conversion is what is testable without a socket, and it is also where the
// mistakes live: the wrong timestamp, the wrong volume field, and a packet for
// an instrument nobody asked about.
func TestToDomainTick(t *testing.T) {
	sbin := domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}
	byToken := map[uint32]domain.InstrumentKey{779521: sbin}

	exchangeTime := time.Date(2026, 3, 4, 10, 15, 30, 0, time.UTC)
	tradeTime := time.Date(2026, 3, 4, 10, 15, 29, 0, time.UTC)
	fallback := time.Date(2026, 3, 4, 23, 59, 59, 0, time.UTC)

	t.Run("a subscribed packet converts", func(t *testing.T) {
		got, ok := toDomainTick(models.Tick{
			InstrumentToken:    779521,
			Timestamp:          models.Time{Time: exchangeTime},
			LastTradeTime:      models.Time{Time: tradeTime},
			LastPrice:          812.35,
			LastTradedQuantity: 17,
			VolumeTraded:       4_500_000,
		}, byToken, fallback)

		if !ok {
			t.Fatal("a subscribed instrument token must resolve")
		}
		if got.Key != sbin {
			t.Errorf("Key = %v, want %v", got.Key, sbin)
		}
		if !got.At.Equal(exchangeTime) {
			t.Errorf("At = %v, want the exchange timestamp %v", got.At, exchangeTime)
		}
		if got.Price != 81235 {
			t.Errorf("Price = %d paise, want 81235", got.Price)
		}
		if got.Volume != 17 {
			t.Errorf("Volume = %d, want this print's size 17, not the day's cumulative total", got.Volume)
		}
	})

	t.Run("an unsubscribed token is dropped, not emitted with a zero key", func(t *testing.T) {
		got, ok := toDomainTick(models.Tick{InstrumentToken: 999999, LastPrice: 100}, byToken, fallback)
		if ok {
			t.Fatalf("an unresolvable token must be dropped; got %+v", got)
		}
		if got != (domain.Tick{}) {
			t.Errorf("a dropped tick must be zero, got %+v", got)
		}
	})

	t.Run("a packet without an exchange timestamp falls back to the last trade time", func(t *testing.T) {
		got, ok := toDomainTick(models.Tick{
			InstrumentToken: 779521,
			LastTradeTime:   models.Time{Time: tradeTime},
			LastPrice:       812.35,
		}, byToken, fallback)
		if !ok {
			t.Fatal("the packet must still convert")
		}
		if !got.At.Equal(tradeTime) {
			t.Errorf("At = %v, want the last trade time %v", got.At, tradeTime)
		}
	})

	t.Run("a packet with no time at all takes the caller's clock, never the zero time", func(t *testing.T) {
		got, ok := toDomainTick(models.Tick{InstrumentToken: 779521, LastPrice: 812.35}, byToken, fallback)
		if !ok {
			t.Fatal("the packet must still convert")
		}
		if !got.At.Equal(fallback) {
			t.Errorf("At = %v, want the fallback clock %v; a zero time sorts before every bar in the session", got.At, fallback)
		}
	})
}

// A streamer with no instruments would open a connection and deliver nothing,
// which is indistinguishable from a broken feed.
func TestSubscribeTicksRejectsAnEmptyUniverse(t *testing.T) {
	c, err := New(Options{APIKey: "k", AccessToken: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.SubscribeTicks(t.Context(), nil, func(domain.Tick) {}); err == nil {
		t.Error("subscribing to no instruments must fail rather than open a silent connection")
	}
}

// Streaming without a token connects and is immediately closed by Kite, which
// surfaces as a reconnect loop rather than as an error the caller can act on.
func TestStreamingRequiresAnAccessToken(t *testing.T) {
	c, err := New(Options{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.SubscribeOrders(t.Context(), func(domain.Order) {}); err == nil {
		t.Error("streaming without an access token must fail at wiring time")
	}
}
