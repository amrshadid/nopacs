package tenant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// apply returns c with d added, as a store adds it to a unit's or a job's
// counts (the skip or quarantine counted under code or reason).
func apply(c tenant.ImportCounts, d tenant.ImportDelta, code tenant.ImportSkipCode, reason tenant.QuarantineReason) tenant.ImportCounts {
	c.Found += d.Found
	c.HandedIn += d.HandedIn
	c.Archived += d.Archived
	c.AlreadyHeld += d.AlreadyHeld
	c.Lost += d.Lost
	c.Failed += d.Failed
	c.Discarded += d.Discarded
	c.Processing += d.Processing
	c.BytesHandedIn += d.Bytes
	if d.Skipped != 0 {
		c.Skipped = map[tenant.ImportSkipCode]int64{code: c.Skipped[code] + d.Skipped}
	}
	if d.Quarantined != 0 {
		c.Quarantined = map[tenant.QuarantineReason]int64{reason: c.Quarantined[reason] + d.Quarantined}
	}
	return c
}

// TestImportDeltasReconcile guards the change each event of an import makes
// to the counts: a skip, a file handed in, one held as it arrived, and
// every outcome a file can then reach (and a failed one's discard or
// retry) keep both identities of the counts, so a store that adds them in
// the transaction of each event never answers counts that do not
// reconcile.
func TestImportDeltasReconcile(t *testing.T) {
	type step struct {
		name string
		d    tenant.ImportDelta
	}
	var c tenant.ImportCounts
	steps := []step{
		{"skip", tenant.ImportSkipDelta()},
		{"held as it arrived", tenant.ImportArrivalDelta(300, tenant.ImportOutcomeQuarantined)},
		{"lost as it arrived", tenant.ImportArrivalDelta(300, tenant.ImportOutcomeLost)},
	}
	for _, o := range []tenant.ImportOutcome{tenant.ImportOutcomeArchived, tenant.ImportOutcomeAlreadyHeld,
		tenant.ImportOutcomeQuarantined, tenant.ImportOutcomeLost, tenant.ImportOutcomeFailed} {
		steps = append(steps, step{"handed in", tenant.ImportArrivalDelta(10, "")}, step{string(o), o.Delta()})
	}
	steps = append(steps, step{"retried", tenant.ImportOutcomeRetried.Delta()},
		step{"failed again", tenant.ImportOutcomeFailed.Delta()}, step{"discarded", tenant.ImportOutcomeDiscarded.Delta()})
	for _, s := range steps {
		c = apply(c, s.d, tenant.SkipNotPart10, tenant.QuarantineBadLabel)
		if r := c.Reconcile(); r != "" {
			t.Fatalf("after %s the counts %+v do not reconcile: %s", s.name, c, r)
		}
	}
	if c.Found != 8 || c.Processing != 0 || c.Archived != 1 || c.AlreadyHeld != 1 || c.Lost != 2 || c.Discarded != 1 ||
		c.Failed != 0 || c.Quarantined[tenant.QuarantineBadLabel] != 2 {
		t.Errorf("the counts are %+v, want every file decided as its outcomes say", c)
	}
	if (tenant.ImportOutcome("nope")).Delta() != (tenant.ImportDelta{}) {
		t.Error("an outcome not of the set changes the counts")
	}
}

// TestImportOutcomesOfEachDecision guards which outcome each deciding
// transaction gives: a commit archives the first arrival of an instance
// in path order and holds the others as already held; a record of a copy
// gone counts lost with no reason, any other quarantined by its reason; a
// reception done leaves what is undecided already held, a reception failed
// or quarantined leaves it failed, never held, and a state that is no end
// decides nothing; each outcome is listed under its kind, or not at all.
func TestImportOutcomesOfEachDecision(t *testing.T) {
	if got := tenant.ImportCommitOutcomes(3); !slices.Equal(got, []tenant.ImportOutcome{tenant.ImportOutcomeArchived,
		tenant.ImportOutcomeAlreadyHeld, tenant.ImportOutcomeAlreadyHeld}) {
		t.Errorf("ImportCommitOutcomes(3) = %v", got)
	}
	if got := tenant.ImportCommitOutcomes(0); len(got) != 0 {
		t.Errorf("ImportCommitOutcomes(0) = %v", got)
	}
	if o, r := tenant.ImportHeldOutcome(tenant.QuarantineIncomingLost); o != tenant.ImportOutcomeLost || r != "" {
		t.Errorf("a copy gone gives %s %q, want lost with no reason", o, r)
	}
	if o, r := tenant.ImportHeldOutcome(tenant.QuarantineBadLabel); o != tenant.ImportOutcomeQuarantined || r != tenant.QuarantineBadLabel {
		t.Errorf("a bad label gives %s %q, want quarantined bad_label", o, r)
	}
	for s, want := range map[reception.State]tenant.ImportOutcome{
		reception.Done: tenant.ImportOutcomeAlreadyHeld, reception.Failed: tenant.ImportOutcomeFailed,
		reception.Quarantined: tenant.ImportOutcomeFailed, reception.Archived: "", reception.Receiving: "",
	} {
		if got := tenant.ImportEndOutcome(s); got != want {
			t.Errorf("ImportEndOutcome(%s) = %q, want %q", s, got, want)
		}
	}
	for o, want := range map[tenant.ImportOutcome]tenant.ImportFileKind{
		tenant.ImportOutcomeArchived: "", tenant.ImportOutcomeAlreadyHeld: "", tenant.ImportOutcomeRetried: "",
		tenant.ImportOutcomeQuarantined: tenant.ImportFileQuarantined, tenant.ImportOutcomeLost: tenant.ImportFileLost,
		tenant.ImportOutcomeFailed: tenant.ImportFileFailed, tenant.ImportOutcomeDiscarded: tenant.ImportFileDiscarded,
	} {
		if got := o.Listed(); got != want {
			t.Errorf("%s is listed as %q, want %q", o, got, want)
		}
	}
}

