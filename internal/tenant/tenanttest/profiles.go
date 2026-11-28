package tenanttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runDeidentification runs the contract suite's groups of profiles,
// lookup tables and the tenant's de-identification keys
// (tenant.Deidentification, tenant.Secrets) against the stores open makes,
// each test on its own store, in parallel. Run runs it.
func runDeidentification(t *testing.T, open OpenPipeline) {
	tests := []struct {
		name string
		fn   func(*penv)
	}{
		{"ProfileVersions", testProfileVersions},
		{"ProfileExpectations", testProfileExpectations},
		{"ProfileRefusals", testProfileRefusals},
		{"ProfileListed", testProfileListed},
		{"ProfileGuarded", testProfileGuarded},
		{"ProfileAtOnce", testProfileAtOnce},
		{"InstanceProfile", testInstanceProfile},
		{"ProfileTrials", testProfileTrials},
		{"PixelTemplates", testPixelTemplates},
		{"LookupCodes", testLookupCodes},
		{"LookupRefusals", testLookupRefusals},
		{"LookupAtOnce", testLookupAtOnce},
		{"LookupImport", testLookupImport},
		{"LookupImportRefusals", testLookupImportRefusals},
		{"DeidentificationKeys", testDeidentificationKeys},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := open(t, SuiteTenant)
			tt.fn(&penv{env: &env{T: t, s: p, ctx: context.Background()}, p: p, open: open})
		})
	}
}

// newVersion returns a version of the profile name whose texts are made
// from doc: bytes no database may change, a NUL and a byte that is not
// UTF-8 among them, so a driver that keeps them as text or JSON fails.
func newVersion(name, doc string) tenant.NewProfileVersion {
	expanded := append([]byte(`{"expanded":"`+doc+`"}`), 0x00, 0xff, '\n')
	sum := sha256.Sum256(expanded)
	return tenant.NewProfileVersion{Name: name, Source: append([]byte(`{"doc":"`+doc+`"}`), 0xfe), Expanded: expanded,
		Checksum: hex.EncodeToString(sum[:]), Note: "why " + doc}
}

// expect returns a pointer to n, for NewProfileVersion.ExpectLatest.
func expect(n int32) *int32 { return &n }

// create stores v, failing the test on an error, and returns the version
// and whether it was made.
func (e *penv) create(v tenant.NewProfileVersion, seconds int) (tenant.ProfileVersion, bool) {
	e.Helper()
	got, created, err := e.p.CreateProfileVersion(e.ctx, v, at(seconds), e.audit(), nil)
	e.ok("CreateProfileVersion", err)
	return got, created
}

// sameVersion fails unless got is the version v made as number n at
// seconds, with its texts when texts is set.
func (e *penv) sameVersion(what string, got tenant.ProfileVersion, v tenant.NewProfileVersion, n int32, seconds int, texts bool) {
	e.Helper()
	if got.Name != v.Name || got.Version != n || got.Checksum != v.Checksum || got.Note != v.Note ||
		got.CreatedByKeyID != "K1234567890a" || !got.Created.Equal(at(seconds)) {
		e.Errorf("%s: %s@%d %s note %q by %q at %v, want %s@%d %s at %v", what, got.Name, got.Version, got.Checksum, got.Note,
			got.CreatedByKeyID, got.Created, v.Name, n, v.Checksum, at(seconds))
	}
	if texts && (!bytes.Equal(got.Source, v.Source) || !bytes.Equal(got.Expanded, v.Expanded)) {
		e.Errorf("%s: the texts are not the bytes given: %q, %q", what, got.Source, got.Expanded)
	}
	if !texts && (got.Source != nil || got.Expanded != nil) {
		e.Errorf("%s: a list answered the texts", what)
	}
}

