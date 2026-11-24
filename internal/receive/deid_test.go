package receive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// archivedObjects returns every DICOM object of a done reception on the
// storage by its SOP Instance UID, read through the scans' manifests, each
// manifest entry checked against its object's bytes.
func archivedObjects(t *testing.T, e *env, r tenant.Reception) map[string][]byte {
	t.Helper()
	scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, sc := range scans {
		path := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label}
		mk, _ := layout.ManifestKey(path)
		m, err := manifest.Decode(bytes.NewReader(readObject(t, e.storage, mk)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m.Files {
			obj := readObject(t, e.storage, f.Key)
			sum := sha256.Sum256(obj)
			if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(obj)) != f.Size {
				t.Errorf("manifest entry %s does not match its object", f.Key)
			}
			out[f.SOPInstanceUID] = obj
		}
		sk, _ := layout.MetadataKey(path)
		out["sidecar:"+sc.Label] = readObject(t, e.storage, sk)
		out["manifest:"+sc.Label] = readObject(t, e.storage, mk)
	}
	return out
}

// tenantFiles returns the bytes of every file of the tenant's SQLite
// database (the database, its write-ahead log and shared memory), for a
// search of what the database holds, whatever table holds it.
func tenantFiles(t *testing.T, e *env) []byte {
	t.Helper()
	var all []byte
	err := filepath.Walk(e.dataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.Contains(filepath.Base(p), ".db") {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the test's own data directory.
		if err != nil {
			return err
		}
		all = append(all, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no database file found in the data directory")
	}
	return all
}

// TestDeidentifyingReceiverArchivesNoOriginal guards a receiver that
// de-identifies: a study of two series is archived with every object, the
// metadata and the manifests holding the keyed remapping of its UIDs and
// none of its patient's name, identifier or study date; one subject and
// session, a scan per series; each instance recording the profile version;
// the tenant's database holding none of the original UIDs, the name or the
// received checksums; the incoming area empty once done, no de-identified
// copy left; and the check after (package check) given each file's
// needles.
func TestDeidentifyingReceiverArchivesNoOriginal(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	e.send(studyFiles...)
	rs := e.settle(1)
	r := rs[0]
	if !done(r) || r.Counts.FilesArchived != int64(len(studyFiles)) {
		t.Fatalf("reception %s, counts %+v", r.State, r.Counts)
	}
	first := corpus(t, studyFiles[0])
	study, _, _ := fileUIDs(t, first)
	if r.StudyUID != remapped(study) {
		t.Errorf("the reception holds study %s, want the remapping of the original", r.StudyUID)
	}
	objs := archivedObjects(t, e, r)
	originals := []string{"Doe^Peter", "98890234", "20010101", study}
	sums := []tenant.SHA256{}
	for _, n := range studyFiles {
		b := corpus(t, n)
		st, se, in := fileUIDs(t, b)
		originals = append(originals, se, in)
		sums = append(sums, sha256.Sum256(b))
		obj, ok := objs[remapped(in)]
		if !ok {
			t.Errorf("%s is not archived under its remapped UID", n)
			continue
		}
		h, err := header.Read(bytes.NewReader(obj), int64(len(obj)))
		if err != nil {
			t.Fatalf("an archived object does not read: %v", err)
		}
		gotStudy, _ := h.UID(header.StudyUID)
		gotSeries, _ := h.UID(header.SeriesUID)
		if gotStudy != remapped(st) || gotSeries != remapped(se) {
			t.Errorf("%s: archived with study %s series %s, want their remappings", n, gotStudy, gotSeries)
		}
		if v, _ := h.Lookup(0x00120062); v != "YES" {
			t.Errorf("%s: Patient Identity Removed is %q", n, v)
		}
		ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, remapped(in))
		if err != nil || ref != testProfile {
			t.Errorf("%s records profile %v (%v), want %v", n, ref, err, testProfile)
		}
	}
	for key, obj := range objs {
		for _, o := range originals {
			if bytes.Contains(obj, []byte(o)) {
				t.Errorf("%s holds an original value of %d bytes", key, len(o))
			}
		}
	}
	db := tenantFiles(t, e)
	for _, o := range originals {
		if bytes.Contains(db, []byte(o)) {
			t.Errorf("the tenant's database holds an original value of %d bytes", len(o))
		}
	}
	for _, s := range sums {
		if bytes.Contains(db, s[:]) || bytes.Contains(db, []byte(s.String())) {
			t.Error("the tenant's database holds a received file's SHA-256")
		}
	}
	if n := e.incomingFiles(); n != 0 {
		t.Errorf("the incoming area holds %d files once the reception is done", n)
	}
	if e.check.calls != len(studyFiles) {
		t.Errorf("the check after ran %d times for %d files", e.check.calls, len(studyFiles))
	}
	for u := range e.check.needles {
		if len(e.check.needles[u]) == 0 {
			t.Errorf("the check of %s was given no needles", u)
		}
	}
}

// TestDeidentificationIsDeterministic guards that a de-identifying
// receiver writes the same bytes for the same file under the same keys
// and profile version: two tenants' data directories, the same study sent
// to each, give byte-identical objects under the same keys. A sidecar names
// the reception that archived each line, an id made at random for each
// tenant, so sidecars are compared with those ids left out.
func TestDeidentificationIsDeterministic(t *testing.T) {
	a := newEnv(t, envOptions{deid: &deidOptions{}})
	b := newEnv(t, envOptions{deid: &deidOptions{}})
	a.send(studyFiles...)
	b.send(studyFiles...)
	ra, rb := a.settle(1)[0], b.settle(1)[0]
	oa, ob := archivedObjects(t, a, ra), archivedObjects(t, b, rb)
	if len(oa) == 0 || len(oa) != len(ob) {
		t.Fatalf("%d and %d objects", len(oa), len(ob))
	}
	for k, v := range oa {
		if strings.HasPrefix(k, "sidecar:") {
			v, ob[k] = withoutReceptions(t, v), withoutReceptions(t, ob[k])
		}
		if !bytes.Equal(v, ob[k]) {
			t.Errorf("%s differs between two runs", k)
		}
	}
}

// withoutReceptions returns a sidecar written again with the reception id
// of each line left out.
func withoutReceptions(t *testing.T, b []byte) []byte {
	t.Helper()
	rd := sidecar.NewReader(bytes.NewReader(b))
	var out bytes.Buffer
	var w *sidecar.Writer
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			w = sidecar.NewWriterV3(&out, *rd.Scan())
		}
		in.Reception = "r"
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestDeidentificationHoldsWhatItCannotProcess guards that a file the
// profile cannot process is held with its reason and never archived: a
// rule whose action is quarantine (profile_refused, the rule's name), a
// date to shift in a file without a Patient ID (deid_failed, no_subject,
// with the element), and a value the check after finds (residual_value,
// the element and the kind of value, never the value). Every record holds
// remapped UIDs only and keeps the original bytes, and a release once the
// check no longer finds the value archives the file through the profile.
func TestDeidentificationHoldsWhatItCannotProcess(t *testing.T) {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, []*v1.ProfileRule{
		{Name: "refuse-mr", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "MRAcquisitionType"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Quarantine{Quarantine: &v1.QuarantineAction{}}}},
		{Name: "shift-dates", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "ContentDate"}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}},
	}...)
	checker := &recordingCheck{}
	e := newEnv(t, envOptions{naming: &naming.Spec{Subject: "sub-{#StudyInstanceUID}", Session: "ses-{#StudyInstanceUID}",
		Scan: "scan-{#SeriesInstanceUID}"}, deid: &deidOptions{doc: doc, check: checker}})
	ct := corpus(t, studyFiles[0])
	_, _, ctUID := fileUIDs(t, ct)
	checker.setFind(func(u string) *check.Residual {
		if u == remapped(ctUID) {
			return &check.Residual{First: check.Finding{Path: "00100010", Kind: check.KindName}, Total: 1}
		}
		return nil
	})
	e.send(studyFiles[0], studyFiles[1], "MR_small.dcm")
	noID := withoutPatientID(t, corpus(t, "liver_1frame.dcm"))
	e.sendBytes(noID, Received)
	e.settle(3)
	byUID := map[string]tenant.QuarantinedFile{}
	for _, q := range e.held() {
		byUID[q.SOPInstanceUID] = q
		if !q.ContentKept {
			t.Errorf("record %s keeps no bytes", q.ID)
		}
	}
	_, _, mrUID := fileUIDs(t, corpus(t, "MR_small.dcm"))
	_, _, liverUID := fileUIDs(t, noID)
	for _, c := range []struct {
		uid     string
		reason  tenant.QuarantineReason
		details map[string]string
	}{
		{remapped(ctUID), tenant.QuarantineResidualValue, map[string]string{"path": "00100010", "kind": "name"}},
		{remapped(mrUID), tenant.QuarantineProfileRefused, map[string]string{"rule": "refuse-mr"}},
		{remapped(liverUID), tenant.QuarantineDeidFailed, map[string]string{"cause": "no_subject", "path": "00080023", "rule": "shift-dates"}},
	} {
		q, ok := byUID[c.uid]
		if !ok {
			t.Errorf("no record holds %s (%s)", c.uid, c.reason)
			continue
		}
		if q.Reason != c.reason || !equalDetails(q.Details, c.details) {
			t.Errorf("held with %s %v, want %s %v", q.Reason, q.Details, c.reason, c.details)
		}
	}
	for _, o := range []string{ctUID, mrUID, liverUID} {
		if _, ok := byUID[o]; ok {
			t.Errorf("a record holds the original UID %s", o)
		}
	}

	// A release, once the check finds nothing, de-identifies the file
	// again and archives it.
	checker.setFind(nil)
	q := byUID[remapped(ctUID)]
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the released file to be archived", 3*time.Second, func() bool {
		ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, remapped(ctUID))
		return err == nil && ref == testProfile
	})
}

