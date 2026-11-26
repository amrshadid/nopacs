package settings

import (
	"testing"
	"time"
)

// TestScrubSettingsRules guards the bounds and defaults of scrub.rate and
// scrub.interval at each edge and one past it, that both are ordinary
// settings a tenant may override, and what each parses to.
func TestScrubSettingsRules(t *testing.T) {
	tab := MustBuiltin()
	check := func(name string, accept, refuse []string) {
		t.Helper()
		d, ok := tab.Lookup(name)
		if !ok {
			t.Fatalf("%s is not built in", name)
		}
		if d.Kind() != Ordinary || !d.TenantOverride {
			t.Errorf("%s is an ordinary setting a tenant may override: %+v", name, d)
		}
		if err := d.Validate(d.Default); err != nil {
			t.Errorf("%s's default %q: %v", name, d.Default, err)
		}
		for _, v := range accept {
			if err := d.Validate(v); err != nil {
				t.Errorf("%s %q: %v", name, v, err)
			}
		}
		for _, v := range refuse {
			if err := d.Validate(v); err == nil {
				t.Errorf("%s %q accepted", name, v)
			}
		}
	}
	check(ScrubRate, []string{"1MiB", "1048576", "64MiB", "64GiB"}, []string{"", "0", "1048575", "1023KiB", "65GiB", "64MB", "-1MiB", "1.5MiB"})
	check(ScrubInterval, []string{"0", "1d", "24h", "30d", "3650d"}, []string{"", "23h", "3651d", "-1d", "00", "1w", "30 d"})
	if d, _ := tab.Lookup(ScrubRate); d.Default != "64MiB" {
		t.Errorf("scrub.rate's default = %q, want 64MiB", d.Default)
	}
	if d, _ := tab.Lookup(ScrubInterval); d.Default != "30d" {
		t.Errorf("scrub.interval's default = %q, want 30d", d.Default)
	}
	if n, err := ParseScrubRate("64MiB"); err != nil || n != 64<<20 {
		t.Errorf("ParseScrubRate(64MiB) = %d, %v", n, err)
	}
	if d, err := ParseScrubInterval("0"); err != nil || d != 0 {
		t.Errorf("ParseScrubInterval(0) = %v, %v; want zero for never", d, err)
	}
	if d, err := ParseScrubInterval("30d"); err != nil || d != 30*24*time.Hour {
		t.Errorf("ParseScrubInterval(30d) = %v, %v", d, err)
	}
}

// TestArchiveSampleRules guards scrub.archive-sample: an ordinary setting a
// tenant may override, "0%" by default (a presence check alone, which bills
// nothing), a whole percentage from 0% to 100% read as a share from 0 to 1,
// and anything else refused.
func TestArchiveSampleRules(t *testing.T) {
	d, ok := MustBuiltin().Lookup(ScrubArchiveSample)
	if !ok || d.Default != "0%" || !d.TenantOverride || d.Kind() != Ordinary {
		t.Fatalf("scrub.archive-sample: %+v %v", d, ok)
	}
	for v, want := range map[string]float64{"0%": 0, "5%": 0.05, "100%": 1} {
		if got, err := ParseArchiveSample(v); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"", "0", "0.5", "101%", "-1%", "05%", "5 %", "%", "1.5%"} {
		if _, err := ParseArchiveSample(v); err == nil {
			t.Errorf("%q was taken", v)
		}
	}
}
