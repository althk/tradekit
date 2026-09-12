package reference

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
	"github.com/althk/tradekit/go/store"
)

// Action kinds.
const (
	// KindSplit is a share split: the share count multiplies by Ratio and
	// the price divides by it.
	KindSplit = "split"
	// KindBonus is a bonus issue. Arithmetically identical to a split once
	// expressed as a ratio — a 1:1 bonus doubles the share count, so Ratio
	// is 2 — and kept as a separate kind only because the two are separate
	// events in every data source, and collapsing them would make an
	// imported action impossible to reconcile against its source.
	KindBonus = "bonus"
	// KindDividend is a cash dividend. Ratio is 1; Amount carries the
	// per-share payment.
	KindDividend = "dividend"
)

// Action is one corporate action.
type Action struct {
	Key domain.InstrumentKey
	// ExDate is the first day the instrument trades WITHOUT the
	// entitlement. Bars strictly before it are adjusted; the ex-date's own
	// bar already reflects the action and must not be touched.
	ExDate time.Time
	// Kind is one of the constants above.
	Kind string
	// Ratio is the factor the share count multiplies by: 10 for a 1:10
	// split, 2 for a 1:1 bonus, 1 for a dividend.
	Ratio float64
	// Amount is the per-share dividend, in minor units. Zero otherwise.
	Amount money.Money
}

// Actions returns the corporate actions for an instrument with an ex-date in
// [from, to], oldest first.
func Actions(ctx context.Context, db *store.DB, key domain.InstrumentKey, from, to time.Time) ([]Action, error) {
	rows, err := db.SQL().QueryContext(ctx, `
		SELECT ex_date, kind, ratio, amount
		FROM corporate_actions
		WHERE exchange = ? AND symbol = ? AND ex_date >= ? AND ex_date <= ?
		ORDER BY ex_date`,
		key.Exchange, key.Symbol, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("reference: reading corporate actions for %s: %w", key, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Action
	for rows.Next() {
		var exDate, kind string
		var ratio float64
		var amount int64
		if err := rows.Scan(&exDate, &kind, &ratio, &amount); err != nil {
			return nil, fmt.Errorf("reference: scanning corporate action: %w", err)
		}
		// The column is a date string, per invariant 4. Parsing it in UTC
		// is safe because only the calendar date is ever compared, and
		// Adjust compares dates rather than instants for that reason.
		at, err := time.Parse(time.DateOnly, exDate)
		if err != nil {
			return nil, fmt.Errorf("reference: corporate action for %s has an unreadable ex_date %q: %w", key, exDate, err)
		}
		out = append(out, Action{
			Key:    key,
			ExDate: at,
			Kind:   kind,
			Ratio:  ratio,
			Amount: money.Money(amount),
		})
	}
	return out, rows.Err()
}

// UpsertActions stores corporate actions, replacing one already held for the
// same instrument, ex-date and kind.
//
// REPLACE rather than IGNORE, unlike candles: a corrected ratio must take
// effect, and unlike a bar an action has no history worth preserving — there is
// one right answer for a split that has already happened.
func UpsertActions(ctx context.Context, db *store.DB, actions []Action) error {
	if len(actions) == 0 {
		return nil
	}
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reference: beginning corporate-action write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR REPLACE INTO corporate_actions
			(exchange, symbol, ex_date, kind, ratio, amount)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("reference: preparing corporate-action insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, a := range actions {
		if err := validate(a); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx,
			a.Key.Exchange, a.Key.Symbol, a.ExDate.Format(time.DateOnly),
			a.Kind, a.Ratio, int64(a.Amount)); err != nil {
			return fmt.Errorf("reference: inserting %s action for %s: %w", a.Kind, a.Key, err)
		}
	}
	return tx.Commit()
}

// validate rejects an action that would silently corrupt a series.
func validate(a Action) error {
	switch a.Kind {
	case KindSplit, KindBonus:
		if a.Ratio <= 0 {
			return fmt.Errorf("reference: a %s for %s needs a positive ratio, got %v", a.Kind, a.Key, a.Ratio)
		}
	case KindDividend:
		if a.Amount <= 0 {
			return fmt.Errorf("reference: a dividend for %s needs a positive amount, got %s", a.Key, a.Amount)
		}
	default:
		return fmt.Errorf("reference: unknown corporate action kind %q for %s", a.Kind, a.Key)
	}
	return nil
}

// Adjust back-adjusts a bar series for corporate actions.
//
// Prices strictly before an action's ex-date are divided by its factor and
// volumes multiplied by it, which is the convention every charting package
// uses: the most recent bar keeps its real traded price and history is
// restated in today's terms, so an indicator computed over the series sees a
// continuous price rather than a 90% single-day crash where a 1:10 split
// happened.
//
// A dividend adjusts prices but NOT volume. No shares were created, so the
// share count is unchanged; only the price gapped down by the payment.
//
// Multiple actions compound, applied in ex-date order. Two 1:2 splits a year
// apart mean a bar before both is divided by four, and applying them in the
// wrong order — or applying only the later one — leaves an error large enough
// to change every result computed over the series.
//
// The input is not modified; an empty action list returns the input unchanged.
func Adjust(candles []domain.Candle, actions []Action) []domain.Candle {
	if len(candles) == 0 || len(actions) == 0 {
		return candles
	}

	ordered := make([]Action, len(actions))
	copy(ordered, actions)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ExDate.Before(ordered[j].ExDate) })

	out := make([]domain.Candle, len(candles))
	copy(out, candles)

	for _, a := range ordered {
		factor, volumeFactor := adjustment(a, out)
		if factor == 1 && volumeFactor == 1 {
			continue
		}
		for i := range out {
			// Compared as calendar dates, not instants: a daily bar is
			// stamped at midnight and an intraday bar at 09:15, and an
			// instant comparison would adjust the ex-date's own morning
			// bars while leaving its afternoon ones alone.
			if !before(out[i].Start, a.ExDate) {
				continue
			}
			out[i].Open = scale(out[i].Open, factor)
			out[i].High = scale(out[i].High, factor)
			out[i].Low = scale(out[i].Low, factor)
			out[i].Close = scale(out[i].Close, factor)
			if volumeFactor != 1 {
				out[i].Volume = int64(float64(out[i].Volume)*volumeFactor + 0.5)
			}
		}
	}
	return out
}

