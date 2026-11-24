package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// codeOf is a forward lookup into lab1.
func codeOf(attribute, value string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.deid.Code, &v1.CodeRequest{Tenant: "lab1", Attribute: attribute, Value: value})
	}
}

// receiveWithTenantKey makes lab1's receiver archive files, its labels
// made with the tenant's own naming key and its files de-identified with
// the tenant's own keys, as a server's reception makes them, and waits
// until the catalog holds them.
func receiveWithTenantKey(t *testing.T, f *placeFixture, receiver string, files ...[]byte) {
	t.Helper()
	store, err := dir.Open(filepath.Join(f.root, "lab1", "mri"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hasher, err := f.registry.NamingHasher(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	p := f.pipelineOf("lab1")
	wait := newSettle()
	pipe, err := receive.New(receive.Config{
		Tenant: "lab1", Store: p, Hasher: hasher, Clock: wait.clock, Wake: wait.Wake, IncomingRoot: filepath.Join(f.dir, "incoming"),
		OpenStorage: func(context.Context, string) (blob.Store, error) { return store, nil }, Lease: 10 * time.Minute,
		DeidKeys: func(ctx context.Context) (engine.Keys, error) { return f.registry.DeidKeys(ctx, "lab1") },
		Check:    newCheck,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pipe.Stop(context.Background()) })
	for _, b := range files {
		out := pipe.Accept(context.Background(), receive.Arrival{Receiver: receiver, Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "PRISMA1", Address: "10.20.0.31:50288"}})
		if out.Status != receive.Received {
			t.Fatalf("a file was %v (%s), want received", out.Status, out.Reason)
		}
	}
	wait.await(t, pipe, p, len(files))
}

// testCode guards the forward lookup: admin on the tenant only; an
// attribute that is not a standard element's, and a value empty, too long
// or not a UID where the attribute's VR is UI, refused; the answer's code
// is the tenant's keyed hash of the value, its padding removed, and the
// subject label scanner1 gives it is the label of the subject archived
// from a file with that Patient ID, which the catalog holds; a lookup
// table's code for the value is answered; the audit entry names the
// attribute, never the value, and no entry of the tenant's log or line of
// the server's log holds the value.
func testCode(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	ct := corpusFile(t, "ct5n-2062.dcm")
	receiveWithTenantKey(t, f, "scanner1", ct)
	hasher, err := f.registry.NamingHasher(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	code := hasher.Hash([]byte(ctID))
	held := mustCall[*v1.ListSubjectsResponse](t, "subjects", call(f.as(local).catalog.ListSubjects, &v1.ListSubjectsRequest{Tenant: "lab1"})).GetSubjects()
	if len(held) != 1 || held[0].GetLabel() != "sub-"+code {
		t.Fatalf("the archive's subjects: %v, want sub-%s", held, code)
	}
	if _, err := f.pipelineOf("lab1").(tenant.LookupTables).AssignLookupCodes(ctx,
		[]tenant.LookupKey{{Table: "subjects", Key: code}}, f.now()); err != nil {
		t.Fatal(err)
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: codeOf("", ctID), code: connect.CodeUnauthenticated},
		{name: "operator", as: f.lab1Operator, call: codeOf("", ctID), code: connect.CodePermissionDenied},
		{name: "no grant", as: f.noGrant, call: codeOf("", ctID), code: connect.CodePermissionDenied},
		{name: "a private tag", as: f.lab1Admin, call: codeOf("00091010", ctID), code: connect.CodeInvalidArgument, msg: "attribute"},
		{name: "an unknown keyword", as: f.lab1Admin, call: codeOf("PatientIdentity", ctID), code: connect.CodeInvalidArgument, msg: "attribute",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "invalid"}},
		{name: "only padding", as: f.lab1Admin, call: codeOf("", "   "), code: connect.CodeInvalidArgument, msg: "value"},
		{name: "too long", as: f.lab1Admin, call: codeOf("", strings.Repeat("1", 1025)), code: connect.CodeInvalidArgument, msg: "value"},
		{name: "not a UID", as: f.lab1Admin, call: codeOf("StudyInstanceUID", "not-a-uid"), code: connect.CodeInvalidArgument, msg: "UID"},
		{name: "admin, with padding", as: f.lab1Admin, call: codeOf("", ctID+"  "),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "00100020", details: map[string]string{"subjects": "1", "lookups": "1"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.CodeResponse)
				if res.GetCode() != code || res.GetAttribute() != "00100020" {
					t.Errorf("code %q of %s, want %q", res.GetCode(), res.GetAttribute(), code)
				}
				s := res.GetSubjects()
				if len(s) != 1 || s[0].GetReceiver() != "scanner1" || s[0].GetSubjectLabel() != held[0].GetLabel() || !s[0].GetHeld() {
					t.Errorf("subjects %v, want scanner1's %s, held", s, held[0].GetLabel())
				}
				if l := res.GetLookups(); len(l) != 1 || l[0].GetTable() != "subjects" || l[0].GetCode() != "1" {
					t.Errorf("lookups %v", l)
				}
			}},
		{name: "a value never archived", as: f.lab1Admin, call: codeOf("PatientID", "NOBODY-1"),
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.CodeResponse)
				if s := res.GetSubjects(); len(s) != 1 || s[0].GetHeld() || len(res.GetLookups()) != 0 {
					t.Errorf("a value never archived: %v", res)
				}
			}},
	})
	f.flush()
	for _, en := range f.tenantLog("lab1") {
		text := en.Object + en.Reason
		for _, v := range en.Details {
			text += " " + v
		}
		if strings.Contains(text, ctID) || strings.Contains(text, "NOBODY-1") {
			t.Errorf("an audit entry holds the value: %+v", en)
		}
	}
	if strings.Contains(f.log.String(), ctID) || strings.Contains(f.log.String(), "NOBODY-1") {
		t.Error("the server's log holds the value")
	}
}

// TestCodeRateBound guards the bounds on forward lookups: one key makes
// CodesPerMinute of them, the next is refused as exhausted and audited as
// failed, and the key is refused on another tenant too (its bucket is for
// every tenant); another key on the same tenant is refused as well (the
// tenant's bucket is for every key), while that key on another tenant is
// not; a second later one more is allowed; and a refused call spends no
// other bucket.
func TestCodeRateBound(t *testing.T) {
	f := newFixture(t)
	f.tenant("lab2")
	both, _ := f.key(f.onTenant("lab1", master.RoleAdmin), f.onTenant("lab2", master.RoleAdmin))
	other, _ := f.key(f.onTenant("lab1", master.RoleAdmin), f.onTenant("lab2", master.RoleAdmin))
	on := func(tenantName string) func(clients) outcome {
		return func(c clients) outcome { return call(c.deid.Code, &v1.CodeRequest{Tenant: tenantName, Value: "ID-X"}) }
	}
	for i := range 60 {
		if o := on("lab1")(f.as(both)); o.err != nil {
			t.Fatalf("lookup %d: %v", i+1, o.err)
		}
	}
	f.run(t, []tcase{
		{name: "the 61st", as: both, call: on("lab1"), code: connect.CodeResourceExhausted, msg: "this key has had 60 forward lookups",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "00100020"}},
		{name: "the key on another tenant", as: both, call: on("lab2"), code: connect.CodeResourceExhausted, msg: "this key"},
		{name: "another key on the same tenant", as: other, call: on("lab1"), code: connect.CodeResourceExhausted, msg: "this tenant"},
		{name: "another key on another tenant", as: other, call: on("lab2")},
	})
	f.advance(1100 * time.Millisecond)
	f.run(t, []tcase{
		{name: "a second later", as: both, call: on("lab1")},
		{name: "and no more", as: both, call: on("lab1"), code: connect.CodeResourceExhausted},
	})
}

// TestImportBound guards the bound on imports: one key makes
// ImportsPerHour of them, stored or refused, and the next is refused as
// exhausted, audited; another key is not.
func TestImportBound(t *testing.T) {
	f := newFixture(t)
	other, _ := f.key(f.onTenant("lab1", master.RoleAdmin))
	for i := range 10 {
		o := importCodes("t", &v1.LookupEntry{Value: "A-1", Code: "A" + strconv.Itoa(i%2)})(f.as(f.lab1Admin))
		if o.err != nil && o.code() != connect.CodeFailedPrecondition {
			t.Fatalf("import %d: %v", i+1, o.err)
		}
	}
	f.run(t, []tcase{
		{name: "the 11th", as: f.lab1Admin, call: importCodes("t", &v1.LookupEntry{Value: "A-1", Code: "A0"}), code: connect.CodeResourceExhausted,
			msg: "10 lookup imports", audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed}},
		{name: "another key", as: other, call: importCodes("t", &v1.LookupEntry{Value: "A-1", Code: "A0"})},
	})
}

// importCodes is an import into lab1's table.
func importCodes(table string, entries ...*v1.LookupEntry) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "lab1", Table: table, Entries: entries})
	}
}

// testImportLookupCodes guards an import: admin on the tenant only; a
// malformed table, no entry, a code not of a code's form, an empty value
// and a value given two codes refused as invalid naming the entry; codes
// stored by the tenant's keyed hash of the value (padding removed), so
// the engine running with the tenant's keys finds an imported value's
// code where a lookup that holds a missing value would otherwise hold the
// file; values coded otherwise or codes taken refused as a precondition
// that counts them and names none (an import must not tell which values
// the table holds), nothing of the import stored, the refusal audited with
// the count; the audit entries carry counts and no value or code.
func testImportLookupCodes(t *testing.T) {
	f := newFixture(t)
	e := func(v, c string) *v1.LookupEntry { return &v1.LookupEntry{Value: v, Code: c} }
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: importCodes("subjects", e(ctID, "P001")), code: connect.CodePermissionDenied},
		{name: "a malformed table", as: f.lab1Admin, call: importCodes("Subjects", e(ctID, "P001")), code: connect.CodeInvalidArgument, msg: "table"},
		{name: "no entry", as: f.lab1Admin, call: importCodes("subjects"), code: connect.CodeInvalidArgument, msg: "entries"},
		{name: "a code in lower case", as: f.lab1Admin, call: importCodes("subjects", e(ctID, "p001")), code: connect.CodeInvalidArgument, msg: "entry 1 code"},
		{name: "an empty value", as: f.lab1Admin, call: importCodes("subjects", e(ctID, "P001"), e(" ", "P002")), code: connect.CodeInvalidArgument, msg: "entry 2 value"},
		{name: "a value given two codes", as: f.lab1Admin, call: importCodes("subjects", e(ctID, "P001"), e(ctID+" ", "P002")), code: connect.CodeInvalidArgument, msg: "entry 2"},
		{name: "admin", as: f.lab1Admin, call: importCodes("subjects", e(ctID, "P001"), e("OTHER-2", "P002"), e(ctID, "P001")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "subjects", details: map[string]string{"entries": "2", "added": "2", "unchanged": "0"}},
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.ImportLookupCodesResponse); res.GetAdded() != 2 || res.GetUnchanged() != 0 || res.GetCodes() != 2 {
					t.Errorf("import %v", res)
				}
			}},
		{name: "a value coded otherwise", as: f.lab1Admin, call: importCodes("subjects", e("NEW-3", "P003"), e("OTHER-2", "P009"), e("NEW-4", "P001")),
			code: connect.CodeFailedPrecondition, msg: "2 of the entries conflict",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "subjects", details: map[string]string{"entries": "3", "conflicts": "2"}},
			check: func(t *testing.T, o outcome) {
				if strings.Contains(o.message(), "entry") {
					t.Errorf("a conflict names an entry: %s", o.message())
				}
			}},
	})
	p := f.pipelineOf("lab1").(tenant.LookupTables)
	hasher, err := f.registry.NamingHasher(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := p.LookupCode(ctx, tenant.LookupKey{Table: "subjects", Key: hasher.Hash([]byte("NEW-3"))}); found {
		t.Error("a refused import stored an entry before the one refused")
	}
	f.flush()
	for _, en := range f.tenantLog("lab1") {
		for _, v := range en.Details {
			if strings.Contains(v, ctID) || strings.Contains(v, "OTHER-2") || strings.Contains(v, "P00") {
				t.Errorf("an audit entry holds a value or a code: %v", en.Details)
			}
		}
	}

	// The engine with the tenant's keys and tables: an imported value is
	// coded, a value not imported is held for its missing code.
	doc := basicDocument()
	doc.Rules = []*v1.ProfileRule{{Name: "code-subject", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "PatientID"}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Lookup{Lookup: &v1.LookupAction{Table: "subjects", OnMissing: v1.LookupMissing_LOOKUP_MISSING_QUARANTINE}}}}}
	v, err := profile.New("coded", 1, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := f.registry.DeidKeys(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), keys, engine.Options{Lookups: tenantLookups{p}, Method: "coded@1"})
	if err != nil {
		t.Fatal(err)
	}
	ct := corpusFile(t, "ct5n-2062.dcm")
	tr, err := receive.TryFile(ctx, en, ct, engine.Reception{}, receive.TrialOptions{Keys: keys, HeaderLimit: 64 << 20, Check: newCheck})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Hold != nil || tr.Undecided {
		t.Fatalf("a file whose Patient ID was imported was held: %+v, undecided %v", tr.Hold, tr.Undecided)
	}
	for _, e := range tr.Record.Entries {
		if e.Path == "00100020" && e.Action != "lookup" {
			t.Errorf("Patient ID was decided by %s", e.Action)
		}
	}
	missing := bytes.ReplaceAll(ct, []byte(ctID), []byte("98890235"))
	tr, err = receive.TryFile(ctx, en, missing, engine.Reception{}, receive.TrialOptions{Keys: keys, HeaderLimit: 64 << 20, Check: newCheck})
	if err != nil {
		t.Fatal(err)
	}
	// A trial reports a missing code as undecided: a receiver holds it.
	if !tr.Undecided {
		t.Errorf("a Patient ID not imported was not reported missing: %+v", tr.Hold)
	}
}

// newCheck makes the check after, as a trial's.
func newCheck(maxFileBytes int64) receive.Checker {
	return check.New(check.Options{MaxFileBytes: maxFileBytes})
}

// tenantLookups is engine.Lookups over a tenant's tables, as a receiver's
// reception reaches them.
type tenantLookups struct{ p tenant.LookupTables }

// Code returns the table's code for key.
func (l tenantLookups) Code(ctx context.Context, table, key string) (string, bool, error) {
	return l.p.LookupCode(ctx, tenant.LookupKey{Table: table, Key: key})
}

// Assign assigns codes to keys in their tables.
func (l tenantLookups) Assign(ctx context.Context, keys []engine.LookupKey) ([]string, error) {
	in := make([]tenant.LookupKey, len(keys))
	for i, k := range keys {
		in[i] = tenant.LookupKey{Table: k.Table, Key: k.Key}
	}
	return l.p.AssignLookupCodes(ctx, in, time.Now())
}

// testListLookupTables guards ListLookupTables: operator on the tenant;
// each table with its count, by name, and nothing else.
func testListLookupTables(t *testing.T) {
	f := newFixture(t)
	mustCall[*v1.ImportLookupCodesResponse](t, "import", importCodes("subjects", &v1.LookupEntry{Value: "A-1", Code: "S1"}, &v1.LookupEntry{Value: "A-2", Code: "S2"})(f.as(f.lab1Admin)))
	mustCall[*v1.ImportLookupCodesResponse](t, "import", importCodes("accessions", &v1.LookupEntry{Value: "X-1", Code: "AC1"})(f.as(f.lab1Admin)))
	list := func(size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.deid.ListLookupTables, &v1.ListLookupTablesRequest{Tenant: "lab1", PageSize: size})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: list(0), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: list(0), audit: inTenant("lab1", master.OutcomeDone), check: func(t *testing.T, o outcome) {
			ts := o.res.(*v1.ListLookupTablesResponse).GetTables()
			if len(ts) != 2 || ts[0].GetName() != "accessions" || ts[0].GetCodes() != 1 || ts[1].GetName() != "subjects" || ts[1].GetCodes() != 2 {
				t.Errorf("tables %v", ts)
			}
		}},
		{name: "a page of one", as: f.lab1Operator, call: list(1), check: func(t *testing.T, o outcome) {
			if res := o.res.(*v1.ListLookupTablesResponse); len(res.GetTables()) != 1 || res.GetNextPageToken() == "" {
				t.Errorf("a page of one: %v", res)
			}
		}},
	})
}

// testDescribeProfile guards DescribeProfile: operator on the tenant; a
// stored version described by class, with the notes and the method
// record: the basic profile's code 113100 with its meaning, the values of
// De-identification Method the engine writes (the meaning, the version,
// the software); a version whose own rule keeps what a base empties
// claims no code and says why, Study ID listed as kept; a document described the same with its checksum; a
// document with a problem refused with its problems; a version the tenant
// does not have not found.
func testDescribeProfile(t *testing.T) {
	f := newFixture(t)
	storeVersion(t, f.env, "research", basicDocument())
	storeVersion(t, f.env, "research", keepStudyID())
	describe := func(p *v1.TrialProfile) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.profiles.DescribeProfile, &v1.DescribeProfileRequest{Tenant: "lab1", Profile: p})
		}
	}
	bad := basicDocument()
	bad.Bases = append(bad.Bases, "annex-e/nothing:2026d")
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: describe(storedVersion("research", 1)), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: describe(storedVersion("research", 1)),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "research@1"},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.DescribeProfileResponse)
				m := res.GetMethod()
				if len(m.GetCodes()) != 1 || m.GetCodes()[0].GetValue() != "113100" || m.GetCodes()[0].GetScheme() != "DCM" || m.GetIdentityRemoved() != "YES" {
					t.Errorf("the method's codes: %v", m)
				}
				if v := m.GetMethodValues(); len(v) != 4 || v[0] != m.GetCodes()[0].GetMeaning() || v[1] != "research@1" || !strings.HasPrefix(v[2], "NoPACs ") {
					t.Errorf("the method's values: %q", v)
				}
				if res.GetVersion().GetVersion() != 1 || len(res.GetChecksum()) != 64 || len(res.GetClasses()) < 4 || len(res.GetNotes()) < 3 {
					t.Errorf("the description: version %v, %d classes, %d notes", res.GetVersion(), len(res.GetClasses()), len(res.GetNotes()))
				}
				for _, c := range res.GetClasses() {
					for _, e := range c.GetEffects() {
						if c.GetClass() == v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_IDENTIFIERS && slices.Contains(e.GetElements(), "StudyID") && e.GetAction() != "empty" {
							t.Errorf("Study ID, which the basic profile empties, is listed under %s", e.GetAction())
						}
					}
				}
			}},
		{name: "a version whose own rule keeps what a base empties", as: f.lab1Operator, call: describe(storedVersion("research", 2)),
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.DescribeProfileResponse)
				m := res.GetMethod()
				if len(m.GetCodes()) != 0 || m.GetWithheld() == "" || len(m.GetMethodValues()) != 3 || m.GetMethodValues()[0] != "research@2" {
					t.Errorf("the method of a version that weakens its base: %v", m)
				}
				for _, c := range res.GetClasses() {
					for _, e := range c.GetEffects() {
						if slices.Contains(e.GetElements(), "StudyID") && e.GetAction() != "keep" {
							t.Errorf("Study ID, kept by the version's own rule, is listed under %s", e.GetAction())
						}
					}
				}
			}},
		{name: "a document", as: f.lab1Operator, call: describe(unsaved(basicDocument())), check: func(t *testing.T, o outcome) {
			res := o.res.(*v1.DescribeProfileResponse)
			if res.GetVersion() != nil || len(res.GetChecksum()) != 64 || !strings.HasPrefix(res.GetMethod().GetMethodValues()[1], "unsaved ") {
				t.Errorf("a document's description: %v", res)
			}
		}},
		{name: "a document with a problem", as: f.lab1Operator, call: describe(unsaved(bad)), code: connect.CodeInvalidArgument,
			check: func(t *testing.T, o outcome) {
				if p := problemsOf(o.err); len(p.GetProblems()) == 0 {
					t.Error("no problems detail")
				}
			}},
		{name: "no such version", as: f.lab1Operator, call: describe(storedVersion("research", 9)), code: connect.CodeNotFound},
	})
}

// TestInstanceMetadataNamesItsProfile guards the profile version an
// instance's metadata answers: an instance archived by a receiver that
// de-identifies is answered with the version that made it (and its line
// of the scan's metadata file, whose checksum is of the de-identified
// bytes, is accepted), while one archived as received names none.
func TestInstanceMetadataNamesItsProfile(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	storeVersion(t, f.env, "research", basicDocument())
	deid := dimseReceiver("deid1", "main", 11121)
	deid.Deidentification = &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "research", Version: 1}}
	mustCall[*v1.AddReceiverResponse](t, "add deid1", addReceiver("lab1", deid)(f.as(local)))
	receiveWithTenantKey(t, f, "deid1", corpusFile(t, "ct5n-2062.dcm"))
	scans := mustCall[*v1.ListScansResponse](t, "scans", call(f.as(local).catalog.ListScans, &v1.ListScansRequest{Tenant: "lab1"})).GetScans()
	if len(scans) != 1 {
		t.Fatalf("%d scans", len(scans))
	}
	s := scans[0]
	md := f.as(f.lab1Reader).metadata
	res := mustCall[*v1.GetScanMetadataResponse](t, "metadata", call(md.GetScanMetadata, &v1.GetScanMetadataRequest{Tenant: "lab1",
		SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetSessionLabel(), ScanLabel: s.GetLabel()}))
	if ins := res.GetInstances(); len(ins) != 1 || ins[0].GetProfile().GetName() != "research" || ins[0].GetProfile().GetVersion() != 1 {
		t.Errorf("the instance's metadata: %v", ins)
	}
	one := mustCall[*v1.GetInstanceMetadataResponse](t, "instance", call(md.GetInstanceMetadata, &v1.GetInstanceMetadataRequest{Tenant: "lab1",
		SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetSessionLabel(), ScanLabel: s.GetLabel(), InstanceId: res.GetInstances()[0].GetInstanceId()}))
	if one.GetInstance().GetProfile().GetName() != "research" {
		t.Errorf("GetInstanceMetadata: %v", one.GetInstance().GetProfile())
	}
}

// unflushed returns the tenant's log as its database holds it now, without
// waiting for the batched audit writer.
func unflushed(t *testing.T, e *env, name string) []master.AuditEntry {
	t.Helper()
	st, err := e.registry.Store(name)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := st.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDeidAuditedBeforeTheAnswer guards that a forward lookup and an
// import, answered or refused, are in the tenant's log before their answer
// (never left to the batched writer, which may drop an entry or reroute
// it), and fail closed: while the tenant's log does not take entries, a
// lookup that would answer, a refused lookup and a refused import are each
// answered unavailable, with no codes, where a read would still answer.
func TestDeidAuditedBeforeTheAnswer(t *testing.T) {
	e := newEnv(t)
	if o := createTenant("pg", pgType, pgAddress)(e.as(local)); o.err != nil {
		t.Fatal(o.err)
	}
	code := func(value string) outcome {
		return call(e.as(local).deid.Code, &v1.CodeRequest{Tenant: "pg", Value: value})
	}
	// An import that stores writes its entry in its own transaction; one
	// refused for a conflict writes it before the answer.
	imp := func() outcome {
		return call(e.as(local).deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "pg", Table: "t",
			Entries: []*v1.LookupEntry{{Value: "A-1", Code: "Z9"}}})
	}
	ok := code("ID-1")
	if ok.err != nil || len(entriesOf(unflushed(t, e, "pg"), ok.requestID)) != 1 {
		t.Fatalf("a lookup: %v; its entry is not in the log at its answer", ok.err)
	}
	bad := code("   ")
	if bad.code() != connect.CodeInvalidArgument || len(entriesOf(unflushed(t, e, "pg"), bad.requestID)) != 1 {
		t.Errorf("a refused lookup: %v; its entry is not in the log at its answer", bad.err)
	}
	conflict := call(e.as(local).deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "pg", Table: "t",
		Entries: []*v1.LookupEntry{{Value: "A-1", Code: "A1"}}})
	if conflict.err != nil {
		t.Fatal(conflict.err)
	}
	again := call(e.as(local).deid.ImportLookupCodes, &v1.ImportLookupCodesRequest{Tenant: "pg", Table: "t",
		Entries: []*v1.LookupEntry{{Value: "A-1", Code: "A2"}}})
	if again.code() != connect.CodeFailedPrecondition || len(entriesOf(unflushed(t, e, "pg"), again.requestID)) != 1 {
		t.Errorf("a refused import: %v; its entry is not in the log at its answer", again.err)
	}
	e.pg.setLogDown("pg", true)
	for name, o := range map[string]outcome{"a lookup": code("ID-2"), "a refused lookup": code("   "), "a refused import": imp()} {
		if o.code() != connect.CodeUnavailable || o.res != nil {
			t.Errorf("%s with the tenant's log down: %v, %v; want unavailable and no answer", name, o.err, o.res)
		}
	}
	e.pg.setLogDown("pg", false)
	if !strings.Contains(e.log.String(), "could not be; the call was refused") {
		t.Error("the server's log does not say why the calls were refused")
	}
}

// TestCodeNotServedOverGET guards that a forward lookup is never answered
// to an HTTP GET, whose request (the value among it) travels in the URL
// that proxies log and browsers repeat in Referer: GET is refused, where
// POST of the same request answers.
func TestCodeNotServedOverGET(t *testing.T) {
	f := newFixture(t)
	q := url.Values{"encoding": {"json"}, "message": {`{"tenant":"lab1","value":"SECRET-778812"}`}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+nopacsv1connect.DeidServiceCodeProcedure+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.lab1Admin.key)
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK || strings.Contains(string(body), `"code"`) {
		t.Errorf("GET answered %d: %s", res.StatusCode, body)
	}
	if o := codeOf("", "SECRET-778812")(f.as(f.lab1Admin)); o.err != nil {
		t.Errorf("POST of the same request: %v", o.err)
	}
}
