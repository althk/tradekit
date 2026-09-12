package backtest

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
)

// Report is everything the template renders.
//
// It is built from core/stats so the numbers in the report are the same ones
// the parity fixture pins. The structure — headline metrics, equity and
// drawdown curves, a per-reason breakdown, the trade log — follows neev's
// report, which was the one donor that rendered more than a summary line.
type Report struct {
	Title    string
	From, To time.Time
	Opening  money.Money
	Metrics  stats.Metrics
	// Curve is the mark-to-market curve, one point per session, from the
	// Snapshotter. Its drawdown sees the days a position was underwater.
	Curve []stats.Point
	// ClosedCurve steps only when a trade closes, starting from Opening. It
	// is the curve trade statistics are computed over, and its drawdown
	// understates the real one; the report shows both and says which is which.
	ClosedCurve []stats.Point
	Trades      []domain.Trade
	ByReason    map[domain.ExitReason]stats.Metrics
}

// Build assembles a report from a run's inputs and outputs.
//
// The trades must all be paper: stats.Summarize refuses a mixed set, and a
// report is the last place a live fill should be able to sneak into a
// simulated result.
func Build(spec RunSpec, trades []domain.Trade, curve []stats.Point) (Report, error) {
	metrics, err := stats.Summarize(trades)
	if err != nil {
		return Report{}, fmt.Errorf("backtest: summarising trades: %w", err)
	}

	// Per-reason metrics reuse Summarize on each group rather than
	// recomputing anything: the breakdown must agree with the headline
	// figures, and two implementations of a win rate never quite do.
	groups := map[domain.ExitReason][]domain.Trade{}
	for _, t := range trades {
		groups[t.ExitReason] = append(groups[t.ExitReason], t)
	}
	byReason := make(map[domain.ExitReason]stats.Metrics, len(groups))
	for reason, group := range groups {
		m, err := stats.Summarize(group)
		if err != nil {
			return Report{}, fmt.Errorf("backtest: summarising %s exits: %w", reason, err)
		}
		byReason[reason] = m
	}

	ordered := make([]domain.Trade, len(trades))
	copy(ordered, trades)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ExitAt.Before(ordered[j].ExitAt) })

	title := spec.Name
	if title == "" {
		title = spec.Strategy
	}
	return Report{
		Title:       title,
		From:        spec.From,
		To:          spec.To,
		Opening:     spec.Opening,
		Metrics:     metrics,
		Curve:       curve,
		ClosedCurve: stats.EquityCurve(ordered, spec.Opening),
		Trades:      ordered,
		ByReason:    byReason,
	}, nil
}

//go:embed report.gohtml
var reportTemplate string

// WriteHTML renders the report as one self-contained file.
//
// The template is embedded, the CSS inlined, and the charts drawn as inline SVG
// computed here. No CDN and no script library, because a report is read months
// later, often from a directory copied somewhere with no network, and a chart
// that renders as an empty box is worse than a table.
//
// Every report says "simulated" in its header. These files get shared and
// screenshotted; one that could be mistaken for live results is a liability.
func (r Report) WriteHTML(w io.Writer) error {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"pct":      formatPct,
		"ratio":    formatRatio,
		"date":     func(t time.Time) string { return t.Format(time.DateOnly) },
		"stamp":    func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"holding":  formatHolding,
		"sign":     signClass,
		"eqSVG":    equitySVG,
		"ddSVG":    drawdownSVG,
		"reasons":  orderedReasons,
		"drawdown": func(c []stats.Point) money.Money { dd, _ := stats.MaxDrawdown(c); return dd },
		"ddpct":    func(c []stats.Point) float64 { _, pct := stats.MaxDrawdown(c); return pct },
	}).Parse(reportTemplate)
	if err != nil {
		return fmt.Errorf("backtest: parsing report template: %w", err)
	}
	if err := tmpl.Execute(w, r); err != nil {
		return fmt.Errorf("backtest: rendering report: %w", err)
	}
	return nil
}

// reasonRow pairs a reason with its metrics, in a stable order for the
// template.
type reasonRow struct {
	Reason  domain.ExitReason
	Metrics stats.Metrics
}

func orderedReasons(by map[domain.ExitReason]stats.Metrics) []reasonRow {
	rows := make([]reasonRow, 0, len(by))
	for reason, m := range by {
		rows = append(rows, reasonRow{Reason: reason, Metrics: m})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Reason < rows[j].Reason })
	return rows
}

