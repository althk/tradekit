package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/costs"
)

// ErrStateNotFound reports that a key has never been written.
//
// It is distinct from a failure so that a cold start reads as normal. One of
// the stores this replaces returned nil for a missing key, which meant a
// caller could not tell "no state yet" from "state loaded successfully into an
// untouched struct" — and a risk manager that cannot tell those apart silently
// starts the day with zeroed counters after a failed read.
var ErrStateNotFound = errors.New("store: state key not found")

// SetState stores a JSON-encoded value under a key.
func (d *DB) SetState(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("store: encoding state %q: %w", key, err)
	}
	_, err = d.db.ExecContext(ctx, `
		INSERT INTO kv_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, string(raw), formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: writing state %q: %w", key, err)
	}
	return nil
}

// GetState decodes the value stored under a key into dest, returning
// ErrStateNotFound when the key has never been written.
func (d *DB) GetState(ctx context.Context, key string, dest any) error {
	var raw string
	err := d.db.QueryRowContext(ctx, `SELECT value FROM kv_state WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrStateNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("store: reading state %q: %w", key, err)
	}
	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return fmt.Errorf("store: decoding state %q: %w", key, err)
	}
	return nil
}

// DeleteState removes a key. Deleting a key that does not exist is not an error.
func (d *DB) DeleteState(ctx context.Context, key string) error {
	if _, err := d.db.ExecContext(ctx, `DELETE FROM kv_state WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: deleting state %q: %w", key, err)
	}
	return nil
}

// StartRun opens a named execution — a live session, a sync pass, a backtest —
// and returns its id, which every row that run produces should carry.
func (d *DB) StartRun(ctx context.Context, kind, name string, params any) (int64, error) {
	encoded := "{}"
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return 0, fmt.Errorf("store: encoding run params: %w", err)
		}
		encoded = string(raw)
	}
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO runs (kind, name, params, started_at, status) VALUES (?, ?, ?, ?, 'running')`,
		kind, name, encoded, formatTime(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("store: starting run: %w", err)
	}
	return res.LastInsertId()
}

// FinishRun closes a run with a terminal status and an optional message.
func (d *DB) FinishRun(ctx context.Context, id int64, status, message string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE runs SET ended_at = ?, status = ?, message = ? WHERE id = ?`,
		formatTime(time.Now().UTC()), status, message, id)
	if err != nil {
		return fmt.Errorf("store: finishing run %d: %w", id, err)
	}
	return nil
}

// UpsertChargeRate records one effective-dated charge rate.
func (d *DB) UpsertChargeRate(ctx context.Context, broker string, seg costs.Segment, kind costs.Kind, r costs.Rate) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO charge_rates (broker, segment, kind, effective_from, rate, flat)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(broker, segment, kind, effective_from) DO UPDATE SET
			rate = excluded.rate, flat = excluded.flat`,
		broker, string(seg), string(kind), formatDate(r.EffectiveFrom), r.Value, boolToInt(r.Flat))
	if err != nil {
		return fmt.Errorf("store: writing charge rate %s/%s/%s: %w", broker, seg, kind, err)
	}
	return nil
}

// ChargeTable loads every stored rate into a costs.Table.
//
// This is the bridge that keeps costs free of a database dependency: the rate
// card is data, and this is where the data comes from.
func (d *DB) ChargeTable(ctx context.Context) (*costs.Table, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT broker, segment, kind, effective_from, rate, flat FROM charge_rates ORDER BY effective_from`)
	if err != nil {
		return nil, fmt.Errorf("store: reading charge rates: %w", err)
	}
	defer rows.Close()

	table := costs.NewTable()
	for rows.Next() {
		var broker, segment, kind, effectiveFrom string
		var rate float64
		var flat int
		if err := rows.Scan(&broker, &segment, &kind, &effectiveFrom, &rate, &flat); err != nil {
			return nil, fmt.Errorf("store: scanning charge rate: %w", err)
		}
		from, err := parseDate(effectiveFrom)
		if err != nil {
			return nil, err
		}
		table.Set(broker, costs.Segment(segment), costs.Kind(kind), costs.Rate{
			Value:         rate,
			Flat:          flat == 1,
			EffectiveFrom: from,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return table, nil
}
