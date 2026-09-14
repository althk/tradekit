package zerodha

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func TestLoginCallbackReportsARefusedLoginInsteadOfExchanging(t *testing.T) {
	c, err := New(Options{APIKey: "key", APISecret: "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	q := url.Values{"status": {"error"}, "message": {"user cancelled"}}
	_, err = c.LoginCallback(context.Background(), q)
	if err == nil {
		t.Fatal("a callback without request_token must fail, not exchange an empty token")
	}
	if !strings.Contains(err.Error(), "user cancelled") {
		t.Errorf("the error must carry Kite's reason so the user can act on it, got %v", err)
	}
	if _, err := c.LoginCallback(context.Background(), url.Values{}); err == nil {
		t.Error("an empty callback must fail rather than panic or hang")
	}
}
