package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

var reliance = domain.InstrumentKey{Exchange: "NSE", Symbol: "RELIANCE"}

// openTest returns a migrated in-memory database, or skips when no driver is
// registered (see driver_test.go and the sqlitedriver tag).
func openTest(t *testing.T) *store.DB {
	t.Helper()
	var driverName string
	for _, name := range sql.Drivers() {
		if name == "sqlite" || name == "sqlite3" {
			driverName = name
			break
		}
	}
	if driverName == "" {
		t.Skip("no SQLite driver registered; run with -tags sqlitedriver")
	}
	db, err := store.Open(driverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRecordThenSinceRoundTripsIncludingDetail(t *testing.T) {
	db := openTest(t)
	j := &Journal{DB: db}
	at := time.Date(2025, 4, 17, 9, 20, 0, 0, time.FixedZone("IST", 5*3600+1800))

	decisions := []Decision{
		{RunID: 1, At: at, Key: reliance, Action: "skip", Reason: "kill switch engaged", Detail: map[string]any{"positions": float64(3), "gate": "kill"}},
		{RunID: 1, At: at.Add(time.Minute), Key: reliance, Action: "enter", Reason: "breakout", Detail: map[string]any{"stop": "1200.00"}},
		{RunID: 2, At: at, Key: reliance, Action: "exit", Reason: "eod"},
		{RunID: 1, At: at.Add(-time.Hour), Key: reliance, Action: "skip", Reason: "before window"},
	}
	for _, d := range decisions {
		if err := j.Record(t.Context(), d); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	got, err := j.Since(t.Context(), 1, at)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("run 1 has two decisions at or after %v, got %d: %+v", at, len(got), got)
	}
	if !got[0].At.Equal(at) || got[0].Action != "skip" || got[0].Reason != "kill switch engaged" {
		t.Errorf("first decision read back wrong: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].Detail, decisions[0].Detail) {
		t.Errorf("detail must round-trip: got %v want %v", got[0].Detail, decisions[0].Detail)
	}
	if got[1].Action != "enter" || got[1].Detail["stop"] != "1200.00" {
		t.Errorf("second decision read back wrong: %+v", got[1])
	}
	if got[0].Key != reliance {
		t.Errorf("the instrument must round-trip, got %v", got[0].Key)
	}
}

func TestADecisionWithNoDetailStoresAnEmptyObject(t *testing.T) {
	db := openTest(t)
	j := &Journal{DB: db}
	if err := j.Record(t.Context(), Decision{RunID: 1, At: time.Now(), Key: reliance, Action: "skip"}); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT detail_json FROM decisions`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "{}" {
		t.Errorf("no detail is stored as {} so the column is always valid JSON, got %q", detail)
	}
}

func TestAJournalWithNoStoreReturnsAnError(t *testing.T) {
	var j *Journal
	if err := j.Record(context.Background(), Decision{}); err == nil {
		t.Error("a nil journal must report the misconfiguration; the caller logs and continues")
	}
}

type journalFixture struct {
	Journal struct {
		Decision struct {
			RunID    int64          `json:"run_id"`
			At       time.Time      `json:"at"`
			Exchange string         `json:"exchange"`
			Symbol   string         `json:"symbol"`
			Action   string         `json:"action"`
			Reason   string         `json:"reason"`
			Detail   map[string]any `json:"detail"`
		} `json:"decision"`
		WantDetailJSON string `json:"want_detail_json"`
	} `json:"journal"`
}

func TestParityJournalDetailEncoding(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "testdata", "parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f journalFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	got, err := EncodeDetail(f.Journal.Decision.Detail)
	if err != nil {
		t.Fatal(err)
	}
	if got != f.Journal.WantDetailJSON {
		t.Errorf("detail_json must be byte-identical to what Python writes for the same decision:\n got %s\nwant %s", got, f.Journal.WantDetailJSON)
	}

	db := openTest(t)
	j := &Journal{DB: db}
	d := f.Journal.Decision
	err = j.Record(t.Context(), Decision{
		RunID: d.RunID, At: d.At, Key: domain.InstrumentKey{Exchange: d.Exchange, Symbol: d.Symbol},
		Action: d.Action, Reason: d.Reason, Detail: d.Detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored, at string
	if err := db.SQL().QueryRowContext(t.Context(), `SELECT detail_json, at FROM decisions`).Scan(&stored, &at); err != nil {
		t.Fatal(err)
	}
	if stored != f.Journal.WantDetailJSON {
		t.Errorf("the stored column must carry the canonical encoding, got %s", stored)
	}
	if at != "2025-04-17T09:20:00+05:30" {
		t.Errorf("the timestamp column must be ISO-8601 with its offset, as the Python store writes it; got %q", at)
	}
}

func TestSlogRendersMoneyAsRupeesNotPaise(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	log.Info("fill", Money("price", money.MustParse("1234.56")), Key(reliance), Side(domain.Buy), Qty(10), Reason("breakout"))

	out := buf.String()
	if !strings.Contains(out, "price=1234.56") {
		t.Errorf("an int64 of paise in a log line is unreadable and silently mistaken for rupees; got %s", out)
	}
	if strings.Contains(out, "123456") {
		t.Errorf("the raw minor units leaked: %s", out)
	}
	if !strings.Contains(out, "key=NSE:RELIANCE") || !strings.Contains(out, "side=buy") || !strings.Contains(out, "qty=10") || !strings.Contains(out, "reason=breakout") {
		t.Errorf("attributes must render as themselves, got %s", out)
	}
}
