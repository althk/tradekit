package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// UpsertInstruments writes the instrument master, replacing rows that exist.
//
// It is one transaction for the whole batch: a partially-refreshed universe is
// worse than a stale one, because a scan over it would silently cover only the
// symbols that happened to be written before the failure.
func (d *DB) UpsertInstruments(ctx context.Context, instruments []domain.Instrument) error {
	if len(instruments) == 0 {
		return nil
	}
	now := formatTime(time.Now().UTC())

	return d.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO instruments
				(exchange, symbol, name, isin, segment, lot_size, tick_size,
				 expiry, strike, option_type, active, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(exchange, symbol) DO UPDATE SET
				name        = excluded.name,
				isin        = excluded.isin,
				segment     = excluded.segment,
				lot_size    = excluded.lot_size,
				tick_size   = excluded.tick_size,
				expiry      = excluded.expiry,
				strike      = excluded.strike,
				option_type = excluded.option_type,
				active      = excluded.active,
				updated_at  = excluded.updated_at`)
		if err != nil {
			return fmt.Errorf("store: preparing instrument upsert: %w", err)
		}
		defer stmt.Close()

		for _, in := range instruments {
			var expiry any
			if in.Expiry != nil {
				expiry = formatDate(*in.Expiry)
			}
			var strike any
			if in.Strike != 0 {
				strike = int64(in.Strike)
			}
			_, err := stmt.ExecContext(ctx,
				in.Key.Exchange, in.Key.Symbol, in.Name, in.ISIN, in.Segment,
				in.LotSize, int64(in.TickSize), expiry, strike, in.OptionType,
				boolToInt(in.Active), now)
			if err != nil {
				return fmt.Errorf("store: upserting instrument %s: %w", in.Key, err)
			}
		}
		return nil
	})
}

const instrumentColumns = `exchange, symbol, name, isin, segment, lot_size, tick_size,
	expiry, strike, option_type, active`

func scanInstrument(sc interface{ Scan(...any) error }) (domain.Instrument, error) {
	var (
		in       domain.Instrument
		tickSize int64
		expiry   sql.NullString
		strike   sql.NullInt64
		active   int
	)
	err := sc.Scan(&in.Key.Exchange, &in.Key.Symbol, &in.Name, &in.ISIN, &in.Segment,
		&in.LotSize, &tickSize, &expiry, &strike, &in.OptionType, &active)
	if err != nil {
		return domain.Instrument{}, err
	}

	in.TickSize = money.Money(tickSize)
	in.Strike = money.Money(strike.Int64)
	in.Active = active == 1
	if expiry.Valid && expiry.String != "" {
		e, err := parseDate(expiry.String)
		if err != nil {
			return domain.Instrument{}, err
		}
		in.Expiry = &e
	}
	return in, nil
}

// Instrument returns one instrument, or ErrNotFound.
func (d *DB) Instrument(ctx context.Context, key domain.InstrumentKey) (domain.Instrument, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT `+instrumentColumns+` FROM instruments WHERE exchange = ? AND symbol = ?`,
		key.Exchange, key.Symbol)

	in, err := scanInstrument(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Instrument{}, fmt.Errorf("%w: instrument %s", ErrNotFound, key)
	}
	if err != nil {
		return domain.Instrument{}, fmt.Errorf("store: reading instrument %s: %w", key, err)
	}
	return in, nil
}

// ActiveInstruments returns the tradable universe, optionally narrowed to one
// segment. An empty segment returns every active instrument.
func (d *DB) ActiveInstruments(ctx context.Context, segment string) ([]domain.Instrument, error) {
	query := `SELECT ` + instrumentColumns + ` FROM instruments WHERE active = 1`
	args := []any{}
	if segment != "" {
		query += ` AND segment = ?`
		args = append(args, segment)
	}
	query += ` ORDER BY exchange, symbol`

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing active instruments: %w", err)
	}
	defer rows.Close()

	var out []domain.Instrument
	for rows.Next() {
		in, err := scanInstrument(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning instrument: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SetBrokerID records a broker's own identifier for an instrument.
//
// These live in their own table so that adding a broker is not a schema change,
// and so an instrument can carry a Kite token and an Upstox instrument key at
// the same time.
func (d *DB) SetBrokerID(ctx context.Context, key domain.InstrumentKey, broker, brokerID string) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO instrument_ids (exchange, symbol, broker, broker_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(exchange, symbol, broker) DO UPDATE SET broker_id = excluded.broker_id`,
		key.Exchange, key.Symbol, broker, brokerID)
	if err != nil {
		return fmt.Errorf("store: setting %s id for %s: %w", broker, key, err)
	}
	return nil
}

// BrokerID returns a broker's identifier for an instrument, or ErrNotFound.
func (d *DB) BrokerID(ctx context.Context, key domain.InstrumentKey, broker string) (string, error) {
	var id string
	err := d.db.QueryRowContext(ctx,
		`SELECT broker_id FROM instrument_ids WHERE exchange = ? AND symbol = ? AND broker = ?`,
		key.Exchange, key.Symbol, broker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s id for %s", ErrNotFound, broker, key)
	}
	if err != nil {
		return "", fmt.Errorf("store: reading %s id for %s: %w", broker, key, err)
	}
	return id, nil
}

// KeyForBrokerID resolves a broker's own identifier back to an instrument key.
// This is the lookup a tick stream needs, where every message carries the
// broker's token and nothing else.
func (d *DB) KeyForBrokerID(ctx context.Context, broker, brokerID string) (domain.InstrumentKey, error) {
	var key domain.InstrumentKey
	err := d.db.QueryRowContext(ctx,
		`SELECT exchange, symbol FROM instrument_ids WHERE broker = ? AND broker_id = ?`,
		broker, brokerID).Scan(&key.Exchange, &key.Symbol)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.InstrumentKey{}, fmt.Errorf("%w: %s id %q", ErrNotFound, broker, brokerID)
	}
	if err != nil {
		return domain.InstrumentKey{}, fmt.Errorf("store: resolving %s id %q: %w", broker, brokerID, err)
	}
	return key, nil
}

// DeactivateMissing marks every instrument on an exchange inactive unless it
// appears in keep, and reports how many were deactivated.
//
// Index membership changes and delisted symbols must stop being scanned, but
// deleting their rows would orphan the candles and trades that reference them.
// Deactivating keeps the history readable.
func (d *DB) DeactivateMissing(ctx context.Context, exchange string, keep []domain.InstrumentKey) (int, error) {
	present := make(map[string]bool, len(keep))
	for _, k := range keep {
		present[k.Symbol] = true
	}

	rows, err := d.db.QueryContext(ctx,
		`SELECT symbol FROM instruments WHERE exchange = ? AND active = 1`, exchange)
	if err != nil {
		return 0, fmt.Errorf("store: listing active symbols on %s: %w", exchange, err)
	}
	var stale []string
	for rows.Next() {
		var symbol string
		if err := rows.Scan(&symbol); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scanning symbol: %w", err)
		}
		if !present[symbol] {
			stale = append(stale, symbol)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	now := formatTime(time.Now().UTC())
	err = d.inTx(ctx, func(tx *sql.Tx) error {
		for _, symbol := range stale {
			_, err := tx.ExecContext(ctx,
				`UPDATE instruments SET active = 0, updated_at = ? WHERE exchange = ? AND symbol = ?`,
				now, exchange, symbol)
			if err != nil {
				return fmt.Errorf("store: deactivating %s:%s: %w", exchange, symbol, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(stale), nil
}
