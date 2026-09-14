package harness

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/ports"
)

// Every project that logs into an Indian broker wrote the same throwaway HTTP
// server to catch the redirect, and no two were the same: one registered on
// http.DefaultServeMux and panicked on the second login of the day, one
// indexed into the query and hung forever when the broker sent status=error,
// most had no timeout. This file is that server, once.

// Callback says where the broker sends the browser after login.
type Callback struct {
	// RedirectURL is the URL registered with the broker, e.g.
	// http://127.0.0.1:8080/auth/callback. Its path is the one served and its
	// port the one listened on; the broker compares it to the registered
	// value character for character, so it is taken whole rather than as
	// separate host, port and path that could drift apart.
	RedirectURL string

	// ListenAddr overrides the address the server binds. Empty binds every
	// interface on RedirectURL's port. Set it when the registered URL is a
	// public name that a proxy or tunnel forwards to a different local port.
	ListenAddr string
}

// BrowserLogin obtains the day's access token: it serves the redirect, logs
// the login URL for the user to visit, exchanges the code the broker sends
// back, and returns the token, which the adapter has already installed on
// itself. The caller persists the token so a restart does not need the
// browser again.
//
// It returns when a callback succeeds or ctx ends; a refused login is shown
// in the browser with a link to try again and the server keeps waiting,
// because the person who can fix it is looking at that page. Give ctx a
// deadline — a login nobody completes must not hold a scheduler forever.
//
// The URL is logged rather than opened in a browser: most of these processes
// run on a box with no browser, and the ones that do not can open it
// themselves from the log line.
func BrowserLogin(ctx context.Context, broker ports.BrowserLogin, cb Callback) (string, error) {
	target, err := url.Parse(cb.RedirectURL)
	if err != nil || target.Host == "" {
		return "", fmt.Errorf("login: RedirectURL %q is not an absolute URL", cb.RedirectURL)
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	addr := cb.ListenAddr
	if addr == "" {
		port := target.Port()
		if port == "" {
			port = "80"
		}
		addr = ":" + port
	}

	// Bind before announcing the URL so a port already in use fails here,
	// synchronously, rather than as a login that never arrives.
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("login: listening on %s for the redirect: %w", addr, err)
	}

	// LoginURL before Serve: a callback can only arrive after the user has
	// the URL, and FYERS mints the state it later checks in this call.
	s := &loginSession{ctx: ctx, broker: broker, path: path, loginURL: broker.LoginURL(), done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("broker login required: open the login URL in a browser", "url", s.loginURL, "callback", cb.RedirectURL)

	select {
	case <-s.done:
		return s.token, nil
	case err := <-serveErr:
		return "", fmt.Errorf("login: callback server: %w", err)
	case <-ctx.Done():
		s.mu.Lock()
		last := s.lastErr
		s.mu.Unlock()
		if last != nil {
			return "", fmt.Errorf("login: %w; last callback failed: %v", ctx.Err(), last)
		}
		return "", fmt.Errorf("login: %w before the browser login completed", ctx.Err())
	}
}

// loginSession is one wait for one token.
type loginSession struct {
	ctx      context.Context
	broker   ports.BrowserLogin
	path     string
	loginURL string

	mu      sync.Mutex
	token   string
	lastErr error
	done    chan struct{} // closed once token is set
}

// handle serves the redirect. The exchange runs inside the handler so the
// browser shows the real outcome: a "success" page followed by a failed
// exchange in the log is exactly the confusion this replaces.
func (s *loginSession) handle(w http.ResponseWriter, r *http.Request) {
	// ServeMux matches "/" as a prefix; anything but the registered path
	// (favicon requests, mostly) is not a callback.
	if r.URL.Path != s.path {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		// A reload of the success page, or the broker retrying the redirect.
		// The first callback already won; do not exchange a second code.
		writePage(w, http.StatusOK, "Login already completed", "You can close this tab.", "")
		return
	}

	token, err := s.broker.LoginCallback(s.ctx, r.URL.Query())
	if err != nil {
		s.lastErr = err
		slog.Warn("broker login callback failed; waiting for another attempt", "err", err)
		retry := fmt.Sprintf(`<p><a href="%s">Try again</a></p>`, html.EscapeString(s.loginURL))
		writePage(w, http.StatusBadRequest, "Login failed", err.Error(), retry)
		return
	}
	s.token = token
	close(s.done)
	writePage(w, http.StatusOK, "Login successful", "You can close this tab.", "")
}

// writePage renders the one-line result page the user sees in the browser.
func writePage(w http.ResponseWriter, status int, title, message, extra string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</title></head><body><h1>")
	b.WriteString(html.EscapeString(title))
	b.WriteString("</h1><p>")
	b.WriteString(html.EscapeString(message))
	b.WriteString("</p>")
	b.WriteString(extra)
	b.WriteString("</body></html>")
	_, _ = w.Write([]byte(b.String()))
}
