package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPurgeRemovesOnlyWhatItOwns guards that a delete's purge removes the
// objects its publish fixed and nothing that sits at its labels since: a
// series of the same study whose naming rule gives it the deleted scan's
// label is held as a label conflict naming the delete before any put (the
// deleted scan's manifest and metadata stay byte for byte), and the purge
// removes every object of the deleted scan and leaves every other object.
func TestPurgeRemovesOnlyWhatItOwns(t *testing.T) {
	r := newRig(t, false)
	spec := naming.Spec{Subject: "sub-{#PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{Modality}"}
	if _, err := r.p.SetReceiver(r.ctx, tenant.Receiver{Name: "dimse", Naming: &spec}, []tenant.ReceiverField{tenant.FieldNaming}, time.Now(),
		r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.archive("ct2n-6293.dcm", "ct2n-6924.dcm")
	e := r.engine()
	path := pathOf(r.scanWith(2))
	owned := r.held(path)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan})
	if err != nil {
		t.Fatal(err)
	}
	before := r.objects()
	r.archive("ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm")
	q := r.quarantined(tenant.QuarantineLabelConflict)
	if len(q) != 3 {
		t.Fatalf("%d files of the series given the deleted scan's label were held, want 3", len(q))
	}
	for _, f := range q {
		if f.Details["correction"] != c.ID || f.Details["fault"] != tenant.FaultHeldByCorrection {
			t.Fatalf("a held file names %v, want the delete %s", f.Details, c.ID)
		}
	}
	during := r.objects()
	r.wantUnchanged(before, during)
	if len(during) != len(before) {
		t.Fatalf("the held series put %d objects", len(during)-len(before))
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	after := r.objects()
	for _, in := range owned {
		if _, ok := after[in.Key]; ok {
			t.Errorf("the deleted instance's object %s is still stored", in.Key)
		}
	}
	mk, _ := layout.ManifestKey(path)
	sk, _ := layout.MetadataKey(path)
	gone := []string{mk, sk, layout.CorrectionsPrefix}
	for _, in := range owned {
		gone = append(gone, in.Key)
	}
	r.wantUnchanged(before, after, gone...)
}

