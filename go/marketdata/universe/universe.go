// Package universe answers which instruments a system trades, and fetches the
// daily bhavcopy that prices them.
//
// It replaces the copy of ind_nifty500list.csv sitting in five project roots,
// and the NSE cookie warm-up that two projects independently discovered.
//
// # Everything is cached to disk
//
// An index list changes monthly and a bhavcopy never changes at all once
// published, so both are cached. A backtest must run offline; re-fetching a
// static file on every run makes that impossible and, against nseindia.com,
// invites the rate limiting the warm-up exists to get around.
package universe

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

// NSE endpoints.
const (
	// nseHome is fetched first, for its cookies. See Client.warmUp.
	nseHome = "https://www.nseindia.com/"
	// indexListURL serves an index's constituents, e.g.
	// ind_nifty500list.csv.
	indexListURL = "https://nsearchives.nseindia.com/content/indices/%s.csv"
	// bhavcopyURL serves the full securities bhavcopy for one day.
	bhavcopyURL = "https://nsearchives.nseindia.com/products/content/sec_bhavdata_full_%s.csv"
)

// browserUserAgent is sent on every NSE request.
//
// NSE serves 403 to a request that does not look like a browser, whatever
// cookies it carries. Both donors send a desktop Chrome string; this is not
// evasion of a rate limit but the minimum needed to be served at all.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Client fetches from NSE, caching what it downloads.
type Client struct {
	// HTTP carries the cookie jar. Zero builds one.
	HTTP *http.Client
	// CacheDir holds downloaded files. Zero disables caching, which is for
	// tests only: a production caller wants the cache.
	CacheDir string
	// BaseURL overrides the archives host. Tests point it at an
	// httptest.Server; production leaves it empty.
	BaseURL string
	// HomeURL overrides the warm-up target, for the same reason.
	HomeURL string

	once    sync.Once
	initErr error
	warmed  bool
	mu      sync.Mutex
}

// New returns a Client caching into dir.
func New(cacheDir string) *Client { return &Client{CacheDir: cacheDir} }

// httpClient lazily builds the client, which must carry a cookie jar.
func (c *Client) httpClient() (*http.Client, error) {
	c.once.Do(func() {
		if c.HTTP != nil {
			return
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			c.initErr = fmt.Errorf("universe: creating cookie jar: %w", err)
			return
		}
		c.HTTP = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	})
	return c.HTTP, c.initErr
}

