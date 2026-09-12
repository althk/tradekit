package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/core/stats"
)

// InsertSignal records a signal and returns its row id.
//
// runID may be zero for live trading, where there is no enclosing run.
func (d *DB) InsertSignal(ctx context.Context, sig domain.Signal, runID int64, paper bool) (int64, error) {
	metadata := "{}"
	if len(sig.Metadata) > 0 {
		raw, err := json.Marshal(sig.Metadata)
		if err != nil {
			return 0, fmt.Errorf("store: encoding signal metadata: %w", err)
		}
		metadata = string(raw)
	}

	var run any
	if runID != 0 {
		run = runID
	}

	res, err := d.db.ExecContext(ctx, `
		INSERT INTO signals (run_id, exchange, symbol, kind, at, price, stop, target, strategy, metadata, paper)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run, sig.Key.Exchange, sig.Key.Symbol, string(sig.Kind), formatTime(sig.At),
		int64(sig.Price), int64(sig.Stop), int64(sig.Target), sig.Strategy, metadata, boolToInt(paper))
	if err != nil {
		return 0, fmt.Errorf("store: inserting signal for %s: %w", sig.Key, err)
	}
	return res.LastInsertId()
}

// UpsertOrder writes an order, updating the mutable fields if it already
// exists. Placing and then reconciling the same order is the normal path, so
// this is called repeatedly for one id.
func (d *DB) UpsertOrder(ctx context.Context, o domain.Order, paper bool) error {
	r := o.Request
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO orders
			(id, exchange, symbol, side, quantity, type, product, limit_price, trigger_price,
			 time_in_force, tag, status, filled_quantity, average_price, placed_at, updated_at, message,
			 protective_id, paper)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status          = excluded.status,
			filled_quantity = excluded.filled_quantity,
			average_price   = excluded.average_price,
			updated_at      = excluded.updated_at,
			message         = excluded.message`,
		o.ID, r.Key.Exchange, r.Key.Symbol, string(r.Side), r.Quantity, string(r.Type), string(r.Product),
		int64(r.LimitPrice), int64(r.TriggerPrice), string(r.TimeInForce), r.Tag,
		string(o.Status), o.FilledQuantity, int64(o.AveragePrice),
		formatTime(o.PlacedAt), formatTime(o.UpdatedAt), o.Message, o.ProtectiveID, boolToInt(paper))
	if err != nil {
		return fmt.Errorf("store: upserting order %s: %w", o.ID, err)
	}
	return nil
}

// SetOrderProtective records the resting stop placed for a filled order, so a
// reconciler can retry any order that has none rather than losing it.
func (d *DB) SetOrderProtective(ctx context.Context, orderID, protectiveID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE orders SET protective_id = ? WHERE id = ?`, protectiveID, orderID)
	if err != nil {
		return fmt.Errorf("store: setting protective id for order %s: %w", orderID, err)
	}
	return nil
}

const orderColumns = `id, exchange, symbol, side, quantity, type, product, limit_price, trigger_price,
	time_in_force, tag, status, filled_quantity, average_price, placed_at, updated_at, message, protective_id`

func scanOrder(sc interface{ Scan(...any) error }) (domain.Order, error) {
	var (
		o                             domain.Order
		side, otype, product, tif     string
		status                        string
		limitPrice, triggerPrice, avg int64
		placedAt, updatedAt           string
	)
	err := sc.Scan(&o.ID, &o.Request.Key.Exchange, &o.Request.Key.Symbol, &side, &o.Request.Quantity,
		&otype, &product, &limitPrice, &triggerPrice, &tif, &o.Request.Tag,
		&status, &o.FilledQuantity, &avg, &placedAt, &updatedAt, &o.Message, &o.ProtectiveID)
	if err != nil {
		return domain.Order{}, err
	}

	o.Request.Side = domain.Side(side)
	o.Request.Type = domain.OrderType(otype)
	o.Request.Product = domain.Product(product)
	o.Request.TimeInForce = domain.TimeInForce(tif)
	o.Request.LimitPrice = money.Money(limitPrice)
	o.Request.TriggerPrice = money.Money(triggerPrice)
	o.Status = domain.OrderStatus(status)
	o.AveragePrice = money.Money(avg)
	if o.PlacedAt, err = parseTime(placedAt); err != nil {
		return domain.Order{}, err
	}
	if o.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.Order{}, err
	}
	return o, nil
}

// Order returns one order by id, or ErrNotFound.
func (d *DB) Order(ctx context.Context, id string) (domain.Order, error) {
	row := d.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = ?`, id)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Order{}, fmt.Errorf("%w: order %s", ErrNotFound, id)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("store: reading order %s: %w", id, err)
	}
	return o, nil
}

