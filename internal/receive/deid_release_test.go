package receive

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// noOriginal fails the test when an object of the reception holds the CT
// patient's name or identifier, or the check after never ran.
func noOriginal(t *testing.T, e *env, r tenant.Reception) {
	t.Helper()
	objs := archivedObjects(t, e, r)
	if len(objs) == 0 {
		t.Fatal("nothing archived")
	}
	for k, o := range objs {
		if bytes.Contains(o, []byte("Doe^Peter")) || bytes.Contains(o, []byte("98890234")) {
			t.Errorf("%s holds the patient's name or identifier at a receiver that de-identifies", k)
		}
	}
	if e.check.calls == 0 {
		t.Error("the check after never ran")
	}
}

// TestAReleaseAfterSwitchToDeidIsDeidentified guards a file held while its
// receiver archived as received (its SOP class not accepted), released
// after the receiver was changed to de-identify: its record, recorded as
// received, joins no reception, and its bytes land again through the
// receiver, remapped and keyed, so it is de-identified and checked; the
// archive holds no value of the original.
func TestAReleaseAfterSwitchToDeidIsDeidentified(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}, sopClasses: []string{"1.2.3.4"}})
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	e.send(studyFiles[0])
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineSOPClassNotAccepted || q.Deidentified {
		t.Fatalf("held %s, de-identified %v", q.Reason, q.Deidentified)
	}
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: testProfile},
		[]tenant.ReceiverField{tenant.FieldDeidentification, tenant.FieldDIMSESOPClasses}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	rel, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rel.ReleaseReception != "" {
		t.Errorf("a record recorded as received joined %s through a receiver that de-identifies", rel.ReleaseReception)
	}
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v", r.State, r.Counts)
	}
	_, _, in := fileUIDs(t, corpus(t, studyFiles[0]))
	if r.StudyUID == "" || len(archivedObjects(t, e, r)) == 0 {
		t.Fatal("nothing archived")
	}
	if _, ok := archivedObjects(t, e, r)[remapped(in)]; !ok {
		t.Error("the released file is not archived under its remapped UID")
	}
	noOriginal(t, e, r)
}

// TestFilesInFlightAtASwitchAreHeld guards files recorded while their
// receiver archived as received and archived after it was changed to
// de-identify: each is held (deid_failed, recorded_as_received), nothing
// is put on the storage, and a release lands it again, de-identified.
func TestFilesInFlightAtASwitchAreHeld(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	e.send(studyFiles[0])
	e.waitFor("the file to be recorded", time.Second, func() bool { return e.p.stats.Recorded.Load() == 1 })
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: testProfile},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r := e.settle(1)[0]
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "recorded_as_received" {
		t.Fatalf("reception %s, held %+v", r.State, qs)
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
		t.Fatalf("the storage holds %v (%v)", objs.Keys, err)
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	rs := e.settle(2)
	for _, x := range rs {
		if done(x) {
			noOriginal(t, e, x)
			return
		}
	}
	t.Fatalf("no reception archived the released file: %+v", rs)
}

// TestAPlacingUIDUnderTheStandardsRootIsHeld guards a file whose SOP
// Instance UID is under the standard's own root (1.2.840.10008.), at a
// receiver that de-identifies: held as bad_uid when it lands, its record
// holding the remapped UID, and nothing written to the storage.
func TestAPlacingUIDUnderTheStandardsRootIsHeld(t *testing.T) {
	const reg = "1.2.840.10008.999.12345"
	b := rewrite(t, corpus(t, studyFiles[0]), "", func(ds *dataset.Dataset) {
		ds.Remove(tag.New(0x0008, 0x0018))
		if err := ds.Add(dataelem.NewDataElement(tag.New(0x0008, 0x0018), dataelem.VR("UI"), []byte(reg))); err != nil {
			t.Fatal(err)
		}
	})
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	e.sendBytes(b, Received)
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineBadUID || q.Details["attribute"] != "SOPInstanceUID" || q.Details["fault"] != tenant.FaultBadUID {
		t.Fatalf("held %s %v", q.Reason, q.Details)
	}
	if q.SOPInstanceUID != remapped(reg) || !q.Deidentified {
		t.Errorf("the record holds %s (de-identified %v), want the remapping", q.SOPInstanceUID, q.Deidentified)
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{}); err != nil || len(objs.Keys) != 0 {
		t.Errorf("the storage holds %v (%v)", objs.Keys, err)
	}
}

// TestChangedIncomingBytesNeverReachTheStorage guards the comparison made
// before a Put: a received file whose bytes in the incoming area changed
// after it was recorded (one byte flipped, the size the same) is never
// written to the storage, at a receiver that archives as received (the
// bytes against the row's checksum) and at one that de-identifies (the
// original against the row's keyed checksum, before it is de-identified);
// its reception fails with the file kept, and the storage holds no object.
func TestChangedIncomingBytesNeverReachTheStorage(t *testing.T) {
	for _, c := range []struct {
		name string
		deid *deidOptions
	}{{"as received", nil}, {"de-identified", &deidOptions{}}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, envOptions{deid: c.deid})
			e.send(studyFiles[0])
			e.waitFor("the file to be recorded", time.Second, func() bool { return e.p.stats.Recorded.Load() == 1 })
			r := e.receptions()[0]
			files, _, err := e.store.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
			if err != nil || len(files) != 1 {
				t.Fatalf("%d files: %v", len(files), err)
			}
			p := filepath.Join(e.dataDir, "incoming", "lab", files[0].Incoming)
			b, err := os.ReadFile(p) // #nosec G304 -- the test's own incoming area.
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)-1] ^= 0xFF
			if err := os.WriteFile(p, b, 0o600); err != nil { // #nosec G703 -- the test's own incoming area.
				t.Fatal(err)
			}
			e.waitFor("the reception to end", 3*time.Second, func() bool {
				rs := e.receptions()
				return len(rs) == 1 && rs[0].State.Ended()
			})
			if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
				t.Errorf("the storage holds %v (%v)", objs.Keys, err)
			}
		})
	}
}