// warmUp fetches the NSE home page to obtain session cookies.
//
// NSE rejects a bare request to its archives with 403. Both donors discovered
// independently that fetching the home page first, and reusing the cookie jar,
// is what makes the subsequent download work. It is done once per client, and
// again after a 403, because that is what an expired session looks like.
func (c *Client) warmUp(ctx context.Context) error {
	client, err := c.httpClient()
	if err != nil {
		return err
	}
	home := c.HomeURL
	if home == "" {
		home = nseHome
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, home, nil)
	if err != nil {
		return fmt.Errorf("universe: building warm-up request: %w", err)
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("universe: warming up against NSE: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	c.mu.Lock()
	c.warmed = true
	c.mu.Unlock()
	return nil
}

// get fetches a URL, warming up first and retrying once on a 403.
func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	client, err := c.httpClient()
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	warmed := c.warmed
	c.mu.Unlock()
	if !warmed {
		if err := c.warmUp(ctx); err != nil {
			// A failed warm-up is not fatal on its own: the request may
			// still be served, and the 403 retry below covers the case
			// where it is not.
			c.mu.Lock()
			c.warmed = true
			c.mu.Unlock()
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("universe: building request for %s: %w", url, err)
		}
		req.Header.Set("User-Agent", browserUserAgent)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Referer", nseHome)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("universe: fetching %s: %w", url, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("universe: reading %s: %w", url, readErr)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusForbidden && attempt == 0:
			// The session expired. Warm up again and retry once.
			if err := c.warmUp(ctx); err != nil {
				return nil, err
			}
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("universe: %s: %w", url, ErrNotPublished)
		default:
			return nil, fmt.Errorf("universe: %s: HTTP %d", url, resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("universe: %s: refused twice, even after warming up", url)
}

// ErrNotPublished reports that NSE has no file for the requested day. It is
// distinct from a failure because it is the normal answer for a holiday, or for
// today before the file is published, and a sync must not log those as errors.
var ErrNotPublished = fmt.Errorf("universe: NSE has published no file for this date")

// fetchCached returns a file's bytes, reading the cache when it holds them.
func (c *Client) fetchCached(ctx context.Context, url, cacheName string) ([]byte, error) {
	if c.CacheDir == "" {
		return c.get(ctx, url)
	}
	path := filepath.Join(c.CacheDir, cacheName)
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return body, nil
	}
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("universe: creating cache directory: %w", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, fmt.Errorf("universe: writing cache file %s: %w", path, err)
	}
	return body, nil
}

// archives returns the archives host, honouring BaseURL.
func (c *Client) archives(format string, args ...any) string {
	url := fmt.Sprintf(format, args...)
	if c.BaseURL == "" {
		return url
	}
	// Replace the scheme and host, keeping the path, so a test server sees
	// the same paths production does.
	if i := strings.Index(url[len("https://"):], "/"); i >= 0 {
		return strings.TrimSuffix(c.BaseURL, "/") + url[len("https://")+i:]
	}
	return c.BaseURL
}

// IndexConstituents returns the instruments in an NSE index.
//
// index is the file's base name, "ind_nifty500list" for the Nifty 500. The
// result is cached: an index list changes monthly, and re-downloading it on
// every run is both wasteful and the reason five projects ended up with a stale
// copy checked into their repositories.
func (c *Client) IndexConstituents(ctx context.Context, index string) ([]domain.InstrumentKey, error) {
	body, err := c.fetchCached(ctx, c.archives(indexListURL, index), index+".csv")
	if err != nil {
		return nil, err
	}
	instruments, err := ParseIndexCSV(body)
	if err != nil {
		return nil, err
	}
	keys := make([]domain.InstrumentKey, 0, len(instruments))
	for _, in := range instruments {
		keys = append(keys, in.Key)
	}
	return keys, nil
}

// Constituents returns the index's members with the name and sector the list
// carries, for a caller populating the instruments table.
func (c *Client) Constituents(ctx context.Context, index string) ([]domain.Instrument, error) {
	body, err := c.fetchCached(ctx, c.archives(indexListURL, index), index+".csv")
	if err != nil {
		return nil, err
	}
	return ParseIndexCSV(body)
}

// byteOrderMark is what a Windows-authored CSV begins with, and it would
// otherwise make the first header name unmatchable — the column is there, the
// lookup misses, and the parser reports a file with no symbol column.
const byteOrderMark = string(rune(0xFEFF))

// Column aliases accepted in an index or watchlist CSV.
//
// The published NSE list uses "Company Name"/"Industry"/"Symbol"; ad-hoc
// watchlist exports use lowercase names and often suffix symbols with ".NS".
// Reading by name rather than position is what lets one parser handle both,
// which is neev's approach and the reason its universe loader survived several
// changes to the published format.
var (
	symbolColumns = []string{"symbol"}
	nameColumns   = []string{"company name", "name"}
	// The ISIN is what Upstox keys cash equity by ("NSE_EQ|INE002A01018"),
	// so a universe without it cannot be traded there.
	isinColumns   = []string{"isin code", "isin"}
	sectorColumns = []string{"industry", "sector"}
)

// ParseIndexCSV reads an index constituent list.
//
// A row with no symbol is skipped rather than failing the batch: the published
// files carry trailing blank lines, and refusing the whole universe over one is
// how a sync comes to do nothing at all.
func ParseIndexCSV(body []byte) ([]domain.Instrument, error) {
	reader := csv.NewReader(strings.NewReader(string(body)))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("universe: reading index list header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, byteOrderMark)))] = i
	}
	symbolAt := findColumn(col, symbolColumns)
	if symbolAt < 0 {
		return nil, fmt.Errorf("universe: index list has no symbol column; header was %v", header)
	}
	nameAt, sectorAt := findColumn(col, nameColumns), findColumn(col, sectorColumns)
	isinAt := findColumn(col, isinColumns)

	var out []domain.Instrument
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("universe: reading index list: %w", err)
		}
		symbol := cleanSymbol(at(row, symbolAt))
		if symbol == "" {
			continue
		}
		out = append(out, domain.Instrument{
			Key:      domain.InstrumentKey{Exchange: "NSE", Symbol: symbol},
			Name:     at(row, nameAt),
			ISIN:     strings.ToUpper(strings.TrimSpace(at(row, isinAt))),
			Segment:  sectorOrEquity(at(row, sectorAt)),
			LotSize:  1,
			TickSize: money.Money(5),
			Active:   true,
		})
	}
	return out, nil
}

