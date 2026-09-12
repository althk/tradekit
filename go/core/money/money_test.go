package money

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Money
	}{
		{"0", 0},
		{"1", 100},
		{"1.5", 150},
		{"1.05", 105},
		{"1234.56", 123456},
		{"-0.05", -5},
		{"-1234.56", -123456},
		{"+7.25", 725},
		{".5", 50},
		{"  12.34  ", 1234},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "abc", "1.234", "1.2.3", "--1", "1e5"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted an invalid amount", in)
		}
	}
}

func TestStringRoundTrips(t *testing.T) {
	for _, in := range []string{"0.00", "1.00", "1.05", "1234.56", "-0.05", "-1234.56"} {
		m := MustParse(in)
		if got := m.String(); got != in {
			t.Errorf("MustParse(%q).String() = %q, want %q", in, got, in)
		}
	}
}

func TestMulFractionRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		m    Money
		f    float64
		want Money
	}{
		{100, 0.005, 1},   // 0.5 rounds away from zero to 1
		{100, 0.015, 2},   // 1.5 rounds to 2, not to banker's 2 by luck
		{100, 0.025, 3},   // 2.5 rounds to 3, where banker's rounding gives 2
		{-100, 0.025, -3}, // and symmetrically for negatives
		{123456, 0.001, 123},
	}
	for _, c := range cases {
		if got := c.m.MulFraction(c.f); got != c.want {
			t.Errorf("Money(%d).MulFraction(%v) = %d, want %d", c.m, c.f, got, c.want)
		}
	}
}

func TestRoundToTick(t *testing.T) {
	const tick Money = 5 // 0.05
	cases := []struct{ in, want Money }{
		{100, 100},
		{102, 100},
		{103, 105},
		{1237, 1235},
		{1238, 1240},
		{-102, -100},
		{-103, -105},
	}
	for _, c := range cases {
		if got := c.in.RoundToTick(tick); got != c.want {
			t.Errorf("Money(%d).RoundToTick(5) = %d, want %d", c.in, got, c.want)
		}
	}
	if got := Money(103).RoundToTick(0); got != 103 {
		t.Errorf("a non-positive tick must leave the amount unchanged, got %d", got)
	}
}

func TestFloorAndCeilToTick(t *testing.T) {
	const tick Money = 5
	if got := Money(103).FloorToTick(tick); got != 100 {
		t.Errorf("FloorToTick = %d, want 100", got)
	}
	if got := Money(101).CeilToTick(tick); got != 105 {
		t.Errorf("CeilToTick = %d, want 105", got)
	}
	if got := Money(100).FloorToTick(tick); got != 100 {
		t.Errorf("an exact multiple must be unchanged by FloorToTick, got %d", got)
	}
	if got := Money(100).CeilToTick(tick); got != 100 {
		t.Errorf("an exact multiple must be unchanged by CeilToTick, got %d", got)
	}
	if got := Money(-103).FloorToTick(tick); got != -105 {
		t.Errorf("FloorToTick(-103) = %d, want -105", got)
	}
	if got := Money(-103).CeilToTick(tick); got != -100 {
		t.Errorf("CeilToTick(-103) = %d, want -100", got)
	}
}

func TestMulAndSign(t *testing.T) {
	if got := MustParse("12.50").Mul(4); got != MustParse("50.00") {
		t.Errorf("12.50 * 4 = %s, want 50.00", got)
	}
	if got := MustParse("-3.00").Abs(); got != MustParse("3.00") {
		t.Errorf("Abs = %s, want 3.00", got)
	}
	if Zero.Sign() != 0 || MustParse("1").Sign() != 1 || MustParse("-1").Sign() != -1 {
		t.Error("Sign did not report -1/0/+1 correctly")
	}
}

func TestFromMajor(t *testing.T) {
	cases := []struct {
		in   float64
		want Money
	}{
		{0, 0},
		{1234.56, 123456},
		{-0.05, -5},
		{0.005, 1},
		{0.015, 2},
	}
	for _, c := range cases {
		if got := FromMajor(c.in); got != c.want {
			t.Errorf("FromMajor(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
