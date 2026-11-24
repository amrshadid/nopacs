package receive

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// issuerPatientID is the one Patient ID every file of the issuer tests
// carries.
const issuerPatientID = "PAT-ISSUER-1"

// issued returns a public CT file as a study of its own (study, series
// and instance UIDs 2.25.n, 2.25.n1, 2.25.n2), of issuerPatientID, from
// issuer, or from no issuer when it is empty.
func issued(t *testing.T, n, issuer string) []byte {
	t.Helper()
	return issuedAs(t, "2.25."+n, "2.25."+n+"1", "2.25."+n+"2", issuer, "LO")
}

// issuedAs returns a public CT file of the study, series and instance
// UIDs given, of issuerPatientID, from issuer written with the VR vr, or
// from no issuer when it is empty.
func issuedAs(t *testing.T, study, series, instance, issuer, vr string) []byte {
	t.Helper()
	set := func(ds *dataset.Dataset, g, e uint16, vr, v string) {
		ds.Remove(tag.New(g, e))
		if err := ds.Add(dataelem.NewDataElement(tag.New(g, e), dataelem.VR(vr), []byte(v))); err != nil {
			t.Fatal(err)
		}
	}
	return rewrite(t, corpus(t, studyFiles[0]), "", func(ds *dataset.Dataset) {
		set(ds, 0x0020, 0x000D, "UI", study)
		set(ds, 0x0020, 0x000E, "UI", series)
		set(ds, 0x0008, 0x0018, "UI", instance)
		set(ds, 0x0010, 0x0020, "LO", issuerPatientID)
		ds.Remove(tag.New(0x0010, 0x0021))
		if issuer != "" {
			set(ds, 0x0010, 0x0021, vr, issuer)
		}
	})
}

// shiftingProfile is basicProfile with the Study Date shifted, not
// removed, so a file's date shift can be read from the archive.
func shiftingProfile() *v1.ProfileDocument {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "study-date",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDate"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}})
	return doc
}

