package settings

import (
	"testing"
	"time"
)

// TestDeleteGraceRules guards delete.grace: an ordinary setting a tenant
// may override, its default 7d, its bounds at each edge and one past them
// (zero in any unit is taken, 90d is the most), and what it parses to.
func TestDeleteGraceRules(t *testing.T) {
	d, ok := MustBuiltin().Lookup(DeleteGrace)
	if !ok {
		t.Fatal("delete.grace is not built in")
	}
	if d.Kind() != Ordinary || !d.TenantOverride || d.Default != "7d" {
		t.Fatalf("delete.grace is an ordinary setting a tenant may override, default 7d: %+v", d)
	}
	for _, v := range []string{"0d", "0s", "0h", "1s", "36h", "7d", "90d", "2160h"} {
		if err := d.Validate(v); err != nil {
			t.Errorf("delete.grace %q: %v", v, err)
		}
	}
	for _, v := range []string{"", "0", "00d", "91d", "2161h", "-1d", "1w", "7 d", "1.5d"} {
		if err := d.Validate(v); err == nil {
			t.Errorf("delete.grace %q accepted", v)
		}
	}
	for v, want := range map[string]time.Duration{"0d": 0, "0s": 0, "3s": 3 * time.Second, "7d": 7 * 24 * time.Hour, "90d": MaxDeleteGrace} {
		if got, err := ParseDeleteGrace(v); err != nil || got != want {
			t.Errorf("ParseDeleteGrace(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
}
