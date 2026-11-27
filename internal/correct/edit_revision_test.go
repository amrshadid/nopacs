package correct

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// undoneUnswept makes an edit of a scan and undoes it, the undo stopped
// after its publish and before its sweep: the edit is undone and its new
// revisions are still on the storage.
func (r *rig) undoneUnswept(t *testing.T, s tenant.CatalogScan) tenant.Correction {
	t.Helper()
	e := r.editEngine()
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the edit = %s (%q), %v", c.State, c.StateReason, err)
	}
	e.SetCrashAt(crashAt("undone"))
	if _, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); err == nil {
		t.Fatal("an undo stopped before its sweep was answered without an error")
	}
	got := r.state(c.ID)
	if got.State != record.StateUndone || got.Swept {
		t.Fatalf("the stopped undo left the edit %s swept %v, want undone and not swept", got.State, got.Swept)
	}
	return got
}

// TestEditRefusedWhileAnUndoneEditIsUnswept guards the overlap of an edit
// with an undone one whose new revisions are not yet removed: the undone
// edit's sweep still removes objects under the scan's labels, so a new
// edit of the scan is refused until the sweep ran, and is made after it.
func TestEditRefusedWhileAnUndoneEditIsUnswept(t *testing.T) {
	onBoth(t, testEditRefusedWhileAnUndoneEditIsUnswept)
}

// testEditRefusedWhileAnUndoneEditIsUnswept is
// TestEditRefusedWhileAnUndoneEditIsUnswept on one database.
func testEditRefusedWhileAnUndoneEditIsUnswept(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	first := r.undoneUnswept(t, s)
	_, err := r.editNow(r.editEngine(), scanTarget(s), setDescription("Localizer"))
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("an edit while an undone edit's sweep is owed answered %v, want a precondition failure", err)
	}
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(first.ID); !got.Swept {
		t.Fatal("the start did not sweep the undone edit")
	}
	if c, err := r.editNow(r.editEngine(), scanTarget(s), setDescription("Localizer")); err != nil || c.State != record.StateCommitted {
		t.Fatalf("an edit after the sweep = %s (%q), %v", c.State, c.StateReason, err)
	}
}

// TestEditAfterAnUndoneEditNumbersAboveIt guards revision numbers: an edit
// made after an undone one gives each instance the revision above the
// undone edit's, never the same one, so no key names two files, and the
// catalog, the records and the storage agree on it.
func TestEditAfterAnUndoneEditNumbersAboveIt(t *testing.T) {
	onBoth(t, testEditAfterAnUndoneEditNumbersAboveIt)
}

// testEditAfterAnUndoneEditNumbersAboveIt is
// TestEditAfterAnUndoneEditNumbersAboveIt on one database.
func testEditAfterAnUndoneEditNumbersAboveIt(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	path := pathOf(s)
	e := r.editEngine()
	first, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil {
		t.Fatal(err)
	}
	if u, err := e.Undo(r.ctx, first.ID, "the description was right", r.audit(), nil); err != nil || !u.Swept {
		t.Fatalf("the undo = %s swept %v, %v", u.State, u.Swept, err)
	}
	c, err := r.editNow(r.editEngine(), scanTarget(s), setDescription("Localizer"))
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the second edit = %s (%q), %v", c.State, c.StateReason, err)
	}
	objs := r.objects()
	for _, in := range r.held(path) {
		want, _ := layout.RevisionKey(path, in.SOPInstanceUID, 3)
		if in.Key != want {
			t.Fatalf("after an undone edit to revision 2 the second edit put an instance at %s, want %s", in.Key, want)
		}
		if _, ok := objs[in.Key]; !ok {
			t.Fatalf("the current revision %s is not on the storage", in.Key)
		}
	}
	for _, x := range r.correctionRecord(c.ID).Edits {
		if x.OldRevision != 1 || x.NewRevision != 3 {
			t.Fatalf("the second edit's record names revision %d to %d, want 1 to 3", x.OldRevision, x.NewRevision)
		}
	}
}

// TestEditSweepLeavesBytesItDidNotWrite guards the sweep of an undone
// edit: it removes a new revision's key only when the object there holds
// the bytes the edit's publish recorded. An object of other bytes at that
// key is not the edit's, and stays.
func TestEditSweepLeavesBytesItDidNotWrite(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	c := r.undoneUnswept(t, s)
	other := []byte("bytes the undone edit did not write")
	key, err := layout.RevisionKey(c.Edits[0].Path, c.Edits[0].UID, c.Edits[0].NewRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.storage.Put(r.ctx, key, bytes.NewReader(other)); err != nil {
		t.Fatal(err)
	}
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); !got.Swept {
		t.Fatal("the start did not sweep the undone edit")
	}
	objs := r.objects()
	if objs[key] != sha256.Sum256(other) {
		t.Fatalf("the sweep removed %s, whose bytes are not those the edit wrote", key)
	}
	for _, in := range c.Edits[1:] {
		k, _ := layout.RevisionKey(in.Path, in.UID, in.NewRevision)
		if _, ok := objs[k]; ok {
			t.Fatalf("the sweep left the undone revision %s, whose bytes are the edit's", k)
		}
	}
}