// TestArrivalsWaitForThePurge guards that a delete's arrivals are allowed
// only once it is purged, one value at a time: before, the request is
// refused and the study sent again is held as was_deleted; after the purge
// and the series allowed, the study sent again is archived whole, every
// object of it stored.
func TestArrivalsWaitForThePurge(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e := r.engine()
	s := r.scanWith(2)
	path := pathOf(s)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan})
	if err != nil {
		t.Fatal(err)
	}
	series := tenant.HeldValue{Kind: tenant.HeldSeries, Value: s.SeriesUID}
	if _, err := r.p.AllowArrivals(r.ctx, c.ID, series, "sent in error", time.Now(), r.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("AllowArrivals before the purge answered %v, want a precondition failure", err)
	}
	r.archive(studyFiles...)
	if q := r.quarantined(tenant.QuarantineWasDeleted); int64(len(q)) != s.Instances {
		t.Fatalf("%d files of the deleted scan sent again were held, want %d", len(q), s.Instances)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.AllowArrivals(r.ctx, c.ID, series, "sent in error", time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.archive(studyFiles...)
	back := r.held(path)
	if int64(len(back)) != s.Instances {
		t.Fatalf("after the purge the scan sent again lists %d instances, want %d", len(back), s.Instances)
	}
	objs := r.objects()
	for _, in := range back {
		if _, ok := objs[in.Key]; !ok {
			t.Errorf("the catalog lists %s, whose object is not stored", in.Key)
		}
	}
}

// TestWithdrawnSubjectLeavesNothing guards that nothing of a withdrawn
// subject stays outside its correction: the study sent again in the grace
// is held as was_deleted with no content and its bytes gone from the
// incoming area; the purge removes those records, gives the subject's
// receptions the correction's id for their labels and study, removes the
// directories that named the subject, and the preview said how many
// records and receptions it would clear.
func TestWithdrawnSubjectLeavesNothing(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	subject := r.scanWith(2).SubjectLabel
	var removed atomic.Int64
	e := r.engineWith(func(c *Config) {
		c.RemoveIncoming = func(_ context.Context, paths []string) error {
			for _, p := range paths {
				if err := os.Remove(filepath.Join(r.dataDir, "incoming", filepath.FromSlash(p))); err == nil {
					removed.Add(1)
				}
			}
			return nil
		}
	})
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	r.archive(studyFiles...)
	q := r.quarantined(tenant.QuarantineWasDeleted)
	if int64(len(q)) != c.Counts.Instances {
		t.Fatalf("%d files of the subject sent again were held, want %d", len(q), c.Counts.Instances)
	}
	for _, f := range q {
		if f.ContentKept || f.Incoming != "" || f.Details["correction"] != c.ID {
			t.Fatalf("a was_deleted record keeps %v at %q, naming %q", f.ContentKept, f.Incoming, f.Details["correction"])
		}
	}
	if left := r.incomingFiles(); len(left) != 0 {
		t.Fatalf("the incoming area keeps %q", left)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if q := r.quarantined(tenant.QuarantineWasDeleted); len(q) != 0 {
		t.Fatalf("after the purge %d was_deleted records of the subject stay", len(q))
	}
	rs, _, err := r.p.ListReceptions(r.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	blanked := 0
	for _, rc := range rs {
		if rc.SubjectLabel == subject {
			t.Fatalf("after the purge a reception names the subject: %+v", rc)
		}
		if rc.SubjectLabel == c.ID {
			blanked++
		}
	}
	if blanked == 0 {
		t.Fatal("no reception of the subject was given the correction's id")
	}
	if dirs := r.dirsNaming(subject); len(dirs) != 0 {
		t.Fatalf("after the purge the storage keeps directories naming the subject: %q", dirs)
	}
}

// TestPreviewCountsWhatThePurgeClears guards that a subject's preview
// counts the quarantined files of the subject and its ended receptions,
// and says the subject's label stays held after the purge.
func TestPreviewCountsWhatThePurgeClears(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	s := r.scanWith(2)
	in := r.held(pathOf(s))[0]
	old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	r.holdDifferent(s, in, append(old, 7))
	pv, err := r.engine().Preview(r.ctx, tenant.CorrectionTarget{Subject: s.SubjectLabel})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Quarantined != 1 || pv.Receptions == 0 || !slices.Equal(pv.HeldAfterPurge, []string{s.SubjectLabel}) {
		t.Fatalf("the preview clears %d quarantined files and %d receptions and holds %v", pv.Quarantined, pv.Receptions, pv.HeldAfterPurge)
	}
}

// TestStoppedRelabelLeavesNoCopies guards a relabel an admin stops while
// it copies, at every write it can be at: nothing stays under the new
// label, not even a directory, and it ends abandoned and swept.
func TestStoppedRelabelLeavesNoCopies(t *testing.T) {
	for _, step := range []string{"relabel-object", "relabel-metadata", "relabel-manifest", "copied"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			e := r.engine()
			subject := r.scanWith(2).SubjectLabel
			c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "renamed-z", Reason: "x",
				Audit: r.audit()})
			if err != nil {
				t.Fatal(err)
			}
			var stopped atomic.Bool
			e.SetCrashAt(func(s string) error {
				if s == step && stopped.CompareAndSwap(false, true) {
					if _, err := e.Undo(r.ctx, c.ID, "stop it", r.audit(), nil); err != nil {
						t.Errorf("Undo: %v", err)
					}
				}
				return nil
			})
			got, err := e.ContinueRelabel(r.ctx, c.ID)
			if err != nil && fault.KindOf(err) != fault.Precondition {
				t.Fatalf("ContinueRelabel = %+v, %v", got, err)
			}
			if !stopped.Load() {
				t.Fatalf("the relabel never reached %s", step)
			}
			if st := r.state(c.ID); st.State != record.StateAbandoned || !st.Swept {
				t.Fatalf("the stopped relabel is %s, swept %v", st.State, st.Swept)
			}
			if left := under(r.objects(), "data/renamed-z/", "meta/renamed-z/"); len(left) != 0 {
				t.Fatalf("after the stop the new label holds %q", left)
			}
			if dirs := r.dirsNaming("renamed-z"); len(dirs) != 0 {
				t.Fatalf("after the stop the storage keeps %q", dirs)
			}
		})
	}
}

// TestStoppedRelabelSweptAtStart guards a relabel stopped while it copied
// whose process then stopped before it removed its copies: the restart
// removes them.
func TestStoppedRelabelSweptAtStart(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	e := r.engine()
	subject := r.scanWith(2).SubjectLabel
	c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "renamed-z", Reason: "x",
		Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	e.SetCrashAt(func(s string) error {
		if s == "relabel-object" && stopped.CompareAndSwap(false, true) {
			if _, err := e.Undo(r.ctx, c.ID, "stop it", r.audit(), nil); err != nil {
				t.Errorf("Undo: %v", err)
			}
			return errCrash
		}
		return nil
	})
	if _, err := e.ContinueRelabel(r.ctx, c.ID); !errors.Is(err, errCrash) {
		t.Fatalf("ContinueRelabel did not stop: %v", err)
	}
	if left := under(r.objects(), "data/renamed-z/"); len(left) == 0 {
		t.Fatal("the stop left no copy to remove")
	}
	if err := r.engine().Recover(r.ctx); err != nil {
		t.Fatal(err)
	}
	if left := under(r.objects(), "data/renamed-z/", "meta/renamed-z/"); len(left) != 0 {
		t.Fatalf("after the restart the new label holds %q", left)
	}
	if st := r.state(c.ID); !st.Swept || st.RecordsState != record.StateAbandoned {
		t.Fatalf("after the restart the relabel is swept %v, its records %s", st.Swept, st.RecordsState)
	}
}

