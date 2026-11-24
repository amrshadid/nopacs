package dimse_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deidKeys are the tenant's keys of de-identification in these tests, its
// naming key the archive's hasher's.
var deidKeys = engine.Keys{
	UID: bytes.Repeat([]byte{'u'}, 32), Date: bytes.Repeat([]byte{'d'}, 32), Hash: bytes.Repeat([]byte{7}, 32),
}

// deidScript is the outside check of de-identified files, from this
// package's directory.
const deidScript = "../../../scripts/check/deid-pydicom.py"

// removedKeywords are attributes PS3.15 Annex E's basic profile removes or
// replaces, whose values pydicom must find nowhere in an archived file.
var removedKeywords = []string{"PatientName", "PatientID", "PatientBirthDate", "StudyInstanceUID", "SeriesInstanceUID",
	"SOPInstanceUID", "StudyDate", "SeriesDate", "AcquisitionDate", "ContentDate", "AccessionNumber", "StudyID",
	"InstitutionName", "ReferringPhysicianName", "OperatorsName", "StationName"}

// deidentifying makes the archive's receiver de-identify with Annex E's
// basic profile, the device's maker and model, the scan's options and a
// content label removed as a site's profile for these files does (each can
// repeat a value the basic profile removes), through the check after of
// package check.
func deidentifying(a *archive, cfg *receive.Config) {
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	doc := &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}
	for _, k := range []string{"Manufacturer", "ManufacturerModelName", "ScanOptions", "ContentLabel"} {
		doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: strings.ToLower(k),
			Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}, Action: remove})
	}
	v, err := profile.New("research", 1, doc, profile.Shipped())
	if err != nil {
		a.t.Fatal(err)
	}
	if _, _, err := a.store.(tenant.Profiles).CreateProfileVersion(a.ctx, tenant.NewProfileVersion{
		Name: "research", Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum()}, t0, a.audit("p"), nil); err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.store.SetReceiver(a.ctx, tenant.Receiver{Name: a.row.Name, Profile: tenant.ProfileRef{Name: "research", Version: 1}},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, a.audit("d"), nil); err != nil {
		a.t.Fatal(err)
	}
	cfg.DeidKeys = func(context.Context) (engine.Keys, error) { return deidKeys, nil }
	cfg.Check = func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }
}