// TestSubjectsAreIssuerAware guards the subject identity through the
// pipeline, two receivers of a tenant that de-identifies: two issuers of
// one Patient ID are two subjects with two date shifts; one issuer
// through a receiver of the default namespace and one of another is one
// subject with one shift; a Patient ID with no issuer through the two is
// two subjects, the default namespace's labeled by its Patient ID alone,
// as before issuers were told apart. A delete of one issuer's subject
// then holds, as was_deleted, a new study from that issuer only: the same
// Patient ID from the other issuer is archived.
func TestSubjectsAreIssuerAware(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: shiftingProfile()}})
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
		Name: "site-b", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", Namespace: "site-b", Profile: testProfile,
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "SITEB", AnyCaller: true},
	}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	files := []struct {
		study, issuer, receiver string
	}{
		{"101", "HOSP_A", "dimse"}, {"102", "HOSP_B", "dimse"}, {"103", "HOSP_A", "site-b"},
		{"104", "", "dimse"}, {"105", "", "site-b"},
	}
	for _, f := range files {
		e.sendTo(f.receiver, "SCANNER", issued(t, f.study, f.issuer))
	}
	rs := e.settle(len(files))
	subject, shifted := map[string]string{}, map[string]string{}
	for _, r := range rs {
		if !done(r) {
			t.Fatalf("reception of %s is %s", r.StudyUID, r.State)
		}
		for _, f := range files {
			if r.StudyUID != remapped("2.25."+f.study) {
				continue
			}
			subject[f.study] = r.SubjectLabel
			for k, obj := range archivedObjects(t, e, r) {
				if k == remapped("2.25."+f.study+"2") {
					h, err := header.Read(bytes.NewReader(obj), int64(len(obj)))
					if err != nil {
						t.Fatal(err)
					}
					shifted[f.study], _ = h.Lookup(0x00080020)
				}
			}
		}
	}
	if len(subject) != len(files) || len(shifted) != len(files) {
		t.Fatalf("subjects %v, dates %v: not one per study", subject, shifted)
	}
	if subject["101"] == subject["102"] || shifted["101"] == shifted["102"] {
		t.Errorf("two issuers of one Patient ID: subjects %s and %s, dates %s and %s", subject["101"], subject["102"], shifted["101"], shifted["102"])
	}
	if subject["101"] != subject["103"] || shifted["101"] != shifted["103"] {
		t.Errorf("one issuer through two receivers: subjects %s and %s, dates %s and %s", subject["101"], subject["103"], shifted["101"], shifted["103"])
	}
	if subject["104"] == subject["105"] {
		t.Errorf("no issuer under two namespaces made one subject %s", subject["104"])
	}
	if want := "sub-" + e.cfg.Hasher.Hash([]byte(issuerPatientID)); subject["104"] != want {
		t.Errorf("no issuer, default namespace: %s, want the Patient ID's own label %s", subject["104"], want)
	}
	if want := "sub-" + e.cfg.Hasher.Hash((naming.SubjectIdentity{PatientID: issuerPatientID, Issuer: "HOSP_A"}).Key()); subject["101"] != want {
		t.Errorf("issuer HOSP_A: %s, want %s", subject["101"], want)
	}

	c, ok := e.store.(tenant.Corrections)
	if !ok {
		t.Fatal("the tenant store does not take corrections")
	}
	target := tenant.CorrectionTarget{Subject: subject["101"]}
	now := e.clock.Now()
	preview, err := c.PreviewDelete(e.ctx, target, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: preview.Confirm, Now: now}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.sendTo("dimse", "SCANNER", issued(t, "106", "HOSP_A"))
	e.sendTo("dimse", "SCANNER", issued(t, "107", "HOSP_B"))
	rs = e.settle(len(files) + 2)
	var heldA, archivedB bool
	for _, r := range rs {
		switch r.StudyUID {
		case remapped("2.25.106"):
			heldA = !done(r) || r.Counts.FilesArchived == 0
		case remapped("2.25.107"):
			archivedB = done(r) && r.Counts.FilesArchived == 1 && r.SubjectLabel == subject["102"]
		}
	}
	var wasDeleted int
	for _, q := range e.held() {
		if q.Reason == tenant.QuarantineWasDeleted {
			wasDeleted++
			if q.SOPInstanceUID != remapped("2.25.1062") {
				t.Errorf("a file of %s was held as was_deleted", q.SOPInstanceUID)
			}
		}
	}
	if !heldA || wasDeleted != 1 {
		t.Errorf("a re-send from the deleted subject's issuer: held %v, %d was_deleted records", heldA, wasDeleted)
	}
	if !archivedB {
		t.Error("the same Patient ID from the other issuer was not archived under its own subject")
	}
}

// TestRemovedReceiverHoldsItsNewSubjects guards the fail-closed naming of
// a reception whose receiver was removed while it was in flight: its study
// has no session yet, its subject would be named under the receiver's
// namespace, gone with it, so every file is held as unrouted, cause
// receiver_removed, and no subject is named under the default namespace.
// A receiver that has received data refuses a change of its namespace.
func TestRemovedReceiverHoldsItsNewSubjects(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
		Name: "site-b", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", Namespace: "site-b", Profile: testProfile,
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "SITEB", AnyCaller: true},
	}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.sendTo("site-b", "SCANNER", issued(t, "201", ""))
	_, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "site-b"}, []tenant.ReceiverField{tenant.FieldNamespace}, t0, e.audit(), nil)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(err.Error(), "add a new receiver") {
		t.Errorf("a namespace change of a receiver that has received data: %v", err)
	}
	if _, err := e.store.RemoveReceiver(e.ctx, "site-b", e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	rs := e.settle(1)
	if rs[0].SubjectLabel != "" || done(rs[0]) {
		t.Errorf("the reception of a removed receiver is %s with subject %q", rs[0].State, rs[0].SubjectLabel)
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineUnrouted || held[0].Details["cause"] != tenant.UnroutedReceiverRemoved {
		t.Errorf("the files held: %+v, want one unrouted, receiver_removed", held)
	}
}
