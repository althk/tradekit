package zerodha

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error // nil means "matches none of the sentinels"
	}{
		{
			name: "an expired token asks for re-authentication, not for the order to be abandoned",
			err:  kiteconnect.Error{Code: http.StatusForbidden, ErrorType: kiteconnect.TokenError, Message: "Invalid `api_key` or `access_token`."},
			want: ErrTokenExpired,
		},
		{
			name: "a network failure is worth retrying",
			err:  kiteconnect.Error{Code: http.StatusGatewayTimeout, ErrorType: kiteconnect.NetworkError, Message: "gateway timeout"},
			want: ErrTransient,
		},
		{
			name: "rate limiting is transient however Kite labels it",
			err:  kiteconnect.Error{Code: http.StatusTooManyRequests, ErrorType: kiteconnect.InputError, Message: "Too many requests"},
			want: ErrTransient,
		},
		{
			name: "Kite's own internal error is transient",
			err:  kiteconnect.Error{Code: http.StatusInternalServerError, ErrorType: kiteconnect.GeneralError, Message: "Something went wrong"},
			want: ErrTransient,
		},
		{
			name: "a rejected order must not be retried",
			err:  kiteconnect.Error{Code: http.StatusBadRequest, ErrorType: kiteconnect.OrderError, Message: "Insufficient funds"},
			want: ErrRejected,
		},
		{
			name: "a malformed request must not be retried",
			err:  kiteconnect.Error{Code: http.StatusBadRequest, ErrorType: kiteconnect.InputError, Message: "Invalid tradingsymbol"},
			want: ErrRejected,
		},
		{
			name: "a permission failure will not succeed on a second attempt",
			err:  kiteconnect.Error{Code: http.StatusForbidden, ErrorType: kiteconnect.PermissionError, Message: "not enabled"},
			want: ErrRejected,
		},
		{
			name: "an error type Kite adds later is treated as a rejection rather than retried",
			err:  kiteconnect.Error{Code: http.StatusBadRequest, ErrorType: "SomethingNewException", Message: "?"},
			want: ErrRejected,
		},
		{
			name: "a non-SDK error carries no classification we can honestly claim",
			err:  errors.New("dial tcp: connection refused"),
			want: nil,
		},
	}

	sentinels := []error{ErrTokenExpired, ErrTransient, ErrRejected}
	for _, c := range cases {
		got := classify(c.err)
		for _, s := range sentinels {
			match := errors.Is(got, s)
			if s == c.want && !match {
				t.Errorf("%s: classify(%v) does not match %v", c.name, c.err, s)
			}
			if s != c.want && match {
				t.Errorf("%s: classify(%v) wrongly matches %v", c.name, c.err, s)
			}
		}
	}
}

func TestClassifyNilStaysNil(t *testing.T) {
	if got := classify(nil); got != nil {
		t.Errorf("classify(nil) = %v; a success must not become a failure", got)
	}
}

func TestClassifyKeepsTheSDKError(t *testing.T) {
	// The caller that needs Kite's own code or message must still be able to
	// reach it; classification adds a label, it does not replace the error.
	original := kiteconnect.Error{Code: http.StatusBadRequest, ErrorType: kiteconnect.OrderError, Message: "Insufficient funds"}

	// Wrapped the way an adapter method wraps it, so the test covers the path
	// callers actually see.
	err := fmt.Errorf("zerodha: placing buy 10 NSE:SBIN: %w", classify(original))

	if !errors.Is(err, ErrRejected) {
		t.Fatal("the sentinel must survive the adapter's fmt.Errorf wrapping")
	}
	var kerr kiteconnect.Error
	if !errors.As(err, &kerr) {
		t.Fatal("the SDK error must remain reachable with errors.As")
	}
	if kerr.Message != original.Message {
		t.Errorf("recovered message = %q, want %q", kerr.Message, original.Message)
	}
}

func TestClassifiedErrorReportsKitesMessage(t *testing.T) {
	// The sentinel is a classification for code to branch on. Prefixing it to
	// the text would bury Kite's own description of what went wrong.
	original := kiteconnect.Error{Code: http.StatusBadRequest, ErrorType: kiteconnect.OrderError, Message: "Insufficient funds"}
	if got, want := classify(original).Error(), original.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
