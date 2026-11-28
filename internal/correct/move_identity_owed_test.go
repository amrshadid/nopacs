package correct

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantMovedIdentity fails the test unless every scan the move moved holds
// the destination's Patient ID, by its metadata and, when pydicom is named,
// by the files themselves (the current revision of each instance).
func (r *rig) wantMovedIdentity(t *testing.T, e *Engine, id string) {
	t.Helper()
	scans, err := r.p.CorrectionScans(r.ctx, id)
	if err != nil || len(scans) == 0 {
		t.Fatalf("the move's scans = %d, %v", len(scans), err)
	}
	var files []string
	for _, s := range scans {
		got, err := e.identityOf(r.ctx, s.Storage, s.NewPath)
		if err != nil || got.id != ctPatientID {
			t.Fatalf("session %s scan %s holds the identity %+v (%v), want Patient ID %s", s.NewPath.Session, s.NewPath.Scan, got, err, ctPatientID)
		}
		for _, in := range r.held(s.NewPath) {
			files = append(files, pathUnder(r.root, in.Key))
		}
	}
	for _, v := range pydicomValues(t, "PatientID", files...) {
		if v != ctPatientID {
			t.Fatalf("pydicom reads a moved file's Patient ID as %q, want %s", v, ctPatientID)
		}
	}
}

// mergeRig archives two sessions of one subject and another subject, and
// merges the first under the second without making the identity rewrite.
func (r *rig) mergeRig(e *Engine) (tenant.Correction, moveRig) {
	r.t.Helper()
	m := r.archiveForMove()
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from}, m.to, true)
	if err != nil || c.State != record.StateCommitted || !c.RewriteIdentity {
		r.t.Fatalf("the merge = %+v, %v", c, err)
	}
	return c, m
}

// TestMoveRewriteAfterPartialRewrite guards a merge of two sessions whose
// identity rewrite stopped after the first session's edit: the next pass
// decides per session, so the second is rewritten too, and the rewrite is
// recorded made only then. Deciding from the first session alone left the
// second with the source's Patient ID.
func TestMoveRewriteAfterPartialRewrite(t *testing.T) {
	r := newRig(t, false)
	e := r.editEngine()
	c, _ := r.mergeRig(e)
	scans, err := r.p.CorrectionScans(r.ctx, c.ID)
	if err != nil || len(scans) < 2 {
		t.Fatalf("the merge moved %d scans, %v", len(scans), err)
	}
	dest, err := e.destinationIdentity(r.ctx, c.ToLabel, scans)
	if err != nil {
		t.Fatal(err)
	}
	changes := []EditChange{{Tag: tagPatientID, Value: dest.id}, {Tag: tagIssuer, Remove: true}}
	// A stop after the first session's edit committed.
	if _, err := e.editIdentity(r.ctx, c, tenant.CorrectionTarget{Subject: c.ToLabel, Session: scans[0].NewPath.Session}, changes); err != nil {
		t.Fatal(err)
	}
	if !r.state(c.ID).RewriteIdentity {
		t.Fatal("the rewrite is recorded made after one of two sessions")
	}
	edits, err := e.RewriteIdentity(r.ctx, c.ID)
	if err != nil || len(edits) != 1 {
		t.Fatalf("the rewrite after the stop made %d edits, %v; want the one session left", len(edits), err)
	}
	if r.state(c.ID).RewriteIdentity {
		t.Fatal("every session holds the identity and the rewrite is still owed")
	}
	r.wantMovedIdentity(t, e, c.ID)
}

// TestMoveRewriteOwedAfterUnavailable guards that an edit of a session that
// stops unavailable after it was prepared leaves the rewrite owed, and that
// the next pass (the start's Recover that abandons the prepared edit, then
// the pass's RewriteOwed) finishes it.
func TestMoveRewriteOwedAfterUnavailable(t *testing.T) {
	r := newRig(t, false)
	e := r.editEngine()
	c, _ := r.mergeRig(e)
	stopped := r.editEngine()
	stopped.SetCrashAt(func(step string) error {
		if step == "object" {
			return fault.New(fault.Unavailable, "storage does not answer")
		}
		return nil
	})
	if _, err := stopped.RewriteIdentity(r.ctx, c.ID); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("the rewrite with a storage that stops = %v, want unavailable", err)
	}
	if !r.state(c.ID).RewriteIdentity {
		t.Fatal("the rewrite is recorded made though its edit stopped unavailable")
	}
	next := r.editEngine()
	if err := next.Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if err := next.RewriteOwed(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.state(c.ID).RewriteIdentity {
		t.Fatal("the next pass left the rewrite owed")
	}
	r.wantMovedIdentity(t, next, c.ID)
}

