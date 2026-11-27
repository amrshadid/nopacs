package correct

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantScanWhole fails unless the scan at p is whole on the storage as the
// catalog says: the catalog keeps the digest of the manifest the storage
// holds, the manifest names each instance at the key the catalog holds it
// at, and every key it names is on the storage.
func (r *rig) wantScanWhole(t *testing.T, what string, p label.Path) {
	t.Helper()
	sc, err := r.p.CatalogScan(r.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(p)
	objs := r.objects()
	if sc.ManifestSHA256 != tenant.SHA256(objs[mk]) {
		t.Fatalf("%s: the catalog keeps the manifest digest %s, the storage's manifest is %s", what, sc.ManifestSHA256, tenant.SHA256(objs[mk]))
	}
	raw, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	cur := map[string]string{}
	for _, in := range r.held(p) {
		cur[in.SOPInstanceUID] = in.Key
	}
	if len(m.Files) != len(cur) {
		t.Fatalf("%s: the manifest names %d instances, the catalog holds %d", what, len(m.Files), len(cur))
	}
	for _, f := range m.Files {
		if cur[f.SOPInstanceUID] != f.Key {
			t.Fatalf("%s: the manifest names %s for an instance the catalog holds at %s", what, f.Key, cur[f.SOPInstanceUID])
		}
		if _, ok := objs[f.Key]; !ok {
			t.Fatalf("%s: the manifest names %s, which is not on the storage", what, f.Key)
		}
	}
}

// stoppedUndo makes an edit of a scan and undoes it, the undo stopped
// after it wrote the scans' files naming the old revisions and before its
// publish: the edit is committed and being undone.
func (r *rig) stoppedUndo(t *testing.T, s tenant.CatalogScan) tenant.Correction {
	t.Helper()
	e := r.editEngine()
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the edit = %s (%q), %v", c.State, c.StateReason, err)
	}
	e.SetCrashAt(crashAt("undo-meta"))
	if _, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); err == nil {
		t.Fatal("an undo stopped part way was answered without an error")
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.StateReason != tenant.StateReasonUndoing {
		t.Fatalf("the stopped undo left the edit %s (%q)", got.State, got.StateReason)
	}
	return c
}

// TestEditBeingUndoneIsNotPurged guards an edit whose undo stopped after
// its scans' files named the old revisions: neither an admin's purge nor
// the server's purge of what is due removes the old revisions those files
// name, and the next start finishes the undo, the scan whole.
func TestEditBeingUndoneIsNotPurged(t *testing.T) { onBoth(t, testEditBeingUndoneIsNotPurged) }

// testEditBeingUndoneIsNotPurged is TestEditBeingUndoneIsNotPurged on one
// database.
func testEditBeingUndoneIsNotPurged(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	path := pathOf(s)
	before := r.held(path)
	c := r.stoppedUndo(t, s)
	if p, err := r.editEngine().PurgeNow(r.ctx, c.ID, r.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("an admin's purge of an edit being undone = %s, %v; want a precondition failure", p.State, err)
	}
	j := editJudge(t)
	later := r.engineWith(func(cfg *Config) { cfg.Judge, cfg.Now = j, func() time.Time { return time.Now().Add(72 * time.Hour) } })
	if _, err := later.PurgeDue(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); got.State != record.StateCommitted {
		t.Fatalf("the server's purge took the edit being undone to %s", got.State)
	}
	objs := r.objects()
	for _, in := range before {
		if _, ok := objs[in.Key]; !ok {
			t.Fatalf("a purge removed the old revision %s the stopped undo's files name", in.Key)
		}
	}
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); got.State != record.StateUndone || !got.Swept {
		t.Fatalf("after the start the edit is %s swept %v", got.State, got.Swept)
	}
	r.wantScanWhole(t, "after the undo", path)
}

// TestEditUndoUnderAHoldIsWrittenBack guards an undo that may no longer be
// published: a hold placed over the scan after the undo stopped part way
// (its files naming the old revisions) makes the next start write each
// scan back to name the edit's new revisions, as the catalog still does,
// and abandon the undo, the edit committed and not being undone, with an
// audit entry of the server saying the undo was taken back and why.
func TestEditUndoUnderAHoldIsWrittenBack(t *testing.T) { onBoth(t, testEditUndoUnderAHold) }

