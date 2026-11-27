package correct

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantNoEdit fails unless the tenant holds no correction and the scan's
// instances are where they were.
func (r *rig) wantNoEdit(t *testing.T, s tenant.CatalogScan, before []tenant.ScanInstance) {
	t.Helper()
	got, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 10})
	if err != nil || len(got) != 0 {
		t.Fatalf("a refused edit left %d corrections (%v)", len(got), err)
	}
	for i, in := range r.held(pathOf(s)) {
		if in.Key != before[i].Key {
			t.Fatalf("a refused edit moved an instance to %s", in.Key)
		}
	}
}

// TestEditRefusesWhatNoEditChanges guards the attributes and values an
// edit never takes, each refused as invalid before anything is recorded:
// the three UIDs the catalog is built on, Pixel Data and what lays out its
// bytes, a private attribute, one of a binary VR, the study's date, and
// values that do not fit their VR (go-dicom's validation) or the default
// repertoire.
func TestEditRefusesWhatNoEditChanges(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	for name, c := range map[string]EditChange{
		"study uid":       {Tag: 0x0020000D, Value: "1.2.3"},
		"series uid":      {Tag: 0x0020000E, Value: "1.2.3"},
		"instance uid":    {Tag: 0x00080018, Value: "1.2.3"},
		"pixel data":      {Tag: 0x7FE00010, Remove: true},
		"rows":            {Tag: 0x00280010, Value: "4"},
		"overlay data":    {Tag: 0x60003000, Remove: true},
		"private":         {Tag: 0x00091010, Value: "x"},
		"binary vr":       {Tag: 0x00181310, Value: "1"},
		"study date":      {Tag: 0x00080020, Value: "20240101"},
		"identity marker": {Tag: 0x00120062, Remove: true},
		"deid method":     {Tag: 0x00120063, Remove: true},
		"dates marker":    {Tag: 0x00280303, Remove: true},
		"integer string":  {Tag: 0x00200011, Value: "twelve"},
		"date":            {Tag: 0x00080022, Value: "2024-13-45"},
		"too long":        {Tag: 0x0008103E, Value: strings.Repeat("x", 65)},
		"not ascii":       {Tag: 0x0008103E, Value: "Série"},
		"removal's value": {Tag: 0x0008103E, Value: "x", Remove: true},
	} {
		_, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: scanTarget(s), Changes: []EditChange{c}})
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: the preview answered %v, want it refused as invalid", name, err)
		}
		_, err = e.Edit(r.ctx, EditRequest{Target: scanTarget(s), Changes: []EditChange{c}, Reason: "a test", Confirm: "c1.1.x", Audit: r.audit()})
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: the edit answered %v, want it refused as invalid", name, err)
		}
	}
	r.wantNoEdit(t, s, before)
}

// TestEditOfOneInstanceRefusesAScanValue guards the values the catalog
// keeps for a whole scan (Series Description, Modality, Series Number): an
// edit of one instance would give the scan's catalog row the value while
// its other instances keep theirs, so it is refused as invalid before
// anything is recorded; another attribute of one instance may be edited.
func TestEditOfOneInstanceRefusesAScanValue(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	one := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label, Instance: before[0].SOPInstanceUID}
	for _, c := range []EditChange{setDescription("Scout view"), {Tag: 0x00080060, Value: "MR"}, {Tag: 0x00200011, Value: "9"}} {
		c, err := r.editNow(e, one, c)
		if fault.KindOf(err) != fault.Invalid {
			sc, _ := r.p.CatalogScan(r.ctx, pathOf(s))
			t.Fatalf("an edit of one instance's scan value answered %s (%v); the scan's catalog row now says %+v", c.State, err, sc.Values)
		}
	}
	r.wantNoEdit(t, s, before)
	if c, err := r.editNow(e, one, EditChange{Tag: 0x00181210, Value: "B30f"}); err != nil || c.State != record.StateCommitted {
		t.Fatalf("an edit of one instance's other attribute = %s, %v", c.State, err)
	}
}