// TestAStudySentByStorescuIsArchivedDeidentified guards a receiver that
// de-identifies, end to end, with tools that are not the archive's: dcmtk's
// storescu sends a study of two series, the reception ends done, and
// pydicom reads every archived file and finds none of the patient's name,
// identifier, dates or original UIDs in any element or in the raw bytes,
// Patient Identity Removed YES, one Study Instance UID across the study
// that is the keyed remapping of the original, and a distinct remapped SOP
// Instance UID per file.
func TestAStudySentByStorescuIsArchivedDeidentified(t *testing.T) {
	py := python(t)
	storescu := tool(t, "storescu")
	r := row()
	a := newArchive(t, r, deidentifying)
	s := listen(t, r, a.p, nil)
	args := []string{"-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port}
	for _, f := range studyFiles {
		args = append(args, corpusPath(f))
	}
	if out, err := command(t, storescu, args...).CombinedOutput(); err != nil { // #nosec G204 -- the test's own arguments.
		t.Fatalf("storescu: %v\n%s", err, out)
	}
	rs := a.settle(1)
	if len(rs) != 1 || rs[0].State != reception.Done || rs[0].Counts.FilesArchived != int64(len(studyFiles)) {
		qs, _, _ := a.store.ListQuarantined(a.ctx, tenant.QuarantineFilter{}, master.Page{Limit: 100})
		for _, q := range qs {
			t.Logf("held: %s %v", q.Reason, q.Details)
		}
		t.Fatalf("receptions %+v", rs)
	}
	checkDeidentified(t, py, a.archived(rs[0]))
	if n := a.incomingFiles(); n != 0 {
		t.Errorf("the incoming area holds %d files after the reception is done", n)
	}
}

// checkDeidentified checks the archived files of the corpus's CT study,
// got by SOP Instance UID, with pydicom: each under the keyed remapping of
// its original's UID, none of the values Annex E's basic profile removes
// found in any element or in the raw bytes, by the outside check
// scripts/check/deid-pydicom.py against each original; one Study Instance
// UID across the study that is the remapping of the original; a distinct
// remapped SOP Instance UID per file.
func checkDeidentified(t *testing.T, py string, got map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	type pair struct {
		Original     string `json:"original"`
		Deidentified string `json:"deidentified"`
		Needles      string `json:"needles"`
	}
	empty := filepath.Join(dir, "needles.json")
	if err := os.WriteFile(empty, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pairs []pair
	var archivedPaths []string
	for _, f := range studyFiles {
		sent := corpusBytes(t, f)
		obj, ok := got[engine.RemapUID(deidKeys.UID, uidOf(t, sent))]
		if !ok {
			t.Fatalf("%s is not archived under its remapped UID", f)
		}
		p := filepath.Join(dir, f)
		if err := os.WriteFile(p, obj, 0o600); err != nil {
			t.Fatal(err)
		}
		abs, _ := filepath.Abs(corpusPath(f))
		pairs = append(pairs, pair{Original: abs, Deidentified: p, Needles: empty})
		archivedPaths = append(archivedPaths, p)
	}
	manifest, err := json.Marshal(map[string]any{"keywords": removedKeywords, "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "check.json")
	if err := os.WriteFile(mp, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := command(t, py, "-I", deidScript, mp).CombinedOutput()
	t.Logf("pydicom: %s", out)
	if err != nil {
		t.Fatalf("pydicom found an original value in an archived file: %v", err)
	}

	// One study, its UID remapped; a distinct remapped UID per instance.
	read := "import sys, pydicom\nfor p in sys.argv[1:]:\n    d = pydicom.dcmread(p)\n    print(d.StudyInstanceUID, d.SOPInstanceUID)\n"
	uids, err := command(t, py, append([]string{"-I", "-c", read}, archivedPaths...)...).Output()
	if err != nil {
		t.Fatalf("pydicom: %v", err)
	}
	study := engine.RemapUID(deidKeys.UID, studyUID(t, corpusBytes(t, studyFiles[0])))
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(uids)), "\n") {
		st, inst, _ := strings.Cut(line, " ")
		if st != study {
			t.Errorf("pydicom reads the study %s, want the remapping of the original", st)
		}
		if seen[inst] || !strings.HasPrefix(inst, "2.25.") {
			t.Errorf("pydicom reads the instance %s twice or not remapped", inst)
		}
		seen[inst] = true
	}
	if len(seen) != len(studyFiles) {
		t.Errorf("pydicom read %d instances, want %d", len(seen), len(studyFiles))
	}
}

// studyUID returns a file's Study Instance UID, read with the pipeline's
// own header reader.
func studyUID(t *testing.T, b []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := h.UID(header.StudyUID)
	return u
}

// TestAStudyStoredOverSTOWIsArchivedDeidentified guards a stow receiver
// that de-identifies, end to end: a study stored over STOW-RS (PS3.18
// 10.5) through the receiver's endpoint is answered stored, the reception
// ends done, and pydicom finds every archived file de-identified as for
// DIMSE (checkDeidentified).
func TestAStudyStoredOverSTOWIsArchivedDeidentified(t *testing.T) {
	py := python(t)
	r := tenant.Receiver{Name: "web", Type: tenant.ReceiverSTOW, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: tenant.DefaultMaxFileBytes,
		STOW: tenant.STOWReceiver{Keys: []string{"key-a"}, MaxRequestBytes: tenant.DefaultMaxRequestBytes}}
	a := newArchive(t, r, deidentifying)
	ep, err := stow.New(stow.Config{Tenant: "lab", Receiver: r, Sink: a.p, Directory: stow.NewDirectory()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ep.Start(a.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ep.Stop(context.Background()) })
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range studyFiles {
		w, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/dicom"}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(corpusBytes(t, f))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://archive.test/tenants/lab/dicom-web/studies", &body)
	req.Header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+mw.Boundary())
	req.Header.Set("Accept", "application/dicom+json")
	rec := httptest.NewRecorder()
	ep.Serve(a.ctx, rec, req, stow.Request{Base: "http://archive.test/tenants/lab/dicom-web",
		Caller: tenant.Caller{Address: "192.0.2.7:51234"}, Association: "req-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("STOW-RS answered %d: %s", rec.Code, rec.Body.String())
	}
	rs := a.settle(1)
	if len(rs) != 1 || rs[0].State != reception.Done || rs[0].Counts.FilesArchived != int64(len(studyFiles)) {
		t.Fatalf("receptions %+v", rs)
	}
	checkDeidentified(t, py, a.archived(rs[0]))
}
