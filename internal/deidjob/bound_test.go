package deidjob

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestDryRunTakesFromTheMemoryBound guards that a dry run's instance takes
// its copies from the server's bound on de-identification's memory
// (Config.Memory): under a bound smaller than the file's copies, the
// instance is reported as a file the bound would hold (deid_failed,
// check_too_large), as a trial run and a reception answer, never judged
// outside the bound; and nothing stays taken from the bound after.
func TestDryRunTakesFromTheMemoryBound(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.archive("ses-1", scanFiles{label: "mr", files: [][]byte{corpus(t, "MR_small.dcm")}})
	bound := check.NewBudget(func(context.Context) (int64, error) { return 1024, nil }, time.Second)
	cfg := f.config()
	cfg.Memory = bound
	s := f.start(cfg)
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.Counts.InstancesRefused != 1 || j.Counts.InstancesSent != 0 {
		t.Fatalf("a dry run under a bound smaller than its file ended %s with %+v", j.State, j.Counts)
	}
	for _, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureWouldHold || x.Detail != "deid_failed/check_too_large" {
			t.Errorf("the instance past the bound was reported %s %q", x.Code, x.Detail)
		}
	}
	if n := f.tallies(j.ID)["hold|deid_failed|check_too_large"]; n != 1 {
		t.Errorf("the hold past the bound is tallied %d times: %v", n, f.tallies(j.ID))
	}
	if n := bound.Used(); n != 0 {
		t.Errorf("%d bytes stay taken from the bound", n)
	}
}
