// Package zerodha adapts Zerodha's Kite Connect to the tradekit ports.
//
// It replaces five separate Kite adapters, which between them had five
// different Broker interfaces, three rate limiters and no shared vocabulary for
// an order status.
//
// # What it implements
//
// The client satisfies ports.Broker, and separately ports.Quoter,
// ports.HistoryProvider, ports.ProtectiveOrders, ports.MarginEstimator,
// ports.InstrumentSource, ports.TokenState and ports.BrowserLogin. A consumer type-asserts for the
// capabilities it needs, so a strategy that requires basket margins fails at
// wiring time against a venue that has none, rather than at 09:15.
//
// # Translation lives in one place
//
// mapping.go holds every conversion between tradekit's domain and Kite's wire
// vocabulary, and deliberately imports nothing from the SDK: it deals only in
// strings and numbers, so it is testable without a network or a broker account.
// constants_test.go, which does import the SDK, asserts that the string
// literals restated there still equal the SDK's own constants — so a rename
// upstream breaks the build rather than silently sending an unrecognised value
// to the exchange.
package zerodha

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/core/ratelimit"
	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// Default request rates, in requests per second.
//
// They are conservative on purpose. Kite's published ceilings are higher, but a
// universe scan that trips the limiter loses the whole pass, and the cost of
// pacing slightly under the limit is a few seconds on a job that runs once a
// day. Tune them through Options if your account's limits differ.
const (
	DefaultRequestsPerSecond           = 3
	DefaultHistoricalRequestsPerSecond = 3
)

// Options configures a Client.
type Options struct {
	// APIKey and APISecret come from the Kite Connect app.
	APIKey    string
	APISecret string

	// AccessToken is the token from a completed login. It is valid for one
	// trading day; see Login for obtaining a fresh one.
	AccessToken string

	// TokenIssuedAt is when AccessToken was obtained. It drives TokenFresh,
	// which a scheduler uses to find out before the open that the token has
	// expired, rather than on the first rejected order. Zero means unknown,
	// which TokenFresh reports as stale.
	TokenIssuedAt time.Time

	// InstrumentToken resolves an instrument to Kite's numeric token, which
	// the historical endpoint and the tick stream require and no other call
	// does.
	//
	// It is a function rather than a lookup inside this package so the
	// adapter does not depend on the store: wire it to the store's
	// BrokerID, to an in-memory map, or to whatever the project already has.
	// Left nil, the client downloads the exchange's instrument list from
	// Kite on first use and keeps it for its lifetime -- one request per
	// exchange per process, which is the right trade for a bot that watches
	// a handful of symbols and has no store of its own.
	InstrumentToken func(domain.InstrumentKey) (int, error)

	// RequestsPerSecond paces every call except historical data.
	RequestsPerSecond float64
	// HistoricalRequestsPerSecond paces the historical endpoint, which Kite
	// meters separately and more strictly.
	HistoricalRequestsPerSecond float64

	// Tag is attached to every order placed through this client, so orders
	// this system placed can be told from ones placed by hand. Kite caps it
	// at 20 characters and rejects longer values.
	Tag string

	// Timeout bounds a single HTTP call. Zero uses the SDK's default.
	Timeout time.Duration
}

// Client is a Kite Connect adapter.
type Client struct {
	kite *kiteconnect.Client
	opts Options

	general    *ratelimit.Limiter
	historical *ratelimit.Limiter

	// mu guards the token pair. A streamer opens its own WebSocket
	// connection and needs the current token, so the client keeps it rather
	// than handing it only to the SDK's HTTP client.
	mu            sync.RWMutex
	accessToken   string
	tokenIssuedAt time.Time

	// tokens is the per-exchange token cache used when Options.InstrumentToken
	// is nil, filled on first use under tokensMu.
	tokensMu sync.Mutex
	tokens   map[string]map[domain.InstrumentKey]int
}

// New returns a client. It does not contact the broker: a token supplied in
// Options is set on the SDK, and an absent one leaves the client usable only
// for Login.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("zerodha: APIKey is required")
	}
	if len(opts.Tag) > 20 {
		// Kite rejects the order outright rather than truncating, which
		// would otherwise show up as a rejection at the worst moment.
		return nil, fmt.Errorf("zerodha: Tag is %d characters; Kite allows at most 20", len(opts.Tag))
	}
	if opts.RequestsPerSecond == 0 {
		opts.RequestsPerSecond = DefaultRequestsPerSecond
	}
	if opts.HistoricalRequestsPerSecond == 0 {
		opts.HistoricalRequestsPerSecond = DefaultHistoricalRequestsPerSecond
	}

	kc := kiteconnect.New(opts.APIKey)
	if opts.Timeout > 0 {
		kc.SetTimeout(opts.Timeout)
	}
	c := &Client{
		kite:          kc,
		opts:          opts,
		general:       ratelimit.New(opts.RequestsPerSecond),
		historical:    ratelimit.New(opts.HistoricalRequestsPerSecond),
		accessToken:   opts.AccessToken,
		tokenIssuedAt: opts.TokenIssuedAt,
	}
	if opts.AccessToken != "" {
		kc.SetAccessToken(opts.AccessToken)
	}
	return c, nil
}