// TestTheBytesCheckedAreTheBytesPut guards the binding of the check after
// to the bytes archived: a de-identified copy changed while the check read
// it (one byte flipped through its name) is refused by the copy's own
// comparison after the check, so it is never put; and a sidecar line is never read
// from a file without frames whose bytes are not those put.
func TestTheBytesCheckedAreTheBytesPut(t *testing.T) {
	checker := &recordingCheck{during: func(src io.ReaderAt) {
		f, ok := src.(*os.File)
		if !ok {
			return
		}
		w, err := os.OpenFile(f.Name(), os.O_WRONLY, 0) // #nosec G304 -- the test's own incoming area.
		if err != nil {
			return
		}
		info, _ := f.Stat()
		one := make([]byte, 1)
		_, _ = f.ReadAt(one, info.Size()-1)
		one[0] ^= 0xFF
		_, _ = w.WriteAt(one, info.Size()-1)
		_ = w.Close()
	}}
	e := newEnv(t, envOptions{deid: &deidOptions{check: checker}, manual: true})
	body := corpus(t, studyFiles[0])
	f, err := e.p.area.Land(e.ctx, "dimse", bytes.NewReader(body), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	st, se, in := fileUIDs(t, body)
	wr := written{f: tenant.ReceivedFile{SOPInstanceUID: remapped(in), StudyUID: remapped(st), SeriesUID: remapped(se),
		Incoming: f.Path, Size: f.Size, SHA256: keyedSum(testDeidKeys, f.SHA256), Deidentified: true}}
	rcv, err := e.store.Receiver(e.ctx, "dimse")
	if err != nil {
		t.Fatal(err)
	}
	_, q, err := e.p.prepareOne(e.ctx, tenant.Reception{ID: "r", Receiver: "dimse"}, rcv, label.Path{Subject: "sub-a", Session: "ses-a", Scan: "scan-a"}, wr)
	if q != nil || err == nil || !strings.Contains(err.Error(), "changed while it was checked") {
		t.Errorf("a copy changed under the check: held %+v, %v", q, err)
	}
	if checker.calls != 1 {
		t.Fatalf("the check ran %d times", checker.calls)
	}

	// A file without pixel data, whose row's checksum is not its bytes'.
	m := newEnv(t, envOptions{manual: true})
	plain := rewrite(t, corpus(t, studyFiles[0]), "", func(ds *dataset.Dataset) { ds.Remove(tag.New(0x7FE0, 0x0010)) })
	g, err := m.p.area.Land(m.ctx, "dimse", bytes.NewReader(plain), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	line := written{f: tenant.ReceivedFile{Incoming: g.Path, Size: g.Size}, size: g.Size, sum: tenant.SHA256{1}}
	if _, _, _, err := m.p.readLine(m.ctx, line); err == nil {
		t.Error("a sidecar line was read from a file without frames whose bytes are not those put")
	}
	line.sum = g.SHA256
	if _, _, _, err := m.p.readLine(m.ctx, line); err != nil {
		t.Errorf("a sidecar line of the bytes put: %v", err)
	}
}

// TestAReleaseViaADeidentifyingReceiverStoppedBeforeIntake guards a release
// of a record recorded as received through another receiver, one that
// de-identifies, when the process stops between the store's release and
// the intake: at the next start the file is checked again by the receiver
// its release named, so it lands remapped and keyed and is de-identified,
// never archived as received by the receiver it first came to.
func TestAReleaseViaADeidentifyingReceiverStoppedBeforeIntake(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}, sopClasses: []string{"1.2.3.4"}})
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
		Name: "deidb", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", Profile: testProfile,
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "DEIDB", AnyCaller: true},
	}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	e.send(studyFiles[0])
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if err := e.p.Stop(e.ctx); err != nil {
		t.Fatal(err)
	}
	// What Release does before the intake, the process then stopping: the
	// landed name, then the store's release through deidb.
	e.manual = true
	e.start()
	if err := e.p.area.Unrecord(q.Incoming); err != nil {
		t.Fatal(err)
	}
	out, err := e.store.ReleaseFile(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Receiver: "deidb",
		ReceptionID: "R000000000000000000000001", Now: t0}, e.audit(), nil)
	if err != nil || out.ReleaseReception != "" || out.ReleaseReceiver != "deidb" {
		t.Fatalf("the release joined %q through %q: %v", out.ReleaseReception, out.ReleaseReceiver, err)
	}
	_ = e.p.area.Close()
	e.manual = false
	// The first receiver would take the file now, were it checked by it.
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDIMSESOPClasses}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitFor("a reception", 3*time.Second, func() bool { return len(e.receptions()) > 0 })
	r := e.settle(1)[0]
	if r.Receiver != "deidb" || !done(r) {
		t.Fatalf("the file was archived by %s, reception %s; want deidb, done", r.Receiver, r.State)
	}
	noOriginal(t, e, r)
}