// TestImportWalkEnd guards the state a unit's walk ends in: failed for a
// failure whatever else holds, canceled when its job has ended, done when
// nothing is processing, walked otherwise.
func TestImportWalkEnd(t *testing.T) {
	for _, c := range []struct {
		reason    string
		jobEnded  bool
		processes int64
		want      tenant.ImportUnitState
	}{
		{tenant.ImportReasonRootUnavailable, true, 3, tenant.ImportUnitFailed},
		{"", true, 3, tenant.ImportUnitCanceled},
		{"", false, 0, tenant.ImportUnitDone},
		{"", false, 2, tenant.ImportUnitWalked},
	} {
		if got := tenant.ImportWalkEnd(c.reason, c.jobEnded, c.processes); got != c.want {
			t.Errorf("ImportWalkEnd(%q, %v, %d) = %s, want %s", c.reason, c.jobEnded, c.processes, got, c.want)
		}
	}
}

// TestImportSkipPathTooLong guards the rule of a skip's path: a path past
// importpath.MaxPathLength, up to the longest a walk meets, is taken as
// path_too_long only, and a path that fits never as path_too_long; a path
// longer still is refused; a job skipping one ends partial, since the
// entry may be DICOM the archive lacks.
func TestImportSkipPathTooLong(t *testing.T) {
	fence := tenant.WorkFence{ID: "imp-1", Attempt: 1}
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	dir := strings.Repeat(strings.Repeat("d", 200)+"/", 10) + "e"
	long := dir + "/" + strings.Repeat("n", 100)
	if len(dir) > importpath.MaxPathLength || len(long) <= importpath.MaxPathLength {
		t.Fatalf("the fixture's paths are %d and %d bytes", len(dir), len(long))
	}
	for _, c := range []struct {
		path string
		code tenant.ImportSkipCode
		ok   bool
	}{
		{long, tenant.SkipPathTooLong, true},
		{dir, tenant.SkipPathTooLong, false},
		{long, tenant.SkipNotPart10, false},
		{dir, tenant.SkipNotPart10, true},
		{long + strings.Repeat("x", importpath.MaxEntryPathLength), tenant.SkipPathTooLong, false},
	} {
		err := tenant.ImportSkip{Fence: fence, Path: c.path, Code: c.code, Now: now}.Validate()
		if (err == nil) != c.ok {
			t.Errorf("a skip of %d bytes as %s: %v, want taken %v", len(c.path), c.code, err, c.ok)
		}
	}
	if !tenant.SkipPathTooLong.MayHideDICOM() || !(tenant.ImportCounts{Found: 1,
		Skipped: map[tenant.ImportSkipCode]int64{tenant.SkipPathTooLong: 1}}).Partial() {
		t.Error("a job that skipped a path too long does not end partial")
	}
	if importpath.Fitting(long) != dir {
		t.Error("an entry too long is not kept under the directory that holds it")
	}
}

// TestImportSkipBadName guards the rule of a skip whose name breaks the
// rule of a segment: taken as bad_name only, in a folder that follows the
// rule or at the root, never with NUL (a path key's separator) or past its
// bound; a name that follows the rule is not bad_name; the row is kept
// under the folder, never the name; a job skipping one ends partial.
func TestImportSkipBadName(t *testing.T) {
	fence := tenant.WorkFence{ID: "imp-1", Attempt: 1}
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		path string
		code tenant.ImportSkipCode
		ok   bool
	}{
		{"proj1/sessA/ct:9", tenant.SkipBadName, true},
		{"proj1/a\\b", tenant.SkipBadName, true},
		{"proj1/\xff\xfe", tenant.SkipBadName, true},
		{"top:level", tenant.SkipBadName, true},
		{"proj1/" + strings.Repeat("n", 300), tenant.SkipBadName, true},
		{"proj1/sessA/ct:9", tenant.SkipNotRegular, false},
		{"proj1/sessA/ct9", tenant.SkipBadName, false},
		{"pro:j1/sessA", tenant.SkipBadName, false},
		{"proj1/a\x00b", tenant.SkipBadName, false},
		{"proj1/" + strings.Repeat(":", tenant.MaxBadNameLength+1), tenant.SkipBadName, false},
	} {
		err := tenant.ImportSkip{Fence: fence, Path: c.path, Code: c.code, Now: now}.Validate()
		if (err == nil) != c.ok {
			t.Errorf("a skip of %q as %s: %v, want taken %v", c.path, c.code, err, c.ok)
		}
	}
	for p, want := range map[string]string{"proj1/sessA/ct:9": "proj1/sessA", "top:level": ""} {
		if got := (tenant.ImportSkip{Path: p, Code: tenant.SkipBadName}).RowPath(); got != want {
			t.Errorf("a bad name at %q is kept under %q, want %q", p, got, want)
		}
	}
	if !tenant.SkipBadName.MayHideDICOM() {
		t.Error("a job that skipped a bad name does not end partial")
	}
}