// sectorOrEquity keeps the domain's segment vocabulary.
//
// An index list's "Industry" column is a sector, which the domain has no field
// for; segment means the instrument class. Everything in an equity index is
// equity, so the sector is dropped rather than smuggled into a field that means
// something else.
func sectorOrEquity(string) string { return "equity" }

// Row is one line of the full securities bhavcopy.
type Row struct {
	Key domain.InstrumentKey
	// Series distinguishes EQ and BE from the many non-equity series in the
	// same file.
	Series    string
	Date      time.Time
	Open      money.Money
	High      money.Money
	Low       money.Money
	Close     money.Money
	PrevClose money.Money
	Volume    int64
	// DeliverableQty and DeliverablePct are the delivery figures the file
	// carries and no candle feed does. They are why a screener reads the
	// bhavcopy at all rather than the broker's own history.
	DeliverableQty int64
	DeliverablePct float64
}

// equitySeries are the series a cash-equity strategy trades. The bhavcopy
// carries dozens more — government securities, warrants, partly paid shares —
// and treating them as equity puts instruments in a universe that cannot be
// traded the way the strategy assumes.
var equitySeries = map[string]bool{"EQ": true, "BE": true}

// Bhavcopy returns the day's full securities file.
//
// A day NSE has published nothing for returns ErrNotPublished, which is the
// normal answer for a weekend, a holiday, or today before publication. It is
// cached: a published bhavcopy never changes.
func (c *Client) Bhavcopy(ctx context.Context, day time.Time) ([]Row, error) {
	stamp := day.Format("02012006")
	body, err := c.fetchCached(ctx, c.archives(bhavcopyURL, stamp), "sec_bhavdata_full_"+stamp+".csv")
	if err != nil {
		return nil, err
	}
	return ParseBhavcopy(body, day)
}