// PendingOrders returns orders that have not reached a terminal state and so
// still need reconciling against the broker.
func (d *DB) PendingOrders(ctx context.Context, paper bool) ([]domain.Order, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE paper = ? AND status NOT IN (?, ?, ?) ORDER BY placed_at`,
		boolToInt(paper), string(domain.StatusComplete), string(domain.StatusCancelled), string(domain.StatusRejected))
	if err != nil {
		return nil, fmt.Errorf("store: listing pending orders: %w", err)
	}
	return collectOrders(rows)
}

// OrdersAwaitingProtective returns filled entries with no resting stop
// recorded. Revisiting these every cycle is what stops a stop that failed to
// place from being silently lost.
func (d *DB) OrdersAwaitingProtective(ctx context.Context, paper bool) ([]domain.Order, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE paper = ? AND status = ? AND filled_quantity > 0 AND protective_id = ''
		 ORDER BY placed_at`,
		boolToInt(paper), string(domain.StatusComplete))
	if err != nil {
		return nil, fmt.Errorf("store: listing orders awaiting a stop: %w", err)
	}
	return collectOrders(rows)
}

func collectOrders(rows *sql.Rows) ([]domain.Order, error) {
	defer rows.Close()
	var out []domain.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecentOrders returns the newest orders in one execution mode, newest first.
// It is the dashboard query: "what has the bot done lately", regardless of
// whether each order is still live.
func (d *DB) RecentOrders(ctx context.Context, limit int, paper bool) ([]domain.Order, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE paper = ? ORDER BY placed_at DESC LIMIT ?`,
		boolToInt(paper), limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing recent orders: %w", err)
	}
	return collectOrders(rows)
}

// RecentSignals returns the newest signals in one execution mode, newest
// first, whether or not the bot acted on them.
func (d *DB) RecentSignals(ctx context.Context, limit int, paper bool) ([]domain.Signal, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT exchange, symbol, kind, at, price, stop, target, strategy, metadata
		FROM signals WHERE paper = ? ORDER BY at DESC, id DESC LIMIT ?`,
		boolToInt(paper), limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing recent signals: %w", err)
	}
	defer rows.Close()

	var out []domain.Signal
	for rows.Next() {
		var (
			sig                 domain.Signal
			kind, at, metadata  string
			price, stop, target int64
		)
		if err := rows.Scan(&sig.Key.Exchange, &sig.Key.Symbol, &kind, &at, &price, &stop, &target, &sig.Strategy, &metadata); err != nil {
			return nil, fmt.Errorf("store: scanning signal: %w", err)
		}
		sig.Kind = domain.SignalKind(kind)
		sig.Price, sig.Stop, sig.Target = money.Money(price), money.Money(stop), money.Money(target)
		if sig.At, err = parseTime(at); err != nil {
			return nil, err
		}
		if metadata != "" && metadata != "{}" {
			if err := json.Unmarshal([]byte(metadata), &sig.Metadata); err != nil {
				return nil, fmt.Errorf("store: signal has unreadable metadata %q: %w", metadata, err)
			}
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}

// InsertTrade records a closed round trip.
//
// key must be unique per matched lot, conventionally "<exit_order_id>:<index>",
// which is what makes re-running a reconciler over the same broker fills
// idempotent rather than duplicating every trade.
func (d *DB) InsertTrade(ctx context.Context, key string, t domain.Trade, runID int64) error {
	var run any
	if runID != 0 {
		run = runID
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO trades
			(key, run_id, exchange, symbol, strategy, side, quantity, entry_price, exit_price,
			 entry_at, exit_at, gross_pnl, charges, net_pnl, exit_reason, paper)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key, run, t.Key.Exchange, t.Key.Symbol, t.Strategy, string(t.Side), t.Quantity,
		int64(t.EntryPrice), int64(t.ExitPrice), formatTime(t.EntryAt), formatTime(t.ExitAt),
		int64(t.GrossPnL), int64(t.Charges), int64(t.NetPnL), string(t.ExitReason), boolToInt(t.Paper))
	if err != nil {
		return fmt.Errorf("store: inserting trade %s: %w", key, err)
	}
	return nil
}

// Trades returns closed trades exited at or after since, oldest first.
//
// Results are scoped to one execution mode. Paper and live rows share this
// table, and blending them contaminates every aggregate derived from it with
// fills that were never real, so the caller must say which it wants.
func (d *DB) Trades(ctx context.Context, since time.Time, paper bool) ([]domain.Trade, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT exchange, symbol, strategy, side, quantity, entry_price, exit_price,
		       entry_at, exit_at, gross_pnl, charges, net_pnl, exit_reason, paper
		FROM trades WHERE exit_at >= ? AND paper = ? ORDER BY exit_at`,
		formatTime(since), boolToInt(paper))
	if err != nil {
		return nil, fmt.Errorf("store: reading trades: %w", err)
	}
	defer rows.Close()

	var out []domain.Trade
	for rows.Next() {
		var (
			t                                domain.Trade
			side, reason                     string
			entry, exit, gross, charges, net int64
			entryAt, exitAt                  string
			paperFlag                        int
		)
		err := rows.Scan(&t.Key.Exchange, &t.Key.Symbol, &t.Strategy, &side, &t.Quantity,
			&entry, &exit, &entryAt, &exitAt, &gross, &charges, &net, &reason, &paperFlag)
		if err != nil {
			return nil, fmt.Errorf("store: scanning trade: %w", err)
		}
		t.Side = domain.Side(side)
		t.ExitReason = domain.ExitReason(reason)
		t.EntryPrice, t.ExitPrice = money.Money(entry), money.Money(exit)
		t.GrossPnL, t.Charges, t.NetPnL = money.Money(gross), money.Money(charges), money.Money(net)
		t.Paper = paperFlag == 1
		if t.EntryAt, err = parseTime(entryAt); err != nil {
			return nil, err
		}
		if t.ExitAt, err = parseTime(exitAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// InsertEquitySnapshot records one sample of account state.
func (d *DB) InsertEquitySnapshot(ctx context.Context, at time.Time, balance, realized, unrealized money.Money, openPositions int, paper bool, runID int64) error {
	var run any
	if runID != 0 {
		run = runID
	}
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO equity_snapshots (run_id, at, balance, realized_pnl, unrealized_pnl, open_positions, paper)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run, formatTime(at), int64(balance), int64(realized), int64(unrealized), openPositions, boolToInt(paper))
	if err != nil {
		return fmt.Errorf("store: inserting equity snapshot: %w", err)
	}
	return nil
}

// EquityCurve returns account balance samples at or after since, oldest first,
// in the shape core/stats consumes.
func (d *DB) EquityCurve(ctx context.Context, since time.Time, paper bool) ([]stats.Point, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT at, balance FROM equity_snapshots WHERE at >= ? AND paper = ? ORDER BY at`,
		formatTime(since), boolToInt(paper))
	if err != nil {
		return nil, fmt.Errorf("store: reading equity snapshots: %w", err)
	}
	defer rows.Close()

	var out []stats.Point
	for rows.Next() {
		var at string
		var balance int64
		if err := rows.Scan(&at, &balance); err != nil {
			return nil, fmt.Errorf("store: scanning equity snapshot: %w", err)
		}
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		out = append(out, stats.Point{At: t, Equity: money.Money(balance)})
	}
	return out, rows.Err()
}