// testProfileVersions guards a profile's versions: the first is 1 and the
// next one more, each answered with its texts byte for byte as given,
// its checksum, note, creator and time; version 0 reads the latest; a
// document equal to the latest is answered with it and stores nothing,
// and its audit entry says so; each creation's audit entry carries the
// version, whether it was made and the checksum.
func testProfileVersions(e *penv) {
	v1 := newVersion("research", "one")
	got, created := e.create(v1, 1)
	if !created {
		e.Error("the first version was not made")
	}
	e.sameVersion("the first version", got, v1, 1, 1, true)
	again, created := e.create(v1, 2)
	if created {
		e.Error("an equal document made a version")
	}
	e.sameVersion("an equal document", again, v1, 1, 1, true)
	v2 := newVersion("research", "two")
	got, created = e.create(v2, 3)
	if !created {
		e.Error("the second version was not made")
	}
	e.sameVersion("the second version", got, v2, 2, 3, true)
	back := v1
	back.Note = "back to the first"
	got, created = e.create(back, 4)
	if !created || got.Version != 3 {
		e.Errorf("the first document after the second: version %d, made %v", got.Version, created)
	}
	read, err := e.p.ProfileVersion(e.ctx, "research", 0)
	e.ok("ProfileVersion latest", err)
	e.sameVersion("the latest", read, back, 3, 4, true)
	read, err = e.p.ProfileVersion(e.ctx, "research", 2)
	e.ok("ProfileVersion 2", err)
	e.sameVersion("version 2", read, v2, 2, 3, true)
	_, err = e.p.ProfileVersion(e.ctx, "research", 4)
	e.wantKind("ProfileVersion past the latest", err, fault.NotFound)
	_, err = e.p.ProfileVersion(e.ctx, "nobody", 0)
	e.wantKind("ProfileVersion of no profile", err, fault.NotFound)
	_, err = e.p.ProfileVersion(e.ctx, "research", -1)
	e.wantKind("ProfileVersion -1", err, fault.Invalid)

	entries := e.allAudit(master.AuditFilter{})
	if len(entries) != 4 {
		e.Fatalf("%d audit entries, want one per call", len(entries))
	}
	want := []string{"3 true", "2 true", "1 false", "1 true"} // newest first
	for i, a := range entries {
		got := a.Details[tenant.DetailProfileVersion] + " " + a.Details[tenant.DetailProfileCreated]
		if got != want[i] || len(a.Details[tenant.DetailProfileChecksum]) != 64 {
			e.Errorf("audit entry %d: %v, want %s", i, a.Details, want[i])
		}
	}
}

// testProfileExpectations guards the expectation of the latest version: 0
// for a profile not there, the latest's number after; another is a
// precondition failure that stores nothing; and a document equal to the latest is answered whatever the
// expectation, so a call written again after its answer was lost gets the
// answer the first got.
func testProfileExpectations(e *penv) {
	v1 := newVersion("expect", "one")
	v1.ExpectLatest = expect(1)
	_, _, err := e.p.CreateProfileVersion(e.ctx, v1, at(1), e.audit(), nil)
	e.wantPrecondition("a first version expecting 1", err)
	v1.ExpectLatest = expect(0)
	if got, created := e.create(v1, 2); !created || got.Version != 1 {
		e.Errorf("a first version expecting 0: version %d, made %v", got.Version, created)
	}
	if got, created := e.create(v1, 3); created || got.Version != 1 {
		e.Errorf("the same call again: version %d, made %v", got.Version, created)
	}
	v2 := newVersion("expect", "two")
	v2.ExpectLatest = expect(0)
	_, _, err = e.p.CreateProfileVersion(e.ctx, v2, at(4), e.audit(), nil)
	e.wantPrecondition("a second version expecting 0", err)
	v2.ExpectLatest = expect(1)
	if got, created := e.create(v2, 5); !created || got.Version != 2 {
		e.Errorf("a second version expecting 1: version %d, made %v", got.Version, created)
	}
	if got, _ := e.p.ProfileVersion(e.ctx, "expect", 0); got.Version != 2 {
		e.Errorf("the latest is %d", got.Version)
	}
}