// equalDetails reports whether two records' details are the same.
func equalDetails(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestReceiverChangedToAsReceivedHoldsWhatCameBefore guards that a file
// that came while its receiver de-identified is never archived as it was
// after the receiver is changed to archive as received: it is held
// (deid_failed, no_profile), and nothing is put on the storage.
func TestReceiverChangedToAsReceivedHoldsWhatCameBefore(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	e.send(studyFiles[0])
	e.waitFor("the file to be recorded", time.Second, func() bool { return e.p.stats.Recorded.Load() == 1 })
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification},
		t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r := e.settle(1)[0]
	if r.State != "quarantined" {
		t.Errorf("reception %s, want quarantined", r.State)
	}
	qs := e.held()
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "no_profile" {
		t.Fatalf("held %+v", qs)
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
		t.Errorf("the storage holds %v (%v)", objs.Keys, err)
	}
}

// TestDeflatedFileIsHeldByTheCheck guards a deflated file (PS3.5 A.5) at
// a receiver that de-identifies: the engine de-identifies it, its pixel
// data copied through the inflating source, but the check after cannot
// search the raw bytes of a deflated data set, so the file is held
// (deid_failed, check_unreadable), never archived unchecked, its original
// kept and its record holding the remapped UID.
func TestDeflatedFileIsHeldByTheCheck(t *testing.T) {
	checker := &recordingCheck{}
	e := newEnv(t, envOptions{deid: &deidOptions{check: checker}})
	b := deflatedCorpus(t)
	e.sendBytes(b, Received)
	r := e.settle(1)[0]
	_, _, in := fileUIDs(t, b)
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed ||
		qs[0].Details["cause"] != "check_unreadable" || qs[0].SOPInstanceUID != remapped(in) || !qs[0].ContentKept {
		t.Fatalf("reception %s, held %+v", r.State, qs)
	}
	if len(checker.needles[remapped(in)]) == 0 {
		t.Error("the deflated file did not reach the check de-identified")
	}
}

