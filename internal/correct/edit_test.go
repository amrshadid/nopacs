package correct

import (
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestEditMakesNewRevisionsOnEveryCopy guards an edit of a scan's series
// description through its whole life with a replica: the preview lists
// every instance and the attribute, its old value only when asked; the
// edit gives every instance its next revision on the primary, the catalog
// names them and the new description, the records on both copies name each
// instance with its two checksums, the replicator puts the new revisions
// on the replica, and pydicom reads the new value there and on the
// primary; the undo puts every instance back at its old revision and the
// catalog's description back, removes the new revisions from both copies,
// and pydicom reads the old value again.
func TestEditMakesNewRevisionsOnEveryCopy(t *testing.T) {
	r := newReplicaRig(t)
	e := r.editEngine()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(5)
	path := pathOf(s)
	before := r.held(path)
	target := scanTarget(s)
	change := setDescription("Calcium score")

	hidden, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: target, Changes: []EditChange{change}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden.Instances) != len(before) || hidden.Confirm == "" {
		t.Fatalf("the preview lists %d instances (token %q), want the scan's %d and a token", len(hidden.Instances), hidden.Confirm, len(before))
	}
	for _, in := range hidden.Instances {
		a := in.Attributes[0]
		if !a.Present || !a.Known || a.Old != "" || a.New != "Calcium score" || in.Revision != 1 {
			t.Fatalf("a preview without values answers %+v at revision %d", a, in.Revision)
		}
	}
	shown, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: target, Changes: []EditChange{change}, ShowValues: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := shown.Instances[0].Attributes[0].Old; got != s.Values.SeriesDescription || got == "" {
		t.Fatalf("a preview with values answers the old value %q, want the scan's %q", got, s.Values.SeriesDescription)
	}

	c, err := e.Edit(r.ctx, EditRequest{Target: target, Changes: []EditChange{change}, Reason: "series named wrongly at the scanner",
		Confirm: hidden.Confirm, Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the edit = %s (%q), %v", c.State, c.StateReason, err)
	}
	after := r.held(path)
	var newKeys, oldKeys []string
	for i, in := range after {
		want, _ := layout.RevisionKey(path, in.SOPInstanceUID, 2)
		if in.Key != want || in.SOPInstanceUID != before[i].SOPInstanceUID {
			t.Fatalf("after the edit an instance is at %s, want its revision 2 at %s", in.Key, want)
		}
		newKeys, oldKeys = append(newKeys, in.Key), append(oldKeys, before[i].Key)
	}
	sc, err := r.p.CatalogScan(r.ctx, path)
	if err != nil || sc.Values.SeriesDescription != "Calcium score" {
		t.Fatalf("the catalog's series description after the edit is %q (%v)", sc.Values.SeriesDescription, err)
	}
	rec := r.correctionRecord(c.ID)
	if rec.Kind != record.KindEdit || len(rec.Edits) != len(before) || rec.Edits[0].NewSHA256 == "" || rec.Level != record.LevelScan {
		t.Fatalf("the edit's record is %+v", rec)
	}
	if onCopy, ok := r.recordOn(r.copy, c.ID); !ok || onCopy.State != record.StateCommitted || len(onCopy.Edits) != len(before) {
		t.Fatalf("the replica's record of the edit is %+v (%v)", onCopy, ok)
	}
	r.wantMetaDigest(t, "an edit", path)
	copyObjs := r.copyObjects()
	for i, k := range newKeys {
		if _, ok := copyObjs[k]; !ok {
			t.Fatalf("the replica lacks the new revision %s after the replicator ran", k)
		}
		if _, ok := copyObjs[oldKeys[i]]; !ok {
			t.Fatalf("the replica no longer holds the superseded revision %s before the purge", oldKeys[i])
		}
	}
	files := []string{pathUnder(r.root, newKeys[0]), pathUnder(r.copyRoot, newKeys[len(newKeys)-1])}
	if got := pydicomValues(t, "SeriesDescription", files...); got != nil && !slices.Equal(got, []string{"Calcium score", "Calcium score"}) {
		t.Fatalf("pydicom reads the series description %q of the new revisions", got)
	}
	for i := range newKeys {
		pydicomSameBut(t, pathUnder(r.root, oldKeys[i]), pathUnder(r.root, newKeys[i]), map[string]string{"SeriesDescription": "Calcium score"})
	}

	u, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil)
	if err != nil || u.State != record.StateUndone || !u.Swept {
		t.Fatalf("the undo = %s swept %v, %v", u.State, u.Swept, err)
	}
	for i, in := range r.held(path) {
		if in.Key != before[i].Key || in.SHA256 != before[i].SHA256 {
			t.Fatalf("after the undo an instance is at %s, want its old revision at %s", in.Key, before[i].Key)
		}
	}
	if sc, err := r.p.CatalogScan(r.ctx, path); err != nil || sc.Values.SeriesDescription != s.Values.SeriesDescription {
		t.Fatalf("the catalog's series description after the undo is %q, want %q (%v)", sc.Values.SeriesDescription, s.Values.SeriesDescription, err)
	}
	r.wantMetaDigest(t, "an edit's undo", path)
	if rec := r.correctionRecord(c.ID); rec.State != record.StateUndone {
		t.Fatalf("the edit's record after the undo is %s", rec.State)
	}
	main, copyObjs := r.objects(), r.copyObjects()
	for _, k := range newKeys {
		if _, ok := main[k]; ok {
			t.Errorf("the primary keeps the undone revision %s", k)
		}
		if _, ok := copyObjs[k]; ok {
			t.Errorf("the replica keeps the undone revision %s", k)
		}
	}
	if got := pydicomValues(t, "SeriesDescription", pathUnder(r.root, oldKeys[0]), pathUnder(r.copyRoot, oldKeys[0])); got != nil &&
		(got[0] != s.Values.SeriesDescription || got[1] != s.Values.SeriesDescription) {
		t.Fatalf("pydicom reads %q after the undo, want the old %q", got, s.Values.SeriesDescription)
	}
}

