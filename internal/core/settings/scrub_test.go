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
