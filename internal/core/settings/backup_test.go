package settings

import (
	"testing"
	"time"
)

// TestBackupMaxDuration guards backup.max-duration: built in, installation
// only, 6h by default; its bounds, 1m and 7d, taken, and a value outside
// them or not a duration refused.
func TestBackupMaxDuration(t *testing.T) {
	tab := MustBuiltin()
	d, ok := tab.Lookup(BackupMaxDuration)
	if !ok || d.Default != "6h" || d.TenantOverride {
		t.Fatalf("the definition: %+v %v", d, ok)
	}
	for v, want := range map[string]time.Duration{"1m": time.Minute, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := ParseBackupMaxDuration(v); err != nil || got != want {
			t.Errorf("%s: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"59s", "8d", "0", "", "6 hours", "-1h"} {
		if _, err := ParseBackupMaxDuration(v); err == nil {
			t.Errorf("%q was taken", v)
		}
	}
}
