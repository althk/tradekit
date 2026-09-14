package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
	"github.com/althk/tradekit/go/store"
)

// Every process that logs into an Indian broker also wrote the same forty
// lines around BrowserLogin: read yesterday's token from the store, try it,
// fall back to the browser, save what came back. This file is those lines,
// once, over the ports rather than over one adapter.

// SessionBroker is what resuming a session needs of an adapter: the browser
// flow for a new token, the freshness check and installer for a stored one,
// and one cheap authenticated call to find out whether the venue still
// honours it. zerodha, upstox and fyers all satisfy it.
type SessionBroker interface {
	ports.BrowserLogin
	ports.TokenState
	Account(ctx context.Context) (domain.Account, error)
}

// Session says where a token is kept and how a new one is obtained.
type Session struct {
	// Key is the store's kv_state key for the token. Two brokers sharing
	// one store need two keys; the default is "broker_session".
	Key string
	// Callback is where the broker redirects the browser. See BrowserLogin.
	Callback Callback
	// Timeout bounds the browser login. Zero means five minutes: long
	// enough to find the phone for the OTP, short enough that a scheduled
	// run nobody is watching does not hang until the next one.
	Timeout time.Duration
}

// storedSession is the persisted token. Tokens expire overnight, so the issue
// time is what decides whether a stored one is still worth trying.
type storedSession struct {
	AccessToken string    `json:"access_token"`
	IssuedAt    time.Time `json:"issued_at"`
}

// EnsureSession makes broker usable for today: the stored token if it is
// still fresh and the venue accepts it, otherwise a browser login, persisted
// for the next run.
//
// The stored token is checked against the venue and not just by date,
// because a login elsewhere — the broker's web app, another bot — invalidates
// it without changing its age. Only ports.ErrTokenExpired from that check
// leads to the browser; any other failure is returned, since logging in again
// cannot fix a network that is down and would burn the day's login on it.
//
// A token that could not be persisted is not an error: the client already
// holds it, and the worst case is the browser again next run.
func EnsureSession(ctx context.Context, broker SessionBroker, db *store.DB, s Session) error {
	if s.Key == "" {
		s.Key = "broker_session"
	}
	if s.Timeout == 0 {
		s.Timeout = 5 * time.Minute
	}

	var saved storedSession
	err := db.GetState(ctx, s.Key, &saved)
	switch {
	case err == nil && saved.AccessToken != "":
		broker.SetAccessToken(saved.AccessToken, saved.IssuedAt)
		if broker.TokenFresh(ctx) {
			_, err := broker.Account(ctx)
			if err == nil {
				slog.Info("reusing stored broker session", "issued_at", saved.IssuedAt.Format(time.RFC3339))
				return nil
			}
			if !errors.Is(err, ports.ErrTokenExpired) {
				return fmt.Errorf("session: checking stored token: %w", err)
			}
			slog.Info("stored broker session is no longer valid; logging in again")
		}
	case err != nil && !errors.Is(err, store.ErrStateNotFound):
		return fmt.Errorf("session: reading stored token: %w", err)
	}

	loginCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	token, err := BrowserLogin(loginCtx, broker, s.Callback)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if err := db.SetState(ctx, s.Key, storedSession{AccessToken: token, IssuedAt: time.Now()}); err != nil {
		slog.Warn("could not persist the broker session; the next run will need the browser again", "err", err)
	}
	return nil
}