// plainLabels is a tenant database whose receiver reads back with a naming
// rule that puts the Patient ID into the subject label unhashed: a row no
// store keeps for a receiver that de-identifies (Receiver.Validate refuses
// it), made here to reach the pipeline's own guard.
type plainLabels struct {
	tenant.Pipeline
}

// Receiver returns the receiver with the plain naming rule.
func (s plainLabels) Receiver(ctx context.Context, name string) (tenant.Receiver, error) {
	r, err := s.Pipeline.Receiver(ctx, name)
	r.Naming = &naming.Spec{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
	return r, err
}

// TestALabelHoldingARemovedValueIsHeld guards the labels of a receiver
// that de-identifies whose naming rule puts a value into a label as
// received (a rule the store refuses for such a receiver, read here from a
// database that answers it anyway): the subject label made of the Patient
// ID, which the profile removes, holds the file as residual_value with the
// label's level, never its value, and nothing is archived.
func TestALabelHoldingARemovedValueIsHeld(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}, db: func(p tenant.Pipeline) tenant.Pipeline {
		return plainLabels{Pipeline: p}
	}})
	e.send(studyFiles[0])
	r := e.settle(1)[0]
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineResidualValue ||
		!equalDetails(qs[0].Details, map[string]string{"path": "subject", "kind": "identifier"}) {
		t.Fatalf("reception %s, held %+v", r.State, qs)
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
		t.Errorf("the storage holds %v (%v)", objs.Keys, err)
	}
}
