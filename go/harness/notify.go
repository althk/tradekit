package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/althk/tradekit/go/core/ratelimit"
)

// Level is how urgent a notification is.
type Level int

// The levels. Info is routine, Warn needs a look, Alert needs a human now.
const (
	Info Level = iota
	Warn
	Alert
)

// String renders the level for a message prefix and for logs.
func (l Level) String() string {
	switch l {
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Alert:
		return "alert"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Notifier sends a message.
//
// Implementations must not block the caller and must not return an error the
// caller is expected to act on. Notify returning nothing is the design
// decision of this file: an error return invites `if err != nil { return err }`
// in the execution path, and then a Telegram outage cancels an exit. Failures
// are logged and counted; a caller who wants to know can read the counter.
type Notifier interface {
	Notify(ctx context.Context, level Level, msg string)
}

// Discard drops everything. It is the default for backtests and tests.
type Discard struct{}

// Notify does nothing.
func (Discard) Notify(context.Context, Level, string) {}

// Multi fans out to several notifiers.
type Multi []Notifier

// Notify delivers to every notifier in turn. Each is non-blocking by contract,
// so the fan-out is too.
func (m Multi) Notify(ctx context.Context, level Level, msg string) {
	for _, n := range m {
		n.Notify(ctx, level, msg)
	}
}

// telegramAPI is where a bot posts.
const telegramAPI = "https://api.telegram.org"

// message is one queued notification.
type message struct {
	level Level
	text  string
}

// Telegram posts to a chat.
//
// Sends are queued and delivered by one background goroutine, so a
// rate-limited or hung API cannot stall a trading loop — which is how dhaara's
// notifier, called from the execution path, once did. The queue is bounded and
// drops the oldest message on overflow: an unbounded queue in an alert storm is
// a memory leak that ends the process during exactly the incident the alerts
// were about.
type Telegram struct {
	token   Secret
	chatID  string
	client  *http.Client
	baseURL string
	limiter *ratelimit.Limiter
	log     *slog.Logger
	size    int
	timeout time.Duration

	mu      sync.Mutex
	pending []message
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	closing sync.Once

	dropped atomic.Int64
	failed  atomic.Int64
	sent    atomic.Int64
}

// Option configures a Telegram notifier.
type Option func(*Telegram)

// WithHTTPClient replaces the default client, which has a ten-second timeout.
func WithHTTPClient(c *http.Client) Option { return func(t *Telegram) { t.client = c } }

// WithBaseURL points the notifier at a test server instead of Telegram.
func WithBaseURL(u string) Option { return func(t *Telegram) { t.baseURL = u } }

// WithQueueSize bounds the number of undelivered messages. Default 256.
func WithQueueSize(n int) Option { return func(t *Telegram) { t.size = n } }

// WithRate bounds sends per second. Telegram allows about one message a second
// to a single chat, which is the default.
func WithRate(perSecond float64) Option {
	return func(t *Telegram) { t.limiter = ratelimit.New(perSecond) }
}

// WithLogger sets where drops and failures are reported. Default slog.Default.
func WithLogger(l *slog.Logger) Option { return func(t *Telegram) { t.log = l } }

// WithCloseTimeout bounds how long Close waits to drain. Default five seconds.
func WithCloseTimeout(d time.Duration) Option { return func(t *Telegram) { t.timeout = d } }

// NewTelegram starts a notifier posting to chatID with the bot token.
func NewTelegram(token Secret, chatID string, opts ...Option) (*Telegram, error) {
	if token == "" || chatID == "" {
		return nil, fmt.Errorf("harness: Telegram needs a bot token and a chat id")
	}
	t := &Telegram{
		token:   token,
		chatID:  chatID,
		client:  &http.Client{Timeout: 10 * time.Second},
		baseURL: telegramAPI,
		limiter: ratelimit.New(1),
		size:    256,
		timeout: 5 * time.Second,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(t)
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	if t.size < 1 {
		t.size = 1
	}
	var ctx context.Context
	ctx, t.cancel = context.WithCancel(context.Background())
	go t.run(ctx)
	return t, nil
}

// Notify queues a message and returns at once. The context is not used to
// wait: a cancelled trading loop still gets its last alert out.
func (t *Telegram) Notify(_ context.Context, level Level, msg string) {
	t.mu.Lock()
	if len(t.pending) >= t.size {
		// Drop the oldest: the newest message is the one describing the
		// current state, which is what a human catching up needs.
		t.pending = t.pending[1:]
		n := t.dropped.Add(1)
		t.mu.Unlock()
		t.log.Warn("harness: notification queue full, dropped oldest", "dropped_total", n)
		t.mu.Lock()
	}
	t.pending = append(t.pending, message{level: level, text: msg})
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Dropped returns how many messages were discarded on overflow.
func (t *Telegram) Dropped() int64 { return t.dropped.Load() }

// Failed returns how many sends Telegram refused or the network lost.
func (t *Telegram) Failed() int64 { return t.failed.Load() }

// Sent returns how many messages were delivered.
func (t *Telegram) Sent() int64 { return t.sent.Load() }

// Close drains pending messages within the close timeout, then stops the
// delivery goroutine. It returns an error when the deadline passed with
// messages undelivered. Calling it twice is safe.
func (t *Telegram) Close() error {
	t.closing.Do(func() { close(t.stop) })
	select {
	case <-t.done:
		return nil
	case <-time.After(t.timeout):
		t.cancel()
		<-t.done
		t.mu.Lock()
		left := len(t.pending)
		t.mu.Unlock()
		return fmt.Errorf("harness: Telegram closed with %d notifications undelivered after %v", left, t.timeout)
	}
}

// run is the delivery goroutine: it sends everything queued, sleeps until
// woken, and on stop drains what is left before exiting.
func (t *Telegram) run(ctx context.Context) {
	defer close(t.done)
	for {
		t.drain(ctx)
		select {
		case <-t.wake:
		case <-t.stop:
			t.drain(ctx)
			return
		case <-ctx.Done():
			return
		}
	}
}

// drain sends queued messages until the queue is empty or ctx ends.
func (t *Telegram) drain(ctx context.Context) {
	for ctx.Err() == nil {
		t.mu.Lock()
		if len(t.pending) == 0 {
			t.mu.Unlock()
			return
		}
		m := t.pending[0]
		t.pending = t.pending[1:]
		t.mu.Unlock()

		if err := t.limiter.Wait(ctx); err != nil {
			return
		}
		if err := t.send(ctx, m); err != nil {
			t.failed.Add(1)
			t.log.Warn("harness: notification failed", "level", m.level.String(), "err", err.Error())
			continue
		}
		t.sent.Add(1)
	}
}

// send posts one message. A non-info level is prefixed so the chat shows
// urgency without needing formatting the API may reject.
func (t *Telegram) send(ctx context.Context, m message) error {
	text := m.text
	switch m.level {
	case Warn:
		text = "[WARN] " + text
	case Alert:
		text = "[ALERT] " + text
	}
	body, err := json.Marshal(map[string]string{"chat_id": t.chatID, "text": text})
	if err != nil {
		return err
	}
	url := t.baseURL + "/bot" + t.token.Reveal() + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		// The error text carries the URL, and the URL carries the token.
		return fmt.Errorf("posting to telegram: %s", redactToken(err.Error(), t.token))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// redactToken masks the bot token wherever it appears in a message, because
// net/http errors quote the request URL.
func redactToken(s string, token Secret) string {
	return string(bytes.ReplaceAll([]byte(s), []byte(token.Reveal()), []byte(Redaction)))
}

// Compile-time proof that every notifier satisfies the interface.
var (
	_ Notifier = Discard{}
	_ Notifier = Multi{}
	_ Notifier = (*Telegram)(nil)
)