// manyTargets is the tenant's database answering one more instance than an
// edit changes for any target.
type manyTargets struct {
	tenant.Corrections
}

// EditTargets answers tenant.MaxEditInstances+1 instances.
func (m manyTargets) EditTargets(_ context.Context, target tenant.CorrectionTarget, limit int) ([]tenant.EditTarget, error) {
	out := make([]tenant.EditTarget, min(limit, tenant.MaxEditInstances+1))
	for i := range out {
		out[i] = tenant.EditTarget{Storage: "main", Path: target.Path(), UID: "1.2.3." + time.Duration(i).String(), Revision: 1}
	}
	return out, nil
}

// CorrectionEdits implements tenant.Edits.
func (manyTargets) CorrectionEdits(context.Context, string) ([]tenant.EditedInstance, error) {
	return nil, nil
}

// BeginEditUndo implements tenant.Edits.
func (manyTargets) BeginEditUndo(context.Context, string, string, time.Time, master.AuditEntry, tenant.Guard) (tenant.Correction, error) {
	return tenant.Correction{}, errors.New("not here")
}

// UndoEdit implements tenant.Edits.
func (manyTargets) UndoEdit(context.Context, tenant.EditUndo) (tenant.Correction, error) {
	return tenant.Correction{}, errors.New("not here")
}

// CheckEditUndo implements tenant.Edits.
func (manyTargets) CheckEditUndo(context.Context, string, time.Time) error {
	return errors.New("not here")
}

// AbandonEditUndo implements tenant.Edits.
func (manyTargets) AbandonEditUndo(context.Context, tenant.EditUndo) (tenant.Correction, error) {
	return tenant.Correction{}, errors.New("not here")
}

// TestEditRefusesMoreThanItsBound guards the bound of one edit: a target
// of more than tenant.MaxEditInstances instances is refused, before any
// of them is read, as one to edit a scan at a time.
func TestEditRefusesMoreThanItsBound(t *testing.T) {
	r := newRig(t, false)
	j := editJudge(t)
	e := r.engineWith(func(c *Config) { c.Store, c.Judge = manyTargets{r.p}, j })
	_, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: tenant.CorrectionTarget{Subject: "S1"}, Changes: []EditChange{setDescription("x")}})
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "more than 2000 instances") {
		t.Fatalf("an edit of more instances than its bound answered %v", err)
	}
}

// TestEditRefusesWhatTheProfileTakesOut guards the edit of a value the
// tenant's profile removes: the new revision is judged before it is put,
// the edit is abandoned with the check's reason, nothing it wrote is left
// on the storage but its record, and the instances stay where they were.
func TestEditRefusesWhatTheProfileTakesOut(t *testing.T) {
	onBoth(t, testEditRefusesWhatTheProfileTakesOut)
}

// testEditRefusesWhatTheProfileTakesOut is
// TestEditRefusesWhatTheProfileTakesOut on one database.
func testEditRefusesWhatTheProfileTakesOut(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before, objs := r.held(pathOf(s)), r.objects()
	c, err := r.editNow(e, scanTarget(s), EditChange{Tag: 0x00080080, Value: "General Hospital"})
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "InstitutionName (0008,0080) out") {
		t.Fatalf("the edit of a value the profile removes answered %v", err)
	}
	if c.State != record.StateAbandoned || c.StateReason != reasonRefused {
		t.Fatalf("the refused edit is %s (%q), want abandoned as refused", c.State, c.StateReason)
	}
	r.wantUnchanged(objs, r.objects(), "meta/.corrections/")
	for i, in := range r.held(pathOf(s)) {
		if in.Key != before[i].Key {
			t.Fatalf("a refused edit moved an instance to %s", in.Key)
		}
	}
}

