// Package upstox adapts the Upstox REST API to the tradekit ports.
//
// It replaces five separate Upstox clients — two in Go, three in Python —
// which between them had three rate limiters, four retry policies and no shared
// vocabulary for an order status.
//
// # What it implements
//
// The client satisfies ports.Broker, and separately ports.Quoter,
// ports.HistoryProvider, ports.ProtectiveOrders, ports.MarginEstimator,
// ports.InstrumentSource and ports.TokenState. A consumer type-asserts for the
// capabilities it needs, so a strategy that requires basket margins fails at
// wiring time against a venue that has none, rather than at 09:15.
//
// # There is no SDK
//
// Upstox publishes no usable Go SDK — dhaara's client documents that the
// generated Swagger one does not compile — so this adapter speaks HTTP
// directly. That makes it more verifiable than the Kite one, not less: every
// call can be exercised end to end against an httptest.Server, and the tests
// here do exactly that. The endpoint paths and field names are traced to donor
// code known to work against the live API rather than invented; upstox-api.txt
// records the file and line for each.
//
// # Translation lives in one place
//
// mapping.go holds every conversion between tradekit's domain and Upstox's wire
// vocabulary, and deals only in strings and numbers, so it is testable without
// a network or a broker account.
package upstox

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/core/ratelimit"
)

// Compile-time proof that the client satisfies the ports it claims. A missing
// method is a build failure here rather than a type assertion that silently
// returns false at wiring time, which is the whole point of capability
// interfaces.
//
// ports.TickObserver is deliberately absent, as it is in the Kite adapter: an
// Upstox stop rests at the broker and triggers without the engine's help, so
// implementing it would claim a responsibility this adapter does not have.
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
// The version sits in the path rather than the host. dhaara reaches Upstox at
// api-v2.upstox.com, which is the legacy alias; the three newer donors all use
// this host, and so does Upstox's current documentation.
const (
	// BaseURL is the v2 root, which serves auth, quotes, the order book,
	// positions, funds and basket margins.
	BaseURL = "https://api.upstox.com/v2"
	// BaseURLV3 is the v3 root, which serves historical candles, order
	// placement and the GTT API. The split is real rather than a migration
	// half-done: breakout500 runs against a live account daily and reads
	// order details on v2 while placing orders on v3.
	BaseURLV3 = "https://api.upstox.com/v3"
)

// Default request rates, in requests per second.
//
// breakout500 paces at 20/s and fanse's config records that the binding
// constraint is a longer window — roughly 2000 requests per 30 minutes, about
// 67/s sustained — so a client-side bucket cannot prevent a rate limit on a
// bulk job, only keep the burst from provoking one immediately. These are
// deliberately under breakout500's figure for the same reason the Kite
// adapter's are: a universe scan that trips the limiter loses the whole pass.
const (
	DefaultRequestsPerSecond           = 10
	DefaultHistoricalRequestsPerSecond = 5
)

// Options configures a Client.
type Options struct {
	// APIKey and APISecret come from the Upstox developer app.
	APIKey    string
	APISecret string
	// RedirectURI must match the app's configured redirect exactly; Upstox
	// checks it on both the dialog and the token exchange.
	RedirectURI string

	// AccessToken is the token from a completed login. Upstox tokens are
	// valid for one trading day; see Login for obtaining a fresh one.
	AccessToken string

	// TokenIssuedAt is when AccessToken was obtained. It drives TokenFresh,
	// which a scheduler uses to find out before the open that the token has
	// expired, rather than on the first rejected order. Zero means unknown,
	// which TokenFresh reports as stale.
	TokenIssuedAt time.Time

	// InstrumentKey resolves an instrument to Upstox's "SEGMENT|ID" key.
	//
	// Upstox addresses cash equity by ISIN ("NSE_EQ|INE002A01018") and
	// derivatives by exchange token, neither of which the adapter can derive
	// from an exchange and a symbol. It is a function rather than a lookup
	// inside this package so the adapter does not depend on the store: wire
	// it to the store's BrokerID, to a map built from Instruments, or to
	// whatever the project already has.
	InstrumentKey func(domain.InstrumentKey) (string, error)

	// RequestsPerSecond paces every call except historical data.
	RequestsPerSecond float64
	// HistoricalRequestsPerSecond paces the historical endpoints, which are
	// the ones a backfill hammers.
	HistoricalRequestsPerSecond float64

	// Tag is attached to every order placed through this client, so orders
	// this system placed can be told from ones placed by hand.
	Tag string

	// Timeout bounds a single HTTP call. Zero uses a 30-second default.
	Timeout time.Duration

	// BaseURL and BaseURLV3 override the API roots. Tests point them at an
	// httptest.Server; production leaves them empty.
	BaseURL   string
	BaseURLV3 string
	// InstrumentsURL overrides the instrument master's location. It is a
	// separate root because the master is served from an assets host, not
	// from the API, and needs no token.
	InstrumentsURL string

	// HTTPClient overrides the HTTP client. Zero builds one with Timeout.
	HTTPClient *http.Client

	// Attempts bounds how many times a retryable call is sent. Zero uses
	// defaultAttempts. One disables retrying entirely.
	Attempts int
	// RetryBase is the first backoff delay, doubled per attempt and
	// overridden by a Retry-After header. Zero uses defaultRetryBase.
	RetryBase time.Duration
}

