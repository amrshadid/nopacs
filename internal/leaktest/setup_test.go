package leaktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// caller is a key the test holds: its id, its secret and what it may do.
type caller struct {
	// name says what the key is.
	name string
	id   string
	// secret is the key's secret, shown once when it was made.
	secret string
	// installation is true for a grant on the installation, false for one on
	// the tenant lab1.
	installation bool
	// role is the grant's role, as the API names it; empty for none.
	role string
}

// scanRef names a scan and one of its instances as the catalog does.
type scanRef struct {
	subject, session, scan, instanceID, sopUID, seriesUID string
}

// world is what the setup made: the names and ids the calls need.
type world struct {
	tenant, other string
	// deid is a scan a receiver that de-identifies archived; raw is one the
	// receiver that archives as received did.
	deid, raw scanRef
	// quarantined are the files held back, by reason: residual value and
	// text-list term.
	quarantined []string
	receptions  []string
	jobID       string
	// holdID is a hold the setup placed on the de-identified subject.
	holdID string
	// own is a seeded file's bytes, which a key sends to a trial as its own.
	own []byte
	// victim and plainVictim are keys the calls may revoke, rotate and
	// regrant: a masked key and a key that is not masked.
	victim, plainVictim caller
	// scanner is the key that sends over STOW-RS; it is not masked.
	scanner caller
	keys    []caller
	// profile is the document of the profile version the receivers use.
	profile string
	// ports are the DIMSE receivers' ports.
	deidPort, rawPort int
}

// profileDocument returns the profile the receivers de-identify with: the
// basic profile of PS3.15 Annex E, a few elements the public files carry
// that name a maker removed, Image Comments cleaned of the staff list's
// terms, and Series Description kept, so a file that holds a removed value
// or a term there is held back by the check after.
func profileDocument() string {
	terms, _ := json.Marshal(staffList())
	rm := func(name, kw string) string {
		return `{"name":"` + name + `","selector":{"keyword":"` + kw + `"},"action":{"remove":{}}}`
	}
	return `{"bases":["annex-e/basic:2026d"],"textLists":[{"name":"staff","version":1,"terms":` + string(terms) + `}],"rules":[` +
		rm("content-label", "ContentLabel") + `,` + rm("scan-options", "ScanOptions") + `,` +
		rm("maker", "Manufacturer") + `,` + rm("model", "ManufacturerModelName") + `,` +
		`{"name":"clean-comments","selector":{"keyword":"ImageComments"},"action":{"clean":{"lists":["staff"]}}},` +
		`{"name":"keep-series-description","selector":{"keyword":"SeriesDescription"},"action":{"keep":{}},"reason":"a site's series names are wanted"}]}`
}

// storescu returns dcmtk's storescu, or skips the test.
func storescu(t *testing.T) string {
	t.Helper()
	var candidates []string
	if d := os.Getenv("NOPACS_DCMTK"); d != "" {
		candidates = append(candidates, filepath.Join(d, "storescu"))
	}
	candidates = append(candidates, "/opt/homebrew/bin/storescu")
	if p, err := exec.LookPath("storescu"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _ := exec.CommandContext(ctx, p, "--version").CombinedOutput() // #nosec G204 G702 -- a test's own tool.
		cancel()
		if bytes.Contains(out, []byte("dcmtk")) {
			return p
		}
	}
	if os.Getenv("NOPACS_REQUIRE_DCMTK") != "" {
		t.Fatal("dcmtk's storescu is not installed, and NOPACS_REQUIRE_DCMTK is set")
	}
	t.Skip("dcmtk's storescu is not installed; put it on the PATH or set NOPACS_DCMTK")
	return ""
}

// makeKey makes a key as the local operator and grants it role on the
// installation or on lab1 (no grant when role is empty).
func (s *site) makeKey(name string, masked, installation bool, role string) caller {
	s.t.Helper()
	var made struct {
		Key    struct{ ID string } `json:"key"`
		Secret string              `json:"secret"`
	}
	s.postInto("/nopacs.v1.AccessService/CreateKey", fmt.Sprintf(`{"holder":%q,"masked":%v,"neverExpire":true}`, name, masked), &made)
	if made.Secret == "" || made.Key.ID == "" {
		s.t.Fatalf("CreateKey %s answered no key", name)
	}
	if role != "" {
		scope := `{"tenant":"lab1"}`
		if installation {
			scope = `{"installation":{}}`
		}
		s.post("/nopacs.v1.AccessService/SetGrant", fmt.Sprintf(`{"keyId":%q,"scope":%s,"role":%q}`, made.Key.ID, scope, role))
	}
	return caller{name: name, id: made.Key.ID, secret: made.Secret, installation: installation, role: role}
}

