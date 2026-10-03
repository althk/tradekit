package upstox

import (
	"testing"
	"time"
)

func TestNextResetIsTheFollowingThreeThirtyIST(t *testing.T) {
	ist := istLocation()
	cases := []struct{ issued, want time.Time }{
		// A 02:00 login dies at 03:30 the same night, before the open.
		{time.Date(2026, 1, 15, 2, 0, 0, 0, ist), time.Date(2026, 1, 15, 3, 30, 0, 0, ist)},
		{time.Date(2026, 1, 15, 8, 45, 0, 0, ist), time.Date(2026, 1, 16, 3, 30, 0, 0, ist)},
	}
	for _, tc := range cases {
		if got := nextReset(tc.issued, tokenResetMinute); !got.Equal(tc.want) {
			t.Errorf("nextReset(%s) = %s, want %s", tc.issued, got, tc.want)
		}
	}
}
