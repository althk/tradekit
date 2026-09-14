package zerodha

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"
)

// SubscribeTicks streams traded prices for the given instruments.
//
// It returns as soon as the connection has been started; handle is called from
// the ticker's own goroutine, so a handler that blocks stalls the whole feed.
// Cancelling ctx closes the connection and stops the goroutines it spawned — a
// subscription that outlives its context leaks one per restart, and a strategy
// that restarts on every session change restarts often.
//
// The feed subscribes in full mode because the domain tick carries an exchange
// timestamp, and Kite's LTP mode does not send one. Stamping ticks with the
// wall clock instead would make every latency measurement read zero.
func (c *Client) SubscribeTicks(ctx context.Context, keys []domain.InstrumentKey, handle func(domain.Tick)) error {
	if len(keys) == 0 {
		return fmt.Errorf("zerodha: SubscribeTicks needs at least one instrument")
	}

	// Kite's feed speaks only numeric instrument tokens, both when
	// subscribing and on every packet it sends back, so the reverse map is
	// built once here rather than looked up per tick.
	tokens := make([]uint32, 0, len(keys))
	byToken := make(map[uint32]domain.InstrumentKey, len(keys))
	for _, k := range keys {
		token, err := c.instrumentToken(ctx, k)
		if err != nil {
			return err
		}
		tokens = append(tokens, uint32(token))
		byToken[uint32(token)] = k
	}

	t, err := c.newTicker()
	if err != nil {
		return err
	}

	t.OnConnect(func() {
		// Subscribing here rather than once at startup is what makes a
		// reconnection resume the feed: OnConnect fires again on every
		// successful reconnect, and a ticker that reconnects without
		// re-subscribing stays open and silent, which looks exactly like
		// a quiet market.
		if err := t.Subscribe(tokens); err != nil {
			slog.Error("zerodha: subscribing to tick feed", "instruments", len(tokens), "error", err)
			return
		}
		if err := t.SetMode(kiteticker.ModeFull, tokens); err != nil {
			slog.Error("zerodha: setting tick feed mode", "instruments", len(tokens), "error", err)
		}
	})
	t.OnTick(func(raw models.Tick) {
		tick, ok := toDomainTick(raw, byToken, time.Now())
		if !ok {
			return
		}
		handle(tick)
	})
	t.OnError(func(err error) {
		slog.Error("zerodha: tick feed", "error", err)
	})
	t.OnReconnect(func(attempt int, delay time.Duration) {
		slog.Warn("zerodha: tick feed reconnecting", "attempt", attempt, "delay", delay)
	})
	t.OnNoReconnect(func(attempt int) {
		slog.Error("zerodha: tick feed gave up reconnecting", "attempts", attempt)
	})

	c.serve(ctx, t)
	return nil
}

// SubscribeOrders streams order updates for the account.
//
// The updates arrive on the same WebSocket protocol as ticks but need no
// subscription, so this opens its own connection rather than sharing one with
// SubscribeTicks: the two have different lifetimes, and a strategy that
// re-subscribes its instruments intraday would otherwise drop order updates
// while it did so.
func (c *Client) SubscribeOrders(ctx context.Context, handle func(domain.Order)) error {
	t, err := c.newTicker()
	if err != nil {
		return err
	}

	t.OnOrderUpdate(func(o kiteconnect.Order) {
		handle(c.toOrder(o))
	})
	t.OnError(func(err error) {
		slog.Error("zerodha: order feed", "error", err)
	})
	t.OnReconnect(func(attempt int, delay time.Duration) {
		slog.Warn("zerodha: order feed reconnecting", "attempt", attempt, "delay", delay)
	})
	t.OnNoReconnect(func(attempt int) {
		slog.Error("zerodha: order feed gave up reconnecting", "attempts", attempt)
	})

	c.serve(ctx, t)
	return nil
}

// newTicker builds a ticker for the current credentials.
//
// The token is read under the lock rather than from Options, so a connection
// opened after a morning re-login uses the token that login produced.
func (c *Client) newTicker() (*kiteticker.Ticker, error) {
	c.mu.RLock()
	token := c.accessToken
	c.mu.RUnlock()
	if token == "" {
		return nil, fmt.Errorf("zerodha: streaming needs an access token; call Login or set Options.AccessToken")
	}

	t := kiteticker.New(c.opts.APIKey, token)
	// Reconnect indefinitely with the SDK's backoff. A feed that stops
	// permanently after a handful of failures leaves a position unmanaged,
	// which is worse than a process that keeps trying and logs that it is.
	t.SetAutoReconnect(true)
	t.SetReconnectMaxRetries(0)
	if err := t.SetReconnectMaxDelay(reconnectMaxDelay); err != nil {
		return nil, fmt.Errorf("zerodha: configuring reconnect backoff: %w", err)
	}
	return t, nil
}

// reconnectMaxDelay caps the backoff between reconnection attempts. A minute is
// long enough not to hammer a broker that is down and short enough that a feed
// which recovers mid-session is picked up within one bar of a 1-minute strategy.
const reconnectMaxDelay = time.Minute

// serve runs the connection until ctx is cancelled.
func (c *Client) serve(ctx context.Context, t *kiteticker.Ticker) {
	go t.ServeWithContext(ctx)
	go func() {
		<-ctx.Done()
		// ServeWithContext returns on cancellation but leaves the socket
		// open; Stop closes it and the goroutines it spawned.
		t.Stop()
	}()
}

// toDomainTick converts a feed packet, reporting whether it could be resolved.
//
// An unknown instrument token means the feed sent something this subscription
// did not ask for — a stale subscription on a reused connection, or a token the
// caller's resolver disagrees about. It is dropped rather than emitted with a
// zero key, because a tick with an empty key matches every strategy's "is this
// mine" check that compares only a symbol.
func toDomainTick(raw models.Tick, byToken map[uint32]domain.InstrumentKey, fallback time.Time) (domain.Tick, bool) {
	key, ok := byToken[raw.InstrumentToken]
	if !ok {
		slog.Warn("zerodha: dropping tick for an unsubscribed instrument token", "token", raw.InstrumentToken)
		return domain.Tick{}, false
	}

	// The exchange timestamp is the one that orders ticks against bars.
	// Indices and the opening packets of a session sometimes carry neither
	// it nor a last-trade time; a zero time would sort before every bar in
	// the session, so the caller's clock is the last resort rather than the
	// default.
	at := raw.Timestamp.Time
	if at.IsZero() {
		at = raw.LastTradeTime.Time
	}
	if at.IsZero() {
		at = fallback
	}

	return domain.Tick{
		Key:   key,
		At:    at,
		Price: paise(raw.LastPrice),
		// LastTradedQuantity is this print's size. VolumeTraded is the
		// day's cumulative total, and reporting that as the tick's volume
		// would make every volume-weighted calculation grow through the
		// session regardless of what actually traded.
		Volume: int64(raw.LastTradedQuantity),
	}, true
}