// Kite exposes the underlying SDK client for calls this adapter does not cover
// — mutual funds, holdings, alerts. Prefer the ports where they exist.
func (c *Client) Kite() *kiteconnect.Client { return c.kite }

// LoginURL returns the URL a user visits to obtain a request token.
func (c *Client) LoginURL() string { return c.kite.GetLoginURL() }

// Login exchanges a request token for an access token and sets it on the
// client, returning the token so the caller can persist it.
//
// Kite's access tokens are valid for one trading day, so this runs once each
// morning; the returned token is what a scheduler stores and passes back
// through Options on the next start.
func (c *Client) Login(requestToken string) (string, error) {
	if c.opts.APISecret == "" {
		return "", fmt.Errorf("zerodha: APISecret is required to exchange a request token")
	}
	session, err := c.kite.GenerateSession(requestToken, c.opts.APISecret)
	if err != nil {
		// A bad or reused request token comes back as a TokenException,
		// which classify reports as ErrTokenExpired — the same signal a
		// caller acts on mid-session, and the same action: log in again.
		return "", fmt.Errorf("zerodha: generating session: %w", classify(err))
	}
	c.SetAccessToken(session.AccessToken, time.Now())
	return session.AccessToken, nil
}

// LoginCallback completes a login from the query Kite redirected back with.
//
// Kite delivers a successful login as ?request_token=...&status=success and a
// refused one as ?status=error&message=..., with no request_token at all; a
// caller indexing into the query blindly panics inside the HTTP handler and
// leaves the process waiting for a token that never comes. The SDK's session
// call takes no context, so ctx is accepted for the port and not used.
func (c *Client) LoginCallback(_ context.Context, query url.Values) (string, error) {
	token := query.Get("request_token")
	if token == "" {
		reason := query.Get("status")
		if msg := query.Get("message"); msg != "" {
			reason += ": " + msg
		}
		if reason == "" {
			reason = "no request_token in callback"
		}
		return "", fmt.Errorf("zerodha: login refused (%s)", reason)
	}
	return c.Login(token)
}

var _ ports.BrowserLogin = (*Client)(nil)

// SetAccessToken installs a token and records when it was issued.
func (c *Client) SetAccessToken(token string, issuedAt time.Time) {
	c.mu.Lock()
	c.accessToken = token
	c.tokenIssuedAt = issuedAt
	c.mu.Unlock()
	c.kite.SetAccessToken(token)
}

// TokenFresh reports whether the access token is valid for the current trading
// day.
//
// Kite's tokens expire overnight, so a scheduler asks this before the open and
// re-authenticates rather than discovering the problem on its first order.
// The comparison is by IST calendar date, which is the granularity the
// expiry actually has; a token of unknown age is reported stale, because
// assuming it is good is the failure this exists to prevent.
func (c *Client) TokenFresh(context.Context) bool {
	c.mu.RLock()
	issued := c.tokenIssuedAt
	c.mu.RUnlock()

	if issued.IsZero() {
		return false
	}
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return false
	}
	now := time.Now().In(loc)
	at := issued.In(loc)
	return now.Year() == at.Year() && now.Month() == at.Month() && now.Day() == at.Day()
}

// call paces a general request and runs it.
//
// The SDK's error is classified here rather than at each call site, so every
// error a caller receives from this adapter carries one of the sentinels in
// errors.go even after the caller's own fmt.Errorf wrapping.
func (c *Client) call(ctx context.Context, fn func() error) error {
	if err := c.general.Wait(ctx); err != nil {
		return err
	}
	return classify(fn())
}

// instrumentToken resolves the numeric token the historical endpoint and the
// ticker need, through Options.InstrumentToken when set and the client's own
// lazily downloaded cache otherwise.
func (c *Client) instrumentToken(ctx context.Context, key domain.InstrumentKey) (int, error) {
	var token int
	var err error
	if c.opts.InstrumentToken != nil {
		token, err = c.opts.InstrumentToken(key)
	} else {
		token, err = c.cachedToken(ctx, key)
	}
	if err != nil {
		return 0, fmt.Errorf("zerodha: resolving instrument token for %s: %w", key, err)
	}
	if token == 0 {
		return 0, fmt.Errorf("zerodha: no instrument token known for %s", key)
	}
	return token, nil
}

// cachedToken looks a key up in the exchange's token map, downloading the map
// on the first miss for that exchange. A failed download is not cached, so a
// transient error on the first call does not poison the process.
func (c *Client) cachedToken(ctx context.Context, key domain.InstrumentKey) (int, error) {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	byKey, ok := c.tokens[key.Exchange]
	if !ok {
		var err error
		byKey, err = c.InstrumentTokens(ctx, key.Exchange)
		if err != nil {
			return 0, err
		}
		if c.tokens == nil {
			c.tokens = map[string]map[domain.InstrumentKey]int{}
		}
		c.tokens[key.Exchange] = byKey
	}
	return byKey[key], nil
}
