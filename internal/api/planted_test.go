package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The identifiers planted in the corpus file for the seeded-identifier
// test: made up, each of the length of the value it replaces (a name and a
// Patient ID), and a birth date the file did not have, so each can be
// searched for in every surface without matching anything else.
const (
	plantedName  = "Qvrnx^Yla"
	plantedID    = "ZK4471QX"
	plantedBirth = "19370521"
)

// plantedFile returns the corpus file ct5n-2062.dcm with the planted
// identifiers: Patient's Name and Patient ID replaced, and a Patient's Birth
// Date written into its empty element (Explicit VR Little Endian, no group
// length in group 0010, so only that element's length changes).
func plantedFile(t testing.TB) []byte {
	t.Helper()
	b := corpusFile(t, "ct5n-2062.dcm")
	for old, planted := range map[string]string{ctName: plantedName, ctID: plantedID} {
		if bytes.Count(b, []byte(old)) != 1 {
			t.Fatalf("the corpus file holds %q %d times", old, bytes.Count(b, []byte(old)))
		}
		b = bytes.Replace(b, []byte(old), []byte(planted), 1)
	}
	emptyBirth := []byte{0x10, 0x00, 0x30, 0x00, 'D', 'A', 0x00, 0x00}
	if bytes.Count(b, emptyBirth) != 1 {
		t.Fatal("the corpus file has no empty Patient's Birth Date")
	}
	return bytes.Replace(b, emptyBirth, append([]byte{0x10, 0x00, 0x30, 0x00, 'D', 'A', 0x08, 0x00}, plantedBirth...), 1)
}

// planted reports which planted identifier text holds, or "".
func planted(text string) string {
	for _, p := range []string{plantedName, plantedID, plantedBirth, "Qvrnx", "ZK4471"} {
		if strings.Contains(text, p) {
			return p
		}
	}
	return ""
}

// surface is what one call answered, as text: its answer in the contract's
// JSON and its error's code and message.
func surface(o outcome) string {
	var b strings.Builder
	if o.res != nil {
		j, _ := protojson.Marshal(o.res)
		b.Write(j)
		// A file's bytes, which the JSON writes in base64, as they are.
		if r, ok := o.res.(*v1.ReadFilesResponse); ok {
			b.Write(r.GetChunk().GetData())
		}
	}
	if o.err != nil {
		// The details' bytes as text: a string a detail holds is in them
		// as it is.
		fmt.Fprintf(&b, " error %v: %s", o.code(), o.err)
		var ce *connect.Error
		if errors.As(o.err, &ce) {
			for _, d := range ce.Details() {
				fmt.Fprintf(&b, " detail %s %q %v", d.Type, d.Value, d.Debug)
			}
		}
	}
	return b.String()
}