// testProfileRefusals guards what a version must be: a profile's name,
// both texts, a checksum that is the SHA-256 of the expanded text, a note
// within its bound and of UTF-8 without NUL, texts within their bound
// together, an expectation not negative; and a valid audit entry. Each
// refusal stores nothing.
func testProfileRefusals(e *penv) {
	good := newVersion("refused", "one")
	for _, c := range []struct {
		what   string
		change func(*tenant.NewProfileVersion)
	}{
		{"a malformed name", func(v *tenant.NewProfileVersion) { v.Name = "Research" }},
		{"no document", func(v *tenant.NewProfileVersion) { v.Source = nil }},
		{"no expanded form", func(v *tenant.NewProfileVersion) { v.Expanded = nil }},
		{"another checksum", func(v *tenant.NewProfileVersion) { v.Checksum = strings.Repeat("0", 64) }},
		{"a checksum in upper case", func(v *tenant.NewProfileVersion) { v.Checksum = strings.ToUpper(v.Checksum) }},
		{"a long note", func(v *tenant.NewProfileVersion) { v.Note = strings.Repeat("n", tenant.MaxProfileNoteLength+1) }},
		{"a note with NUL", func(v *tenant.NewProfileVersion) { v.Note = "a\x00b" }},
		{"texts too large", func(v *tenant.NewProfileVersion) {
			v.Source = make([]byte, tenant.MaxProfileTextBytes-len(v.Expanded)+1)
		}},
		{"a negative expectation", func(v *tenant.NewProfileVersion) { v.ExpectLatest = expect(-1) }},
	} {
		v := good
		c.change(&v)
		_, _, err := e.p.CreateProfileVersion(e.ctx, v, at(1), e.audit(), nil)
		e.wantKind(c.what, err, fault.Invalid)
	}
	bad := e.audit()
	bad.ID = ""
	_, _, err := e.p.CreateProfileVersion(e.ctx, good, at(1), bad, nil)
	e.wantKind("an invalid audit entry", err, fault.Invalid)
	if _, err := e.p.ProfileVersion(e.ctx, "refused", 0); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a refused version was stored: %v", err)
	}
	if n := len(e.allAudit(master.AuditFilter{})); n != 0 {
		e.Errorf("refusals wrote %d audit entries", n)
	}
	// The largest texts the bound allows are kept whole.
	big := good
	big.Source = bytes.Repeat([]byte{0x01, 0xfe}, (tenant.MaxProfileTextBytes-len(big.Expanded))/2)
	got, created := e.create(big, 2)
	read, err := e.p.ProfileVersion(e.ctx, "refused", got.Version)
	e.ok("ProfileVersion of the largest", err)
	if !created || !bytes.Equal(read.Source, big.Source) {
		e.Errorf("the largest texts: made %v, %d bytes read of %d", created, len(read.Source), len(big.Source))
	}
}

// testProfileListed guards the lists: profiles by name with their latest
// version and its time, every one exactly once across pages; a profile's
// versions newest first without their texts, every one exactly once; a
// profile not there is NotFound; a cursor of one profile's versions is
// refused with another's.
func testProfileListed(e *penv) {
	for i := range 5 {
		name := fmt.Sprintf("profile-%d", 4-i)
		for n := range i + 1 {
			e.create(newVersion(name, fmt.Sprintf("doc-%d", n)), 10*i+n)
		}
	}
	all := allPages(e, 2, func(p master.Page) ([]tenant.ProfileSummary, string, error) { return e.p.ListProfiles(e.ctx, p) })
	if len(all) != 5 {
		e.Fatalf("%d profiles listed", len(all))
	}
	for i, s := range all {
		latest := int32(5 - i)
		if s.Name != fmt.Sprintf("profile-%d", i) || s.LatestVersion != latest || !s.LatestCreated.Equal(at(10*(4-i)+int(latest)-1)) {
			e.Errorf("profile %d: %+v", i, s)
		}
	}
	versions := allPages(e, 2, func(p master.Page) ([]tenant.ProfileVersion, string, error) {
		return e.p.ListProfileVersions(e.ctx, "profile-0", p)
	})
	if len(versions) != 5 {
		e.Fatalf("%d versions listed", len(versions))
	}
	for i, v := range versions {
		n := int32(5 - i)
		e.sameVersion(fmt.Sprintf("version %d listed", n), v, newVersion("profile-0", fmt.Sprintf("doc-%d", n-1)), n, 40+int(n)-1, false)
	}
	_, _, err := e.p.ListProfileVersions(e.ctx, "nobody", master.Page{Limit: 10})
	e.wantKind("ListProfileVersions of no profile", err, fault.NotFound)
	_, next, err := e.p.ListProfileVersions(e.ctx, "profile-0", master.Page{Limit: 2})
	e.ok("ListProfileVersions", err)
	_, _, err = e.p.ListProfileVersions(e.ctx, "profile-1", master.Page{Limit: 2, Cursor: next})
	e.wantKind("a cursor of another profile", err, fault.Invalid)
	_, _, err = e.p.ListProfiles(e.ctx, master.Page{Limit: 2, Cursor: next})
	e.wantKind("a cursor of the versions given to the profiles", err, fault.Invalid)
	_, _, err = e.p.ListProfiles(e.ctx, master.Page{Limit: 0})
	e.wantKind("a limit of 0", err, fault.Invalid)
}