func formatPct(f float64) string { return fmt.Sprintf("%.2f%%", f*100) }

// formatRatio renders a profit factor, whose honest value with no losing trades
// is +Inf.
func formatRatio(f float64) string {
	if math.IsInf(f, 1) {
		return "∞"
	}
	if math.IsNaN(f) {
		return "–"
	}
	return fmt.Sprintf("%.2f", f)
}

func formatHolding(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
}

func signClass(m money.Money) string {
	switch {
	case m > 0:
		return "pos"
	case m < 0:
		return "neg"
	}
	return ""
}

// Chart geometry, shared by both curves so they align on the page.
const (
	chartWidth  = 800
	chartHeight = 260
	chartPad    = 24
)

// equitySVG draws the mark-to-market curve as a filled line.
func equitySVG(curve []stats.Point) template.HTML {
	if len(curve) == 0 {
		return template.HTML(`<p class="empty">No equity points recorded.</p>`)
	}
	lo, hi := curve[0].Equity, curve[0].Equity
	for _, p := range curve {
		lo, hi = min(lo, p.Equity), max(hi, p.Equity)
	}
	if lo == hi {
		lo, hi = lo-1, hi+1
	}
	span := float64(hi - lo)
	points := make([]string, 0, len(curve))
	for i, p := range curve {
		x := xAt(i, len(curve))
		y := chartHeight - chartPad - float64(p.Equity-lo)/span*(chartHeight-2*chartPad)
		points = append(points, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	line := "M" + strings.Join(points, " L")
	area := fmt.Sprintf("%s L%.1f,%d L%d,%d Z", line, xAt(len(curve)-1, len(curve)), chartHeight-chartPad, chartPad, chartHeight-chartPad)
	return template.HTML(fmt.Sprintf(
		`<svg viewBox="0 0 %d %d" role="img" aria-label="equity curve">`+
			`<rect width="100%%" height="100%%" fill="#f8fafc" rx="6"/>`+
			`<path d="%s" fill="#10b98122"/>`+
			`<path d="%s" fill="none" stroke="#10b981" stroke-width="2"/>`+
			`<text x="%d" y="16" class="lbl">%s</text><text x="%d" y="%d" class="lbl">%s</text></svg>`,
		chartWidth, chartHeight, area, line,
		chartPad, hi.String(), chartPad, chartHeight-6, lo.String()))
}

// drawdownSVG draws the fall from each running peak, as a fraction of it.
func drawdownSVG(curve []stats.Point) template.HTML {
	if len(curve) == 0 {
		return template.HTML(`<p class="empty">No equity points recorded.</p>`)
	}
	falls := make([]float64, len(curve))
	peak := curve[0].Equity
	worst := 0.0
	for i, p := range curve {
		peak = max(peak, p.Equity)
		if peak > 0 {
			falls[i] = float64(peak-p.Equity) / float64(peak)
		}
		worst = max(worst, falls[i])
	}
	if worst == 0 {
		worst = 0.01
	}
	points := make([]string, 0, len(curve))
	for i, f := range falls {
		y := chartPad + f/worst*(chartHeight-2*chartPad)
		points = append(points, fmt.Sprintf("%.1f,%.1f", xAt(i, len(curve)), y))
	}
	path := fmt.Sprintf("M%d,%d L%s L%.1f,%d Z", chartPad, chartPad, strings.Join(points, " L"), xAt(len(curve)-1, len(curve)), chartPad)
	return template.HTML(fmt.Sprintf(
		`<svg viewBox="0 0 %d %d" role="img" aria-label="drawdown">`+
			`<rect width="100%%" height="100%%" fill="#f8fafc" rx="6"/>`+
			`<path d="%s" fill="#ef444433" stroke="#ef4444" stroke-width="1.5"/>`+
			`<text x="%d" y="%d" class="lbl">-%s</text></svg>`,
		chartWidth, chartHeight, path, chartPad, chartHeight-6, formatPct(worst)))
}

// xAt spreads n points across the chart's width; a single point sits at the
// left edge rather than dividing by zero.
func xAt(i, n int) float64 {
	if n <= 1 {
		return chartPad
	}
	return chartPad + float64(i)*float64(chartWidth-2*chartPad)/float64(n-1)
}
