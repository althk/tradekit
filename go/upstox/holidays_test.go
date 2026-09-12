package upstox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A calendar as Upstox serves it: a plain holiday, a special session that
// lists NSE as both closed and open (a trading day with odd hours), a
// settlement holiday that closes nothing on NSE, and a BSE-only closure.
const holidaysPayload = `{"status":"success","data":[
  {"date":"2026-01-26","description":"Republic Day","holiday_type":"TRADING_HOLIDAY",
   "closed_exchanges":["NSE","BSE","NFO","MCX"],"open_exchanges":[]},
  {"date":"2026-02-01","description":"Budget Day","holiday_type":"SPECIAL_TIMING",
   "closed_exchanges":["NSE","BSE"],
   "open_exchanges":[{"exchange":"NSE","start_time":1769916600000,"end_time":1769938200000}]},
  {"date":"2026-03-30","description":"Settlement","holiday_type":"SETTLEMENT_HOLIDAY",
   "closed_exchanges":["CDS"],"open_exchanges":[]},
  {"date":"2026-10-20","description":"BSE only","holiday_type":"TRADING_HOLIDAY",
   "closed_exchanges":["BSE"],"open_exchanges":[]},
  {"date":"2026-12-25","description":"Christmas","holiday_type":"TRADING_HOLIDAY",
   "closed_exchanges":["NSE","BSE"],"open_exchanges":[]}
]}`

func TestHolidaysClosedAppliesTheStrictReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the holiday endpoint is public; no token must be sent")
		}
		_, _ = w.Write([]byte(holidaysPayload))
	}))
	defer srv.Close()

	h := &Holidays{URL: srv.URL, HTTP: srv.Client()}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	closed, err := h.Closed(context.Background(), "NSE", from, to)
	if err != nil {
		t.Fatalf("Closed returned %v", err)
	}

	if closed["2026-01-26"] != "Republic Day" {
		t.Errorf("a plain trading holiday must be closed, got %v", closed)
	}
	if _, ok := closed["2026-02-01"]; ok {
		t.Error("a special session lists NSE as open; treating it as a holiday skips a real trading day")
	}
	if _, ok := closed["2026-03-30"]; ok {
		t.Error("a settlement holiday that does not close NSE must not read as closed")
	}
	if _, ok := closed["2026-10-20"]; ok {
		t.Error("a BSE-only closure must not close NSE")
	}
	if _, ok := closed["2026-12-25"]; ok {
		t.Error("days outside [from, to] must be excluded")
	}
}

func TestHolidaysRejectsAnEmptyCalendar(t *testing.T) {
	if _, err := parseHolidays([]byte(`{"status":"success","data":[]}`), "NSE"); err == nil {
		t.Fatal("an empty calendar must be an error, not a year with no holidays")
	}
}