// TestRelabelBackAndForth guards that a relabel asked again after the
// same one ended is a new relabel: X to Y, Y to X, X to Y leaves the
// subject at Y.
func TestRelabelBackAndForth(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	e := r.engine()
	x := r.scanWith(2).SubjectLabel
	var ids []string
	for _, m := range [][2]string{{x, "renamed-y"}, {"renamed-y", x}, {x, "renamed-y"}} {
		c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: m[0]}, ToLabel: m[1], Reason: "x", Audit: r.audit()})
		if err != nil {
			t.Fatal(err)
		}
		if c.State != record.StatePrepared {
			t.Fatalf("relabel %s to %s answered %s, %s", m[0], m[1], c.ID, c.State)
		}
		if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if ids[0] == ids[2] {
		t.Fatal("the third relabel answered the first")
	}
	if _, err := r.p.CatalogSubject(r.ctx, "renamed-y"); err != nil {
		t.Fatalf("after the third relabel the subject is not renamed-y: %v", err)
	}
	objs := r.objects()
	for _, s := range r.scans() {
		for _, in := range r.held(pathOf(s)) {
			if _, ok := objs[in.Key]; !ok {
				t.Fatalf("the catalog lists %s, whose object is not stored", in.Key)
			}
		}
	}
}

// TestInstancePurgeWaitsForAReception guards an instance's purge against a
// reception into the same scan at once: with the pipeline's scan lock the
// two never lose each other's entries, so after both the scan's manifest
// on the storage lists exactly the instances the catalog holds, and the
// catalog holds that manifest's checksum.
func TestInstancePurgeWaitsForAReception(t *testing.T) {
	r := newRig(t, false)
	p := r.pipeline()
	defer func() { _ = p.Stop(context.Background()) }()
	r.accept(p, "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm")
	r.waitEnded()
	path := pathOf(r.scanWith(3))
	victim := r.held(path)[0]
	e := r.engineWith(func(c *Config) { c.LockScan = p.LockScan })
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan, Instance: victim.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	e.SetCrashAt(func(step string) error {
		if step == "meta-read" && once.CompareAndSwap(false, true) {
			// A reception of two more files of the scan, given time to
			// write the scan if nothing stops it.
			r.accept(p, "ct5n-3023.dcm", "ct5n-3353.dcm")
			time.Sleep(2 * time.Second)
		}
		return nil
	})
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.waitEnded()
	held := r.held(path)
	if len(held) != 4 {
		t.Fatalf("after both the scan lists %d instances, want 4", len(held))
	}
	mk, _ := layout.ManifestKey(path)
	raw, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var listed, want []string
	for _, f := range m.Files {
		listed = append(listed, f.SOPInstanceUID)
	}
	for _, in := range held {
		want = append(want, in.SOPInstanceUID)
	}
	sort.Strings(listed)
	sort.Strings(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("the stored manifest lists %q, the catalog %q", listed, want)
	}
	sc, err := r.p.CatalogScan(r.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ManifestSHA256 != tenant.SHA256(sha256.Sum256(raw)) {
		t.Fatal("the catalog does not hold the stored manifest's checksum")
	}
}

// TestFailedPurgeGoesOnAtTheNextPass guards that a purge that stopped
// part way is finished by the next PurgeDue, not only by a restart.
func TestFailedPurgeGoesOnAtTheNextPass(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	subject := r.scanWith(2).SubjectLabel
	c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	e := r.engine()
	e.SetCrashAt(crashAt("purge-object"))
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); !errors.Is(err, errCrash) {
		t.Fatalf("the purge did not stop: %v", err)
	}
	if st := r.state(c.ID); st.State != record.StatePurging {
		t.Fatalf("the stopped purge is %s", st.State)
	}
	n, err := r.engine().PurgeDue(r.ctx)
	if err != nil || n != 1 {
		t.Fatalf("PurgeDue = %d, %v; want the purge taken up", n, err)
	}
	if st := r.state(c.ID); st.State != record.StatePurged {
		t.Fatalf("after the next pass the purge is %s", st.State)
	}
	if left := under(r.objects(), "data/"+subject+"/", "meta/"+subject+"/"); len(left) != 0 {
		t.Fatalf("after the next pass the storage holds %q", left)
	}
}