// Client is an Upstox REST adapter.
type Client struct {
	http      *http.Client
	baseURL   string
	baseV3    string
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
	if opts.APIKey == "" {
		return nil, fmt.Errorf("upstox: APIKey is required")
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
		baseV3:        strings.TrimSuffix(orDefault(opts.BaseURLV3, BaseURLV3), "/"),
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
// authorisation code, which Upstox delivers to RedirectURI as ?code=.
func (c *Client) LoginURL() string {
	q := url.Values{}
	q.Set("client_id", c.opts.APIKey)
	q.Set("redirect_uri", c.opts.RedirectURI)
	q.Set("response_type", "code")
	return c.baseURL + "/login/authorization/dialog?" + q.Encode()
}

// tokenResponse is the subset of the token exchange this adapter consumes.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
}

// Login exchanges an authorisation code for an access token and installs it,
// returning the token so the caller can persist it.
//
// Upstox tokens are valid for one trading day, so this runs once each morning;
// the returned token is what a scheduler stores and passes back through Options
// on the next start.
//
// The exchange is form-encoded rather than JSON — it is the one call on the API
// that is — so it does not go through doJSON.
func (c *Client) Login(ctx context.Context, authCode string) (string, error) {
	if c.opts.APISecret == "" {
		return "", fmt.Errorf("upstox: APISecret is required to exchange an authorisation code")
	}

	form := url.Values{}
	form.Set("code", authCode)
	form.Set("client_id", c.opts.APIKey)
	form.Set("client_secret", c.opts.APISecret)
	form.Set("redirect_uri", c.opts.RedirectURI)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/login/authorization/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("upstox: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	var out tokenResponse
	if err := c.doForm(ctx, req, &out); err != nil {
		return "", fmt.Errorf("upstox: exchanging authorisation code: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("upstox: token exchange returned an empty access_token")
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

// token returns the current access token.
func (c *Client) token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken
}

// TokenFresh reports whether the access token is valid for the current trading
// day.
//
// Upstox's tokens expire overnight, so a scheduler asks this before the open
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
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return false
	}
	now := time.Now().In(loc)
	at := issued.In(loc)
	return now.Year() == at.Year() && now.Month() == at.Month() && now.Day() == at.Day()
}

// instrumentKeyFor resolves the "SEGMENT|ID" key every endpoint addresses an
// instrument by.
func (c *Client) instrumentKeyFor(key domain.InstrumentKey) (string, error) {
	if c.opts.InstrumentKey == nil {
		return "", fmt.Errorf("upstox: Options.InstrumentKey is not set; Upstox addresses %s by an ISIN-based key the adapter cannot derive", key)
	}
	resolved, err := c.opts.InstrumentKey(key)
	if err != nil {
		return "", fmt.Errorf("upstox: resolving instrument key for %s: %w", key, err)
	}
	if resolved == "" {
		return "", fmt.Errorf("upstox: no instrument key known for %s", key)
	}
	return resolved, nil
}
