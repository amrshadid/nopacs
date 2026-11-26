package settings

import (
	"testing"
	"time"
)

// TestReplicaAndRetentionDefinitions guards the definitions of the settings
// of replicas, retention and the quarantine's warnings: each built in with
// its default and its scope, the two that guard retention's clock and notice
// set on a tenant by the installation's admin only.
func TestReplicaAndRetentionDefinitions(t *testing.T) {
	tab := MustBuiltin()
	for name, want := range map[string]struct {
		def            string
		byInstallation bool
	}{
		ReplicaRepair: {"manual", false}, ReplicaRepairLease: {"1m", false}, RetentionShortenNotice: {"30d", true},
		RetentionMaxClockStep: {"2d", true}, RetentionMaxPerPass: {"1000", false}, QuarantineWarnAge: {"30d", false},
		QuarantineWarnBytes: {"0", false},
	} {
		d, ok := tab.Lookup(name)
		if !ok || d.Default != want.def || !d.TenantOverride || d.TenantSetByInstallation != want.byInstallation {
			t.Errorf("%s: %+v %v", name, d, ok)
		}
	}
}

// TestReplicaAndRetentionBounds guards each setting's bounds: the least and
// the most taken, one past either refused, and a value of another form
// refused.
func TestReplicaAndRetentionBounds(t *testing.T) {
	durations := map[string]struct {
		parse   func(string) (time.Duration, error)
		in      map[string]time.Duration
		refused []string
	}{
		ReplicaRepairLease: {ParseRepairLease, map[string]time.Duration{"30s": 30 * time.Second, "1h": time.Hour}, []string{"29s", "61m", "0", "1 m"}},
		RetentionShortenNotice: {ParseShortenNotice, map[string]time.Duration{"1d": 24 * time.Hour, "365d": 365 * 24 * time.Hour},
			[]string{"23h", "366d", ""}},
		RetentionMaxClockStep: {ParseMaxClockStep, map[string]time.Duration{"1h": time.Hour, "30d": 30 * 24 * time.Hour}, []string{"59m", "31d"}},
		QuarantineWarnAge: {ParseQuarantineWarnAge, map[string]time.Duration{"1d": 24 * time.Hour, "3650d": 3650 * 24 * time.Hour},
			[]string{"23h", "3651d"}},
	}
	for name, c := range durations {
		for v, want := range c.in {
			if got, err := c.parse(v); err != nil || got != want {
				t.Errorf("%s %q: %v %v", name, v, got, err)
			}
		}
		for _, v := range c.refused {
			if _, err := c.parse(v); err == nil {
				t.Errorf("%s %q was taken", name, v)
			}
		}
	}
	for v, want := range map[string]int{"1": 1, "100000": 100000} {
		if got, err := ParseMaxPerPass(v); err != nil || got != want {
			t.Errorf("max-per-pass %q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"0", "100001", "01", "+5", "", "1e3"} {
		if _, err := ParseMaxPerPass(v); err == nil {
			t.Errorf("max-per-pass %q was taken", v)
		}
	}
	for _, v := range []string{"manual", "auto"} {
		if got, err := ParseReplicaRepair(v); err != nil || got != v {
			t.Errorf("replica.repair %q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"", "Auto", "on"} {
		if _, err := ParseReplicaRepair(v); err == nil {
			t.Errorf("replica.repair %q was taken", v)
		}
	}
}
