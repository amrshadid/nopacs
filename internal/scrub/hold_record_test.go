package scrub

import (
	"bytes"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAHoldRecordIsNotAnOrphan guards that the walk passes over the hold
// records under meta/.holds/ as it does the correction records: a hold
// record is not a scan's object, and a walk that took it for an orphan
// would report it, and could have it cleared, though a hold outlives a lost
// database only by it.
func TestAHoldRecordIsNotAnOrphan(t *testing.T) {
	f := newFixture(t)
	f.archive("scan-a", corpus(t, "MR_small.dcm"))
	key, err := layout.HoldKey("01jd5x7k2m3n4p5q6r7s8t9v0w")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Put(f.ctx, key, bytes.NewReader([]byte("{}\n"))); err != nil {
		t.Fatal(err)
	}
	j := f.scrub(f.config())
	if j.State != tenant.JobDone {
		t.Fatalf("the scrub ended as %+v", j)
	}
	if got := f.findings(true); len(got) != 0 {
		t.Errorf("a hold record made the findings %v", got)
	}
}
