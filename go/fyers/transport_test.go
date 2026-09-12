package fyers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
)

// newTestClient builds a client pointed at srv with pacing disabled and the
// backoff sleep captured rather than performed.
func newTestClient(t *testing.T, srv *httptest.Server) (*Client, *[]time.Duration) {
	t.Helper()
	c, err := New(Options{
		AppID:           "APP-100",
		AccessToken:     "token",
		BaseURL:         srv.URL,
		DataURL:         srv.URL,
		SymbolMasterURL: srv.URL,
		// Pacing is exercised by limiter_test.go; here it would only make
		// every test wait.
		RequestsPerSecond:           -1,
		HistoricalRequestsPerSecond: -1,
		RetryBase:                   time.Second,
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

// routes maps a request path to the body served for it, so a test states the
// recorded responses it needs and nothing else.
type routes map[string]string

// newRouted returns a client backed by a server that answers from routes and
// records the bodies it was sent, keyed by path.
func newRouted(t *testing.T, r routes) (*Client, map[string][]byte) {
	t.Helper()
	sent := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil {
			body, _ := io.ReadAll(req.Body)
			sent[req.URL.Path] = body
		}
		body, ok := r[req.URL.Path]
		if !ok {
			t.Errorf("unexpected request to %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, _ := newTestClient(t, srv)
	return c, sent
}

func TestAuthorizationHeaderCarriesTheAppID(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = req.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"s":"ok","code":200,"message":"","fund_limit":[{"id":10,"title":"Available Balance","equityAmount":1}]}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	if _, err := c.Account(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "APP-100:token" {
		t.Errorf("FYERS authenticates with appId:token, not a bearer token; a bare token is answered with -15 and no hint. Got %q", got)
	}
}

func TestRetryAfterMillisecondsIsPreferredThenTheCallSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Retry-After-Ms", "150")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"s":"error","code":-429,"message":"Rate limit exceeded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"s":"ok","code":200,"message":"","orderBook":[]}`))
	}))
	defer srv.Close()
	c, slept := newTestClient(t, srv)

	if _, err := c.OpenOrders(context.Background()); err != nil {
		t.Fatalf("a rate limit followed by success must succeed, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected one retry, saw %d calls", calls)
	}
	if len(*slept) != 1 || (*slept)[0] != 150*time.Millisecond {
		t.Errorf("the millisecond header is the precise one and must win over Retry-After; slept %v", *slept)
	}
}

func TestServerErrorRetriesAndGivesUpAtTheBound(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, slept := newTestClient(t, srv)

	_, err := c.OpenOrders(context.Background())
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("a 502 is FYERS's side failing and must classify transient, got %v", err)
	}
	if atomic.LoadInt32(&calls) != defaultAttempts {
		t.Errorf("expected %d attempts, saw %d", defaultAttempts, calls)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(*slept) != len(want) {
		t.Fatalf("expected the backoff schedule %v, slept %v", want, *slept)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Errorf("attempt %d: backoff must double, want %v got %v", i+1, want[i], (*slept)[i])
		}
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"s":"error","code":-50,"message":"Invalid input"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	_, err := c.OpenOrders(context.Background())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("a 400 is the request being wrong and must classify rejected, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("a rejection re-sent unchanged is refused again; saw %d calls", calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != -50 {
		t.Errorf("the envelope code must be carried for diagnosis, got %v", err)
	}
}

func TestExpiredTokenUnderA200IsStillATokenFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"s":"error","code":-8,"message":"Token expired"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	_, err := c.OpenOrders(context.Background())
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("FYERS reports an expired token by envelope code, not always by status; the remedy is a login, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("retrying with the same token cannot succeed; saw %d calls", calls)
	}
}

func TestARefusalUnderA200IsNotReadAsSuccess(t *testing.T) {
	c, _ := newRouted(t, routes{
		"/orders/sync": `{"s":"error","code":-99,"message":"Insufficient funds"}`,
	})
	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Buy, Quantity: 1,
		Type: domain.Market, Product: domain.MIS,
	})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("an order rejection arrives under HTTP 200 with s:error; reading it as placed invents a position, got %v", err)
	}
}

func TestPlaceOrderIsNeverRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)

	_, err := c.PlaceOrder(context.Background(), domain.OrderRequest{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "SBIN"}, Side: domain.Buy, Quantity: 1,
		Type: domain.Market, Product: domain.MIS,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("retrying a POST that may have placed an order places a second one; saw %d calls", calls)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := c.OpenOrders(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled scan must stop making requests, not work through its retry budget; got %v", err)
	}
}

func TestBackoffCapsAnAbsurdRetryAfter(t *testing.T) {
	if got := backoff(time.Second, 1, 3*time.Hour); got != defaultMaxRetryWait {
		t.Errorf("an absurd Retry-After from a confused proxy must not stall a sync for the day; got %v", got)
	}
	if got := parseRetryAfter("2.5", ""); got != 2500*time.Millisecond {
		t.Errorf("Retry-After in seconds must be honoured, got %v", got)
	}
	if got := parseRetryAfter("", "abc"); got != 0 {
		t.Errorf("an unparseable header must be ignored, got %v", got)
	}
}
