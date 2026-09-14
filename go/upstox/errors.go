package upstox

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/althk/tradekit/go/core/ports"
)

// The three answers a caller actually acts on.
//
// They are the same three the Kite adapter defines, deliberately: a consumer
// that switches venues should not have to relearn what a refusal means.
// Everything a broker can refuse reduces to re-authenticate, retry, or stop.
var (
	// ErrTokenExpired means the access token is no longer valid and the
	// caller must log in again. Retrying with the same token cannot succeed.
	ErrTokenExpired = fmt.Errorf("upstox: %w", ports.ErrTokenExpired)
	// ErrTransient means the failure was in transport, in Upstox's own
	// infrastructure, or in a rate limit, and the same call may succeed if
	// repeated.
	ErrTransient = errors.New("upstox: transient failure")
	// ErrRejected means Upstox refused the request on its merits. Repeating
	// it unchanged will be refused again.
	ErrRejected = errors.New("upstox: rejected")
)

// rateLimitCode is Upstox's rate-limit error code.
//
// It is checked in the body as well as against the status, because Upstox
// sometimes returns it under a 2xx: breakout500's client documents exactly
// that, and a rate limit read as success is a caller that carries on hammering
// the endpoint with an empty result set.
const rateLimitCode = "UDAPI10005"

// APIError is one refusal from Upstox, carrying enough of the response to
// diagnose it. It wraps one of the sentinels above, so errors.Is classifies it
// and errors.As reaches the code and message.
type APIError struct {
	// StatusCode is the HTTP status. It is 0 when the failure was in
	// transport and no response arrived.
	StatusCode int
	// Code is Upstox's own error code from the envelope, such as
	// "UDAPI1148". Empty when the body carried no envelope.
	Code string
	// Message is Upstox's description of the refusal.
	Message string

	sentinel error
}

// Error renders the broker's own description. The sentinel is a classification
// for code to branch on, not text worth repeating to a human who already has
// Upstox's account of what went wrong.
func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("upstox: HTTP %d: %s: %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("upstox: HTTP %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("upstox: HTTP %d", e.StatusCode)
	}
}

// Unwrap returns the sentinel so errors.Is matches it.
func (e *APIError) Unwrap() error { return e.sentinel }

// Retryable reports whether repeating the call could succeed. It is the
// question the transport asks before spending another attempt.
func (e *APIError) Retryable() bool { return errors.Is(e.sentinel, ErrTransient) }

// isRateLimited reports whether a response is Upstox saying "ask again later".
//
// The extracted envelope code is compared exactly and the raw body only for the
// quoted form, because a prefix match is wrong here: UDAPI100050 is "invalid
// token", and reading it as the rate-limit code UDAPI10005 would classify an
// expired token as retryable — so a scheduler would spend its whole retry
// budget re-sending calls that cannot succeed instead of logging in again.
func isRateLimited(code, body string) bool {
	return strings.EqualFold(code, rateLimitCode) || strings.Contains(body, `"`+rateLimitCode+`"`)
}

// classify picks the sentinel for a response.
//
// The status wins over the body, except that a rate-limit code forces
// ErrTransient regardless of status — the 2xx case above.
func classify(status int, code, body string) error {
	if isRateLimited(code, body) {
		return ErrTransient
	}
	switch {
	case status == http.StatusUnauthorized:
		// Upstox answers an expired daily token with 401. It is the one
		// refusal whose remedy is a login rather than a retry or a fix.
		return ErrTokenExpired
	case status == http.StatusTooManyRequests:
		return ErrTransient
	case status >= 500:
		// Upstox's side failing, not the request being wrong.
		return ErrTransient
	default:
		// Every other 4xx — a bad instrument key, an unknown order id, a
		// missing entitlement, a malformed payload. An unrecognised
		// refusal is treated as a rejection rather than as retryable,
		// because the cost of not retrying a transient failure is one
		// missed call, and the cost of retrying a rejection is the same
		// bad order sent repeatedly.
		return ErrRejected
	}
}
