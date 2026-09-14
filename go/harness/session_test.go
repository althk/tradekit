package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/ports"
)

// sessionFake is a fakeBroker that also holds a token. Account accepts the
// token named by accepts and reports every other one as expired, unless
// accountErr is set, which it returns as is.
type sessionFake struct {
	fakeBroker
	token      string
	issuedAt   time.Time
	accepts    string
	accountErr error
}

func (f *sessionFake) SetAccessToken(token string, issuedAt time.Time) {
	f.token, f.issuedAt = token, issuedAt
}

func (f *sessionFake) TokenFresh(context.Context) bool {
	return f.token != "" && time.Since(f.issuedAt) < 12*time.Hour
}

func (f *sessionFake) Account(context.Context) (domain.Account, error) {
	if f.accountErr != nil {
		return domain.Account{}, f.accountErr
	}
	if f.token != f.accepts {
		return domain.Account{}, fmt.Errorf("fake: %w", ports.ErrTokenExpired)
	}
	return domain.Account{}, nil
}

// completeLogin answers the callback server once it is up, so EnsureSession
// can return. It runs in the background because EnsureSession blocks.
func completeLogin(t *testing.T, callback string) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(callback + "?code=good")
			if err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestEnsureSessionReusesAStoredTokenTheVenueAccepts(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	if err := db.SetState(ctx, "s", storedSession{AccessToken: "stored", IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	broker := &sessionFake{accepts: "stored"}

	if err := EnsureSession(ctx, broker, db, Session{Key: "s"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if broker.token != "stored" || broker.callbacks.Load() != 0 {
		t.Errorf("a fresh, accepted token must be reused without a browser login; token=%q callbacks=%d",
			broker.token, broker.callbacks.Load())
	}
}

func TestEnsureSessionLogsInWhenTheVenueRejectsTheStoredToken(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.SetState(ctx, "s", storedSession{AccessToken: "stale", IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	broker := &sessionFake{accepts: "token-good"}
	port := freePort(t)
	callback := fmt.Sprintf("http://127.0.0.1:%s/auth/callback", port)
	completeLogin(t, callback)

	err := EnsureSession(ctx, broker, db, Session{Key: "s", Callback: Callback{RedirectURL: callback, ListenAddr: "127.0.0.1:" + port}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if broker.callbacks.Load() != 1 {
		t.Errorf("a token the venue reports expired must lead to one browser login, got %d", broker.callbacks.Load())
	}
	var saved storedSession
	if err := db.GetState(ctx, "s", &saved); err != nil || saved.AccessToken != "token-good" {
		t.Errorf("the new token must be persisted for the next run, got %+v %v", saved, err)
	}
}

func TestEnsureSessionLogsInWhenNothingIsStored(t *testing.T) {
	db := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	broker := &sessionFake{accepts: "token-good"}
	port := freePort(t)
	callback := fmt.Sprintf("http://127.0.0.1:%s/auth/callback", port)
	completeLogin(t, callback)

	err := EnsureSession(ctx, broker, db, Session{Callback: Callback{RedirectURL: callback, ListenAddr: "127.0.0.1:" + port}})
	if err != nil {
		t.Fatalf("a cold start must log in, not fail on the missing key: %v", err)
	}
	var saved storedSession
	if err := db.GetState(ctx, "broker_session", &saved); err != nil || saved.AccessToken != "token-good" {
		t.Errorf("the token must be saved under the default key, got %+v %v", saved, err)
	}
}

func TestEnsureSessionDoesNotLogInOverAnUnrelatedFailure(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	if err := db.SetState(ctx, "s", storedSession{AccessToken: "stored", IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	down := errors.New("fake: connection refused")
	broker := &sessionFake{accepts: "stored", accountErr: down}

	err := EnsureSession(ctx, broker, db, Session{Key: "s"})
	if !errors.Is(err, down) {
		t.Fatalf("a failure that is not token expiry must be returned, got %v", err)
	}
	if broker.callbacks.Load() != 0 {
		t.Errorf("a network failure must not burn a browser login, got %d callbacks", broker.callbacks.Load())
	}
}
