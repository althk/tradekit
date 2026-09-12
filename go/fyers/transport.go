package fyers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/ratelimit"
)

// Bounds on a single call's retry behaviour.
//
// Four attempts with a two-second base doubles to 2s, 4s, 8s — about fourteen
// seconds of patience, which is long enough to ride out a per-second rate
// limit window and short enough that a stuck sync is noticed rather than
// hanging until the session ends. The per-minute window is a different matter:
// FYERS blocks an account for the rest of the day after the third breach, so
// the transport honours Retry-After rather than guessing.
const (
	defaultAttempts     = 4
	defaultRetryBase    = 2 * time.Second
	defaultMaxRetryWait = 60 * time.Second
)

// maxResponseBytes bounds a response read. The symbol master is fetched by a
// different path; nothing on the JSON API is legitimately this large, and an
// unbounded ReadAll against a confused endpoint is how a sync exhausts memory.
const maxResponseBytes = 16 << 20

// request is one call's parameters.
type request struct {
	method string
	// base overrides the client's API root. Market data lives on a
	// different root from trading, so the base cannot be a property of the
	// client alone.
	base    string
	path    string
	query   url.Values
	body    any
	out     any
	limiter *ratelimit.Limiter

	// retry allows the call to be repeated on a transient failure.
	//
	// It is explicit, and false for every order mutation, because retrying
	// a POST that placed an order places a second one. Making it a property
	// of the HTTP method instead would be wrong, since FYERS cancels an
	// order with DELETE and modifies one with PATCH, and neither of those
	// is safe to repeat either.
	retry bool
}

// envelope is the header every FYERS response carries. S is "ok" or "error";
// Code is FYERS's own status, which is not the HTTP status and is negative for
// every refusal.
type envelope struct {
	S       string `json:"s"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// doJSON performs a request, decodes the JSON response into req.out, and
// classifies any failure into one of the three sentinels.
//
// Retries are bounded, honour Retry-After when the server sends one, and stop
// immediately on a cancelled context — a scan that has been cancelled must stop
// making requests, not work through its backlog.
func (c *Client) doJSON(ctx context.Context, req request) error {
	attempts := 1
	if req.retry {
		attempts = c.attempts
	}

	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lim := req.limiter
		if lim == nil {
			lim = c.general
		}
		if err := lim.Wait(ctx); err != nil {
			return err
		}

		retryAfter, err := c.doOnce(ctx, req)
		if err == nil {
			return nil
		}
		last = err

		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == attempts {
			return err
		}
		if err := c.sleep(ctx, backoff(c.retryBase, attempt, retryAfter)); err != nil {
			return err
		}
	}
	return last
}

// doOnce issues one request. It returns the Retry-After the server asked for,
// in addition to the error, so the caller can honour it rather than guessing.
func (c *Client) doOnce(ctx context.Context, req request) (retryAfter time.Duration, err error) {
	base := req.base
	if base == "" {
		base = c.baseURL
	}
	u := base + req.path
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}

	var reader io.Reader
	if req.body != nil {
		raw, err := json.Marshal(req.body)
		if err != nil {
			return 0, fmt.Errorf("fyers: encoding request body for %s: %w", req.path, err)
		}
		reader = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, u, reader)
	if err != nil {
		return 0, fmt.Errorf("fyers: building request %s %s: %w", req.method, req.path, err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if h := c.authHeader(); h != "" {
		httpReq.Header.Set("Authorization", h)
	}
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure has no status to classify, and repeating it
		// is legitimate: the connection, not the request, is what failed.
		return 0, &APIError{Message: err.Error(), sentinel: ErrTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, &APIError{StatusCode: resp.StatusCode, Message: err.Error(), sentinel: ErrTransient}
	}
	retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), resp.Header.Get("X-Retry-After-Ms"))

	env := readEnvelope(raw)
	// A refusal can arrive under a 2xx: the reference documents an order
	// rejection as HTTP 200 with s:"error" and code -99, and a token
	// failure on some endpoints the same way. The body is therefore
	// inspected even when the status says the call succeeded.
	if resp.StatusCode >= 400 || env.refused() {
		message := env.Message
		if message == "" {
			message = truncate(strings.TrimSpace(string(raw)), 300)
		}
		return retryAfter, &APIError{
			StatusCode: resp.StatusCode,
			Code:       env.Code,
			Message:    message,
			sentinel:   classify(resp.StatusCode, env.Code),
		}
	}

	if req.out != nil {
		if err := json.Unmarshal(raw, req.out); err != nil {
			// A body that will not decode is FYERS answering something
			// other than what it documents. It is not retryable: the
			// same malformed answer will arrive again.
			return 0, &APIError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("decoding %s: %v", req.path, err),
				sentinel:   ErrRejected,
			}
		}
	}
	return 0, nil
}

// refused reports whether the envelope says the call failed.
//
// s:"error" is the documented signal. A negative code with no s at all is
// treated the same way, because the token-failure responses seen in the wild
// carry the code and message but not always the status field.
func (e envelope) refused() bool {
	if strings.EqualFold(strings.TrimSpace(e.S), responseError) {
		return true
	}
	return e.S == "" && e.Code < 0
}

// readEnvelope decodes the header fields, tolerating a body that is not an
// object at all — the history endpoint on an empty range has been seen to
// answer with a bare array — by returning an empty envelope.
func readEnvelope(raw []byte) envelope {
	var env envelope
	_ = json.Unmarshal(raw, &env)
	return env
}

// backoff returns how long to wait before an attempt, preferring the server's
// own Retry-After over the client's exponential schedule.
//
// The server knows when its window resets and the client does not, so a
// Retry-After is obeyed even when it is longer than the schedule would be — but
// it is capped, because an absurd value from a confused proxy would otherwise
// stall a sync for the rest of the day.
func backoff(base time.Duration, attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > defaultMaxRetryWait {
			return defaultMaxRetryWait
		}
		return retryAfter
	}
	d := base << (attempt - 1)
	if d > defaultMaxRetryWait {
		return defaultMaxRetryWait
	}
	return d
}

// parseRetryAfter reads the wait the server asked for.
//
// FYERS sends both the standard Retry-After in seconds and X-Retry-After-Ms
// in milliseconds. The millisecond header is preferred when present because
// it is the precise one: a Retry-After of "1" for a 150ms window would make a
// ten-order burst take ten seconds instead of one and a half.
func parseRetryAfter(seconds, millis string) time.Duration {
	if ms, err := strconv.ParseInt(strings.TrimSpace(millis), 10, 64); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	seconds = strings.TrimSpace(seconds)
	if seconds == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(seconds, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(seconds); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// truncate bounds a message taken from a response body, so one confused
// endpoint cannot fill the log with megabytes of HTML.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
