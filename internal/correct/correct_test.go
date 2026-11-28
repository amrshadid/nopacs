package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestDeleteSubjectThenPurge guards a subject's delete end to end: hidden
// from the catalog at once with its bytes kept for the grace, its record on
// the storage; after the purge no key under its labels on the storage,
// every other object byte for byte as it was, each removed object recorded
// removed; and the study sent again is held as was_deleted, naming the
// delete.
func TestDeleteSubjectThenPurge(t *testing.T) { onBoth(t, deleteSubjectThenPurge) }

// deleteSubjectThenPurge is TestDeleteSubjectThenPurge on one driver.
func deleteSubjectThenPurge(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles...)
	e := r.engine()
	subject := r.scanWith(2).SubjectLabel
	var instances int64
	for _, s := range r.scans() {
		if s.SubjectLabel == subject {
			instances += s.Instances
		}
	}
	before := r.objects()
	gone := []string{"data/" + subject + "/", "meta/" + subject + "/"}
	if len(under(before, gone...)) == 0 {
		t.Fatalf("the storage holds nothing of %s", subject)
	}
	pv, err := e.Preview(r.ctx, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Purges) != 1 || pv.Purges[0].Storage != "main" || pv.Purges[0].Expected != blob.Removed || pv.Counts.Instances != instances ||
		!pv.PurgeAfter.After(pv.Issued) {
		t.Fatalf("the preview is %+v", pv)
	}
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != record.StateCommitted {
		t.Fatalf("a delete answered %s", c.State)
	}
	if _, err := r.p.CatalogSubject(r.ctx, subject); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a deleted subject is read: %v", err)
	}
	if rec := r.correctionRecord(c.ID); rec.State != record.StateCommitted || rec.Level != record.LevelSubject || len(rec.Scans) == 0 {
		t.Fatalf("the record is %+v", rec)
	}
	r.wantUnchanged(before, r.objects())
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	after := r.objects()
	if left := under(after, gone...); len(left) != 0 {
		t.Fatalf("after the purge the storage holds %q", left)
	}
	r.wantUnchanged(before, after, gone...)
	removed := int64(len(under(before, gone...)))
	if p.State != record.StatePurged || p.Counts.ObjectsRemoved != removed || len(p.Dispositions) != 1 ||
		p.Dispositions[0].Removed != removed || p.Dispositions[0].Hidden+p.Dispositions[0].Unknown+p.Dispositions[0].Retained != 0 {
		t.Fatalf("the purged delete is %+v, want %d objects removed", p, removed)
	}
	rec := r.correctionRecord(c.ID)
	if rec.State != record.StatePurged || rec.Purge == nil || rec.Purge.Removed != removed {
		t.Fatalf("the purged record is %+v", rec)
	}
	r.archive(studyFiles...)
	q, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{Reason: tenant.QuarantineWasDeleted}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(q)) != instances {
		t.Fatalf("%d files of the subject sent again were held as was_deleted, want its %d", len(q), instances)
	}
	if got := r.receptionsByState(); got[reception.Quarantined] == 0 {
		t.Fatalf("no reception of the study sent again ended quarantined: %v", got)
	}
	for _, f := range q {
		if f.Details["correction"] != c.ID {
			t.Fatalf("a file held as was_deleted names %q, want %s", f.Details["correction"], c.ID)
		}
	}
	if _, err := r.p.CatalogSubject(r.ctx, subject); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the subject sent again is in the catalog: %v", err)
	}
}

// TestInstanceDeleteKeepsItsScan guards an instance's delete: its scan's
// other instances stay listed throughout; after the purge the scan's
// manifest and metadata on the storage list exactly them, the catalog holds
// the new manifest's checksum, and every other object is as it was.
func TestInstanceDeleteKeepsItsScan(t *testing.T) { onBoth(t, instanceDeleteKeepsItsScan) }