// TestPlantedIdentifiersReachNoMaskedKey is the seeded-identifier test: a
// file with planted identifiers (a made-up name, Patient ID and birth date)
// is archived as received, held in quarantine and tried, and every surface
// a masked key reaches is searched for them: the answer of every method
// that can carry header data (trial, explanation, quarantine records and
// their headers, value codes, lookups, metadata, files, DICOMweb, the
// download, the catalog, the event stream, receptions, a send job), its
// errors and a canceled trial; trials and explanations of the held file,
// one with a value pattern, which a masked key is refused; a masked
// admin's rotation of a plain key, which must be refused; and, after
// everything, both audit logs as masked admins read them (no planted
// identifier, no UID as received), the server's log, both audit logs and
// the tenant's event feed. A plain key without show_values is searched the
// same way over the previews (trial, explanation, header). The plain key's
// trial with show_values must show them: the search is shown to find what
// is there.
func TestPlantedIdentifiersReachNoMaskedKey(t *testing.T) {
	f := newPlaceFixture(t)
	file := plantedFile(t)
	a := receiveArchive(t, f, [][]byte{file})
	path := a.scans(t)[0]
	in := a.held(t, path)[0]
	storeVersion(t, f.env, "research", basicDocument())
	held := f.heldFile(t, "scanner1", false)
	read := func(tenant.QuarantinedFile) ([]byte, error) { return file, nil }
	f.parts.heldFile.Store(&read)
	header := `{"00100010":{"vr":"PN","Value":[{"Alphabetic":"` + plantedName + `"}]},"00100020":{"vr":"LO","Value":["` + plantedID + `"]},"00100030":{"vr":"DA","Value":["` + plantedBirth + `"]}}`
	h := func(tenant.QuarantinedFile, int) (string, bool, error) { return header, false, nil }
	f.parts.header.Store(&h)
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	maskedInst, _ := f.maskedKey(onInstallation(master.RoleAdmin))
	receptions, _, err := a.p.ListReceptions(ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1})
	if err != nil || len(receptions) != 1 {
		t.Fatalf("the archive's reception: %v %v", receptions, err)
	}
	receptionID := receptions[0].ID
	sess, err := a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}

	sent := []*v1.TrialFile{{Name: "planted.dcm", Content: file}}
	research := storedVersion("research", 1)
	previews := map[string]func(clients) outcome{
		"try, a sent file": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: research, Files: sent})
		},
		"explain, a sent file": func(c clients) outcome {
			return call(c.profiles.ExplainProfile, &v1.ExplainProfileRequest{Tenant: "lab1", Profile: research, File: sent[0]})
		},
	}
	// A held file's previews: a plain key's, values hidden, answer none;
	// a masked key is refused them.
	heldPreviews := map[string]func(clients) outcome{
		"try, a held file": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: research, QuarantineRecords: []string{held}})
		},
		"explain, a held file": func(c clients) outcome {
			return call(c.profiles.ExplainProfile, &v1.ExplainProfileRequest{Tenant: "lab1", Profile: research, QuarantineRecord: held})
		},
		"try, a value pattern over a held file": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", QuarantineRecords: []string{held}, Profile: unsaved(&v1.ProfileDocument{Rules: []*v1.ProfileRule{{Name: "guess",
				Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: "Q.*", Vr: "PN"}}},
				Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}}})})
		},
	}
	maskedOnly := map[string]func(clients) outcome{
		"try with values": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: research, Files: sent, ShowValues: true})
		},
		"try, an unsaved document with a problem": func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: unsaved(&v1.ProfileDocument{Bases: []string{"no-such-base"}}), Files: sent})
		},
		"try, canceled": func(c clients) outcome {
			cctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			cctx, info := connect.NewClientContext(cctx)
			_, err := c.profiles.TryProfile(cctx, &v1.TryProfileRequest{Tenant: "lab1", Profile: research, Files: sent})
			return outcome{err: err, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
		},
		"quarantine record": func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held})
		},
		"quarantine header": func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true})
		},
		"quarantine header with values": func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true, ShowValues: true})
		},
		"quarantine list": func(c clients) outcome {
			return call(c.quarantine.ListQuarantinedFiles, &v1.ListQuarantinedFilesRequest{Tenant: "lab1"})
		},
		"code of the planted ID": func(c clients) outcome {
			return call(c.deid.Code, &v1.CodeRequest{Tenant: "lab1", Value: plantedID})
		},
		"import of the planted ID": func(c clients) outcome {
			return call(c.deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "lab1", Table: "ids", Entries: []*v1.LookupEntry{{Value: plantedID, Code: "P1"}}})
		},
		"scan metadata": func(c clients) outcome { return call(c.metadata.GetScanMetadata, scanRequest(path)) },
		"instance metadata": func(c clients) outcome {
			return call(c.metadata.GetInstanceMetadata, instanceRequest(path, in.SOPInstanceUID))
		},
		"read files": func(c clients) outcome {
			return readFilesAll(c, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject})
		},
		"sessions": func(c clients) outcome {
			return call(c.catalog.ListSessions, &v1.ListSessionsRequest{Tenant: "lab1", SubjectLabel: path.Subject})
		},
		"scan": func(c clients) outcome {
			return call(c.catalog.GetScan, &v1.GetScanRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan})
		},
		"events": func(c clients) outcome { return watchOnce(c, &v1.WatchRequest{Scope: tenantScope("lab1")}) },
		"receptions": func(c clients) outcome {
			return call(c.receptions.ListReceptions, &v1.ListReceptionsRequest{Tenant: "lab1"})
		},
		"reception": func(c clients) outcome {
			return call(c.receptions.GetReception, &v1.GetReceptionRequest{Tenant: "lab1", Id: receptionID})
		},
		"a send job": func(c clients) outcome {
			return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Send{Send: target("pacs", path.Subject)}})
		},
	}
	search := func(who, name string, o outcome) {
		t.Helper()
		if p := planted(surface(o)); p != "" {
			t.Errorf("%s, %s: the planted %q is in the answer: %s", who, name, p, clipBody([]byte(surface(o))))
		}
	}
	for name, c := range previews {
		for who, as := range map[string]caller{"masked key": masked, "plain key without show_values": f.lab1Operator} {
			o := c(f.as(as))
			if o.err != nil {
				// A preview that failed would search nothing.
				t.Errorf("%s, %s: %v", who, name, o.err)
			}
			search(who, name, o)
		}
	}
	for name, c := range maskedOnly {
		o := c(f.as(masked))
		search("masked key", name, o)
		// A reception's UIDs are the ones received, whatever the profile.
		if strings.HasPrefix(name, "reception") && strings.Contains(surface(o), sess.StudyUID) {
			t.Errorf("masked key, %s: the Study Instance UID as received is in the answer", name)
		}
	}
	for name, c := range heldPreviews {
		search("masked key", name, c(f.as(masked)))
		o := c(f.as(f.lab1Operator))
		if o.err != nil {
			t.Errorf("plain key without show_values, %s: %v", name, o.err)
		}
		search("plain key without show_values", name, o)
	}
	// A masked admin of the installation rotating a plain key would hold a
	// plain successor, which reads everything the planted file holds.
	if o := call(f.as(maskedInst).access.RotateKey, &v1.RotateKeyRequest{Id: f.noGrantID}); o.err == nil {
		t.Errorf("a masked admin rotated a plain key: %v", o.res)
	}
	s := serveWeb(t, f.env)
	for _, p := range []string{
		downloadPath(path, in.SOPInstanceUID),
		webPath("studies?includefield=all"),
		webPath("studies/" + sess.StudyUID + "/metadata"),
		webPath("studies/" + sess.StudyUID + "/instances?includefield=all"),
		webPath("studies/" + sess.StudyUID),
	} {
		got := s.get(t, http.MethodGet, p, masked, "Accept", "*/*")
		if pl := planted(string(got.body)); pl != "" {
			t.Errorf("masked key, %s: the planted %q is in the answer (%d)", p, pl, got.status)
		}
	}
	h2 := `{"00100010":{"vr":"PN","Value":[{"Alphabetic":"` + plantedName + `"}]}}`
	hv := func(tenant.QuarantinedFile, int) (string, bool, error) { return h2, false, nil }
	f.parts.header.Store(&hv)
	search("plain key without show_values", "quarantine header", call(f.as(f.lab1Operator).quarantine.GetQuarantinedFile,
		&v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true}))

	// The search finds what is there: the plain key's trial with values.
	shown := call(f.as(f.lab1Operator).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: research, Files: sent, ShowValues: true})
	if planted(surface(shown)) == "" {
		t.Fatalf("the plain key's trial with show_values shows no planted identifier: %v", shown.err)
	}

	// The audit logs as masked admins read them, after every call above,
	// hold no planted identifier and no UID as received.
	f.flush()
	for name, o := range map[string]outcome{
		"the tenant's audit log":       call(f.as(masked).audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: tenantScope("lab1"), PageSize: 1000}),
		"the installation's audit log": call(f.as(maskedInst).audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: installation(), PageSize: 1000}),
	} {
		if o.err != nil {
			t.Errorf("masked admin, %s: %v", name, o.err)
			continue
		}
		if len(o.res.(*v1.ListAuditEntriesResponse).GetEntries()) == 0 {
			t.Errorf("masked admin, %s: no entry; the search searched nothing", name)
		}
		search("masked admin", name, o)
		if strings.Contains(surface(o), sess.StudyUID) {
			t.Errorf("masked admin, %s: the Study Instance UID as received is in the answer", name)
		}
	}

	// Nothing written anywhere holds one: the log, both audit logs, and the
	// tenant's event feed.
	if p := planted(f.log.String()); p != "" {
		t.Errorf("the planted %q is in the server's log", p)
	}
	for name, log := range map[string][]master.AuditEntry{"installation": f.masterLog(), "lab1": f.tenantLog("lab1")} {
		for _, en := range log {
			if p := planted(fmt.Sprintf("%+v", en)); p != "" {
				t.Errorf("the planted %q is in the %s audit log: %+v", p, name, en)
			}
		}
	}
	events, err := f.pipelineOf("lab1").(tenant.Events).ReadEvents(ctx, "", nil, tenant.MaxEventPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) == 0 {
		t.Fatal("the reception wrote no event; the search of the feed searched nothing")
	}
	for _, ev := range events.Events {
		if p := planted(fmt.Sprintf("%+v", ev.Payload)); p != "" {
			t.Errorf("the planted %q is in an event: %+v", p, ev)
		}
	}
}
