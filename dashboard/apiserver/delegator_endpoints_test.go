package apiserver

import "testing"

// TestClampDays pins the ?days= clamping contract for /delegator/quota:
// empty / invalid / non-positive falls back to the default, in-range
// values pass through, and oversized values clamp UP to the max rather
// than silently reset to the default.
func TestClampDays(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"empty falls back to default", "", defaultQuotaDays},
		{"non-numeric falls back to default", "abc", defaultQuotaDays},
		{"negative-looking (sign rejected) falls back", "-5", defaultQuotaDays},
		{"zero falls back to default", "0", defaultQuotaDays},
		{"one passes through", "1", 1},
		{"mid-range passes through", "7", 7},
		{"max passes through", "30", maxQuotaDays},
		{"over max clamps down to max", "31", maxQuotaDays},
		{"way over max clamps down to max", "100000", maxQuotaDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampDays(tc.raw); got != tc.want {
				t.Fatalf("clampDays(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