// TestMoveRewriteOwedUnderAHold guards that a hold placed on the
// destination after the move was published leaves the identity rewrite
// owed, with no edit recorded, and that the pass finishes it once the hold
// is released.
func TestMoveRewriteOwedUnderAHold(t *testing.T) {
	r := newRig(t, false)
	e := r.editEngine()
	c, m := r.mergeRig(e)
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: m.to}, Reason: "audit",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RewriteIdentity(r.ctx, c.ID); !errors.Is(err, tenant.ErrHeld) {
		t.Fatalf("the rewrite under a hold = %v, want HELD", err)
	}
	if err := e.RewriteOwed(r.ctx); !errors.Is(err, tenant.ErrHeld) {
		t.Fatalf("the pass under a hold = %v, want HELD", err)
	}
	if !r.state(c.ID).RewriteIdentity {
		t.Fatal("the rewrite is recorded made under a hold")
	}
	if list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindEdit}}, pageAll); err != nil || len(list) != 0 {
		t.Fatalf("the edits under a hold are %+v, %v; want none", list, err)
	}
	if _, err := r.p.ReleaseHold(r.ctx, tenant.HoldRelease{ID: id, Reason: "settled", Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.RewriteOwed(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.state(c.ID).RewriteIdentity {
		t.Fatal("the pass left the rewrite owed after the hold was released")
	}
	r.wantMovedIdentity(t, e, c.ID)
}

// refusingJudge refuses every edit, as the profile's hold or the check
// after does a rewritten file it would not keep.
type refusingJudge struct{}

// JudgeEdit implements EditJudge.
func (refusingJudge) JudgeEdit(context.Context, EditedFile) error {
	return fault.New(fault.Precondition, "the check after refuses the new revision")
}

// TestMoveRewriteRefusedForGoodIsRecorded guards that a rewrite whose edits
// the check after refuses is not silently marked made: the move records the
// refusal with its fixed reason (which show and GetCorrection answer), the
// rewrite is owed no more, the pass tries nothing again, and the moved
// instances keep the identity they had.
func TestMoveRewriteRefusedForGoodIsRecorded(t *testing.T) {
	r := newRig(t, false)
	c, _ := r.mergeRig(r.editEngine())
	e := r.engineWith(func(cfg *Config) { cfg.Judge = refusingJudge{} })
	if _, err := e.RewriteIdentity(r.ctx, c.ID); err != nil {
		t.Fatalf("the refused rewrite answered %v; a refusal for good is recorded, not an error", err)
	}
	got := r.state(c.ID)
	if got.RewriteIdentity || got.IdentityRefused != tenant.IdentityRefusedProfile {
		t.Fatalf("the move after a refusal is owed %v, refused for %q; want not owed, refused for %q", got.RewriteIdentity,
			got.IdentityRefused, tenant.IdentityRefusedProfile)
	}
	edits := func() int {
		list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindEdit}}, pageAll)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	before := edits()
	if before == 0 {
		t.Fatal("no edit was tried")
	}
	if err := e.RewriteOwed(r.ctx); err != nil || edits() != before {
		t.Fatalf("the pass after a refusal made %d more edits, %v; want none", edits()-before, err)
	}
	scans, err := r.p.CorrectionScans(r.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := e.identityOf(r.ctx, scans[0].Storage, scans[0].NewPath); err != nil || id.id == ctPatientID {
		t.Fatalf("a refused rewrite changed the identity to %+v, %v", id, err)
	}
}

// sessionRefusingJudge refuses, as the check after does, the edits of the
// instances of one session and judges no others.
type sessionRefusingJudge struct{ session string }

// JudgeEdit implements EditJudge.
func (j sessionRefusingJudge) JudgeEdit(_ context.Context, f EditedFile) error {
	if f.Target.Path.Session == j.session {
		return fault.New(fault.Precondition, "the check after refuses the new revision")
	}
	return nil
}

// TestMoveRewriteDoesNotRetryASettledSession guards the passes that run
// while a merge's identity rewrite stays owed (a hold on one session): the
// session whose edit was refused for good is settled by that edit, so it is
// edited once, not again at every pass. Once the hold is released the other
// session is rewritten and the refusal is recorded.
func TestMoveRewriteDoesNotRetryASettledSession(t *testing.T) {
	r := newRig(t, false)
	c, m := r.mergeRig(r.editEngine())
	scans, err := r.p.CorrectionScans(r.ctx, c.ID)
	if err != nil || len(scans) < 2 {
		t.Fatalf("the merge moved %d scans, %v", len(scans), err)
	}
	refused, held := scans[0].NewPath.Session, scans[len(scans)-1].NewPath.Session
	if refused == held {
		t.Fatalf("the merge moved one session, %s", refused)
	}
	e := r.engineWith(func(cfg *Config) { cfg.Judge = sessionRefusingJudge{session: refused} })
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: m.to, Session: held}, Reason: "audit",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := e.RewriteOwed(r.ctx); !errors.Is(err, tenant.ErrHeld) {
			t.Fatalf("pass %d under a hold = %v, want HELD", i+1, err)
		}
	}
	list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindEdit}}, pageAll)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ed := range list {
		if ed.Target.Session == refused {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d passes made %d edits of the refused session, want exactly one", 4, n)
	}
	if got := r.state(c.ID); !got.RewriteIdentity || got.IdentityRefused != "" {
		t.Fatalf("under a hold the move is owed %v, refused for %q; want owed, nothing recorded", got.RewriteIdentity, got.IdentityRefused)
	}
	if _, err := r.p.ReleaseHold(r.ctx, tenant.HoldRelease{ID: id, Reason: "settled", Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.RewriteOwed(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.state(c.ID)
	if got.RewriteIdentity || got.IdentityRefused != tenant.IdentityRefusedProfile {
		t.Fatalf("after the release the move is owed %v, refused for %q; want not owed, refused for %q", got.RewriteIdentity,
			got.IdentityRefused, tenant.IdentityRefusedProfile)
	}
	for _, s := range scans {
		id, err := e.identityOf(r.ctx, s.Storage, s.NewPath)
		if err != nil {
			t.Fatal(err)
		}
		if (id.id == ctPatientID) != (s.NewPath.Session == held) {
			t.Fatalf("session %s holds the Patient ID %q after the rewrite; the other session's is %s", s.NewPath.Session, id.id, ctPatientID)
		}
	}
}

// TestSessionTooLargeCountsASeriesOnce guards the size a move's identity
// rewrite checks before it edits a session: more instances than one edit
// changes is refused for good, a scan held on two storages counts once, and
// another session's scans do not count.
func TestSessionTooLargeCountsASeriesOnce(t *testing.T) {
	half := int64(tenant.MaxEditInstances/2 + 1)
	scans := []tenant.CorrectionScan{
		{NewPath: label.Path{Subject: "s", Session: "a", Scan: "1"}, SeriesUID: "1", Instances: half},
		{NewPath: label.Path{Subject: "s", Session: "a", Scan: "1"}, SeriesUID: "1", Instances: half, Storage: "replica"},
		{NewPath: label.Path{Subject: "s", Session: "b", Scan: "2"}, SeriesUID: "2", Instances: half},
	}
	if sessionTooLarge(scans, "a") {
		t.Fatal("a scan held twice was counted twice")
	}
	scans = append(scans, tenant.CorrectionScan{NewPath: label.Path{Subject: "s", Session: "a", Scan: "3"}, SeriesUID: "3", Instances: half})
	if !sessionTooLarge(scans, "a") {
		t.Fatal("a session of more instances than one edit changes was not refused")
	}
	if sessionTooLarge(scans, "b") {
		t.Fatal("another session's scans were counted")
	}
}
