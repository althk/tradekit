package fyers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLoginURLMintsAStateTheCallbackMustEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"s":"ok","code":200,"access_token":"fresh-token"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	c.opts.AppSecret = "secret"

	if _, err := c.LoginCallback(context.Background(), url.Values{"auth_code": {"x"}}); err == nil {
		t.Fatal("a callback before any LoginURL has no state to check against and must be refused")
	}

	u, err := url.Parse(c.LoginURL())
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if len(state) < 16 {
		t.Fatalf("LoginURL must carry a random state, got %q", state)
	}
	// Only the latest state counts: a second LoginURL supersedes the first.
	u, _ = url.Parse(c.LoginURL())
	if latest := u.Query().Get("state"); latest == state {
		t.Fatal("each LoginURL must mint a fresh state, or a replayed callback passes the check")
	} else {
		state = latest
	}

	_, err = c.LoginCallback(context.Background(), url.Values{"auth_code": {"abc"}, "state": {"forged"}})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("a callback with the wrong state must be refused before any exchange, got %v", err)
	}
	_, err = c.LoginCallback(context.Background(), url.Values{"state": {state}})
	if err == nil {
		t.Fatal("a callback with the right state but no auth_code is a refused login and must fail")
	}
	token, err := c.LoginCallback(context.Background(), url.Values{"auth_code": {"abc"}, "state": {state}})
	if err != nil || token != "fresh-token" {
		t.Fatalf("LoginCallback = (%q, %v); want the exchanged token", token, err)
	}
}
