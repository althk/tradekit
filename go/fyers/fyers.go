// Package fyers adapts the FYERS API v3 to the tradekit ports.
//
// # What it implements
//
// The client satisfies ports.Broker, and separately ports.Quoter,
// ports.HistoryProvider, ports.ProtectiveOrders, ports.MarginEstimator,
// ports.InstrumentSource and ports.TokenState. A consumer type-asserts for the
// capabilities it needs, so a strategy that requires basket margins fails at
// wiring time against a venue that has none, rather than at 09:15.
//
// # The SDK is not imported
//
// FYERS publishes a Go SDK, but it returns every response as an undecoded
// string and its HTTP layer swallows transport errors, so this adapter speaks
// HTTP directly as the Upstox one does. That keeps every call exercisable end
// to end against an httptest.Server, offline. The SDK is still worth having:
// constants_test.go imports it and asserts the endpoint paths and product
// codes restated here still match, so a rename upstream fails the build rather
// than reaching the exchange.
//
// # Translation lives in one place
//
// mapping.go holds every conversion between tradekit's domain and FYERS's wire
// vocabulary, and deals only in strings and numbers, so it is testable without
// a network or a broker account.
//
// # Streaming is not implemented
//
// FYERS's market-data socket speaks a proprietary binary protocol that cannot
// be verified without a live session. ports.Streamer is therefore not claimed,
// as it is not by the Upstox adapter; a project that needs live ticks polls
// Quote or brings its own feed. ports.TickObserver is not claimed either, for
// the reason the other live adapters omit it: a FYERS stop rests at the broker
// and triggers without the engine's help.
package fyers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/core/ratelimit"
)

// Compile-time proof that the client satisfies the ports it claims. A missing
// method is a build failure here rather than a type assertion that silently
// returns false at wiring time, which is the whole point of capability
// interfaces.
var (
	_ ports.Broker           = (*Client)(nil)
	_ ports.Quoter           = (*Client)(nil)
	_ ports.HistoryProvider  = (*Client)(nil)
	_ ports.ProtectiveOrders = (*Client)(nil)
	_ ports.MarginEstimator  = (*Client)(nil)
	_ ports.InstrumentSource = (*Client)(nil)
	_ ports.TokenState       = (*Client)(nil)
)

// API roots.
//
// Trading and market data are served from different roots under the same
// host. The split is FYERS's, not a migration half-done: quotes, depth and
// history live under /data while everything that touches the account lives
// under /api/v3.
const (
	// BaseURL is the trading root: auth, profile, funds, orders, positions,
	// GTT and margins.
	BaseURL = "https://api-t1.fyers.in/api/v3"
	// DataURL is the market-data root: quotes, depth and history.
	DataURL = "https://api-t1.fyers.in/data"
	// SymbolMasterURL is the directory the per-segment symbol master JSON
	// files are served from, without authentication.
	SymbolMasterURL = "https://public.fyers.in/sym_details"
)

// Default request rates, in requests per second.
//
// FYERS's published limits are 10 per second, 200 per minute and 100,000 per
// day on the standard plan, and an account that breaches the per-minute window
// three times is blocked for the rest of the day. The general rate sits under
// the per-second figure; the historical rate sits well under it because a
// backfill is the one job that can sustain a request rate for a full minute,
// and 200 per minute is 3.3 per second.
const (
	DefaultRequestsPerSecond           = 8
	DefaultHistoricalRequestsPerSecond = 3
)

// Options configures a Client.
type Options struct {
	// AppID is the app's client id, of the form "XXXXXXXXXX-100". It is
	// sent with every request: FYERS authenticates with "appId:token"
	// rather than a bare bearer token.
	AppID string
	// AppSecret is needed only to exchange an authorisation code.
	AppSecret string
	// RedirectURI must match the app's configured redirect exactly.
	RedirectURI string

	// AccessToken is the token from a completed login. FYERS tokens are
	// valid until the end of the trading day; see Login for obtaining a
	// fresh one.
	AccessToken string

	// TokenIssuedAt is when AccessToken was obtained. It drives TokenFresh,
	// which a scheduler uses to find out before the open that the token has
	// expired, rather than on the first rejected order. Zero means unknown,
	// which TokenFresh reports as stale.
	TokenIssuedAt time.Time

	// RequestsPerSecond paces every call except historical data.
	RequestsPerSecond float64
	// HistoricalRequestsPerSecond paces the history endpoint, which is the
	// one a backfill hammers.
	HistoricalRequestsPerSecond float64

	// Tag is attached to every order placed through this client, so orders
	// this system placed can be told from ones placed by hand. FYERS
	// echoes it back prefixed with "1:".
	Tag string

	// Timeout bounds a single HTTP call. Zero uses a 30-second default.
	Timeout time.Duration

	// BaseURL, DataURL and SymbolMasterURL override the roots. Tests point
	// them at an httptest.Server; production leaves them empty.
	BaseURL         string
	DataURL         string
	SymbolMasterURL string

	// HTTPClient overrides the HTTP client. Zero builds one with Timeout.
	HTTPClient *http.Client

	// Attempts bounds how many times a retryable call is sent. Zero uses
	// defaultAttempts. One disables retrying entirely.
	Attempts int
	// RetryBase is the first backoff delay, doubled per attempt and
	// overridden by a Retry-After header. Zero uses defaultRetryBase.
	RetryBase time.Duration
}

