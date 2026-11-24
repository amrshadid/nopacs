package registry

import (
	"context"
	"strings"
	"testing"
)

// counted is a Driver that also says how many files one tenant's store holds.
type counted struct {
	Driver
	n int
}

// FilesPerTenant implements FileCounter.
func (c counted) FilesPerTenant() int { return c.n }

// TestFileLimitWarningUsesTheDriversCount guards that the warning at start
// counts each tenant by what its driver says one tenant holds, not by the
// SQLite figure: three tenants of a driver that says 4 need 3*4+256 = 268,
// where TestFileLimitWarning's driver, which says nothing, is counted as 11
// each (289). A driver that says 0 is counted as the default.
func TestFileLimitWarningUsesTheDriversCount(t *testing.T) {
	for _, tt := range []struct {
		per  int
		want string
	}{{4, "files_needed=268"}, {0, "files_needed=289"}} {
		e := newEnv(t)
		e.seed("a", "b", "c")
		cfg := e.config()
		cfg.Drivers[flakyDriver] = counted{Driver: e.flaky, n: tt.per}
		r := e.newRegistry(cfg)
		r.fileLimit = func() int64 { return 100 }
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if log := e.logBuf.String(); !strings.Contains(log, tt.want) || !strings.Contains(log, "limit=100") {
			t.Errorf("a driver that says %d: want %s in the log:\n%s", tt.per, tt.want, log)
		}
	}
}
