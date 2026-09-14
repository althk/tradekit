package zerodha

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/althk/tradekit/go/core/ports"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"
)

// The three answers a caller actually acts on.
//
// Everything a broker can refuse reduces to one of these: re-authenticate,
// retry, or stop. Matching on message text — which is what every project this
// adapter replaces did — is how a bot comes to place the same rejected order
// forty times.
var (
	// ErrTokenExpired means the access token is no longer valid and the
	// caller must log in again. Retrying with the same token cannot succeed.
	ErrTokenExpired = fmt.Errorf("zerodha: %w", ports.ErrTokenExpired)
	// ErrTransient means the failure was in transport, in Kite's own
	// infrastructure, or in the rate limiter, and the same call may succeed
	// if repeated.
	ErrTransient = errors.New("zerodha: transient failure")
	// ErrRejected means Kite refused the request on its merits. Repeating it
	// unchanged will be refused again.
	ErrRejected = errors.New("zerodha: rejected")
)

// classify wraps a Kite error with one of the sentinels above so callers can
// use errors.Is. An error that is not a kiteconnect.Error — a cancelled
// context, a local validation failure — is returned unchanged, because
// labelling it would claim knowledge this function does not have.
//
// The SDK's error is still reachable with errors.As, so a caller that needs
// Kite's own code or message has not lost it.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var kerr kiteconnect.Error
	if !errors.As(err, &kerr) {
		return err
	}
	return &classified{sentinel: sentinelFor(kerr), err: err}
}

// sentinelFor picks the sentinel for a Kite error.
func sentinelFor(err kiteconnect.Error) error {
	// Rate limiting arrives as HTTP 429 carrying whichever ErrorType Kite
	// felt like attaching — usually InputException, which would otherwise
	// read as a permanent rejection and stop a sync that only needed to
	// slow down. The status wins over the type here.
	if err.Code == http.StatusTooManyRequests {
		return ErrTransient
	}
	switch err.ErrorType {
	case kiteconnect.TokenError:
		return ErrTokenExpired
	case kiteconnect.NetworkError, kiteconnect.GeneralError, kiteconnect.DataError:
		// All three are Kite's side failing, not the request being wrong:
		// the OMS being unreachable, an unexpected internal error, and a
		// data feed that could not be read. Repeating them is legitimate.
		return ErrTransient
	default:
		// OrderException, InputException, PermissionException,
		// UserException, TwoFAException — and anything Kite adds later.
		// An unrecognised type is treated as a rejection rather than as
		// retryable, because the cost of not retrying a transient failure
		// is one missed call, and the cost of retrying a rejection is the
		// same bad order sent repeatedly.
		return ErrRejected
	}
}

// classified carries both the sentinel and the SDK's own error, so errors.Is
// finds the first and errors.As the second.
type classified struct {
	sentinel error
	err      error
}

// Error reports the underlying message. The sentinel is a classification for
// code to branch on, not text worth repeating to a human who already has Kite's
// own description of what went wrong.
func (c *classified) Error() string { return c.err.Error() }

// Unwrap returns both branches so errors.Is matches the sentinel and errors.As
// reaches kiteconnect.Error.
func (c *classified) Unwrap() []error { return []error{c.sentinel, c.err} }
