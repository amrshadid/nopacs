package correct

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ctPatientID is the Patient ID of the CT study of ct2n-*.dcm, the
// destination of the tests' moves; the MR files hold another.
const ctPatientID = "98890234"

// TestEditJudgeTakesArchivedIdentity guards the judge of a Patient ID set:
// a person's value is refused, since the profile's floor never keeps a
// Patient ID and its run over the edited file changes it; a value read
// from the tenant's own archived files (FromArchive) is the profile's own
// output and is judged by the profile's hold and the check after alone.
func TestEditJudgeTakesArchivedIdentity(t *testing.T) {
	file, err := os.ReadFile(filepath.Join(corpusDir, studyFiles[0])) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.EditTarget{Profile: editProfile, Receiver: "dimse"}
	j := editJudge(t)
	err = j.JudgeEdit(context.Background(), EditedFile{Target: target, File: file, Set: []uint32{tagPatientID}})
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a person's Patient ID answered %v, want a refusal", err)
	}
	if err := j.JudgeEdit(context.Background(), EditedFile{Target: target, File: file, Set: []uint32{tagPatientID}, FromArchive: true}); err != nil {
		t.Fatalf("a Patient ID read from the archive was refused: %v", err)
	}
}

// TestMoveRewritesIdentity guards a move asked to rewrite identity, under
// the tests' profile: once published, the moved session is edited, each
// instance given a new revision under the destination whose Patient ID is
// the destination's archived one and which holds no issuer (the
// destination's files hold none), nothing else of the data set changed
// (pydicom reads it so, when NOPACS_CHECK_EDIT_PYTHON names it); the
// rewrite is recorded made and is not made again; the move can no longer
// be undone.
func TestMoveRewritesIdentity(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.editEngine()
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, true)
	if err != nil || c.State != record.StateCommitted || !c.RewriteIdentity {
		t.Fatalf("the move = %+v, %v", c, err)
	}
	before := r.held(pathOf(r.scanOf(secondSeriesUID)))
	edits, err := e.RewriteIdentity(r.ctx, c.ID)
	if err != nil || len(edits) != 1 || edits[0].Kind != record.KindEdit || edits[0].State != record.StateCommitted {
		t.Fatalf("the identity rewrite = %+v, %v", edits, err)
	}
	if got := r.state(c.ID); got.RewriteIdentity {
		t.Fatal("the identity rewrite is not recorded made")
	}
	s := r.scanOf(secondSeriesUID)
	got, err := e.identityOf(r.ctx, s.Storage, pathOf(s))
	if err != nil || got.id != ctPatientID || got.hasIssue {
		t.Fatalf("the moved scan's metadata names the identity %+v, %v; want %s and no issuer", got, err, ctPatientID)
	}
	after := r.held(pathOf(s))
	if len(after) != len(before) {
		t.Fatalf("the moved scan holds %d instances after the rewrite, %d before", len(after), len(before))
	}
	var files []string
	for i, in := range after {
		if tenant.RevisionOfKey(in.Key) != tenant.RevisionOfKey(before[i].Key)+1 || in.Key == before[i].Key {
			t.Fatalf("the rewrite gave %s the key %s; it had %s", in.SOPInstanceUID, in.Key, before[i].Key)
		}
		files = append(files, pathUnder(r.root, in.Key))
		pydicomSameBut(t, pathUnder(r.root, before[i].Key), pathUnder(r.root, in.Key), map[string]string{"PatientID": ctPatientID},
			"IssuerOfPatientID")
	}
	if vals := pydicomValues(t, "PatientID", files...); vals != nil && (len(vals) != 1 || vals[0] != ctPatientID) {
		t.Fatalf("pydicom reads the Patient ID of the new revisions as %v, want %s", vals, ctPatientID)
	}
	if again, err := e.RewriteIdentity(r.ctx, c.ID); err != nil || len(again) != 0 {
		t.Fatalf("the rewrite asked again = %+v, %v; want nothing made", again, err)
	}
	if _, err := e.Undo(r.ctx, c.ID, "r", r.audit(), nil); err == nil {
		t.Fatal("a move whose moved session an edit changed since was undone")
	}
}

// TestMoveRewriteAfterAStopBeforeThePublish guards that a move stopped
// while it copied, with the identity rewrite asked, is published by the
// restart's Recover and its rewrite made in the same pass.
func TestMoveRewriteAfterAStopBeforeThePublish(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.editEngine()
	e.SetCrashAt(crashAt("copied"))
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, true)
	if err == nil || c.State != record.StatePrepared {
		t.Fatalf("the move was not stopped before its publish: %+v, %v", c, err)
	}
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.RewriteIdentity {
		t.Fatalf("after the restart the move is %s, rewrite owed %v; want committed with the rewrite made", got.State, got.RewriteIdentity)
	}
	s := r.scanOf(secondSeriesUID)
	if got, err := e.identityOf(r.ctx, s.Storage, pathOf(s)); err != nil || got.id != ctPatientID {
		t.Fatalf("after the restart the moved scan's identity is %+v, %v", got, err)
	}
}

// TestMoveRewriteSurvivesAStop guards that the identity rewrite a move
// asked for is not lost to a stop after its publish: the restart's Recover
// makes it.
func TestMoveRewriteSurvivesAStop(t *testing.T) {
	r := newRig(t, false)
	m := r.archiveForMove()
	e := r.editEngine()
	c, err := r.moveNow(e, tenant.CorrectionTarget{Subject: m.from, Session: m.session}, m.to, true)
	if err != nil || !c.RewriteIdentity {
		t.Fatalf("the move = %+v, %v", c, err)
	}
	restarted := r.editEngine()
	if err := restarted.Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); got.RewriteIdentity {
		t.Fatal("after the restart the identity rewrite is still owed")
	}
	list, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindEdit}}, pageAll)
	if err != nil || len(list) != 1 || list[0].State != record.StateCommitted {
		t.Fatalf("after the restart the edits are %+v, %v", list, err)
	}
	s := r.scanOf(secondSeriesUID)
	if got, err := e.identityOf(r.ctx, s.Storage, pathOf(s)); err != nil || got.id != ctPatientID {
		t.Fatalf("after the restart the moved scan's identity is %+v, %v", got, err)
	}
}
