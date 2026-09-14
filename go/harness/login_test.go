package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBroker is a BrowserLogin that accepts ?code=good and refuses the rest.
type fakeBroker struct {
	callbacks atomic.Int32
}

func (f *fakeBroker) LoginURL() string { return "https://broker.example/login?x=1&y=2" }

func (f *fakeBroker) LoginCallback(_ context.Context, q url.Values) (string, error) {
	f.callbacks.Add(1)
	if q.Get("code") != "good" {
		return "", errors.New("fake: login refused")
	}
	return "token-" + q.Get("code"), nil
}

// freePort asks the kernel for an unused port. The window between closing
// and re-binding is small enough for a test.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// loginResult is what BrowserLogin returned, once it has.
type loginResult struct {
	token string
	err   error
}

// startLogin runs BrowserLogin in the background against a fresh port and
// waits until the callback server answers, so the test can send callbacks.
func startLogin(t *testing.T, ctx context.Context, broker *fakeBroker) (callback string, result <-chan loginResult) {
	t.Helper()
	port := freePort(t)
	callback = fmt.Sprintf("http://127.0.0.1:%s/auth/callback", port)
	ch := make(chan loginResult, 1)
	go func() {
		token, err := BrowserLogin(ctx, broker, Callback{RedirectURL: callback, ListenAddr: "127.0.0.1:" + port})
		ch <- loginResult{token, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return callback, ch
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback server never came up on %s", port)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func get(t *testing.T, rawURL string) (int, string) {
	t.Helper()
	resp, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestBrowserLoginExchangesTheCallbackAndReturnsTheToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	broker := &fakeBroker{}
	callback, result := startLogin(t, ctx, broker)

	status, body := get(t, callback+"?code=good")
	if status != http.StatusOK || !strings.Contains(body, "Login successful") {
		t.Fatalf("browser saw %d %q; a completed login must show success", status, body)
	}
	got := <-result
	if got.err != nil || got.token != "token-good" {
		t.Fatalf("BrowserLogin returned (%q, %v); want the exchanged token", got.token, got.err)
	}
}

func TestBrowserLoginShowsARefusalAndKeepsWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	broker := &fakeBroker{}
	callback, result := startLogin(t, ctx, broker)

	status, body := get(t, callback+"?status=error&message=cancelled")
	if status != http.StatusBadRequest || !strings.Contains(body, "login refused") {
		t.Fatalf("browser saw %d %q; a refused login must say so", status, body)
	}
	if !strings.Contains(body, `href="https://broker.example/login?x=1&amp;y=2"`) {
		t.Fatalf("refusal page %q lacks a try-again link to the login URL", body)
	}
	select {
	case got := <-result:
		t.Fatalf("BrowserLogin returned (%q, %v) after a refusal; it must wait for another attempt", got.token, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	get(t, callback+"?code=good")
	got := <-result
	if got.err != nil || got.token != "token-good" {
		t.Fatalf("second attempt returned (%q, %v); want the token", got.token, got.err)
	}
}

func TestBrowserLoginIgnoresCallbacksAfterTheFirstSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	broker := &fakeBroker{}
	callback, result := startLogin(t, ctx, broker)

	get(t, callback+"?code=good")
	<-result
	// The server may already be shut down; a reload that gets through must
	// not exchange again, and one that does not is equally fine.
	if resp, err := http.Get(callback + "?code=good"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "already completed") {
			t.Fatalf("reload after success saw %q", body)
		}
	}
	if n := broker.callbacks.Load(); n != 1 {
		t.Fatalf("broker exchanged %d codes; a second callback must not reach it", n)
	}
}

func TestBrowserLoginRejectsRequestsOffTheCallbackPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	broker := &fakeBroker{}
	callback, _ := startLogin(t, ctx, broker)

	base := strings.TrimSuffix(callback, "/auth/callback")
	if status, _ := get(t, base+"/favicon.ico?code=good"); status != http.StatusNotFound {
		t.Fatalf("favicon request got %d; only the registered path is a callback", status)
	}
	if n := broker.callbacks.Load(); n != 0 {
		t.Fatalf("broker saw %d callbacks from an unrelated path", n)
	}
}

func TestBrowserLoginGivesUpWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	broker := &fakeBroker{}
	callback, result := startLogin(t, ctx, broker)
	get(t, callback+"?code=bad")

	got := <-result
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; an unfinished login must surface the deadline", got.err)
	}
	if !strings.Contains(got.err.Error(), "login refused") {
		t.Fatalf("err = %v; the last refusal is the useful diagnostic and must be included", got.err)
	}
}

func TestBrowserLoginFailsFastOnABadRedirectURLOrABusyPort(t *testing.T) {
	broker := &fakeBroker{}
	if _, err := BrowserLogin(context.Background(), broker, Callback{RedirectURL: "/auth/callback"}); err == nil {
		t.Fatal("a relative RedirectURL was accepted; it cannot name a port to listen on")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busy := Callback{RedirectURL: "http://" + l.Addr().String() + "/cb", ListenAddr: l.Addr().String()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := BrowserLogin(ctx, broker, busy); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; a port in use must fail immediately, not wait for a callback that cannot arrive", err)
	}
}