// instanceDeleteKeepsItsScan is TestInstanceDeleteKeepsItsScan on one
// driver.
func instanceDeleteKeepsItsScan(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles...)
	e := r.engine()
	s := r.scanWith(2)
	path := pathOf(s)
	ins := r.held(path)
	victim := ins[0]
	before := r.objects()
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan, Instance: victim.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.held(path); len(got) != len(ins)-1 {
		t.Fatalf("after the publish the scan lists %d instances, want %d", len(got), len(ins)-1)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(path)
	sk, _ := layout.MetadataKey(path)
	raw, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != len(ins)-1 {
		t.Fatalf("the new manifest lists %d files, want %d", len(m.Files), len(ins)-1)
	}
	for _, f := range m.Files {
		if f.SOPInstanceUID == victim.SOPInstanceUID {
			t.Fatal("the new manifest lists the deleted instance")
		}
	}
	rc, err := r.storage.Read(r.ctx, sk, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	rd := sidecar.NewReader(rc)
	lines := 0
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if in.SOPInstanceUID == victim.SOPInstanceUID {
			t.Fatal("the new metadata lists the deleted instance")
		}
		lines++
	}
	_ = rc.Close()
	if lines != len(ins)-1 {
		t.Fatalf("the new metadata lists %d instances", lines)
	}
	sc, err := r.p.CatalogScan(r.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ManifestSHA256 != tenant.SHA256(sha256.Sum256(raw)) {
		t.Fatal("the catalog does not hold the new manifest's checksum")
	}
	after := r.objects()
	if _, ok := after[victim.Key]; ok {
		t.Fatal("the deleted instance's object is still stored")
	}
	r.wantUnchanged(before, after, victim.Key, mk, sk)
}

// TestReplaceClearsTheDamageMark guards a replace from a quarantined file
// for an instance a scrub marked damaged: the new bytes at the revision 2
// key, the old object kept until the purge, the manifest and metadata
// naming the new key, the damage mark cleared; ResolveInstances answers
// revision 1 superseded; the purge removes the old object only.
func TestReplaceClearsTheDamageMark(t *testing.T) { onBoth(t, replaceClearsTheDamageMark) }

// replaceClearsTheDamageMark is TestReplaceClearsTheDamageMark on one
// driver.
func replaceClearsTheDamageMark(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles...)
	e := r.engine()
	s := r.scanWith(2)
	path := pathOf(s)
	in := r.held(path)[0]
	r.markDamaged(path, in)
	if marks, err := r.p.DamagedInstances(r.ctx, []string{in.SOPInstanceUID}); err != nil || len(marks) != 1 {
		t.Fatalf("the instance is not marked: %v, %v", marks, err)
	}
	old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0) // the corrected file a sender sent again
	q := r.holdDifferent(s, in, corrected)
	line := r.lineFor(path, in.SOPInstanceUID)
	c, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: line,
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	if c.State != record.StateCommitted || c.NewRevision != 2 {
		t.Fatalf("a replace answered %+v", c)
	}
	key, _ := layout.RevisionKey(path, in.SOPInstanceUID, 2)
	h, err := r.p.HeldInstance(r.ctx, path, in.SOPInstanceUID)
	if err != nil || h.Key != key || h.Size != int64(len(corrected)) {
		t.Fatalf("the replaced instance is %+v, %v; want %s", h, err, key)
	}
	if marks, _ := r.p.DamagedInstances(r.ctx, []string{in.SOPInstanceUID}); len(marks) != 0 {
		t.Fatal("the replace left the damage mark")
	}
	objs := r.objects()
	if objs[key] != sha256.Sum256(corrected) || objs[in.Key] != sha256.Sum256(old) {
		t.Fatal("the new revision is not stored beside the old")
	}
	if got := r.lineFor(path, in.SOPInstanceUID); got.Key != key || got.Revision != 2 {
		t.Fatalf("the metadata's line is %+v, want the new key", got)
	}
	res, err := r.p.ResolveInstances(r.ctx, []tenant.InstanceRef{{UID: in.SOPInstanceUID, Revision: 1}, {UID: in.SOPInstanceUID, Revision: 2}})
	if err != nil || res[0].Disposition != tenant.ResolvedSuperseded || res[1].Disposition != tenant.ResolvedCurrent {
		t.Fatalf("resolve answers %+v, %v", res, err)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	objs = r.objects()
	if _, ok := objs[in.Key]; ok || objs[key] != sha256.Sum256(corrected) {
		t.Fatal("the purge did not remove the old revision alone")
	}
}

// TestRelabelMovesTheTree guards a relabel: nothing moves for readers
// while it copies; the publish moves the whole subject; every object is at
// its key under the new label with the same bytes; the old tree stays until
// the purge, which removes it alone.
func TestRelabelMovesTheTree(t *testing.T) { onBoth(t, relabelMovesTheTree) }

