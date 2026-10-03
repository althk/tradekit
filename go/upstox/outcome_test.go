package upstox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
)

// placePath is the endpoint PlaceOrder posts to.
const placePath = "/order/place"

func marketBuy() domain.OrderRequest {
	return domain.OrderRequest{
		Key: domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}, Side: domain.Buy,
		Quantity: 1, Type: domain.Market, Product: domain.CNC,
	}
}

// TestAmbiguousPlacementIsNotTransient pins the rule that keeps a caller from
// retrying its way into a duplicate order: a placement that may have reached
// the broker is reported as an unknown outcome, never as "retry".
func TestAmbiguousPlacementIsNotTransient(t *testing.T) {
	cases := map[string]func(release <-chan struct{}) http.HandlerFunc{
		"server error": func(<-chan struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGatewayTimeout) }
		},
		"undecodable success": func(<-chan struct{}) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>ok</html>")) }
		},
		"timeout after sending": func(release <-chan struct{}) http.HandlerFunc {
			return func(http.ResponseWriter, *http.Request) { <-release }
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var calls int
			h := handler(release)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == placePath {
					calls++
				}
				h(w, r)
			}))
			defer srv.Close()
			defer close(release)
			c, _ := newTestClient(t, srv)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := c.PlaceOrder(ctx, marketBuy())
			if !errors.Is(err, ports.ErrUnknownOutcome) || !errors.Is(err, ErrUnknownOutcome) {
				t.Errorf("err = %v, want ErrUnknownOutcome: the order may already be live", err)
			}
			if errors.Is(err, ErrTransient) {
				t.Error("an ambiguous placement must not read as transient; retrying it places a second order")
			}
			if calls > 1 {
				t.Errorf("placement was sent %d times, want once", calls)
			}
		})
	}
}

func TestPlacementThatNeverConnectedIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := newTestClient(t, srv)
	srv.Close() // nothing listens: the request cannot have reached anyone
	_, err := c.PlaceOrder(context.Background(), marketBuy())
	if !errors.Is(err, ErrTransient) {
		t.Errorf("err = %v, want ErrTransient for a connection that was refused", err)
	}
}