// Client is a FYERS API v3 adapter.
type Client struct {
	http      *http.Client
	baseURL   string
	dataURL   string
	masterURL string
	opts      Options
	attempts  int
	retryBase time.Duration

	general    *ratelimit.Limiter
	historical *ratelimit.Limiter

	// sleep is injectable so tests exercise the backoff schedule without
	// waiting for it.
	sleep func(context.Context, time.Duration) error

	// mu guards the token pair.
	mu            sync.RWMutex
	accessToken   string
	tokenIssuedAt time.Time
}

// New returns a client. It does not contact the broker: a token supplied in
// Options is installed, and an absent one leaves the client usable only for
// LoginURL and Login.
func New(opts Options) (*Client, error) {
	if opts.AppID == "" {
		return nil, fmt.Errorf("fyers: AppID is required")
	}
	if opts.RequestsPerSecond == 0 {
		opts.RequestsPerSecond = DefaultRequestsPerSecond
	}
	if opts.HistoricalRequestsPerSecond == 0 {
		opts.HistoricalRequestsPerSecond = DefaultHistoricalRequestsPerSecond
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}

	c := &Client{
		http:          opts.HTTPClient,
		baseURL:       strings.TrimSuffix(orDefault(opts.BaseURL, BaseURL), "/"),
		dataURL:       strings.TrimSuffix(orDefault(opts.DataURL, DataURL), "/"),
		masterURL:     strings.TrimSuffix(orDefault(opts.SymbolMasterURL, SymbolMasterURL), "/"),
		opts:          opts,
		attempts:      opts.Attempts,
		retryBase:     opts.RetryBase,
		general:       ratelimit.New(opts.RequestsPerSecond),
		historical:    ratelimit.New(opts.HistoricalRequestsPerSecond),
		sleep:         ratelimit.Sleep,
		accessToken:   opts.AccessToken,
		tokenIssuedAt: opts.TokenIssuedAt,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: opts.Timeout}
	}
	if c.attempts < 1 {
		c.attempts = defaultAttempts
	}
	if c.retryBase <= 0 {
		c.retryBase = defaultRetryBase
	}
	return c, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// LoginURL returns the URL a user visits to authorise the app and obtain an
// authorisation code, which FYERS delivers to RedirectURI as ?auth_code=.
//
// state is echoed back unchanged on the redirect; a caller should send a
// random value and check it, as the reference recommends.
func (c *Client) LoginURL(state string) string {
	q := url.Values{}
	q.Set("client_id", c.opts.AppID)
	q.Set("redirect_uri", c.opts.RedirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	return c.baseURL + "/generate-authcode?" + q.Encode()
}

// appIDHash is the SHA-256 of "appId:appSecret", which is what the token
// exchange takes instead of the secret itself.
func (c *Client) appIDHash() string {
	sum := sha256.Sum256([]byte(c.opts.AppID + ":" + c.opts.AppSecret))
	return hex.EncodeToString(sum[:])
}

// Login exchanges an authorisation code for an access token and installs it,
// returning the token so the caller can persist it.
//
// FYERS tokens are valid for one trading day, so this runs once each morning;
// the returned token is what a scheduler stores and passes back through Options
// on the next start.
//
// The call is never retried: an authorisation code is single-use, so a second
// attempt with the same code is refused even when the first failed in
// transport.
func (c *Client) Login(ctx context.Context, authCode string) (string, error) {
	if c.opts.AppSecret == "" {
		return "", fmt.Errorf("fyers: AppSecret is required to exchange an authorisation code")
	}

	var out struct {
		envelope
		AccessToken string `json:"access_token"`
	}
	err := c.doJSON(ctx, request{
		method: http.MethodPost,
		path:   "/validate-authcode",
		body: map[string]string{
			"grant_type": "authorization_code",
			"appIdHash":  c.appIDHash(),
			"code":       authCode,
		},
		out:   &out,
		retry: false,
	})
	if err != nil {
		return "", fmt.Errorf("fyers: exchanging authorisation code: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("fyers: token exchange returned an empty access_token")
	}
	c.SetAccessToken(out.AccessToken, time.Now())
	return out.AccessToken, nil
}

// SetAccessToken installs a token and records when it was issued.
func (c *Client) SetAccessToken(token string, issuedAt time.Time) {
	c.mu.Lock()
	c.accessToken = token
	c.tokenIssuedAt = issuedAt
	c.mu.Unlock()
}

// authHeader returns the Authorization value, "appId:accessToken", or an empty
// string when no token is installed.
//
// The app id travels with every request. It is the one detail a caller porting
// from Kite or Upstox would get wrong: a bare bearer token is answered with
// "invalid token" (-15) rather than anything that names the missing part.
func (c *Client) authHeader() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.accessToken == "" {
		return ""
	}
	return c.opts.AppID + ":" + c.accessToken
}

// TokenFresh reports whether the access token is valid for the current trading
// day.
//
// FYERS's tokens expire overnight, so a scheduler asks this before the open
// and re-authenticates rather than discovering the problem on its first order.
// The comparison is by IST calendar date, which is the granularity the expiry
// actually has; a token of unknown age is reported stale, because assuming it
// is good is the failure this exists to prevent.
func (c *Client) TokenFresh(context.Context) bool {
	c.mu.RLock()
	issued := c.tokenIssuedAt
	token := c.accessToken
	c.mu.RUnlock()

	if token == "" || issued.IsZero() {
		return false
	}
	now := time.Now().In(ist)
	at := issued.In(ist)
	return now.Year() == at.Year() && now.Month() == at.Month() && now.Day() == at.Day()
}
