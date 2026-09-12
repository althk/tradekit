package upstox

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
// seconds of patience, which is long enough to ride out a rate-limit window and
// short enough that a stuck sync is noticed rather than hanging until the
// session ends.
const (
	defaultAttempts     = 4
	defaultRetryBase    = 2 * time.Second
	defaultMaxRetryWait = 60 * time.Second
)

// maxResponseBytes bounds a response read. The instrument master is fetched by
// a different path; nothing on the JSON API is legitimately this large, and an
// unbounded ReadAll against a confused endpoint is how a sync exhausts memory.
const maxResponseBytes = 16 << 20

// request is one call's parameters.
type request struct {
	method string
	// base overrides the client's API root. The expired-contract endpoints
	// live on a different version root from the ones beside them, so the
	// base cannot be a property of the client alone.
	base    string
	path    string
	query   url.Values
	body    any
	out     any
	limiter *ratelimit.Limiter

	// retry allows the call to be repeated on a transient failure.
	//
	// It is explicit, and false for every order mutation, because retrying
	// a POST that placed an order places a second one. dhaara's doJSON
	// carries the same flag for the same reason; making it a property of
	// the HTTP method instead would be wrong, since Upstox cancels an
	// order with DELETE and modifies one with PUT.
	retry bool
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
			return 0, fmt.Errorf("upstox: encoding request body for %s: %w", req.path, err)
		}
		reader = bytes.NewReader(raw)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, u, reader)
	if err != nil {
		return 0, fmt.Errorf("upstox: building request %s %s: %w", req.method, req.path, err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if token := c.token(); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
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
	retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))

	code, message := apiError(raw)
	// A rate limit can arrive under a 2xx, so the body is inspected even
	// when the status says the call succeeded.
	if resp.StatusCode >= 400 || isRateLimited(code, string(raw)) {
		if message == "" {
			message = truncate(strings.TrimSpace(string(raw)), 300)
		}
		return retryAfter, &APIError{
			StatusCode: resp.StatusCode,
			Code:       code,
			Message:    message,
			sentinel:   classify(resp.StatusCode, code, string(raw)),
		}
	}

	if req.out != nil {
		if err := json.Unmarshal(raw, req.out); err != nil {
			// A body that will not decode is Upstox answering something
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

// apiError extracts the code and message from Upstox's error envelope,
// returning empty strings when the body carries none.
func apiError(raw []byte) (code, message string) {
	var env struct {
		Errors []struct {
			ErrorCode string `json:"errorCode"`
			Message   string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Errors) == 0 {
		return "", ""
	}
	return env.Errors[0].ErrorCode, env.Errors[0].Message
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

// parseRetryAfter reads the header in its seconds form, which is what Upstox
// sends. An HTTP-date form is accepted too rather than being discarded.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(h, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(h); err == nil {
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

// doForm executes a pre-built request and decodes its JSON response, applying
// the same error classification as doJSON.
//
// It exists for the token exchange, which is the one call on the API that is
// form-encoded rather than JSON. It is never retried: an authorisation code is
// single-use, so a second attempt with the same code is refused even when the
// first failed in transport.
func (c *Client) doForm(ctx context.Context, req *http.Request, out any) error {
	if err := c.general.Wait(ctx); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &APIError{Message: err.Error(), sentinel: ErrTransient}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &APIError{StatusCode: resp.StatusCode, Message: err.Error(), sentinel: ErrTransient}
	}
	code, message := apiError(raw)
	if resp.StatusCode >= 400 {
		if message == "" {
			message = truncate(strings.TrimSpace(string(raw)), 300)
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Code:       code,
			Message:    message,
			sentinel:   classify(resp.StatusCode, code, string(raw)),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &APIError{StatusCode: resp.StatusCode, Message: err.Error(), sentinel: ErrRejected}
	}
	return nil
}
