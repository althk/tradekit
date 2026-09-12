package harness

import (
	"log/slog"

	"github.com/althk/tradekit/go/core/domain"
	"github.com/althk/tradekit/go/core/money"
)

// Logging is for operating the process, and every project uses log/slog. This
// file adds no logging framework, only the attribute helpers that make a
// trading log readable: money as "1234.56" rather than an int64 of paise —
// which is unreadable and, worse, silently mistaken for rupees — and an
// instrument as "NSE:RELIANCE" rather than a struct dump.

// MoneyValue renders an amount as its decimal string.
func MoneyValue(m money.Money) slog.Value { return slog.StringValue(m.String()) }

// Money is a log attribute for an amount under the given key.
func Money(key string, m money.Money) slog.Attr { return slog.Attr{Key: key, Value: MoneyValue(m)} }

// Key is the "key" attribute for an instrument, rendered as EXCHANGE:SYMBOL.
func Key(k domain.InstrumentKey) slog.Attr { return slog.String("key", k.String()) }

// Side is the "side" attribute for an order or position side.
func Side(s domain.Side) slog.Attr { return slog.String("side", string(s)) }

// Qty is the "qty" attribute for a quantity.
func Qty(n int) slog.Attr { return slog.Int("qty", n) }

// Reason is the "reason" attribute, for the text a risk gate or a strategy
// gives when it declines to act.
func Reason(r string) slog.Attr { return slog.String("reason", r) }