// testProfileGuarded guards a creation whose Guard refuses: nothing is
// stored, no audit entry is written, and the error is the Guard's.
func testProfileGuarded(e *penv) {
	refuse := func(context.Context) error { return fault.New(fault.Denied, "revoked") }
	_, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("guarded", "one"), at(1), e.audit(), refuse)
	e.wantKind("a refused creation", err, fault.Denied)
	if _, err := e.p.ProfileVersion(e.ctx, "guarded", 0); fault.KindOf(err) != fault.NotFound {
		e.Error("a refused creation stored its version")
	}
	if n := len(e.allAudit(master.AuditFilter{})); n != 0 {
		e.Errorf("a refused creation wrote %d audit entries", n)
	}
}

// testProfileAtOnce guards creations at once: the same document from many
// callers expecting no profile makes one version, every caller answered
// with it; different documents expecting the same latest store one, the
// others refused as a precondition failure, never a second version unaware
// of the first, and never a failure of another kind.
func testProfileAtOnce(e *penv) {
	const n = 8
	type result struct {
		v       tenant.ProfileVersion
		created bool
		err     error
	}
	run := func(version func(i int) tenant.NewProfileVersion) []result {
		out := make([]result, n)
		done := make(chan struct{}, n)
		for i := range n {
			go func() {
				defer func() { done <- struct{}{} }()
				out[i].v, out[i].created, out[i].err = e.p.CreateProfileVersion(e.ctx, version(i), at(1), e.audit(), nil)
			}()
		}
		for range n {
			<-done
		}
		return out
	}
	same := run(func(int) tenant.NewProfileVersion {
		v := newVersion("at-once", "same")
		v.ExpectLatest = expect(0)
		return v
	})
	made := 0
	for _, r := range same {
		e.ok("the same document at once", r.err)
		if r.created {
			made++
		}
		if r.v.Version != 1 {
			e.Errorf("the same document at once answered version %d", r.v.Version)
		}
	}
	if made != 1 {
		e.Errorf("the same document at once made %d versions", made)
	}
	others := run(func(i int) tenant.NewProfileVersion {
		v := newVersion("at-once", fmt.Sprintf("other-%d", i))
		v.ExpectLatest = expect(1)
		return v
	})
	made = 0
	for _, r := range others {
		switch {
		case r.err == nil && r.created && r.v.Version == 2:
			made++
		case fault.KindOf(r.err) != fault.Precondition:
			e.Errorf("a document at once: version %d, made %v, %v", r.v.Version, r.created, r.err)
		}
	}
	if latest, _ := e.p.ProfileVersion(e.ctx, "at-once", 0); made != 1 || latest.Version != 2 {
		e.Errorf("different documents at once made %d versions; the latest is %d", made, latest.Version)
	}
}

// testInstanceProfile guards the profile version each held instance
// records: one committed with a version is answered with it, one committed
// as received with none, each its own in one commit of both kinds; a commit naming a version the tenant does not
// have is a precondition failure that commits nothing; an instance not
// held is NotFound; the catalog's reads of instances (ListHeldInstances,
// HeldInstance) answer each instance's version as InstanceProfile does.
func testInstanceProfile(e *penv) {
	e.addStorage("store")
	e.addReceiver("dicom", "store", 11112)
	v, _ := e.create(newVersion("research", "one"), 1)
	r, files := e.identifiedWith("dicom", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2), file(r, 1, 3), file(r, 1, 4), file(r, 1, 5)}
	})
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	_, err := e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	// commits commits files, the i-th recording profiles[i], in one commit.
	commits := func(files []tenant.ReceivedFile, profiles ...tenant.ProfileRef) error {
		p := e.labelsOf(r.StudyUID, files[0].SeriesUID)
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: files[0].SeriesUID,
			SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", At: r.StepTime}
		for i, f := range files {
			key, err := layout.InstanceKey(p, f.SOPInstanceUID)
			e.ok("InstanceKey", err)
			c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size,
				Size: f.Size, ArchivedSHA256: f.SHA256, Profile: profiles[i]})
		}
		_, err := e.p.CommitScan(e.ctx, c)
		return err
	}
	commit := func(f tenant.ReceivedFile, profile tenant.ProfileRef) error {
		return commits([]tenant.ReceivedFile{f}, profile)
	}
	e.wantPrecondition("a commit naming a version not stored", commit(files[0], tenant.ProfileRef{Name: "research", Version: 2}))
	_, err = e.p.InstanceProfile(e.ctx, files[0].SOPInstanceUID)
	e.wantKind("InstanceProfile of an instance not committed", err, fault.NotFound)
	e.ok("a commit with a version", commit(files[0], v.Ref()))
	e.ok("a commit as received", commit(files[1], tenant.ProfileRef{}))
	got, err := e.p.InstanceProfile(e.ctx, files[0].SOPInstanceUID)
	if e.ok("InstanceProfile", err); got != v.Ref() {
		e.Errorf("InstanceProfile = %v, want %v", got, v.Ref())
	}
	got, err = e.p.InstanceProfile(e.ctx, files[1].SOPInstanceUID)
	if e.ok("InstanceProfile as received", err); !got.IsZero() {
		e.Errorf("an instance archived as received records %v", got)
	}
	_, err = e.p.InstanceProfile(e.ctx, "1.2.3")
	e.wantKind("InstanceProfile of no instance", err, fault.NotFound)
	_, err = e.p.InstanceProfile(e.ctx, "not a uid")
	e.wantKind("InstanceProfile of a malformed UID", err, fault.Invalid)
	err = commit(files[2], tenant.ProfileRef{Name: "research", Version: 0})
	e.wantKind("a commit naming version 0", err, fault.Invalid)
	// One commit of an instance de-identified and one archived as
	// received: each records its own.
	e.ok("a commit of both kinds", commits(files[3:5], tenant.ProfileRef{}, v.Ref()))
	for i, want := range []tenant.ProfileRef{{}, v.Ref()} {
		got, err := e.p.InstanceProfile(e.ctx, files[3+i].SOPInstanceUID)
		if e.ok("InstanceProfile of a commit of both kinds", err); got != want {
			e.Errorf("instance %d of a commit of both kinds records %v, want %v", i, got, want)
		}
	}
	// The catalog's reads of instances answer the same.
	path := e.labelsOf(r.StudyUID, files[0].SeriesUID)
	held, _, err := e.p.ListHeldInstances(e.ctx, path, master.Page{Limit: 10})
	e.ok("ListHeldInstances", err)
	want := map[string]tenant.ProfileRef{files[0].SOPInstanceUID: v.Ref(), files[1].SOPInstanceUID: {},
		files[3].SOPInstanceUID: {}, files[4].SOPInstanceUID: v.Ref()}
	for _, in := range held {
		if w, ok := want[in.SOPInstanceUID]; ok && in.Profile != w {
			e.Errorf("ListHeldInstances: %s records %v, want %v", in.SOPInstanceUID, in.Profile, w)
		}
		delete(want, in.SOPInstanceUID)
	}
	if len(want) != 0 {
		e.Errorf("ListHeldInstances left out %v", want)
	}
	one, err := e.p.HeldInstance(e.ctx, path, files[0].SOPInstanceUID)
	if e.ok("HeldInstance", err); one.Profile != v.Ref() {
		e.Errorf("HeldInstance records %v, want %v", one.Profile, v.Ref())
	}
}

// CheckTamperedProfile guards that a profile version whose stored row was
// changed outside the store is never answered as the version it claims to
// be: after tamper runs a statement on the store's database that changes a
// version's checksum, reading the version fails as Internal (the store
// checks the SHA-256 of the expanded text on every read of the texts),
// while a version untouched still reads. Each driver's tests call it with
// a way to run SQL on the database p is open on.
func CheckTamperedProfile(tb testing.TB, p tenant.Profiles, tamper func(stmt string)) {
	tb.Helper()
	ctx := context.Background()
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: T0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
			TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
	}
	for i, name := range []string{"tampered", "untouched"} {
		if _, _, err := p.CreateProfileVersion(ctx, newVersion(name, "one"), T0, audit(fmt.Sprintf("a%d", i)), nil); err != nil {
			tb.Fatalf("CreateProfileVersion: %v", err)
		}
	}
	tamper(`UPDATE profile_versions SET checksum = '` + strings.Repeat("0", 64) + `' WHERE name = 'tampered'`)
	for _, version := range []int32{0, 1} {
		if _, err := p.ProfileVersion(ctx, "tampered", version); err == nil || fault.KindOf(err) != fault.Internal {
			tb.Errorf("ProfileVersion %d of a tampered row: %v, want Internal", version, err)
		}
	}
	if _, err := p.ProfileVersion(ctx, "untouched", 0); err != nil {
		tb.Errorf("ProfileVersion of an untouched row: %v", err)
	}
}
