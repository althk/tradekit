package upstox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
)

// newTestClient returns a client pointed at srv with pacing and real sleeping
// disabled, and records the delays the retry schedule asked for.
func newTestClient(t *testing.T, srv *httptest.Server) (*Client, *[]time.Duration) {
	t.Helper()
	c, err := New(Options{
		APIKey:      "key",
		AccessToken: "token",
		BaseURL:     srv.URL,
		BaseURLV3:   srv.URL,
		// Pacing is exercised by limiter_test.go; here it would only make
		// every test wait.
		RequestsPerSecond:           -1,
		HistoricalRequestsPerSecond: -1,
		RetryBase:                   time.Second,
		InstrumentKey: func(k domain.InstrumentKey) (string, error) {
			return "NSE_EQ|" + k.Symbol, nil
		},
	})
	if err != nil {
		t.Fatalf("building the test client: %v", err)
	}
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	}
	return c, &slept
}

func TestRetryAfterIsHonouredThenTheCallSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"errorCode":"UDAPI10005","message":"too many requests"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"ok":true}}`))
	}))
	defer srv.Close()

	c, slept := newTestClient(t, srv)
	var out envelope[map[string]bool]
	err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", out: &out, retry: true})
	if err != nil {
		t.Fatalf("a 429 followed by a success must succeed, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected one retry after the 429, got %d calls", calls)
	}
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("Retry-After says when the server's window resets and must win over the client's schedule; slept %v", *slept)
	}
	if !out.Data["ok"] {
		t.Error("the successful response must be decoded into out")
	}
}

func TestServerErrorRetriesAndGivesUpAtTheBound(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c, slept := newTestClient(t, srv)
	err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", retry: true})
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("a 5xx is Upstox's side failing and must classify as transient, got %v", err)
	}
	if int(calls) != defaultAttempts {
		t.Errorf("a retryable failure must be tried exactly %d times, got %d", defaultAttempts, calls)
	}
	// One fewer sleep than attempts: nothing waits after the last one.
	if len(*slept) != defaultAttempts-1 {
		t.Fatalf("expected %d backoffs, got %v", defaultAttempts-1, *slept)
	}
	for i := 1; i < len(*slept); i++ {
		if (*slept)[i] <= (*slept)[i-1] {
			t.Errorf("backoff must grow between attempts, got %v", *slept)
			break
		}
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":[{"errorCode":"UDAPI1148","message":"range too long"}]}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", retry: true})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("a 4xx is a refusal on the merits and must classify as rejected, got %v", err)
	}
	if calls != 1 {
		t.Errorf("retrying a rejection only burns the quota the next caller needs; made %d calls", calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "UDAPI1148" {
		t.Errorf("the broker's own error code must survive to the caller, got %v", err)
	}
}

func TestUnauthorizedClassifiesAsTokenExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"errorCode":"UDAPI100050","message":"Invalid token"}]}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", retry: true})
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("401 means log in again, not retry; got %v", err)
	}
	if errors.Is(err, ErrTransient) {
		t.Error("an expired token must never read as retryable: the same token cannot succeed")
	}
}

func TestRateLimitCodeUnderA2xxIsStillTransient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Upstox sometimes answers a breached rate limit with a
			// 200-level status carrying the code in the body.
			_, _ = w.Write([]byte(`{"errors":[{"errorCode":"UDAPI10005","message":"rate limit"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	var out envelope[map[string]bool]
	if err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", out: &out, retry: true}); err != nil {
		t.Fatalf("a rate limit under a 2xx must be retried, not read as success; got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected the rate-limited 2xx to be retried once, got %d calls", calls)
	}
}

func TestPostIsNeverRetriedEvenOnServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	err := c.doJSON(context.Background(), request{
		method: http.MethodPost,
		path:   "/order/place",
		body:   map[string]int{"quantity": 1},
		retry:  false,
	})
	if err == nil {
		t.Fatal("a 500 must be reported, not swallowed")
	}
	if calls != 1 {
		t.Errorf("retrying a POST that placed an order places a second one; made %d calls", calls)
	}
}

func TestPlaceOrderDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"},
		Side:     domain.Buy,
		Quantity: 10,
		Type:     domain.Market,
		Product:  domain.CNC,
	})
	if err == nil {
		t.Fatal("a 503 on order placement must be reported")
	}
	if calls != 1 {
		t.Errorf("PlaceOrder must send exactly one request whatever the failure; made %d", calls)
	}
}

func TestCancelledContextStopsMidBackoff(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel while the first backoff is being served, which is where a
	// cancelled scan must stop rather than working through its backlog.
	c.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}

	err := c.doJSON(ctx, request{method: http.MethodGet, path: "/x", retry: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must surface as context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("no request may be issued after cancellation; made %d", calls)
	}
}

func TestParseRetryAfterReadsSecondsAndDates(t *testing.T) {
	if got := parseRetryAfter("2.5"); got != 2500*time.Millisecond {
		t.Errorf("a fractional Retry-After must be honoured, got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("an absent header means no server-specified wait, got %v", got)
	}
	if got := parseRetryAfter("-3"); got != 0 {
		t.Errorf("a negative wait is meaningless and must fall back to the schedule, got %v", got)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 {
		t.Errorf("the HTTP-date form must be read rather than discarded, got %v", got)
	}
}

func TestBackoffIsCapped(t *testing.T) {
	if got := backoff(time.Second, 20, 0); got != defaultMaxRetryWait {
		t.Errorf("an unbounded doubling would stall a sync for hours; got %v", got)
	}
	if got := backoff(time.Second, 1, time.Hour); got != defaultMaxRetryWait {
		t.Errorf("an absurd Retry-After from a confused proxy must be capped; got %v", got)
	}
}

func TestUndecodableBodyIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`<html>maintenance</html>`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	var out envelope[map[string]any]
	err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", out: &out, retry: true})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("a body that will not decode is not retryable; the same answer arrives again. got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected one call, got %d", calls)
	}
}

func TestAuthorizationHeaderCarriesTheCurrentToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	c.SetAccessToken("refreshed", time.Now())
	if err := c.doJSON(context.Background(), request{method: http.MethodGet, path: "/x", retry: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "Bearer refreshed"; seen != want {
		t.Errorf("a token replaced mid-session must be used on the next call; header was %q, want %q", seen, want)
	}
}

func TestLoginExchangesTheCodeAndInstallsTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("the token exchange must be form-encoded: %v", err)
		}
		if got := r.FormValue("grant_type"); got != "authorization_code" {
			t.Errorf("grant_type must be authorization_code, got %q", got)
		}
		if got := r.FormValue("code"); got != "abc123" {
			t.Errorf("the authorisation code must be sent, got %q", got)
		}
		_, _ = w.Write([]byte(`{"access_token":"fresh-token"}`))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv)
	c.opts.APISecret = "secret"
	token, err := c.Login(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("a good code must exchange cleanly, got %v", err)
	}
	if token != "fresh-token" {
		t.Errorf("the token must be returned so a scheduler can persist it, got %q", token)
	}
	if c.token() != "fresh-token" {
		t.Error("the token must also be installed on the client, or the next call still uses the stale one")
	}
	if !c.TokenFresh(context.Background()) {
		t.Error("a token issued just now must read as fresh")
	}
}

func TestLoginNeedsASecret(t *testing.T) {
	c, err := New(Options{APIKey: "key"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := c.Login(context.Background(), "abc"); err == nil {
		t.Error("exchanging a code without an API secret cannot succeed and must fail before the request")
	}
}

func TestLoginURLCarriesTheAppsParameters(t *testing.T) {
	c, err := New(Options{APIKey: "key-1", RedirectURI: "https://example.test/cb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := c.LoginURL()
	for _, want := range []string{
		"client_id=key-1",
		"redirect_uri=https%3A%2F%2Fexample.test%2Fcb",
		"response_type=code",
		"/login/authorization/dialog?",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the login URL must contain %q, got %q", want, got)
		}
	}
}
