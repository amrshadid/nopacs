package settings

import "testing"

// TestReadSettingsRules guards the bounds of read.per-caller (1 to 1024) and
// read.per-server (16 to 65536) at each edge and one past it, their
// defaults, and that both are installation-only.
func TestReadSettingsRules(t *testing.T) {
	tab := MustBuiltin()
	check := func(name, def string, accept, refuse []string) {
		t.Helper()
		d, ok := tab.Lookup(name)
		if !ok {
			t.Fatalf("%s is not built in", name)
		}
		if d.Default != def {
			t.Errorf("%s defaults to %q, want %q", name, d.Default, def)
		}
		if d.Kind() != InstallationOnly {
			t.Errorf("%s is %v, want installation-only", name, d.Kind())
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
	check(ReadPerCaller, "16", []string{"1", "16", "1024"}, []string{"0", "1025", "", "-1", "016", "1.5", " 8", "many"})
	check(ReadPerServer, "256", []string{"16", "256", "65536"}, []string{"15", "65537", "", "0", "0256", "1e3"})
}
