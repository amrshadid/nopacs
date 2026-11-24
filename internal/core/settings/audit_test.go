package settings

import (
	"testing"
	"time"
)

// TestParseSealInterval guards audit.seal-interval's bounds, 1m to 1d both
// included, in the one duration form.
func TestParseSealInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"1m": time.Minute, "10m": 10 * time.Minute, "24h": 24 * time.Hour, "1d": 24 * time.Hour, "60s": time.Minute,
		"59s": -1, "25h": -1, "2d": -1, "0": -1, "": -1, "10 m": -1, "-1m": -1, "1.5m": -1,
	} {
		got, err := ParseSealInterval(v)
		if want < 0 {
			if err == nil {
				t.Errorf("%q accepted as %v, want refused", v, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", v, got, err, want)
		}
	}
}

// TestParseAuditRetention guards audit.retention: "0" keeps everything and
// reads as 0; otherwise 90d to 36500d both included, so a short period
// cannot erase the recent record; nothing else, not "0d" nor an empty value.
func TestParseAuditRetention(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"0": 0, "90d": 90 * 24 * time.Hour, "2160h": 90 * 24 * time.Hour, "3650d": 3650 * 24 * time.Hour, "36500d": 36500 * 24 * time.Hour,
		"0d": -1, "": -1, "1d": -1, "24h": -1, "89d": -1, "2159h": -1, "36501d": -1, "00": -1, "forever": -1, "-1d": -1,
	} {
		got, err := ParseAuditRetention(v)
		if want < 0 {
			if err == nil {
				t.Errorf("%q accepted as %v, want refused", v, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", v, got, err, want)
		}
	}
}

// TestAuditSettingsBuiltin guards the two audit settings in the built-in
// list: installation-only, with the defaults the contract states ("10m",
// "0").
func TestAuditSettingsBuiltin(t *testing.T) {
	tab := MustBuiltin()
	for name, def := range map[string]string{AuditSealInterval: "10m", AuditRetention: "0"} {
		d, ok := tab.Lookup(name)
		if !ok {
			t.Errorf("%s is not built in", name)
			continue
		}
		if d.Default != def || d.Kind() != InstallationOnly {
			t.Errorf("%s: default %q kind %v, want %q installation-only", name, d.Default, d.Kind(), def)
		}
	}
}
