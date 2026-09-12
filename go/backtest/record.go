package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
	"github.com/althk/tradekit/go/store"
)

// Recorder persists one backtest run and everything it produced.
//
// A backtest whose output is only stdout cannot be compared against last
// week's, which is how parameter changes get adopted on the strength of a
// number nobody can reproduce. The runs, trades and equity_snapshots tables
// exist for this; nothing else writes them for a backtest.
type Recorder struct {
	DB *store.DB
}

// RunSpec is a run's inputs.
//
// Params is stored as JSON text rather than as a column per strategy parameter:
// the sweep varies different parameters per strategy, and a schema that has to
// change for each one will not be changed, it will be worked around.
type RunSpec struct {
	Name     string
	Strategy string
	From, To time.Time
	Params   any
	Opening  money.Money
}

// runParams is the JSON shape written to runs.params, so a run's window and
// opening balance are readable without a schema change.
type runParams struct {
	Strategy string          `json:"strategy"`
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
	Opening  money.Money     `json:"opening"`
	Params   json.RawMessage `json:"params"`
}

// Save writes the run, its trades and its curve in one transaction.
//
// One transaction because a partially written run is worse than no run: it
// will be read back and compared as if it were complete.
//
// Every trade must carry Paper: true. The store scopes queries by mode and
// stats.Summarize refuses a mixed set; a backtest that wrote its trades as live
// would poison the live statistics of whatever project shares the database.
// A live trade is rejected with an error naming it, before anything is written,
// because the caller passing live trades into a backtest recorder has a bug
// worth surfacing.
func (r *Recorder) Save(ctx context.Context, spec RunSpec, trades []domain.Trade, curve []stats.Point) (int64, error) {
	if r.DB == nil {
		return 0, fmt.Errorf("backtest: Recorder.DB is required")
	}
	for i, t := range trades {
		if !t.Paper {
			return 0, fmt.Errorf("backtest: trade %d (%s %s %d @ %s, exited %v) is not marked paper; a backtest recorder refuses live trades",
				i, t.Key, t.Side, t.Quantity, t.EntryPrice, t.ExitAt)
		}
	}

	rawParams, err := json.Marshal(spec.Params)
	if err != nil {
		return 0, fmt.Errorf("backtest: encoding run params: %w", err)
	}
	if spec.Params == nil {
		rawParams = []byte("{}")
	}
	encoded, err := json.Marshal(runParams{
		Strategy: spec.Strategy, From: spec.From, To: spec.To, Opening: spec.Opening, Params: rawParams,
	})
	if err != nil {
		return 0, fmt.Errorf("backtest: encoding run params: %w", err)
	}

	tx, err := r.DB.SQL().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("backtest: beginning run write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO runs (kind, name, params, started_at, ended_at, status) VALUES ('backtest', ?, ?, ?, ?, 'ok')`,
		spec.Name, string(encoded), now, now)
	if err != nil {
		return 0, fmt.Errorf("backtest: inserting run: %w", err)
	}
	runID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("backtest: reading run id: %w", err)
	}

	// The same column list and encodings store.InsertTrade uses, inside this
	// transaction. The key is unique per run and index, so a run is never
	// half-duplicated by a retry.
	tradeStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO trades
			(key, run_id, exchange, symbol, strategy, side, quantity, entry_price, exit_price,
			 entry_at, exit_at, gross_pnl, charges, net_pnl, exit_reason, paper)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`)
	if err != nil {
		return 0, fmt.Errorf("backtest: preparing trade insert: %w", err)
	}
	defer func() { _ = tradeStmt.Close() }()
	for i, t := range trades {
		key := fmt.Sprintf("backtest:%d:%d", runID, i)
		if _, err := tradeStmt.ExecContext(ctx,
			key, runID, t.Key.Exchange, t.Key.Symbol, t.Strategy, string(t.Side), t.Quantity,
			int64(t.EntryPrice), int64(t.ExitPrice), formatTime(t.EntryAt), formatTime(t.ExitAt),
			int64(t.GrossPnL), int64(t.Charges), int64(t.NetPnL), string(t.ExitReason)); err != nil {
			return 0, fmt.Errorf("backtest: inserting trade %s: %w", key, err)
		}
	}

	pointStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO equity_snapshots (run_id, at, balance, realized_pnl, unrealized_pnl, open_positions, paper)
		VALUES (?, ?, ?, 0, 0, 0, 1)`)
	if err != nil {
		return 0, fmt.Errorf("backtest: preparing snapshot insert: %w", err)
	}
	defer func() { _ = pointStmt.Close() }()
	for _, p := range curve {
		if _, err := pointStmt.ExecContext(ctx, runID, formatTime(p.At), int64(p.Equity)); err != nil {
			return 0, fmt.Errorf("backtest: inserting equity snapshot at %v: %w", p.At, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("backtest: committing run: %w", err)
	}
	return runID, nil
}

// formatTime matches the store's timestamp encoding, so rows written here read
// back through store.Trades and store.EquityCurve.
func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }
