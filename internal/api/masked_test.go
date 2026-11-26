package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maskedKey makes a masked key in the master with the grants given, as
// "key create --masked" stores one, and returns it as a caller with its id.
func (e *env) maskedKey(grants ...master.Grant) (caller, string) {
	e.t.Helper()
	made, err := secret.MakeAPIKey()
	if err != nil {
		e.t.Fatal(err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "an agent", Created: t0, Masked: true}
	if err := e.master.CreateKey(ctx, k, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	for _, g := range grants {
		g.KeyID = k.ID
		if _, err := e.master.SetGrant(ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
	return caller{key: made.Key()}, k.ID
}

// maskedRefusal is the audit entry of a request the check refused to a
// masked key: in the installation's log, refused, with the mark.
func maskedRefusal() *wantAudit {
	return &wantAudit{outcome: master.OutcomeRefused, reason: "masked key", details: map[string]string{access.DetailMasked: "true"}}
}

// streamFirst opens a server stream with open, reads its first message and
// ends it, as an outcome: enough to see whether the call was refused.
func streamFirst[Res any](open func(context.Context) (interface {
	Receive() (Res, error)
	Close() error
}, error)) outcome {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cctx, info := connect.NewClientContext(cctx)
	s, err := open(cctx)
	if err != nil {
		return outcome{err: err, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
	}
	defer func() { _ = s.Close() }()
	_, err = s.Receive()
	return outcome{err: err, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
}

// readFilesAll reads a ReadFiles stream to its end, as an outcome: its
// error, or none when it ended whole; res is one chunk holding the bytes of
// every chunk read, so a search of the answer searches every file sent.
func readFilesAll(c clients, req *v1.ReadFilesRequest) outcome {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cctx, info := connect.NewClientContext(cctx)
	s, err := c.data.ReadFiles(cctx, req)
	if err != nil {
		return outcome{err: err, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
	}
	defer func() { _ = s.Close() }()
	chunk := &v1.FileChunk{}
	all := &v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_Chunk{Chunk: chunk}}
	for {
		msg, err := s.Receive()
		if errors.Is(err, io.EOF) {
			return outcome{res: all, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
		}
		if err != nil {
			// What was sent before the error is kept for a search.
			return outcome{res: all, err: err, requestID: info.ResponseHeader().Get("Nopacs-Request-Id")}
		}
		chunk.Data = append(chunk.Data, msg.GetChunk().GetData()...)
	}
}

// TestMaskedKeyIsRefusedOriginals guards every request that answers
// original values whatever it names: each is refused to a masked key whose
// role suffices, as permission denied with the fixed message, and recorded
// refused in the installation's log with the mark; the same call by a plain
// key with the same role passes the check.
func TestMaskedKeyIsRefusedOriginals(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	storeVersion(t, f.env, "research", basicDocument())
	maskedAdmin, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	maskedInst, _ := f.maskedKey(onInstallation(master.RoleAdmin))
	ct := corpusFile(t, "ct5n-2062.dcm")
	calls := []struct {
		name   string
		plain  caller
		masked caller
		call   func(clients) outcome
	}{
		{"a quarantined file's header", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true})
		}},
		{"a quarantined file's header with its values", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true, ShowValues: true})
		}},
		{"a search of quarantined files by a UID as received", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.quarantine.ListQuarantinedFiles, &v1.ListQuarantinedFilesRequest{Tenant: "lab1", StudyInstanceUid: "2.25.1234567890"})
		}},
		{"a trial's values", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), ShowValues: true,
				Files: []*v1.TrialFile{{Name: "a.dcm", Content: ct}}})
		}},
		{"a trial of a held file, values hidden", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecords: []string{held}})
		}},
		{"an explanation of a held file", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.profiles.ExplainProfile, &v1.ExplainProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), QuarantineRecord: held})
		}},
		{"a search of receptions by a UID as received", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.receptions.ListReceptions, &v1.ListReceptionsRequest{Tenant: "lab1", StudyInstanceUid: "2.25.1234567890"})
		}},
		{"a value's codes", f.lab1Admin, maskedAdmin, func(c clients) outcome {
			return call(c.deid.Code, &v1.CodeRequest{Tenant: "lab1", Value: "12345"})
		}},
		{"an import of lookup codes", f.lab1Admin, maskedAdmin, func(c clients) outcome {
			return call(c.deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "lab1", Table: "sites", Entries: []*v1.LookupEntry{{Value: "12345", Code: "S1"}}})
		}},
		{"an import's files", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.jobs.ListImportFiles, &v1.ListImportFilesRequest{Tenant: "lab1", Id: "id-0000001"})
		}},
		{"an import's units", f.lab1Operator, maskedAdmin, func(c clients) outcome {
			return call(c.jobs.ListImportUnits, &v1.ListImportUnitsRequest{Tenant: "lab1", Id: "id-0000001"})
		}},
		{"a backup", f.instAdmin, maskedInst, func(c clients) outcome {
			return streamFirst(func(cctx context.Context) (interface {
				Receive() (*v1.CreateBackupResponse, error)
				Close() error
			}, error) {
				return c.backups.CreateBackup(cctx, &v1.CreateBackupRequest{})
			})
		}},
		{"an export", f.instAdmin, maskedInst, func(c clients) outcome {
			return streamFirst(func(cctx context.Context) (interface {
				Receive() (*v1.ExportTenantResponse, error)
				Close() error
			}, error) {
				return c.backups.ExportTenant(cctx, &v1.ExportTenantRequest{Tenant: "nolab"})
			})
		}},
	}
	for _, c := range calls {
		o := c.call(f.as(c.masked))
		if o.code() != connect.CodePermissionDenied || o.message() != access.MessageMasked {
			t.Errorf("%s, masked: %v (%v), want permission denied: %s", c.name, o.code(), o.err, access.MessageMasked)
			continue
		}
		f.checkAudit(t, c.name+", masked", o.requestID, *maskedRefusal())
		if p := c.call(f.as(c.plain)); p.code() == connect.CodePermissionDenied {
			t.Errorf("%s, plain key with the role: refused: %v", c.name, p.err)
		}
	}
}

// TestShowValues guards the values a preview hides by default: a trial
// answers each element's lengths and whether it changed, and no value,
// unless show_values asks, which is recorded in the run's entry; a
// quarantined file's header is answered without values (its VR and
// structure kept) unless show_values asks, also recorded; show_values
// without include_header is invalid; and a masked key is refused both.
func TestShowValues(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	storeVersion(t, f.env, "research", basicDocument())
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleOperator))
	ct := corpusFile(t, "ct5n-2062.dcm")
	try := func(show bool) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: storedVersion("research", 1), ShowValues: show,
				Files: []*v1.TrialFile{{Name: "a.dcm", Content: ct}}})
		}
	}
	name := func(o outcome) *v1.TrialElement {
		return elementAt(o.res.(*v1.TryProfileResponse).GetResults()[0], "00100010")
	}
	f.run(t, []tcase{
		{name: "a trial, values hidden", as: f.lab1Operator, call: try(false),
			check: func(t *testing.T, o outcome) {
				el := name(o)
				if el == nil || el.GetBefore() != "" || el.GetAfter() != "" || el.GetBeforeLength() != int32(len(ctName)) || !el.GetChanged() || el.GetVr() != "PN" || el.GetAction() == "" {
					t.Errorf("Patient Name without values: %v", el)
				}
				for _, r := range o.res.(*v1.TryProfileResponse).GetResults() {
					for _, e := range r.GetElements() {
						if e.GetBefore() != "" || e.GetAfter() != "" {
							t.Errorf("a value answered without show_values: %v", e)
						}
					}
				}
			}},
		{name: "a trial, values shown", as: f.lab1Operator, call: try(true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"show_values": "true", "files": "1"}},
			check: func(t *testing.T, o outcome) {
				if el := name(o); el == nil || el.GetBefore() != ctName || el.GetBeforeLength() != int32(len(ctName)) {
					t.Errorf("Patient Name with values: %v", el)
				}
			}},
		{name: "a trial, values asked by a masked key", as: masked, call: try(true), code: connect.CodePermissionDenied, msg: access.MessageMasked, audit: maskedRefusal()},
		{name: "a trial, values hidden, by a masked key", as: masked, call: try(false), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{access.DetailMasked: "true"}},
			check: func(t *testing.T, o outcome) {
				if el := name(o); el == nil || el.GetBefore() != "" || el.GetBeforeLength() == 0 {
					t.Errorf("a masked key's trial: %v", el)
				}
			}},
	})
	header := `{"00100010":{"vr":"PN","Value":[{"Alphabetic":"Doe^Peter"}]},"00400275":{"vr":"SQ","Value":[{"00401001":{"vr":"SH","Value":["RP-77"]}}]},"7FE00010":{"vr":"OW","InlineBinary":"AAAA"},"00081030":{"vr":"LO"}}`
	h := func(tenant.QuarantinedFile, int) (string, bool, error) { return header, false, nil }
	f.parts.header.Store(&h)
	get := func(show bool) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, IncludeHeader: true, ShowValues: show})
		}
	}
	f.run(t, []tcase{
		{name: "a header, values hidden", as: f.lab1Operator, call: get(false),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"include_header": "true"}},
			check: func(t *testing.T, o outcome) {
				want := `{"00081030":{"vr":"LO"},"00100010":{"vr":"PN"},"00400275":{"Value":[{"00401001":{"vr":"SH"}}],"vr":"SQ"},"7FE00010":{"vr":"OW"}}`
				if got := o.res.(*v1.GetQuarantinedFileResponse).GetHeaderJson(); got != want {
					t.Errorf("header without values:\n%s\nwant:\n%s", got, want)
				}
			}},
		{name: "a header, values shown", as: f.lab1Operator, call: get(true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"include_header": "true", "show_values": "true"}},
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.GetQuarantinedFileResponse).GetHeaderJson(); got != header {
					t.Errorf("header with values: %s", got)
				}
			}},
		{name: "values without a header", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: held, ShowValues: true})
		}, code: connect.CodeInvalidArgument, msg: "show_values"},
		{name: "a header, by a masked key", as: masked, call: get(false), code: connect.CodePermissionDenied, msg: access.MessageMasked, audit: maskedRefusal()},
	})
}

// TestMaskedKeyReadsRecordsWithoutOriginals guards the quarantine records a
// masked key is answered: without the UIDs as received and the details that
// hold one, which a plain reader is answered; the rest of the record as it
// is.
func TestMaskedKeyReadsRecordsWithoutOriginals(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	id := f.newID()
	q := tenant.QuarantinedFile{
		ID: id, Reason: tenant.QuarantineUIDElsewhere, Receiver: "scanner1",
		Details: map[string]string{"study_instance_uid": "2.25.4242", "series_instance_uid": "2.25.4343"},
		Caller:  tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"},
		Size:    10, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", SOPInstanceUID: "2.25.4444", StudyUID: "2.25.4545", SeriesUID: "2.25.4646",
		TransferSyntaxUID: "1.2.840.10008.1.2.1", Incoming: "q/" + id, ContentKept: true, Quarantined: f.now(),
	}
	if _, err := f.pipelineOf("lab1").HoldFiles(ctx, []tenant.QuarantinedFile{q}); err != nil {
		t.Fatal(err)
	}
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleOperator))
	get := func(c caller) *v1.QuarantinedFile {
		return mustCall[*v1.GetQuarantinedFileResponse](t, "get", call(f.as(c).quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: id})).GetFile()
	}
	listed := func(c caller) *v1.QuarantinedFile {
		return mustCall[*v1.ListQuarantinedFilesResponse](t, "list", call(f.as(c).quarantine.ListQuarantinedFiles, &v1.ListQuarantinedFilesRequest{Tenant: "lab1"})).GetFiles()[0]
	}
	for name, r := range map[string]*v1.QuarantinedFile{"get": get(masked), "list": listed(masked)} {
		if r.GetStudyInstanceUid() != "" || r.GetSeriesInstanceUid() != "" || r.GetSopInstanceUid() != "" || len(r.GetDetails()) != 0 {
			t.Errorf("%s, masked: %v", name, r)
		}
		if r.GetSopClassUid() != q.SOPClassUID || r.GetTransferSyntaxUid() != q.TransferSyntaxUID || r.GetCaller().GetCallingAeTitle() != "PRISMA1" || r.GetReason() != v1.QuarantineReason_QUARANTINE_REASON_UID_ELSEWHERE {
			t.Errorf("%s, masked, lost what is not an original: %v", name, r)
		}
	}
	if r := get(f.lab1Reader); r.GetStudyInstanceUid() != "2.25.4545" || r.GetDetails()["study_instance_uid"] != "2.25.4242" {
		t.Errorf("a plain reader: %v", r)
	}
}

// TestAsReceivedDataIsRefusedToAMaskedKey guards the data an archive holds
// as received, the sender's own values: its metadata, its files through
// ReadFiles, the plain HTTP download and DICOMweb (metadata, a search of
// instances, a retrieve) are refused to a masked reader, each recorded with
// the mark, and answered to a plain reader.
func TestAsReceivedDataIsRefusedToAMaskedKey(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "ct5n-2062.dcm")})
	path := a.scans(t)[0]
	in := a.held(t, path)[0]
	if !in.Profile.IsZero() {
		t.Fatal("the instance was de-identified; the test needs one archived as received")
	}
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleReader))
	refused := func(name string, o outcome) {
		t.Helper()
		if o.code() != connect.CodePermissionDenied || o.message() != access.MessageMasked {
			t.Errorf("%s, masked: %v (%v)", name, o.code(), o.err)
			return
		}
		// A stream records its opening and its end; the end is the refusal.
		var found bool
		for _, en := range entriesOf(f.tenantLog("lab1"), o.requestID) {
			if en.Outcome == master.OutcomeRefused && en.Details[access.DetailMasked] == "true" && strings.Contains(en.Reason, "masked key") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no refusal with the mark in lab1's log: %+v", name, entriesOf(f.tenantLog("lab1"), o.requestID))
		}
	}
	rpcs := map[string]func(clients) outcome{
		"scan metadata": func(c clients) outcome { return call(c.metadata.GetScanMetadata, scanRequest(path)) },
		"instance metadata": func(c clients) outcome {
			return call(c.metadata.GetInstanceMetadata, instanceRequest(path, in.SOPInstanceUID))
		},
		"read files": func(c clients) outcome {
			return readFilesAll(c, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject})
		},
	}
	for name, rpc := range rpcs {
		refused(name, rpc(f.as(masked)))
		if o := rpc(f.as(f.lab1Reader)); o.err != nil {
			t.Errorf("%s, plain reader: %v", name, o.err)
		}
	}
	s := serveWeb(t, f.env)
	sess, err := a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	series := "studies/" + sess.StudyUID + "/series/" + sc.SeriesUID
	for _, p := range []string{
		downloadPath(path, in.SOPInstanceUID),
		webPath(series + "/metadata"),
		webPath(series + "/instances"),
		webPath(series + "/instances/" + in.SOPInstanceUID),
		webPath("studies/" + sess.StudyUID),
	} {
		got := s.get(t, http.MethodGet, p, masked, "Accept", "*/*")
		if got.status != http.StatusForbidden || strings.Contains(string(got.body), in.SOPInstanceUID) {
			t.Errorf("%s, masked: %d %q", p, got.status, clipBody(got.body))
		}
		if plain := s.get(t, http.MethodGet, p, f.lab1Reader, "Accept", "*/*"); plain.status != http.StatusOK {
			t.Errorf("%s, plain reader: %d %q", p, plain.status, clipBody(plain.body))
		}
	}
}

// TestMaskedAdminRotatesNoPlainKey guards RotateKey from a masked admin of
// the installation: the rotation of a plain key, whose successor's secret
// would be answered plain, is refused as permission denied with the fixed
// message and recorded refused with the mark; the plain key keeps no
// successor; the same call by a plain admin passes.
func TestMaskedAdminRotatesNoPlainKey(t *testing.T) {
	f := newPlaceFixture(t)
	maskedInst, _ := f.maskedKey(onInstallation(master.RoleAdmin))
	o := call(f.as(maskedInst).access.RotateKey, &v1.RotateKeyRequest{Id: f.noGrantID})
	if o.code() != connect.CodePermissionDenied || o.message() != access.MessageMasked {
		t.Fatalf("a masked admin rotating a plain key: %v (%v)", o.code(), o.err)
	}
	if s := surface(o); strings.Contains(s, "nopacs_") {
		t.Errorf("the refusal holds a key: %s", s)
	}
	f.checkAudit(t, "the refusal", o.requestID, wantAudit{outcome: master.OutcomeRefused, object: f.noGrantID, reason: "masked key", details: map[string]string{access.DetailMasked: "true"}})
	if k, err := f.master.Key(ctx, f.noGrantID); err != nil || k.Successor != "" {
		t.Errorf("the plain key after the refusal: successor %q, %v", k.Successor, err)
	}
	if p := call(f.as(f.instAdmin).access.RotateKey, &v1.RotateKeyRequest{Id: f.noGrantID}); p.err != nil {
		t.Errorf("a plain admin rotating the key: %v", p.err)
	}
}

// TestHeldFileTrialIsNoOracle guards a trial of a held file with a value
// pattern the caller writes: what it answers (the pattern's matches, the
// rule that decided) tells whether a held value starts with a guess, one
// guess a run, so the value can be read back without show_values. The
// plain operator's run shows the answer does tell (the planted name's
// first letter matches); the masked key is refused the run as permission
// denied with the fixed message, and keeps its trial of a file it sends.
func TestHeldFileTrialIsNoOracle(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	file := plantedFile(t)
	held := f.heldFile(t, "scanner1", false)
	read := func(tenant.QuarantinedFile) ([]byte, error) { return file, nil }
	f.parts.heldFile.Store(&read)
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleOperator))
	guess := func(pat string) *v1.TrialProfile {
		return unsaved(&v1.ProfileDocument{Rules: []*v1.ProfileRule{{Name: "guess",
			Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: pat, Vr: "PN"}}},
			Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}}})
	}
	matches := func(as caller, pat string) (int32, outcome) {
		o := call(f.as(as).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: guess(pat), QuarantineRecords: []string{held}})
		if o.err != nil {
			return 0, o
		}
		return elementAt(o.res.(*v1.TryProfileResponse).GetResults()[0], "00100010").GetPatternMatches(), o
	}
	if right, _ := matches(f.lab1Operator, plantedName[:1]+".*"); right == 0 {
		t.Fatal("the plain operator's guess of the first letter matched nothing; the oracle this test closes is not shown")
	}
	if wrong, _ := matches(f.lab1Operator, "X.*"); wrong != 0 {
		t.Fatalf("a wrong guess matched %d times", wrong)
	}
	_, o := matches(masked, plantedName[:1]+".*")
	if o.code() != connect.CodePermissionDenied || o.message() != access.MessageMasked {
		t.Fatalf("the masked key's guess: %v (%v)", o.code(), o.err)
	}
	f.checkAudit(t, "the masked key's guess", o.requestID, *maskedRefusal())
	sent := call(f.as(masked).profiles.TryProfile, &v1.TryProfileRequest{Tenant: "lab1", Profile: guess("X.*"), Files: []*v1.TrialFile{{Name: "own.dcm", Content: file}}})
	if sent.err != nil {
		t.Errorf("the masked key's trial of a file it sent: %v", sent.err)
	}
}

// TestMaskedKeyReadsReceptionsWithoutUIDs guards the receptions a masked
// key reads: their Study Instance UID and their scans' Series Instance
// UIDs are the ones received, original values, and are answered empty to
// it in a listing and a reception's own answer, the rest as it is; a plain
// reader is answered them.
func TestMaskedKeyReadsReceptionsWithoutUIDs(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "ct5n-2062.dcm")})
	path := a.scans(t)[0]
	sess, err := a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleReader))
	list := func(as caller) []*v1.Reception {
		return mustCall[*v1.ListReceptionsResponse](t, "list", call(f.as(as).receptions.ListReceptions, &v1.ListReceptionsRequest{Tenant: "lab1"})).GetReceptions()
	}
	get := func(as caller, id string) *v1.GetReceptionResponse {
		return mustCall[*v1.GetReceptionResponse](t, "get", call(f.as(as).receptions.GetReception, &v1.GetReceptionRequest{Tenant: "lab1", Id: id}))
	}
	plain := list(f.lab1Reader)
	if len(plain) != 1 || plain[0].GetStudyInstanceUid() != sess.StudyUID {
		t.Fatalf("the plain reader's receptions: %v", plain)
	}
	plainGot := get(f.lab1Reader, plain[0].GetId())
	if len(plainGot.GetScans()) != 1 || plainGot.GetScans()[0].GetSeriesInstanceUid() == "" {
		t.Fatalf("the plain reader's reception has no scan with its UID: %v", plainGot)
	}
	listed := list(masked)
	got := get(masked, plain[0].GetId())
	for name, r := range map[string]*v1.Reception{"listed": listed[0], "got": got.GetReception()} {
		if r.GetStudyInstanceUid() != "" {
			t.Errorf("%s, masked: the Study Instance UID %q", name, r.GetStudyInstanceUid())
		}
		if r.GetId() != plain[0].GetId() || r.GetSubjectLabel() != path.Subject || r.GetCounts().GetFilesArchived() != 1 {
			t.Errorf("%s, masked, lost what is not an original: %v", name, r)
		}
	}
	if len(got.GetScans()) != 1 || got.GetScans()[0].GetSeriesInstanceUid() != "" || got.GetScans()[0].GetLabel() != path.Scan {
		t.Errorf("the masked key's scans: %v", got.GetScans())
	}
	if s := surface(outcome{res: got}); strings.Contains(s, sess.StudyUID) {
		t.Errorf("the masked key's reception holds the study's UID: %s", s)
	}
}

// TestMaskedKeyChoosesNoDestination guards what decides where the
// archive's bytes go: a send job, a replica's attachment, a storage or a
// sender added or changed, and a receiver made to forward are refused to a
// masked key whose role suffices, as permission denied with the fixed
// message, recorded refused with the mark and the reason; the same calls by
// a plain key with the role pass the check; and a receiver change that
// forwards nowhere is not refused it for this.
func TestMaskedKeyChoosesNoDestination(t *testing.T) {
	d := newJobData(t)
	f := d.f
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	calls := map[string]func(clients) outcome{
		"a send job": createJob(target("pacs", d.session.GetSubjectLabel())),
		"a replica attached": func(c clients) outcome {
			return call(c.replicas.AttachReplica, &v1.AttachReplicaRequest{Tenant: "lab1", Replica: "elsewhere", Primary: "mri"})
		},
		"a storage added": func(c clients) outcome {
			return call(c.storages.AddStorage, &v1.AddStorageRequest{Tenant: "lab1", Storage: &v1.Storage{Name: "elsewhere"}})
		},
		"a storage changed": func(c clients) outcome {
			return call(c.storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: &v1.Storage{Name: "mri"}, UpdateMask: mask("description")})
		},
		"a sender added": func(c clients) outcome {
			return call(c.senders.AddSender, &v1.AddSenderRequest{Tenant: "lab1", Sender: dimseTo("peer", "pacs.lab.example", 104)})
		},
		"a sender changed": func(c clients) outcome {
			return call(c.senders.SetSender, &v1.SetSenderRequest{Tenant: "lab1", Sender: dimseTo("pacs", "pacs.lab.example", 11112), UpdateMask: mask("dimse.port")})
		},
		"a receiver added that forwards": func(c clients) outcome {
			return call(c.receivers.AddReceiver, &v1.AddReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "relay", ForwardTo: []string{"pacs"}}})
		},
		"a receiver made to forward": func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner1", ForwardTo: []string{"pacs"}}, UpdateMask: mask("forward_to")})
		},
		"a route that forwards": func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner1", Routing: &v1.ReceiverRouting{Routes: []*v1.Route{
				{Name: "out", ForwardTo: &v1.RouteForwardTo{Senders: []string{"pacs"}}}}}}, UpdateMask: mask("routing")})
		},
	}
	for name, c := range calls {
		o := c(f.as(masked))
		if o.code() != connect.CodePermissionDenied || o.message() != access.MessageMaskedDestination {
			t.Errorf("%s, masked: %v (%v), want permission denied: %s", name, o.code(), o.err, access.MessageMaskedDestination)
			continue
		}
		f.checkAudit(t, name+", masked", o.requestID, wantAudit{outcome: master.OutcomeRefused, reason: "where archived data goes", details: map[string]string{access.DetailMasked: "true"}})
		if p := c(f.as(f.lab1Admin)); p.code() == connect.CodePermissionDenied {
			t.Errorf("%s, plain key with the role: refused: %v", name, p.err)
		}
	}
	stop := call(f.as(masked).receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "scanner1"}, UpdateMask: mask("forward_to")})
	if stop.message() == access.MessageMaskedDestination {
		t.Errorf("a receiver change that forwards nowhere was refused to the masked key: %v", stop.err)
	}
}
