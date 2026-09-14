package fyers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/althk/tradekit/go/core/ports"
)

// The three answers a caller actually acts on.
//
// They are the same three the Kite and Upstox adapters define, deliberately: a
// consumer that switches venues should not have to relearn what a refusal
// means. Everything a broker can refuse reduces to re-authenticate, retry, or
// stop.
var (
	// ErrTokenExpired means the access token is no longer valid and the
	// caller must log in again. Retrying with the same token cannot succeed.
	ErrTokenExpired = fmt.Errorf("fyers: %w", ports.ErrTokenExpired)
	// ErrTransient means the failure was in transport, in FYERS's own
	// infrastructure, or in a rate limit, and the same call may succeed if
	// repeated.
	ErrTransient = errors.New("fyers: transient failure")
	// ErrRejected means FYERS refused the request on its merits. Repeating
	// it unchanged will be refused again.
	ErrRejected = errors.New("fyers: rejected")
)

// FYERS's application-level error codes, carried in the "code" field of the
// envelope alongside the HTTP status.
//
// The token codes matter because FYERS answers an expired token with HTTP 401
// on most endpoints but not all; the body's code is the reliable signal. The
// rate-limit code matters for the opposite reason: the reference documents it
// as the body's way of saying 429, and a rate limit read as a rejection would
// stop a sync that only needed to wait.
const (
	codeTokenExpired      = -8
	codeTokenInvalid      = -15
	codeTokenUnverifiable = -16
	codeTokenBad          = -17
	codeRateLimited       = -429
)

// APIError is one refusal from FYERS, carrying enough of the response to
// diagnose it. It wraps one of the sentinels above, so errors.Is classifies it
// and errors.As reaches the code and message.
type APIError struct {
	// StatusCode is the HTTP status. It is 0 when the failure was in
	// transport and no response arrived.
	StatusCode int
	// Code is FYERS's own code from the envelope, such as -50 for an
	// invalid parameter. Zero when the body carried none.
	Code int
	// Message is FYERS's description of the refusal.
	Message string

	sentinel error
}

// Error renders the broker's own description. The sentinel is a classification
// for code to branch on, not text worth repeating to a human who already has
// FYERS's account of what went wrong.
func (e *APIError) Error() string {
	switch {
	case e.Code != 0 && e.Message != "":
		return fmt.Sprintf("fyers: HTTP %d: code %d: %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("fyers: HTTP %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("fyers: HTTP %d", e.StatusCode)
	}
}

// Unwrap returns the sentinel so errors.Is matches it.
func (e *APIError) Unwrap() error { return e.sentinel }

// Retryable reports whether repeating the call could succeed. It is the
// question the transport asks before spending another attempt.
func (e *APIError) Retryable() bool { return errors.Is(e.sentinel, ErrTransient) }

// isTokenCode reports whether an envelope code means the token is unusable.
func isTokenCode(code int) bool {
	switch code {
	case codeTokenExpired, codeTokenInvalid, codeTokenUnverifiable, codeTokenBad:
		return true
	default:
		return false
	}
}

// classify picks the sentinel for a response.
//
// The envelope's code is consulted before the status, because FYERS reports
// both and the code is the more specific: a 400 carrying -8 is an expired
// token, not a bad request, and re-sending it would only be refused again.
func classify(status, code int) error {
	switch {
	case isTokenCode(code) || status == http.StatusUnauthorized:
		// The one refusal whose remedy is a login rather than a retry
		// or a fix.
		return ErrTokenExpired
	case code == codeRateLimited || status == http.StatusTooManyRequests:
		return ErrTransient
	case status >= 500:
		// FYERS's side failing, not the request being wrong.
		return ErrTransient
	default:
		// Every other refusal — a bad symbol, an unknown order id, a
		// missing entitlement, a rejected order. An unrecognised refusal
		// is treated as a rejection rather than as retryable, because
		// the cost of not retrying a transient failure is one missed
		// call, and the cost of retrying a rejection is the same bad
		// order sent repeatedly.
		return ErrRejected
	}
}
