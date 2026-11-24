package receive

import (
	"bytes"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// archivedDate returns the Study Date of the archived object of the
// instance of original UID u, and whether it is archived.
func archivedDate(t *testing.T, e *env, r tenant.Reception, u string) (string, bool) {
	t.Helper()
	obj, ok := archivedObjects(t, e, r)[remapped(u)]
	if !ok {
		return "", false
	}
	h, err := header.Read(bytes.NewReader(obj), int64(len(obj)))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := h.Lookup(0x00080020)
	return v, true
}

// TestStudyMixingIssuersHoldsTheMismatchingFile guards one subject, one
// date shift per session: a study one of whose series carries Issuer of
// Patient ID HOSP_A and the other none is named from one file of one
// series (which one goes by the remapped Series Instance UIDs), and every
// file whose own subject label is another is held (deid_failed,
// subject_mismatch) rather than archived under the session's subject with
// its dates shifted by another subject's offset; the files that agree are
// archived with one shift.
func TestStudyMixingIssuersHoldsTheMismatchingFile(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: shiftingProfile()}})
	const study = "2.25.301"
	e.sendTo("dimse", "SCANNER",
		issuedAs(t, study, "2.25.3011", "2.25.30111", "HOSP_A", "LO"),
		issuedAs(t, study, "2.25.3011", "2.25.30112", "HOSP_A", "LO"),
		issuedAs(t, study, "2.25.3012", "2.25.30121", "", "LO"))
	r := e.settle(1)[0]
	hospA := "sub-" + e.cfg.Hasher.Hash((naming.SubjectIdentity{PatientID: issuerPatientID, Issuer: "HOSP_A"}).Key())
	none := "sub-" + e.cfg.Hasher.Hash([]byte(issuerPatientID))
	agree, mismatch := []string{"2.25.30111", "2.25.30112"}, []string{"2.25.30121"}
	switch r.SubjectLabel {
	case hospA:
	case none:
		agree, mismatch = mismatch, agree
	default:
		t.Fatalf("the session's subject %s is neither HOSP_A's nor the Patient ID's alone", r.SubjectLabel)
	}
	dates := map[string]bool{}
	for _, u := range agree {
		d, ok := archivedDate(t, e, r, u)
		if !ok {
			t.Errorf("%s, whose subject is the session's, was not archived", u)
		}
		dates[d] = true
	}
	if len(dates) != 1 {
		t.Errorf("the files of the session's subject have %d date shifts", len(dates))
	}
	held := map[string]tenant.QuarantinedFile{}
	for _, q := range e.held() {
		held[q.SOPInstanceUID] = q
	}
	for _, u := range mismatch {
		if _, ok := archivedDate(t, e, r, u); ok {
			t.Errorf("%s, of another subject identity, was archived under the session's subject", u)
		}
		if q, ok := held[remapped(u)]; !ok || q.Reason != tenant.QuarantineDeidFailed || q.Details["cause"] != subjectMismatch {
			t.Errorf("%s held as %+v, want deid_failed subject_mismatch", u, q)
		}
	}
	if len(held) != len(mismatch) {
		t.Errorf("%d files held, want %d", len(held), len(mismatch))
	}
}

// TestEngineTakesTheIdentityTheReceptionRead guards the identity the
// pipeline hands the engine: a file with no issuer through a receiver of
// namespace site-b is filed under the subject of its Patient ID in site-b,
// and its dates are shifted by exactly the offset of that identity
// (engine.ShiftDays over its key under the tenant's date key), never by the
// Patient ID alone, which an engine left to read the identity itself
// without the receiver's namespace would key on.
func TestEngineTakesTheIdentityTheReceptionRead(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: shiftingProfile()}})
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Namespace: "site-b"},
		[]tenant.ReceiverField{tenant.FieldNamespace}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	b := issuedAs(t, "2.25.401", "2.25.4011", "2.25.40111", "", "LO")
	sent, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := sent.Lookup(0x00080020)
	e.clock.Advance(2 * time.Second)
	e.sendTo("dimse", "SCANNER", b)
	r := e.settle(1)[0]
	id := naming.SubjectIdentity{PatientID: issuerPatientID, Namespace: "site-b"}
	if want := "sub-" + e.cfg.Hasher.Hash(id.Key()); r.SubjectLabel != want {
		t.Fatalf("the subject is %s, want site-b's %s", r.SubjectLabel, want)
	}
	got, ok := archivedDate(t, e, r, "2.25.40111")
	if !ok {
		t.Fatal("the file was not archived")
	}
	day, err := time.Parse("20060102", original)
	if err != nil {
		t.Fatal(err)
	}
	want := day.AddDate(0, 0, -engine.ShiftDays(testDeidKeys.Date, string(id.Key()), 365)).Format("20060102")
	alone := day.AddDate(0, 0, -engine.ShiftDays(testDeidKeys.Date, issuerPatientID, 365)).Format("20060102")
	if got != want {
		t.Errorf("the archived Study Date is %s, want %s (site-b's identity); the Patient ID alone gives %s", got, want, alone)
	}
}

// TestRelabeledSessionKeepsReceiving guards that a session whose subject
// a committed relabel renamed keeps receiving its patient's files: a later
// arrival's own subject label is the one the relabel moved away from, and
// it is archived under the new one, not held as subject_mismatch.
func TestRelabeledSessionKeepsReceiving(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: shiftingProfile()}})
	const study = "2.25.501"
	e.sendTo("dimse", "SCANNER", issuedAs(t, study, "2.25.5011", "2.25.50111", "", "LO"))
	first := e.settle(1)[0]
	c, ok := e.store.(tenant.Corrections)
	if !ok {
		t.Fatal("the tenant store does not take corrections")
	}
	id, err := tenant.NewCorrectionID(e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.CorrectionTarget{Subject: first.SubjectLabel}
	if _, _, err := c.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel, Target: target,
		ToLabel: "sub-renamed", Reason: "the subject's code in the study", Now: e.clock.Now()}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	scans, err := c.CorrectionScans(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string]tenant.SHA256{}
	for _, s := range scans {
		manifests[s.SeriesUID] = tenant.SHA256{1}
	}
	if _, err := c.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: e.clock.Now(), Grace: time.Hour, Manifests: manifests}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.sendTo("dimse", "SCANNER", issuedAs(t, study, "2.25.5012", "2.25.50121", "", "LO"))
	rs := e.settle(2)
	for _, r := range rs {
		if r.ID == first.ID {
			continue
		}
		if !done(r) || r.SubjectLabel != "sub-renamed" || r.Counts.FilesArchived != 1 {
			t.Errorf("the later arrival: %s under %s, %d archived; want done under sub-renamed", r.State, r.SubjectLabel, r.Counts.FilesArchived)
		}
	}
	if held := e.held(); len(held) != 0 {
		t.Errorf("files held: %+v", held)
	}
}

// ownPath returns the labels a file filed by itself under the receiver's
// rule gets for its subject, with the session and scan labels given: the
// path a test hands a step that de-identifies, whose file must agree with
// its session's subject (subjectAgrees).
func ownPath(tb testing.TB, e *env, rcv tenant.Receiver, body []byte, session, scan string) label.Path {
	tb.Helper()
	h, err := header.Read(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		tb.Fatal(err)
	}
	rule, err := rcv.NamingRule()
	if err != nil {
		tb.Fatal(err)
	}
	subject, err := rule.SubjectWithParts(h, nil, e.cfg.Hasher)
	if err != nil {
		tb.Fatal(err)
	}
	return label.Path{Subject: subject, Session: session, Scan: scan}
}
