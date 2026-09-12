package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/store"
)

// Journal records why a decision was made, keyed to the run.
//
// A log line is prose; a journal entry is a row you can query alongside the
// trade it produced. This is the thing no project had and every project's
// author has wanted at 9:20 on a Monday.
type Journal struct {
	DB *store.DB
}

// Decision is one considered action.
//
// Rejections matter as much as actions: "why did nothing happen today" is the
// harder question. risk.Gate already returns a reason with ErrBlocked; wiring
// that reason straight into Reason is the payoff for having made the gates
// return reasons rather than a bare boolean, because "the kill switch stopped
// it" becomes queryable rather than buried in a log.
type Decision struct {
	RunID  int64
	At     time.Time
	Key    domain.InstrumentKey
	Action string // "enter", "skip", "exit", "resize"
	Reason string
	// Detail is free-form context, stored as JSON.
	Detail map[string]any
}

// Record writes one decision.
//
// Recording must never fail the trade. The caller logs and swallows this
// error; an observability system that can stop trading is a liability, not an
// asset.
func (j *Journal) Record(ctx context.Context, d Decision) error {
	if j == nil || j.DB == nil {
		return fmt.Errorf("harness: Journal has no store")
	}
	detail, err := EncodeDetail(d.Detail)
	if err != nil {
		return fmt.Errorf("harness: encoding decision detail: %w", err)
	}
	_, err = j.DB.SQL().ExecContext(ctx, `
		INSERT INTO decisions (run_id, at, exchange, symbol, action, reason, detail_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.RunID, d.At.Format(time.RFC3339Nano), d.Key.Exchange, d.Key.Symbol, d.Action, d.Reason, detail)
	if err != nil {
		return fmt.Errorf("harness: recording decision: %w", err)
	}
	return nil
}

// Since returns a run's decisions made at or after from, oldest first.
func (j *Journal) Since(ctx context.Context, runID int64, from time.Time) ([]Decision, error) {
	if j == nil || j.DB == nil {
		return nil, fmt.Errorf("harness: Journal has no store")
	}
	rows, err := j.DB.SQL().QueryContext(ctx, `
		SELECT at, exchange, symbol, action, reason, detail_json
		FROM decisions WHERE run_id = ? AND at >= ? ORDER BY at, id`,
		runID, from.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("harness: reading decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Decision
	for rows.Next() {
		var at, exchange, symbol, action, reason, detail string
		if err := rows.Scan(&at, &exchange, &symbol, &action, &reason, &detail); err != nil {
			return nil, fmt.Errorf("harness: scanning decision: %w", err)
		}
		d := Decision{RunID: runID, Key: domain.InstrumentKey{Exchange: exchange, Symbol: symbol}, Action: action, Reason: reason}
		if d.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("harness: decision has an unreadable timestamp %q: %w", at, err)
		}
		if detail != "" && detail != "{}" {
			if err := json.Unmarshal([]byte(detail), &d.Detail); err != nil {
				return nil, fmt.Errorf("harness: decision has unreadable detail %q: %w", detail, err)
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EncodeDetail renders a decision's detail as the JSON the row stores: compact,
// keys sorted, HTML left unescaped.
//
// The form matters more than it looks. Python writes the same row with
// json.dumps(sort_keys=True, separators=(",", ":")), and without both sides
// agreeing the same decision produces different bytes in each language, so any
// future comparison or hash of journal rows diverges for no reason. Go sorts
// map keys already; the HTML escaping it does by default is what Python does
// not, hence the encoder.
func EncodeDetail(detail map[string]any) (string, error) {
	if len(detail) == 0 {
		return "{}", nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(detail); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