// sendDIMSE sends files to a receiver's port with dcmtk's storescu.
func sendDIMSE(t *testing.T, scu string, port int, files ...string) {
	t.Helper()
	args := append([]string{"-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port)}, files...)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, scu, args...).CombinedOutput(); err != nil { // #nosec G204 -- a test's own tool and files.
		t.Fatalf("storescu: %v\n%s", err, out)
	}
}

// sendSTOW stores files into a STOW-RS receiver with the scanner's key
// (PS3.18 10.5) and requires the request to be accepted.
func (s *site) sendSTOW(key string, files ...string) {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- the test's own files.
		if err != nil {
			s.t.Fatal(err)
		}
		w, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/dicom"}})
		if err != nil {
			s.t.Fatal(err)
		}
		_, _ = w.Write(b)
	}
	_ = mw.Close()
	req, _ := http.NewRequestWithContext(s.t.Context(), http.MethodPost, "http://"+s.listen+"/tenants/lab1/dicom-web/studies", &buf)
	req.Header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+mw.Boundary())
	req.Header.Set("Accept", "application/dicom+json")
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusAccepted && res.StatusCode != http.StatusOK {
		s.t.Fatalf("STOW-RS: %d %s", res.StatusCode, body)
	}
}

// setUp makes the tenants, storages, profile, keys and receivers, sends the
// seeded files in each way, waits until they are archived or held, and
// returns what it made. Everything it does is the local operator's; the
// masked keys act afterwards.
func (s *site) setUp(t *testing.T, files []seedFile) *world {
	t.Helper()
	scu := storescu(t)
	w := &world{tenant: "lab1", other: "lab2", profile: profileDocument(), deidPort: freePort(t), rawPort: freePort(t)}
	for w.rawPort == w.deidPort {
		w.rawPort = freePort(t)
	}
	setting := func(name, scope, value string) {
		s.post("/nopacs.v1.SettingsService/SetSetting", fmt.Sprintf(`{"setting":{"name":%q,"scope":%s,"value":%q}}`, name, scope, value))
	}
	setting("storage.directory-roots", `{"installation":{}}`, "fast="+s.roots)
	// Every key and every refusal comes from one address, which the
	// server would block after twenty failures: the test is of what answers
	// say, not of that bound.
	setting("access.failure-limit", `{"installation":{}}`, "10000")
	setting("sender.destinations", `{"installation":{}}`, "127.0.0.1")
	for _, tn := range []string{w.tenant, w.other} {
		s.post("/nopacs.v1.TenantService/CreateTenant", fmt.Sprintf(`{"name":%q,"databaseType":"DATABASE_TYPE_SQLITE"}`, tn))
	}
	setting("receiver.ports", `{"tenant":"lab1"}`, fmt.Sprintf("%d,%d", w.deidPort, w.rawPort))
	setting("receiver.allow-as-received", `{"tenant":"lab1"}`, "true")
	for _, name := range []string{"main", "raw", "spare"} {
		s.post("/nopacs.v1.StorageService/AddStorage", fmt.Sprintf(`{"tenant":"lab1","storage":{"name":%q,"directory":{"root":"fast","path":%q}}}`, name, name))
	}
	s.post("/nopacs.v1.ProfileService/CreateProfileVersion", `{"tenant":"lab1","name":"research","document":`+w.profile+`}`)

	w.scanner = s.makeKey("scanner", false, false, "ROLE_WRITER")
	w.victim = s.makeKey("victim", true, false, "")
	w.plainVictim = s.makeKey("plain-victim", false, false, "")
	dimse := func(name string, port int, storage, how string) {
		s.post("/nopacs.v1.ReceiverService/AddReceiver", fmt.Sprintf(`{"tenant":"lab1","receiver":{"name":%q,"enabled":true,"storage":%q,"quietPeriod":"5s",%s,`+
			`"dimse":{"port":%d,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`, name, storage, how, port))
	}
	dimse("scan-deid", w.deidPort, "main", `"profile":{"name":"research","version":1}`)
	dimse("scan-raw", w.rawPort, "raw", `"asReceived":{}`)
	s.post("/nopacs.v1.ReceiverService/AddReceiver", fmt.Sprintf(`{"tenant":"lab1","receiver":{"name":"web-deid","enabled":true,"storage":"main","quietPeriod":"5s",`+
		`"profile":{"name":"research","version":1},"stow":{"keyIds":[%q]}}}`, w.scanner.id))
	s.waitFor("the receivers to be up", 30*time.Second, func() bool {
		return strings.Count(string(s.post("/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"lab1"}`)), `"RECEIVER_STATE_UP"`) == 3
	})
	path := func(name string) string {
		for _, f := range files {
			if f.name == name {
				return f.path
			}
		}
		t.Fatalf("no seed %s", name)
		return ""
	}
	sendDIMSE(t, scu, w.deidPort, path("deid-dimse"), path("held-residual"))
	sendDIMSE(t, scu, w.rawPort, path("raw-dimse"))
	s.sendSTOW(w.scanner.secret, path("deid-stow"), path("held-list-term"))

	var scans struct {
		Scans []struct {
			SubjectLabel, SessionLabel, Label, SeriesInstanceUid, Receiver string
		} `json:"scans"`
	}
	s.waitFor("the files to be archived and held", 90*time.Second, func() bool {
		s.postInto("/nopacs.v1.CatalogService/ListScans", `{"tenant":"lab1"}`, &scans)
		var held struct {
			Files []struct{ ID string } `json:"files"`
		}
		s.postInto("/nopacs.v1.QuarantineService/ListQuarantinedFiles", `{"tenant":"lab1"}`, &held)
		return len(scans.Scans) == 3 && len(held.Files) == 2
	})
	for _, sc := range scans.Scans {
		ref := scanRef{subject: sc.SubjectLabel, session: sc.SessionLabel, scan: sc.Label, seriesUID: sc.SeriesInstanceUid}
		var md struct {
			Instances []struct{ InstanceId, SopInstanceUid string } `json:"instances"`
		}
		s.postInto("/nopacs.v1.MetadataService/GetScanMetadata", fmt.Sprintf(`{"tenant":"lab1","subjectLabel":%q,"sessionLabel":%q,"scanLabel":%q}`,
			ref.subject, ref.session, ref.scan), &md)
		if len(md.Instances) == 0 {
			t.Fatalf("scan %s/%s/%s holds no instance", ref.subject, ref.session, ref.scan)
		}
		ref.instanceID, ref.sopUID = md.Instances[0].InstanceId, md.Instances[0].SopInstanceUid
		if sc.Receiver == "scan-raw" {
			w.raw = ref
		} else if w.deid.subject == "" {
			w.deid = ref
		}
	}
	var held struct {
		Files []struct{ ID string } `json:"files"`
	}
	s.postInto("/nopacs.v1.QuarantineService/ListQuarantinedFiles", `{"tenant":"lab1"}`, &held)
	for _, f := range held.Files {
		w.quarantined = append(w.quarantined, f.ID)
	}
	var recs struct {
		Receptions []struct{ ID string } `json:"receptions"`
	}
	s.postInto("/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"lab1"}`, &recs)
	for _, r := range recs.Receptions {
		w.receptions = append(w.receptions, r.ID)
	}
	if w.raw.subject == "" || w.deid.subject == "" || len(w.quarantined) != 2 || len(w.receptions) == 0 {
		t.Fatalf("the setup found %+v", w)
	}
	var job struct {
		Job struct{ ID string } `json:"job"`
	}
	s.postInto("/nopacs.v1.JobService/CreateJob", `{"tenant":"lab1","scrub":{"storage":"main"}}`, &job)
	w.jobID = job.Job.ID
	s.post("/nopacs.v1.SenderService/AddSender", `{"tenant":"lab1","sender":{"name":"pacs","enabled":true,"dimse":{"host":"127.0.0.1","port":1,"calledAeTitle":"PACS","callingAeTitle":"NOPACS"}}}`)
	s.post("/nopacs.v1.RetentionService/PlaceHold", fmt.Sprintf(`{"tenant":"lab1","target":{"subjectLabel":%q},"reason":"a hold for the test"}`, w.deid.subject))
	var holds struct {
		Holds []struct{ ID string } `json:"holds"`
	}
	s.postInto("/nopacs.v1.RetentionService/ListHolds", `{"tenant":"lab1"}`, &holds)
	if len(holds.Holds) != 1 {
		t.Fatalf("the setup placed one hold and found %d", len(holds.Holds))
	}
	w.holdID = holds.Holds[0].ID
	for _, f := range files {
		if f.name == "deid-dimse" {
			b, err := os.ReadFile(f.path) // #nosec G304 -- the test's own file.
			if err != nil {
				t.Fatal(err)
			}
			w.own = b
		}
	}

	// The keys that act: one of every grant, the lowest first, so that the
	// ones that may change the most come last.
	for _, k := range []struct {
		name string
		inst bool
		role string
	}{
		{"no-grant", false, ""},
		{"tenant-reader", false, "ROLE_READER"},
		{"installation-reader", true, "ROLE_READER"},
		{"tenant-writer", false, "ROLE_WRITER"},
		{"tenant-operator", false, "ROLE_OPERATOR"},
		{"installation-operator", true, "ROLE_OPERATOR"},
		{"tenant-admin", false, "ROLE_ADMIN"},
		{"installation-admin", true, "ROLE_ADMIN"},
	} {
		w.keys = append(w.keys, s.makeKey(k.name, true, k.inst, k.role))
	}
	return w
}
