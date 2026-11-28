package settings

import (
	"testing"
	"time"
)

// TestReceiverStartLimit guards receiver.start-limit: built in, 15m by
// default, a tenant's value set by the installation's admin; its bounds, 1m
// and 24h, taken, and a value outside them or not a duration refused.
func TestReceiverStartLimit(t *testing.T) {
	tab := MustBuiltin()
	d, ok := tab.Lookup(ReceiverStartLimit)
	if !ok || d.Default != "15m" || !d.TenantOverride || !d.TenantSetByInstallation || d.TenantOnly {
		t.Fatalf("the definition: %+v %v", d, ok)
	}
	for v, want := range map[string]time.Duration{"1m": time.Minute, "15m": 15 * time.Minute, "24h": 24 * time.Hour, "1d": 24 * time.Hour} {
		if got, err := ParseStartLimit(v); err != nil || got != want {
			t.Errorf("%s: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"59s", "25h", "2d", "0", "", "15 minutes", "-1m"} {
		if _, err := ParseStartLimit(v); err == nil {
			t.Errorf("%q was taken", v)
		}
	}
}