// adjustment returns the price and volume factors for one action.
//
// A dividend's factor depends on the price it was paid against — the last close
// before the ex-date — so the series is needed as well as the action. That
// close is the standard reference: the adjustment is (close - dividend) /
// close, which removes exactly the value that left the company.
func adjustment(a Action, candles []domain.Candle) (price, volume float64) {
	switch a.Kind {
	case KindSplit, KindBonus:
		if a.Ratio <= 0 {
			return 1, 1
		}
		return 1 / a.Ratio, a.Ratio
	case KindDividend:
		last, ok := lastCloseBefore(candles, a.ExDate)
		if !ok || last <= 0 || a.Amount <= 0 || a.Amount >= last {
			// A dividend larger than the price it was paid against is a
			// data error, and applying it would produce a negative or
			// zero price — which every indicator downstream would
			// happily compute over. Leaving the series unadjusted is a
			// small, visible error; a negative price is a large,
			// invisible one.
			return 1, 1
		}
		return float64(last-a.Amount) / float64(last), 1
	default:
		return 1, 1
	}
}

// lastCloseBefore returns the closing price of the last bar before the ex-date.
func lastCloseBefore(candles []domain.Candle, exDate time.Time) (money.Money, bool) {
	var last money.Money
	var found bool
	for _, c := range candles {
		if before(c.Start, exDate) {
			last, found = c.Close, true
			continue
		}
		break
	}
	return last, found
}

// before compares a bar's timestamp with the ex-date by calendar date.
//
// The bar's OWN calendar date is used, not the ex-date's zone. Actions parses
// ex-dates in UTC, and a daily bar is stamped at midnight in the exchange's
// zone; converting that instant to UTC lands it on the previous evening, so the
// ex-date's own bar would be adjusted along with the history it is supposed to
// anchor. The ex-date is a date, not an instant, and the bar's zone is the
// exchange's, so the bar's local date is the one that counts.
func before(t, exDate time.Time) bool {
	return t.Format(time.DateOnly) < exDate.Format(time.DateOnly)
}

// scale multiplies a price by a factor, rounding half away from zero to match
// core/money.
func scale(m money.Money, factor float64) money.Money {
	return m.MulFraction(factor)
}