// relabelMovesTheTree is TestRelabelMovesTheTree on one driver.
func relabelMovesTheTree(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles...)
	e := r.engine()
	subject := r.scanWith(2).SubjectLabel
	before := r.objects()
	c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "renamed-1",
		Reason: "wrong subject id at the scanner", Audit: r.audit()})
	if err != nil || c.State != record.StatePrepared {
		t.Fatalf("Relabel = %+v, %v", c, err)
	}
	if _, err := r.p.CatalogSubject(r.ctx, subject); err != nil {
		t.Fatalf("a prepared relabel moved its subject: %v", err)
	}
	c, err = e.ContinueRelabel(r.ctx, c.ID)
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("ContinueRelabel = %+v, %v", c, err)
	}
	if _, err := r.p.CatalogSubject(r.ctx, "renamed-1"); err != nil {
		t.Fatal(err)
	}
	after := r.objects()
	oldTree := under(before, "data/"+subject+"/", "meta/"+subject+"/")
	for _, k := range oldTree {
		nk := "data/renamed-1/" + k[len("data/"+subject+"/"):]
		if k[:5] == "meta/" {
			nk = "meta/renamed-1/" + k[len("meta/"+subject+"/"):]
		}
		if _, ok := after[nk]; !ok {
			t.Fatalf("%s has no copy at %s", k, nk)
		}
		if k[:5] == "data/" && after[nk] != before[k] {
			t.Fatalf("the copy of %s differs", k)
		}
	}
	for _, s := range r.scans() {
		if s.SubjectLabel == "renamed-1" {
			for _, in := range r.held(pathOf(s)) {
				if _, ok := after[in.Key]; !ok {
					t.Fatalf("the catalog names %s, which is not stored", in.Key)
				}
			}
		}
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	purged := r.objects()
	if left := under(purged, "data/"+subject+"/", "meta/"+subject+"/"); len(left) != 0 {
		t.Fatalf("after the purge the old tree holds %q", left)
	}
	r.wantUnchanged(after, purged, "data/"+subject+"/", "meta/"+subject+"/", layout.CorrectionsPrefix)
}

// markDamaged records, as a scrub's listing unit would, the finding that
// the instance is damaged, which marks it.
func (r *rig) markDamaged(p label.Path, in tenant.ScanInstance) {
	r.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	j, err := r.p.CreateJob(ctx, tenant.NewJob{ID: "scrub0000001", Kind: tenant.JobScrub, Created: now}, r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	w, err := r.p.ClaimWork(ctx, tenant.ScrubWorkKind, "test", now.Add(time.Second), time.Minute)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.p.StartJobUnit(ctx, w.Fence(), now.Add(time.Second)); err != nil {
		r.t.Fatal(err)
	}
	size, found := in.Size, in.Size-1
	f := tenant.ScrubFinding{ID: tenant.ScrubFindingID(j.ID, "main", in.Key, tenant.FindingDamaged), Kind: tenant.FindingDamaged,
		Storage: "main", Key: in.Key, Path: p, SOPInstanceUID: in.SOPInstanceUID, Revision: 1, ExpectedSize: &size, FoundSize: &found,
		ExpectedSHA256: in.ArchivedSHA256, FoundSHA256: tenant.SHA256{1}}
	if _, _, err := r.p.RecordScrubFinding(ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: now.Add(time.Second)}); err != nil {
		r.t.Fatal(err)
	}
}

// holdDifferent holds b as a different file of in's UID, from in's
// reception, and returns its id.
func (r *rig) holdDifferent(s tenant.CatalogScan, in tenant.ScanInstance, b []byte) string {
	r.t.Helper()
	recv, _, err := r.p.Reception(r.ctx, in.ReceptionID)
	if err != nil {
		r.t.Fatal(err)
	}
	q := tenant.QuarantinedFile{ID: "qreplace0001", Reason: tenant.QuarantineDifferentFile, Receiver: "dimse", ReceptionID: in.ReceptionID,
		Size: int64(len(b)), SHA256: tenant.SHA256(sha256.Sum256(b)), KeptSHA256: in.SHA256, SOPClassUID: "1.2.840.10008.5.1.4.1.1.2",
		SOPInstanceUID: in.SOPInstanceUID, StudyUID: recv.StudyUID, SeriesUID: s.SeriesUID, TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Incoming: "quarantine/qreplace0001", ContentKept: true, Quarantined: time.Now().UTC().Truncate(time.Microsecond)}
	if _, err := r.p.HoldFiles(r.ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		r.t.Fatal(err)
	}
	return q.ID
}

// lineFor returns the instance's line of the scan's metadata.
func (r *rig) lineFor(p label.Path, uid string) sidecar.Instance {
	r.t.Helper()
	b, err := lineOf(r.ctx, r.storage, p, uid)
	if err != nil {
		r.t.Fatal(err)
	}
	var in sidecar.Instance
	if err := json.Unmarshal(b, &in); err != nil {
		r.t.Fatal(err)
	}
	return in
}
