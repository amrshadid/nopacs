package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testProfileTrials guards the record of change control: a version never
// tried is NotFound, a trial of a version the tenant lacks is refused, a
// recorded trial reads back with its counts, a second trial of the same
// receiver and version replaces the first and another receiver's is its
// own, and counts that do not add up are invalid.
func testProfileTrials(e *penv) {
	trials, ok := e.p.(tenant.ProfileTrials)
	if !ok {
		e.Fatal("the store keeps no profile trials")
	}
	got, _ := e.create(newVersion("research", "one"), 1)
	ref := got.Ref()
	_, err := trials.ProfileTrial(e.ctx, "dimse", ref)
	e.wantKind("a version never tried", err, fault.NotFound)
	first := tenant.ProfileTrial{Receiver: "dimse", Profile: ref, Tried: at(10), KeyID: "K1234567890a", Sessions: 3, Files: 12, Changed: 5, Held: 1}
	e.wantKind("a trial of an unknown version", trials.RecordProfileTrial(e.ctx,
		tenant.ProfileTrial{Receiver: "dimse", Profile: tenant.ProfileRef{Name: "research", Version: 9}, Tried: at(10)}, e.audit(), nil), fault.NotFound)
	e.ok("RecordProfileTrial", trials.RecordProfileTrial(e.ctx, first, e.audit(), nil))
	read, err := trials.ProfileTrial(e.ctx, "dimse", ref)
	e.ok("ProfileTrial", err)
	if read != first {
		e.Errorf("the trial reads back as %+v, want %+v", read, first)
	}
	second := first
	second.Tried, second.Files, second.Changed, second.Held = at(20), 20, 7, 0
	e.ok("RecordProfileTrial again", trials.RecordProfileTrial(e.ctx, second, e.audit(), nil))
	if read, err = trials.ProfileTrial(e.ctx, "dimse", ref); err != nil || read != second {
		e.Errorf("the second trial reads back as %+v (%v), want it to replace the first", read, err)
	}
	if _, err = trials.ProfileTrial(e.ctx, "stow", ref); fault.KindOf(err) != fault.NotFound {
		e.Errorf("another receiver's trial answered %v, want NotFound", err)
	}
	bad := first
	bad.Changed, bad.Held = 10, 10
	e.wantKind("counts past the files", trials.RecordProfileTrial(e.ctx, bad, e.audit(), nil), fault.Invalid)
}
