package upstox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultHolidaysURL is Upstox's market-holiday calendar. The endpoint needs
// no authentication, which is why the calendar is available before the daily
// login and why a Zerodha-trading project can use it too.
const DefaultHolidaysURL = "https://api.upstox.com/v2/market/holidays"

// Holidays is a ports.HolidaySource backed by Upstox's public calendar.
//
// It replaces three copies of the same fetch (chartinkbot, breakout500,
// fanse), which between them had two readings of what "closed" means. This
// one is the strict reading: an exchange is closed on a day when it appears
// in closed_exchanges and not in open_exchanges. A SPECIAL_TIMING entry such
// as the muhurat session lists NSE under both, and it is a trading day with
// unusual hours, not a holiday.
//
// It does not cache. Wrap it in marketdata/reference.Holidays for the disk
// cache and the fail-open behaviour a live process wants.
type Holidays struct {
	// URL overrides DefaultHolidaysURL; tests point it at an httptest.Server.
	URL string
	// HTTP overrides the client. Nil uses one with a 30-second timeout.
	HTTP *http.Client
}

// Closed returns the days in [from, to] on which exchange does not trade,
// keyed "2006-01-02" and valued by Upstox's description.
func (h *Holidays) Closed(ctx context.Context, exchange string, from, to time.Time) (map[string]string, error) {
	url := h.URL
	if url == "" {
		url = DefaultHolidaysURL
	}
	client := h.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("upstox: building holiday request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstox: fetching holidays: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstox: fetching holidays: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("upstox: reading holidays: %w", err)
	}

	all, err := parseHolidays(raw, exchange)
	if err != nil {
		return nil, err
	}
	lo, hi := from.Format(time.DateOnly), to.Format(time.DateOnly)
	out := map[string]string{}
	for day, desc := range all {
		if day >= lo && day <= hi {
			out[day] = desc
		}
	}
	return out, nil
}

// holidayEntry is one row of the calendar response.
type holidayEntry struct {
	Date            string `json:"date"`
	Description     string `json:"description"`
	HolidayType     string `json:"holiday_type"`
	ClosedExchanges []string
	OpenExchanges   []string
}

// UnmarshalJSON tolerates both shapes Upstox has used for the exchange lists:
// plain strings, and objects carrying an "exchange" field alongside the
// special session's timings.
func (e *holidayEntry) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Date            string            `json:"date"`
		Description     string            `json:"description"`
		HolidayType     string            `json:"holiday_type"`
		ClosedExchanges []json.RawMessage `json:"closed_exchanges"`
		OpenExchanges   []json.RawMessage `json:"open_exchanges"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	e.Date, e.Description, e.HolidayType = wire.Date, wire.Description, wire.HolidayType
	e.ClosedExchanges = exchangeNames(wire.ClosedExchanges)
	e.OpenExchanges = exchangeNames(wire.OpenExchanges)
	return nil
}

func exchangeNames(items []json.RawMessage) []string {
	var out []string
	for _, item := range items {
		var name string
		if err := json.Unmarshal(item, &name); err == nil {
			out = append(out, name)
			continue
		}
		var obj struct {
			Exchange string `json:"exchange"`
		}
		if err := json.Unmarshal(item, &obj); err == nil && obj.Exchange != "" {
			out = append(out, obj.Exchange)
		}
	}
	return out
}

// parseHolidays reads the calendar response into closed days for one
// exchange. An empty calendar is an error rather than "no holidays": the
// endpoint has never legitimately returned one, and treating it as such would
// silently make every holiday a trading day.
func parseHolidays(raw []byte, exchange string) (map[string]string, error) {
	var resp struct {
		Status string         `json:"status"`
		Data   []holidayEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("upstox: parsing holidays: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("upstox: holiday calendar is empty")
	}

	out := map[string]string{}
	for _, e := range resp.Data {
		if len(e.Date) < len(time.DateOnly) {
			continue
		}
		if contains(e.ClosedExchanges, exchange) && !contains(e.OpenExchanges, exchange) {
			out[e.Date[:len(time.DateOnly)]] = e.Description
		}
	}
	return out, nil
}

func contains(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
