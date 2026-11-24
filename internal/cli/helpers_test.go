package cli

import (
	"testing"
	"time"
)

// TestParseSpan guards the lengths of time a person types for --expires and
// --since: each unit, and the refusals (no unit, no number, zero, negative, an
// overflow that would wrap into the past).
func TestParseSpan(t *testing.T) {
	ok := map[string]time.Duration{
		"90d": 90 * 24 * time.Hour, "36h": 36 * time.Hour, "15m": 15 * time.Minute,
		"30s": 30 * time.Second, "2w": 14 * 24 * time.Hour, "1d": 24 * time.Hour,
	}
	for in, want := range ok {
		if got, err := parseSpan(in); err != nil || got != want {
			t.Errorf("parseSpan(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "90", "0d", "-1d", "1.5h", "5y", "d5", "99999999999999d", "9223372036854775807h"} {
		if got, err := parseSpan(in); err == nil {
			t.Errorf("parseSpan(%q) = %v, want an error", in, got)
		}
	}
}

// TestParseTime guards the three forms of --since and --until: an age before
// now, a date in the shown zone, and a full time with its offset.
func TestParseTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	eat := time.FixedZone("EAT", 3*3600)
	cases := map[string]time.Time{
		"24h":                       now.Add(-24 * time.Hour),
		"7d":                        now.Add(-7 * 24 * time.Hour),
		"2026-11-01":                time.Date(2026, 11, 1, 0, 0, 0, 0, eat),
		"2026-11-17T09:30:00+03:00": time.Date(2026, 11, 17, 6, 30, 0, 0, time.UTC),
		"2026-11-17T09:30:00Z":      time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseTime("since", in, now, eat)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-13-01", "11/17/2026", "2026-11-17 09:30"} {
		if _, err := parseTime("since", in, now, eat); err == nil {
			t.Errorf("parseTime(%q) succeeded, want an error", in)
		}
	}
}