// TestEditPurgeRemovesTheSupersededRevisionEverywhere guards an edit of
// two values and a removal, and its purge: pydicom reads each new revision
// as its old one with only those changes; each superseded revision is
// removed from the primary and the replica, the new revisions stay, the
// scan reads the new value, and the edit ends purged with its records
// saying so on both copies.
func TestEditPurgeRemovesTheSupersededRevisionEverywhere(t *testing.T) {
	r := newReplicaRig(t)
	e := r.editEngine()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(2)
	if s.Instances != 2 {
		s = r.scans()[0]
	}
	path := pathOf(s)
	before := r.held(path)
	c, err := r.editNow(e, scanTarget(s), setDescription("Localizer"), EditChange{Tag: 0x00200011, Value: "41"},
		EditChange{Tag: 0x00181210, Remove: true})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the edit = %s (%q), %v", c.State, c.StateReason, err)
	}
	r.replicate()
	if sc, _ := r.p.CatalogScan(r.ctx, path); sc.Values.SeriesNumber != 41 || !sc.Values.HasSeriesNumber {
		t.Fatalf("the catalog's series number after the edit is %d (%v), want 41", sc.Values.SeriesNumber, sc.Values.HasSeriesNumber)
	}
	for i, in := range r.held(path) {
		pydicomSameBut(t, pathUnder(r.root, before[i].Key), pathUnder(r.root, in.Key), map[string]string{"SeriesDescription": "Localizer",
			"SeriesNumber": "41"}, "ConvolutionKernel")
	}
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil || p.State != record.StatePurged {
		t.Fatalf("the edit's purge = %s, %v", p.State, err)
	}
	main, copyObjs := r.objects(), r.copyObjects()
	for _, in := range before {
		if _, ok := main[in.Key]; ok {
			t.Errorf("the primary keeps the superseded revision %s after the purge", in.Key)
		}
		if _, ok := copyObjs[in.Key]; ok {
			t.Errorf("the replica keeps the superseded revision %s after the purge", in.Key)
		}
	}
	var keys []string
	for _, in := range r.held(path) {
		if _, ok := main[in.Key]; !ok || !strings.HasSuffix(in.Key, ".r2.dcm") {
			t.Fatalf("after the purge the current revision %s is not on the primary", in.Key)
		}
		if _, ok := copyObjs[in.Key]; !ok {
			t.Fatalf("after the purge the current revision %s is not on the replica", in.Key)
		}
		keys = append(keys, pathUnder(r.copyRoot, in.Key))
	}
	if d := disposition(p, "copy"); d.Removed != int64(len(before)) {
		t.Errorf("the replica's disposition is %+v, want its %d superseded revisions removed", d, len(before))
	}
	if rec, ok := r.recordOn(r.copy, c.ID); !ok || rec.State != record.StatePurged {
		t.Errorf("the replica's record after the purge is %+v (%v)", rec, ok)
	}
	if got := pydicomValues(t, "SeriesNumber", keys...); got != nil && got[0] != "41" {
		t.Fatalf("pydicom reads the series number %q on the replica after the purge", got)
	}
}

// TestEditUndoneAfterAStopIsFinished guards an undo stopped after its
// manifests and metadata were written and before its publish: the edit is
// still committed, being undone, and the next start finishes it, the old
// revisions in use and the new ones removed.
func TestEditUndoneAfterAStopIsFinished(t *testing.T) { onBoth(t, testEditUndoneAfterAStop) }

// testEditUndoneAfterAStop is TestEditUndoneAfterAStopIsFinished on one
// database.
func testEditUndoneAfterAStop(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	e := r.editEngine()
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(crashAt("undo-meta"))
	if _, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); err == nil {
		t.Fatal("an undo stopped part way was answered without an error")
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.StateReason != tenant.StateReasonUndoing {
		t.Fatalf("the stopped undo left the edit %s (%q)", got.State, got.StateReason)
	}
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.state(c.ID)
	if got.State != record.StateUndone || !got.Swept {
		t.Fatalf("after the start the edit is %s swept %v", got.State, got.Swept)
	}
	objs := r.objects()
	for i, in := range r.held(pathOf(s)) {
		if in.Key != before[i].Key {
			t.Fatalf("after the start an instance is at %s, want %s", in.Key, before[i].Key)
		}
		if k, _ := layout.RevisionKey(pathOf(s), in.SOPInstanceUID, 2); objs[k] != [32]byte{} {
			t.Fatalf("the undone revision %s is still on the storage", k)
		}
	}
}
