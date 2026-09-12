package harness

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chat is a fake Telegram that records what it was sent, optionally hanging.
type chat struct {
	mu       sync.Mutex
	texts    []string
	hang     chan struct{} // when non-nil, requests block until it is closed
	requests atomic.Int32
	server   *httptest.Server
}

func newChat(t *testing.T) *chat {
	t.Helper()
	c := &chat{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		if c.hang != nil {
			<-c.hang
		}
		if !strings.HasPrefix(r.URL.Path, "/bot") || !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg map[string]string
		_ = json.Unmarshal(body, &msg)
		c.mu.Lock()
		c.texts = append(c.texts, msg["text"])
		c.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *chat) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNotifyDoesNotBlockWhenTheTransportHangs(t *testing.T) {
	c := newChat(t)
	c.hang = make(chan struct{})
	tg, err := NewTelegram("tok", "chat", WithBaseURL(c.server.URL), WithLogger(quiet()), WithCloseTimeout(50*time.Millisecond), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			tg.Notify(context.Background(), Alert, "position open with no stop")
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked on a hung transport; a notifier that can stall the trading loop is exactly the failure this file exists to prevent")
	}

	if err := tg.Close(); err == nil {
		t.Error("Close must report that it gave up with messages undelivered")
	}
	close(c.hang)
}

func TestOverflowDropsOldestAndReportsTheCount(t *testing.T) {
	c := newChat(t)
	c.hang = make(chan struct{})
	tg, err := NewTelegram("tok", "chat", WithBaseURL(c.server.URL), WithQueueSize(3), WithLogger(quiet()), WithCloseTimeout(2*time.Second), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}
	// The first message is taken by the worker and hangs in flight; the
	// next three fill the queue; the last two evict the oldest queued.
	for _, m := range []string{"m0", "m1", "m2", "m3", "m4", "m5"} {
		tg.Notify(context.Background(), Info, m)
		time.Sleep(5 * time.Millisecond)
	}
	if got := tg.Dropped(); got != 2 {
		t.Errorf("two messages must have been dropped, counter says %d", got)
	}
	close(c.hang)
	if err := tg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := c.received()
	if strings.Join(got, ",") != "m0,m3,m4,m5" {
		t.Errorf("the newest messages survive and the oldest are dropped; delivered %v", got)
	}
}

func TestCloseDrainsPendingMessagesWithinItsDeadline(t *testing.T) {
	c := newChat(t)
	tg, err := NewTelegram("tok", "chat", WithBaseURL(c.server.URL), WithLogger(quiet()), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		tg.Notify(context.Background(), Warn, "w")
	}
	start := time.Now()
	if err := tg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Error("Close took too long draining a healthy transport")
	}
	if n := len(c.received()); n != 20 {
		t.Errorf("Close must deliver everything queued, delivered %d of 20", n)
	}
	if tg.Sent() != 20 || tg.Failed() != 0 {
		t.Errorf("counters: sent %d failed %d", tg.Sent(), tg.Failed())
	}
	if got := c.received()[0]; got != "[WARN] w" {
		t.Errorf("a warning is prefixed so the chat shows urgency, got %q", got)
	}
	if err := tg.Close(); err != nil {
		t.Errorf("a second Close must be safe, got %v", err)
	}
}

func TestAFailingTransportIsCountedNotRaised(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil))
	tg, err := NewTelegram("s3cret-token", "chat", WithBaseURL(server.URL), WithLogger(log), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}
	tg.Notify(context.Background(), Info, "hello")
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
	if tg.Failed() != 1 {
		t.Errorf("a refused send is counted, got %d", tg.Failed())
	}
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "notification failed") {
		t.Errorf("the failure must be logged, got %q", logged)
	}
	if strings.Contains(logged, "s3cret-token") {
		t.Errorf("the bot token leaked into the log: %s", logged)
	}
}

func TestATransportErrorRedactsTheToken(t *testing.T) {
	// A closed server makes client.Do fail, and net/http's error quotes the
	// URL — which carries the token in its path.
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	tg, err := NewTelegram("s3cret-token", "chat", WithBaseURL(url), WithLogger(quiet()), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.send(context.Background(), message{level: Info, text: "x"}); err == nil || strings.Contains(err.Error(), "s3cret-token") {
		t.Errorf("a transport error must not carry the token: %v", err)
	}
	_ = tg.Close()
}

func TestDiscardAndMultiSatisfyTheInterface(t *testing.T) {
	var n Notifier = Discard{}
	n.Notify(context.Background(), Alert, "nothing happens")

	c := newChat(t)
	tg, err := NewTelegram("tok", "chat", WithBaseURL(c.server.URL), WithLogger(quiet()), WithRate(0))
	if err != nil {
		t.Fatal(err)
	}
	Multi{Discard{}, tg}.Notify(context.Background(), Info, "fan out")
	if err := tg.Close(); err != nil {
		t.Fatal(err)
	}
	if got := c.received(); len(got) != 1 || got[0] != "fan out" {
		t.Errorf("Multi must reach every notifier, chat saw %v", got)
	}
}

func TestNewTelegramNeedsCredentials(t *testing.T) {
	if _, err := NewTelegram("", "chat"); err == nil {
		t.Error("a notifier with no token cannot send; refuse at construction")
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
