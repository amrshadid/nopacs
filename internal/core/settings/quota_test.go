package settings

import "testing"

// TestQuotaValues guards the rules of the quota's settings: an empty bound
// is no bound, a bound is at least one, and the warning is a whole
// percentage from 1% to 100%.
func TestQuotaValues(t *testing.T) {
	for v, want := range map[string]int64{"": 0, "1": 1, "10MiB": 10 << 20, "2TiB": 2 << 40} {
		if n, err := ParseQuotaBytes(v); err != nil || n != want {
			t.Errorf("quota.bytes %q: %d, %v; want %d", v, n, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "1.5GiB", "10 MiB", "01", "10GB"} {
		if _, err := ParseQuotaBytes(v); err == nil {
			t.Errorf("quota.bytes %q is taken", v)
		}
	}
	for v, want := range map[string]int64{"": 0, "1": 1, "1000000": 1000000, "4611686018427387904": 1 << 62} {
		if n, err := ParseQuotaInstances(v); err != nil || n != want {
			t.Errorf("quota.instances %q: %d, %v; want %d", v, n, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "1e6", "01", "4611686018427387905", "1MiB"} {
		if _, err := ParseQuotaInstances(v); err == nil {
			t.Errorf("quota.instances %q is taken", v)
		}
	}
	for v, want := range map[string]int{"1%": 1, "90%": 90, "100%": 100} {
		if n, err := ParseQuotaWarn(v); err != nil || n != want {
			t.Errorf("quota.warn %q: %d, %v; want %d", v, n, err, want)
		}
	}
	for _, v := range []string{"", "0%", "101%", "90", "9.5%", "090%", "%"} {
		if _, err := ParseQuotaWarn(v); err == nil {
			t.Errorf("quota.warn %q is taken", v)
		}
	}
}
