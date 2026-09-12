package backtest

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

var update = flag.Bool("update", false, "rewrite testdata/report.golden.html from the current renderer")

func renderSample(t *testing.T) []byte {
	t.Helper()
	report, err := Build(sampleSpec(), sampleTrades(), sampleCurve())
	if err != nil {
		t.Fatalf("building report: %v", err)
	}
	var buf bytes.Buffer
	if err := report.WriteHTML(&buf); err != nil {
		t.Fatalf("rendering report: %v", err)
	}
	return buf.Bytes()
}

func TestReportMatchesTheGoldenFile(t *testing.T) {
	got := renderSample(t)
	path := filepath.Join("testdata", "report.golden.html")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (regenerate with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the rendered report drifted from testdata/report.golden.html; if the change is intended, regenerate with `go test -run Golden -update`")
	}
}

// externalRef matches any src or href pointing off the file.
var externalRef = regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*https?://`)

func TestReportIsSelfContained(t *testing.T) {
	got := string(renderSample(t))
	if m := externalRef.FindString(got); m != "" {
		t.Errorf("the report references %q; a report is read months later from a directory with no network, and a chart that renders as an empty box is worse than a table", m)
	}
	if strings.Contains(got, "<script") {
		t.Error("the report must not carry scripts; the charts are inline SVG computed in Go")
	}
	if strings.Count(got, "<svg") != 2 {
		t.Errorf("expected an equity and a drawdown chart, found %d SVG elements", strings.Count(got, "<svg"))
	}
}

func TestReportLabelsTheModeAndRendersMoneyThroughString(t *testing.T) {
	got := string(renderSample(t))
	if !strings.Contains(got, "SIMULATED") {
		t.Error("every report must say it is simulated; one that could be mistaken for live results is a liability")
	}
	// Net P&L of the sample: (40-2) + (-30-2) + (60-2) + (-15-2) = 47.00.
	if !strings.Contains(got, ">47.00<") {
		t.Error("net P&L must render through money.String as 47.00, not as 4700 paise or a float")
	}
	if strings.Contains(got, "4700<") {
		t.Error("a Money rendered as raw minor units leaked into the report")
	}
}

func TestBuildRefusesMixedModes(t *testing.T) {
	trades := sampleTrades()
	trades[0].Paper = false
	if _, err := Build(sampleSpec(), trades, sampleCurve()); err == nil {
		t.Error("a report over paper and live trades together is a number that looks plausible and means nothing")
	}
}

func TestBuildGroupsByExitReasonWithTheSameArithmetic(t *testing.T) {
	report, err := Build(sampleSpec(), sampleTrades(), sampleCurve())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ByReason) != 3 {
		t.Fatalf("expected target, stop and trail groups, got %v", report.ByReason)
	}
	var total money.Money
	for _, m := range report.ByReason {
		total += m.NetPnL
	}
	if total != report.Metrics.NetPnL {
		t.Errorf("the per-reason breakdown must sum to the headline net P&L: %s vs %s", total, report.Metrics.NetPnL)
	}
	if report.ByReason[domain.ExitTarget].Trades != 2 {
		t.Errorf("two target exits expected, got %d", report.ByReason[domain.ExitTarget].Trades)
	}
}

func TestReportRendersWithNoTrades(t *testing.T) {
	report, err := Build(sampleSpec(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := report.WriteHTML(&buf); err != nil {
		t.Fatalf("an empty run must still render: %v", err)
	}
	if !strings.Contains(buf.String(), "No closed trades") {
		t.Error("an empty run must say so rather than render an empty table")
	}
}