// TestEditCheckAfterRefusesARemovedIdentifier guards the check after an
// edit: a series description the profile keeps, set to the Patient ID the
// profile takes out, would put the identifier back into the archive, and
// is refused with the check's hold, the edit abandoned.
func TestEditCheckAfterRefusesARemovedIdentifier(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout 98890234"))
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "the check after holds the edited file (residual_value") {
		t.Fatalf("an edit that puts the Patient ID back answered %v", err)
	}
	if c.State != record.StateAbandoned || c.StateReason != reasonRefused {
		t.Fatalf("the refused edit is %s (%q), want abandoned as refused", c.State, c.StateReason)
	}
	if strings.Contains(err.Error(), "98890234") {
		t.Fatalf("the refusal names the value it found: %v", err)
	}
}

// TestEditRefusedWhileHeld guards a hold: an edit of data a hold covers is
// refused (HELD) and records nothing.
func TestEditRefusedWhileHeld(t *testing.T) { onBoth(t, testEditRefusedWhileHeld) }

// testEditRefusedWhileHeld is TestEditRefusedWhileHeld on one database.
func testEditRefusedWhileHeld(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: s.SubjectLabel}, Reason: "audit",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	_, err = r.editNow(e, scanTarget(s), setDescription("Scout view"))
	var held *tenant.HeldRefusal
	if !errors.As(err, &held) || held.Hold != id {
		t.Fatalf("an edit of held data answered %v, want the hold's refusal", err)
	}
	r.wantNoEdit(t, s, before)
}

// TestEditAskedAgainAnswersTheSameEdit guards a lost answer asked again:
// the same token and changes answer the edit they made, though its target
// changed (each instance at its new revision), and make no other; the same
// token with other changes, or for another target, is refused.
func TestEditAskedAgainAnswersTheSameEdit(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	change := setDescription("Scout view")
	p, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: scanTarget(s), Changes: []EditChange{change}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(target tenant.CorrectionTarget, c EditChange) (tenant.Correction, error) {
		return e.Edit(r.ctx, EditRequest{Target: target, Changes: []EditChange{c}, Reason: "series named wrongly", Confirm: p.Confirm,
			Audit: r.audit()})
	}
	first, err := ask(scanTarget(s), change)
	if err != nil || first.State != record.StateCommitted {
		t.Fatalf("the edit = %s, %v", first.State, err)
	}
	again, err := ask(scanTarget(s), change)
	if err != nil || again.ID != first.ID {
		t.Fatalf("the edit asked again answered %q (%v), want %q", again.ID, err, first.ID)
	}
	if _, err := ask(scanTarget(s), setDescription("Other")); !errors.Is(err, errTokenMismatch) {
		t.Fatalf("the token with other changes answered %v", err)
	}
	other := tenant.CorrectionTarget{Subject: s.SubjectLabel}
	if _, err := ask(other, change); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("the token for another target answered %v", err)
	}
	all, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 10})
	if err != nil || len(all) != 1 {
		t.Fatalf("the tenant holds %d corrections (%v), want the one edit", len(all), err)
	}
}

// TestEditRefusesAStaleToken guards the preview's token: one given for
// other changes, or for the target before it changed, is refused, and so
// is an edit when the server judges none.
func TestEditRefusesAStaleToken(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	p, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: scanTarget(s), Changes: []EditChange{setDescription("Scout view")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Edit(r.ctx, EditRequest{Target: scanTarget(s), Changes: []EditChange{setDescription("Other")}, Reason: "a test",
		Confirm: p.Confirm, Audit: r.audit()})
	if !errors.Is(err, errTokenMismatch) {
		t.Fatalf("an edit carrying another edit's token answered %v", err)
	}
	_, err = r.engine().Edit(r.ctx, EditRequest{Target: scanTarget(s), Changes: []EditChange{setDescription("Scout view")}, Reason: "a test",
		Confirm: p.Confirm, Audit: r.audit()})
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("an edit on an engine that judges none answered %v", err)
	}
	r.wantNoEdit(t, s, before)
}