// testEditUndoUnderAHold is TestEditUndoUnderAHoldIsWrittenBack on one
// database.
func testEditUndoUnderAHold(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	path := pathOf(s)
	c := r.stoppedUndo(t, s)
	r.placeHold(t, s.SubjectLabel)
	if err := r.editEngine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.StateReason != "" {
		t.Fatalf("after the start under a hold the edit is %s (%q), want committed and its undo abandoned", got.State, got.StateReason)
	}
	r.wantScanWhole(t, "an undo abandoned under a hold", path)
	entries, _, err := r.p.(tenant.Store).ListAudit(r.ctx, master.AuditFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range entries {
		if a.Object == c.ID && a.LocalUser == "server" && strings.HasPrefix(a.Reason, "undo taken back: ") && strings.Contains(a.Reason, "HELD") {
			found = true
		}
	}
	if !found {
		t.Fatal("the start took the undo back and wrote no audit entry of the server saying why")
	}
}

// TestEditUndoRefusedAtItsPublishIsWrittenBack guards an undo whose publish
// is refused after it wrote the scans' files (a hold placed between its
// check and its publish): each scan is written back to name the new
// revisions, the catalog keeping their digests, and the undo is abandoned.
func TestEditUndoRefusedAtItsPublishIsWrittenBack(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	path := pathOf(s)
	e := r.editEngine()
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(func(step string) error {
		if step == "undo-meta" {
			r.placeHold(t, s.SubjectLabel)
		}
		return nil
	})
	if _, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("an undo refused at its publish answered %v", err)
	}
	if got := r.state(c.ID); got.State != record.StateCommitted || got.StateReason != "" {
		t.Fatalf("the refused undo left the edit %s (%q)", got.State, got.StateReason)
	}
	r.wantScanWhole(t, "an undo refused at its publish", path)
}

// TestEditUndoAskedTwiceWritesNothingTheSecondTime guards two runs of one
// edit's undo (a lost answer asked again): both read the edit being
// undone; the first publishes and sweeps it; the second, once it holds the
// scans' locks, finds it no longer being undone and writes nothing, so the
// scans' files name the first revisions the catalog holds, not the swept
// new ones.
func TestEditUndoAskedTwiceWritesNothingTheSecondTime(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	e := r.editEngine()
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil {
		t.Fatal(err)
	}
	ed, err := e.edits()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := ed.BeginEditUndo(r.ctx, c.ID, "the description was right", time.Now(), r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); err != nil || !first.Swept {
		t.Fatalf("the first undo = %s swept %v, %v", first.State, first.Swept, err)
	}
	second, err := e.continueUndo(r.ctx, stale)
	if err != nil || second.State != record.StateUndone {
		t.Fatalf("the second run of the undo = %s, %v; want the undone edit answered", second.State, err)
	}
	r.wantScanWhole(t, "an undo run twice", pathOf(s))
}

// placeHold places a hold over a subject.
func (r *rig) placeHold(t *testing.T, subject string) {
	t.Helper()
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: subject}, Reason: "audit",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
}

// TestEditUndoKeepsScansLockedUntilItsPublish guards an undo against a
// reception of its scan: the scans stay locked from the write of their
// files until the undo is published, so a reception (which builds the
// manifest from the catalog, still naming the new revisions) waits, and
// after both the scan is whole: every instance at its first revision, the
// reception's among them, the catalog keeping the digest of the manifest
// the storage holds.
func TestEditUndoKeepsScansLockedUntilItsPublish(t *testing.T) {
	onBoth(t, testEditUndoKeepsScansLocked)
}

// testEditUndoKeepsScansLocked is TestEditUndoKeepsScansLockedUntilItsPublish
// on one database.
func testEditUndoKeepsScansLocked(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:6]...)
	s := r.scanWith(4)
	path := pathOf(s)
	e := r.editEngine()
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the edit = %s, %v", c.State, err)
	}
	p := r.pipeline()
	defer func() { _ = p.Stop(context.Background()) }()
	e.SetCrashAt(func(step string) error {
		if step == "undo-meta" {
			// The reception runs while the undo is between its write and its
			// publish; it is given time to commit, and must wait instead.
			r.accept(p, studyFiles[6])
			time.Sleep(3 * time.Second)
		}
		return nil
	})
	if u, err := e.Undo(r.ctx, c.ID, "the description was right", r.audit(), nil); err != nil || u.State != record.StateUndone {
		t.Fatalf("the undo = %s, %v", u.State, err)
	}
	r.waitEnded()
	if n := len(r.held(path)); n != 5 {
		t.Fatalf("the scan holds %d instances, want 5", n)
	}
	for _, in := range r.held(path) {
		if plain, _ := layout.InstanceKey(path, in.SOPInstanceUID); in.Key != plain {
			t.Fatalf("after the undo and the reception an instance is at %s, want its first revision", in.Key)
		}
	}
	r.wantScanWhole(t, "an undo with a reception of its scan", path)
}
