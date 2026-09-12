// Package money represents monetary amounts as integer minor units.
//
// A Money value is a count of the currency's smallest unit — paise for INR,
// cents for USD. Integers are used rather than float64 because accumulated
// profit and loss must be exact, and rather than a decimal library because the
// same representation has to exist unchanged in Go, in Python and in a SQLite
// INTEGER column.
//
// A Money value carries no currency tag. Mixing currencies in one calculation
// is prevented by construction elsewhere: a Trade, a Position and an Account
// each belong to a single instrument or venue.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is an amount in the currency's minor unit.
type Money int64

// Zero is the additive identity, provided for readability at call sites.
const Zero Money = 0

// scale is the number of minor units in one major unit. Both currencies
// tradekit targets (INR, USD) use 100, and a currency that does not would need
// its own type rather than a variable here.
const scale = 100

// ErrParse reports a string that is not a valid decimal amount.
var ErrParse = errors.New("money: cannot parse amount")

// FromMajor converts a whole-currency amount, rounding half away from zero.
//
// It is intended for configuration and test fixtures, where the input is a
// literal such as 1234.56. Prices arriving from a broker API should go through
// Parse instead, which does not pass through binary floating point at all.
func FromMajor(v float64) Money {
	return Money(roundHalfAwayFromZero(v * scale))
}

// Major returns the amount as a whole-currency float. It is for display and
// for handing values to code that cannot accept integers, such as a plotting
// library. Never round-trip through it: FromMajor(m.Major()) is not guaranteed
// to equal m for very large amounts.
func (m Money) Major() float64 { return float64(m) / scale }

// Parse reads a decimal string such as "1234.56", "-0.05" or "1234" into
// minor units, without floating-point intermediate steps.
//
// More than two decimal places is an error rather than a silent truncation: a
// price the exchange cannot quote is a bug upstream, and rounding it here
// would hide it.
func Parse(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrParse)
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	intPart, fracPart, hasFrac := strings.Cut(s, ".")
	if intPart == "" && !hasFrac {
		return 0, fmt.Errorf("%w: %q", ErrParse, s)
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: %q has more than 2 decimal places", ErrParse, s)
	}
	// strconv.ParseInt would accept a second sign here ("--1") and any
	// underscore separators Go literals allow, neither of which is a price.
	if !allDigits(intPart) || !allDigits(fracPart) {
		return 0, fmt.Errorf("%w: %q", ErrParse, s)
	}

	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrParse, s)
	}

	var minor int64
	if hasFrac && fracPart != "" {
		// "5" means 50 paise, "05" means 5 paise.
		padded := fracPart + strings.Repeat("0", 2-len(fracPart))
		minor, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrParse, s)
		}
	}

	total := units*scale + minor
	if neg {
		total = -total
	}
	return Money(total), nil
}

// MustParse is Parse for constants and test fixtures, panicking on bad input.
func MustParse(s string) Money {
	m, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return m
}

// String renders the amount with exactly two decimal places and no thousands
// separators, so that it round-trips through Parse.
func (m Money) String() string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%02d", v/scale, v%scale)
	if neg {
		return "-" + s
	}
	return s
}

// Mul scales the amount by a whole quantity, as in price times shares.
func (m Money) Mul(qty int64) Money { return Money(int64(m) * qty) }

// MulFraction scales the amount by a ratio — a charge rate, a percentage, an
// ATR multiple — rounding half away from zero.
func (m Money) MulFraction(f float64) Money {
	return Money(roundHalfAwayFromZero(float64(m) * f))
}

// Abs returns the magnitude of the amount.
func (m Money) Abs() Money {
	if m < 0 {
		return -m
	}
	return m
}

// Sign reports -1, 0 or +1.
func (m Money) Sign() int {
	switch {
	case m < 0:
		return -1
	case m > 0:
		return 1
	default:
		return 0
	}
}

// RoundToTick returns the nearest multiple of tick, which is how a limit price
// must be expressed for the exchange to accept it. A non-positive tick returns
// the amount unchanged, because refusing to round is safer than inventing a
// tick size the venue may not use.
func (m Money) RoundToTick(tick Money) Money {
	if tick <= 0 {
		return m
	}
	return roundQuotient(int64(m), int64(tick)) * tick
}

// FloorToTick returns the largest multiple of tick not greater than the
// amount. Use it for a sell limit, where rounding up could place the order
// outside the book.
func (m Money) FloorToTick(tick Money) Money {
	if tick <= 0 {
		return m
	}
	q := int64(m) / int64(tick)
	if int64(m)%int64(tick) < 0 {
		q--
	}
	return Money(q) * tick
}

// CeilToTick returns the smallest multiple of tick not less than the amount.
// Use it for a buy limit, for the mirror-image reason to FloorToTick.
func (m Money) CeilToTick(tick Money) Money {
	if tick <= 0 {
		return m
	}
	q := int64(m) / int64(tick)
	if int64(m)%int64(tick) > 0 {
		q++
	}
	return Money(q) * tick
}

// allDigits reports whether s consists only of ASCII digits. An empty string
// is accepted: it is the absent fractional part of "12.".
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// roundQuotient divides and rounds half away from zero, without floating point.
func roundQuotient(a, b int64) Money {
	q, r := a/b, a%b
	if r == 0 {
		return Money(q)
	}
	twice := 2 * r
	if twice < 0 {
		twice = -twice
	}
	if twice >= b {
		if (a < 0) != (b < 0) {
			q--
		} else {
			q++
		}
	}
	return Money(q)
}

// roundHalfAwayFromZero matches Python's decimal ROUND_HALF_UP and is the
// single rounding rule tradekit uses. Go's math.Round already rounds half away
// from zero; this wrapper exists so the rule is named once and the Python
// implementation has an obvious counterpart.
func roundHalfAwayFromZero(v float64) int64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int64(math.Round(v))
}