// ParseBhavcopy reads the full securities bhavcopy.
//
// A malformed row is skipped rather than failing the batch. The file is a few
// thousand rows and a single unparseable price — the file uses "-" for an
// absent delivery figure, among others — must not cost the whole day's data.
func ParseBhavcopy(body []byte, day time.Time) ([]Row, error) {
	reader := csv.NewReader(strings.NewReader(string(body)))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("universe: reading bhavcopy header: %w", err)
	}
	col := map[string]int{}
	for i, name := range header {
		col[strings.ToUpper(strings.TrimSpace(name))] = i
	}

	var out []Row
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("universe: reading bhavcopy: %w", err)
		}
		field := func(name string) string {
			i, ok := col[name]
			if !ok || i >= len(record) {
				return ""
			}
			return strings.TrimSpace(record[i])
		}

		symbol := field("SYMBOL")
		series := strings.ToUpper(field("SERIES"))
		if symbol == "" || !equitySeries[series] {
			continue
		}
		open, okOpen := parsePrice(field("OPEN_PRICE"))
		high, okHigh := parsePrice(field("HIGH_PRICE"))
		low, okLow := parsePrice(field("LOW_PRICE"))
		closePrice, okClose := parsePrice(field("CLOSE_PRICE"))
		if !okOpen || !okHigh || !okLow || !okClose {
			continue
		}
		prevClose, _ := parsePrice(field("PREV_CLOSE"))
		deliverableQty, _ := parseInt(field("DELIV_QTY"))
		deliverablePct, _ := parseFloat(field("DELIV_PER"))
		volume, _ := parseInt(field("TTL_TRD_QNTY"))

		out = append(out, Row{
			Key:            domain.InstrumentKey{Exchange: "NSE", Symbol: symbol},
			Series:         series,
			Date:           day,
			Open:           open,
			High:           high,
			Low:            low,
			Close:          closePrice,
			PrevClose:      prevClose,
			Volume:         volume,
			DeliverableQty: deliverableQty,
			DeliverablePct: deliverablePct,
		})
	}
	return out, nil
}

// Candles converts bhavcopy rows into daily bars.
func Candles(rows []Row) []domain.Candle {
	out := make([]domain.Candle, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Candle{
			Key:       r.Key,
			Timeframe: domain.D1,
			Start:     r.Date,
			Open:      r.Open,
			High:      r.High,
			Low:       r.Low,
			Close:     r.Close,
			Volume:    r.Volume,
		})
	}
	return out
}

// SyncReport is what one universe sync did.
type SyncReport struct {
	// Fetched counts the index's members.
	Fetched int
	// Deactivated counts the instruments that left the index.
	Deactivated int
}

// Sync brings the instruments table into line with an index.
//
// Instruments that left the index are deactivated, never deleted. Deleting one
// orphans every candle and trade that references it — and those rows are the
// history a backtest of the strategy that held it is computed from.
func (c *Client) Sync(ctx context.Context, db *store.DB, index string) (SyncReport, error) {
	instruments, err := c.Constituents(ctx, index)
	if err != nil {
		return SyncReport{}, err
	}
	if len(instruments) == 0 {
		// Refusing an empty list is deliberate: an NSE outage that served
		// an empty file would otherwise deactivate the entire universe,
		// and the next run would find nothing to trade.
		return SyncReport{}, fmt.Errorf("universe: index %q returned no constituents; refusing to deactivate the whole universe", index)
	}
	if err := db.UpsertInstruments(ctx, instruments); err != nil {
		return SyncReport{}, err
	}

	keep := make([]domain.InstrumentKey, 0, len(instruments))
	for _, in := range instruments {
		keep = append(keep, in.Key)
	}
	deactivated, err := db.DeactivateMissing(ctx, "NSE", keep)
	if err != nil {
		return SyncReport{}, err
	}
	return SyncReport{Fetched: len(instruments), Deactivated: deactivated}, nil
}

// findColumn returns the index of the first matching header alias, or -1.
func findColumn(col map[string]int, candidates []string) int {
	for _, c := range candidates {
		if i, ok := col[c]; ok {
			return i
		}
	}
	return -1
}

// at reads a field by index, tolerating a short row.
func at(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// cleanSymbol normalises a symbol, stripping the ".NS" suffix a watchlist
// export carries.
func cleanSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	return strings.TrimSuffix(s, ".NS")
}

// parsePrice reads a rupee price into paise, reporting whether it was readable.
// The bhavcopy writes "-" for an absent value.
func parsePrice(s string) (money.Money, bool) {
	v, ok := parseFloat(s)
	if !ok {
		return 0, false
	}
	return money.FromMajor(v), true
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseInt(s string) (int64, bool) {
	v, ok := parseFloat(s)
	if !ok {
		return 0, false
	}
	return int64(v), true
}
