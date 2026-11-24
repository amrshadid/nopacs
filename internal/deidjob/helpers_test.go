package deidjob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// corpusDir holds the public test files this package judges.
const corpusDir = "../receive/testdata"

// corpus reads a public test file.
func corpus(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, name)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tenantKeys stand in for the tenant's keys of de-identification: the
// files the tests archive de-identified are made under them, as a
// receiver of the tenant would make them.
var tenantKeys = engine.Keys{UID: bytes.Repeat([]byte{1}, engine.MinKeyLength), Date: bytes.Repeat([]byte{2}, engine.MinKeyLength),
	Hash: bytes.Repeat([]byte{3}, engine.MinKeyLength)}

// kw selects an element by keyword.
func kw(k string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
}

// withRules returns basicProfile with rules added after its own.
func withRules(rules ...*v1.ProfileRule) *v1.ProfileDocument {
	d := basicProfile()
	d.Rules = append(d.Rules, rules...)
	return d
}

// keepWhen is a rule keeping the element of keyword k when the condition
// on subject equals value holds (negated with negate).
func keepWhen(name, k string, subject v1.ConditionSubject, value string, negate bool) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: kw(k), Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}},
		When: []*v1.ProfileCondition{{Subject: subject, Test: &v1.ProfileCondition_Equals{Equals: value}, Negate: negate}}}
}

// basicProfile is the basic profile with the removals a reception of the
// corpus needs to archive it (the device's maker and model, the scan's
// options, a segmentation's content label).
func basicProfile() *v1.ProfileDocument {
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	return &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: []*v1.ProfileRule{
		{Name: "content-label", Selector: kw("ContentLabel"), Action: remove},
		{Name: "scan-options", Selector: kw("ScanOptions"), Action: remove},
		{Name: "maker", Selector: kw("Manufacturer"), Action: remove},
		{Name: "model", Selector: kw("ManufacturerModelName"), Action: remove},
	}}
}

// clock is a test's clock: the machine's, offset by off.
type clock struct{ off atomic.Int64 }

// Now returns the time.
func (c *clock) Now() time.Time { return time.Now().UTC().Add(time.Duration(c.off.Load())) }

// settingsOf is the settings a test gives.
type settingsOf struct{}

// HeaderLimit answers receiver.header-limit.
func (settingsOf) HeaderLimit(context.Context) (int64, error) { return header.MaxHeader, nil }

// FailuresKept answers job.failures-kept.
func (settingsOf) FailuresKept(context.Context) (int, error) { return tenant.DefaultFailuresKept, nil }

// PixelLimit answers deid.pixel-limit.
func (settingsOf) PixelLimit(context.Context) (int64, error) { return receive.DefaultPixelLimit, nil }

// oneTenant is the installation of one tenant, "lab".
type oneTenant struct{ p tenant.Pipeline }

// Names answers the one tenant.
func (o *oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database.
func (o *oneTenant) Store(string) (tenant.Store, error) { return o.p, nil }

// storages is the tenant's one storage, opened by the test.
type storages struct{ s blob.Store }

// Get answers the storage.
func (s storages) Get(context.Context, string) (blob.Store, error) { return s.s, nil }

// Close keeps it open: the test closes it.
func (storages) Close() {}

// fixture is a tenant's SQLite database with a directory storage and
// archived instances on it.
type fixture struct {
	t     testing.TB
	ctx   context.Context
	dir   string
	p     tenant.Pipeline
	store blob.Store
	root  string
	clock *clock
	reads *checkedread.Slots
	seq   atomic.Int64
	// keyCalls counts the calls for the tenant's UID key.
	keyCalls atomic.Int64
	// files are the archived files by SOP Instance UID, keys their
	// storage keys.
	files map[string][]byte
	keys  map[string]string
}

// newFixture makes the tenant "lab" with a storage "main" and a receiver.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), dir: t.TempDir(), clock: &clock{}, reads: &checkedread.Slots{},
		files: map[string][]byte{}, keys: map[string]string{}}
	s, err := (driver.SQLiteTenants{DataDir: f.dir}).Open(f.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
	f.root = filepath.Join(f.dir, "storage")
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	f.store = ds
	now := f.clock.Now().Add(-48 * time.Hour)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, now, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, now, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: f.clock.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

// storeVersion stores doc as the profile's next version and returns it.
func (f *fixture) storeVersion(name string, doc *v1.ProfileDocument) tenant.ProfileRef {
	f.t.Helper()
	store := f.p.(tenant.Deidentification)
	latest := int32(0)
	if v, err := store.ProfileVersion(f.ctx, name, 0); err == nil {
		latest = v.Version
	}
	v, err := profile.New(name, latest+1, doc, profile.Shipped())
	if err != nil {
		f.t.Fatal(err)
	}
	got, _, err := store.CreateProfileVersion(f.ctx, tenant.NewProfileVersion{Name: name, Source: v.Source(),
		Expanded: v.ExpandedText(), Checksum: v.Checksum(), Note: "a test's"}, f.clock.Now().Add(-47*time.Hour), f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return got.Ref()
}

// deidentify de-identifies a corpus file by doc under the tenant's keys, as
// a receiver that names ref would archive it, failing the test when it
// would hold it.
func deidentify(t testing.TB, file []byte, doc *v1.ProfileDocument, ref tenant.ProfileRef) []byte {
	t.Helper()
	return deidentifyAt(t, file, doc, ref, engine.Reception{})
}

// deidentifyAt is deidentify of a file received as rec says.
func deidentifyAt(t testing.TB, file []byte, doc *v1.ProfileDocument, ref tenant.ProfileRef, rec engine.Reception) []byte {
	t.Helper()
	v, err := profile.New(ref.Name, ref.Version, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), tenantKeys, engine.Options{Method: ref.String()})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := receive.TryFile(context.Background(), en, file, rec, receive.TrialOptions{Keys: tenantKeys,
		HeaderLimit: header.MaxHeader, Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }})
	if err != nil || tr.Hold != nil || tr.Written == nil {
		t.Fatalf("a corpus file is not archived by the profile: %v, %+v", err, tr.Hold)
	}
	return tr.Written
}

// uidsOf returns a file's study, series and instance UIDs.
func uidsOf(t testing.TB, b []byte) (study, series, sop string) {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	study, _ = h.UID(header.StudyUID)
	series, _ = h.UID(header.SeriesUID)
	sop, _ = h.UID(header.SOPInstanceUID)
	return study, series, sop
}

// rewrite reads a file whole, lets change change its data set, and writes
// it again in its own transfer syntax: an archived file planted with a
// fault.
func rewrite(t testing.TB, b []byte, change func(ds *dataset.Dataset)) []byte {
	t.Helper()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds := df.GetDataset()
	change(ds)
	path := filepath.Join(t.TempDir(), "rewritten.dcm")
	out, err := os.Create(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	fw := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(out))
	text := func(tg tag.Tag) string {
		e, _ := ds.Get(tg)
		v, _ := e.Bytes()
		return strings.TrimRight(string(v), "\x00 ")
	}
	fw.SetFileMetaInfo(&filewriter.FileMetaInfo{MediaStorageSOPClassUID: text(tag.New(0x0008, 0x0016)),
		MediaStorageSOPInstanceUID: text(tag.New(0x0008, 0x0018)), TransferSyntaxUID: df.FileMetaInfo.TransferSyntaxUID})
	if err := fw.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := fw.Write(); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// scanFiles are the files of one scan to archive, under its label, each
// recording profile (zero: archived as received).
type scanFiles struct {
	label   string
	files   [][]byte
	profile tenant.ProfileRef
}

// archive archives the scans into a session of subject sub-01 through the
// tenant port, as a reception would, and puts each file on the storage
// under its key; the reception ends done an hour before the clock's now.
// Each scan's series UID is its first file's; the session's study UID is
// the first file's.
func (f *fixture) archive(session string, scans ...scanFiles) {
	f.t.Helper()
	t := f.t
	at := f.clock.Now().Add(-time.Hour)
	study, _, _ := uidsOf(t, scans[0].files[0])
	r, _, err := f.p.OpenReception(f.ctx, "recv", study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	type held struct {
		scan        int
		uid, series string
		b           []byte
		sum         tenant.SHA256
	}
	var all []held
	var files []tenant.ReceivedFile
	series := make([]string, len(scans))
	for si, sc := range scans {
		_, series[si], _ = uidsOf(t, sc.files[0])
		for _, b := range sc.files {
			_, _, u := uidsOf(t, b)
			sum := tenant.SHA256(sha256.Sum256(b))
			all = append(all, held{scan: si, uid: u, series: series[si], b: b, sum: sum})
			files = append(files, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: u, SeriesUID: series[si],
				StudyUID: study, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
				Size: int64(len(b)), SHA256: sum, Incoming: r.ID + "/" + u + ".dcm",
				Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}, Association: "assoc1"})
		}
	}
	if _, err := f.p.AddReceivedFiles(f.ctx, files, at); err != nil {
		t.Fatal(err)
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", session, "main"
		}
		if r, err = f.p.StepReception(f.ctx, st, nil); err != nil {
			t.Fatalf("step %s: %v", ev, err)
		}
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(files))
	for i, fl := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: fl.SOPInstanceUID, StudyUID: fl.StudyUID, SeriesUID: fl.SeriesUID, SHA256: fl.SHA256}
	}
	if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims, r.StepTime, nil); err != nil {
		t.Fatal(err)
	}
	for si, sc := range scans {
		p := label.Path{Subject: "sub-01", Session: session, Scan: sc.label}
		var confirmed []tenant.ConfirmedInstance
		for _, h := range all {
			if h.scan != si {
				continue
			}
			key, err := layout.InstanceKey(p, h.uid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Put(f.ctx, key, bytes.NewReader(h.b)); err != nil {
				t.Fatal(err)
			}
			f.files[h.uid], f.keys[h.uid] = h.b, key
			confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: h.uid, Key: key, Length: int64(len(h.b)),
				Size: int64(len(h.b)), ArchivedSHA256: h.sum, Profile: sc.profile})
		}
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: study, SeriesUID: series[si], SubjectLabel: p.Subject,
			SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "main", ManifestSHA256: tenant.SHA256(sha256.Sum256([]byte(p.Scan))),
			At: r.StepTime, Instances: confirmed}
		if _, err := f.p.CommitScan(f.ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
}

// createDeid makes a deid job of the session, or of the whole tenant when
// session is empty.
func (f *fixture) createDeid(mode tenant.DeidMode, ref tenant.ProfileRef, session string) tenant.Job {
	f.t.Helper()
	target := tenant.JobTarget{}
	if session != "" {
		target = tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: session}
	}
	j, err := f.p.CreateJob(f.ctx, tenant.NewJob{ID: f.id("job"), Kind: tenant.JobDeid, Mode: mode, Profile: ref, Target: target,
		Created: f.clock.Now()}, f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// config returns the supervisor's configuration over the fixture.
func (f *fixture) config() Config {
	return Config{
		Tenants: &oneTenant{p: f.p}, Installation: settingsOf{},
		TenantSettings: func(tenant.Store) TenantSettings { return settingsOf{} },
		OpenStorages:   func(string, tenant.Pipeline) Storages { return storages{s: f.store} },
		UIDKey: func(context.Context, string) ([]byte, error) {
			f.keyCalls.Add(1)
			return bytes.Clone(tenantKeys.UID), nil
		},
		Reads: f.reads, Now: f.clock.Now, Interval: time.Second, PollInterval: 20 * time.Millisecond,
		seams: seams{renew: 200 * time.Millisecond},
	}
}

// start starts a supervisor over cfg, stopped at the test's end.
func (f *fixture) start(cfg Config) *Supervisor {
	f.t.Helper()
	s, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.Start(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

// waitJob waits until ok accepts the job, poking the supervisor, and
// returns it; it fails the test after the bound.
func (f *fixture) waitJob(s *Supervisor, id string, bound time.Duration, ok func(tenant.Job) bool) tenant.Job {
	f.t.Helper()
	deadline := time.Now().Add(bound)
	for {
		j, err := f.p.Job(f.ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		if ok(j) {
			return j
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s did not reach the state awaited in %s: %+v", id, bound, j)
		}
		if s != nil {
			s.Poke("lab")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ended accepts a job that has ended.
func ended(j tenant.Job) bool { return j.State.Ended() }

// failures returns every failure of a job, notes included, by instance.
func (f *fixture) failures(id string) map[string]tenant.JobFailure {
	f.t.Helper()
	out, _, err := f.p.ListJobFailures(f.ctx, id, true, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		f.t.Fatal(err)
	}
	m := make(map[string]tenant.JobFailure, len(out))
	for _, x := range out {
		m[x.SOPInstanceUID] = x
	}
	return m
}

// tallies returns a job's tallies by key.
func (f *fixture) tallies(id string) map[string]int64 {
	f.t.Helper()
	ts, err := f.p.JobTallies(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	m := map[string]int64{}
	for _, x := range ts {
		m[x.Key] = x.Count
	}
	return m
}

// snapshot is what a deid job must not change: every row of the tenant's
// database but those of jobs, their work, their events and the audit
// log, and every byte on the storage.
func (f *fixture) snapshot() string {
	f.t.Helper()
	var b strings.Builder
	var dbs []string
	_ = filepath.WalkDir(f.dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".db") {
			dbs = append(dbs, path)
		}
		return nil
	})
	if len(dbs) != 1 {
		f.t.Fatalf("the tenant's database files: %v", dbs)
	}
	db, err := sql.Open("sqlite", "file:"+dbs[0]+"?mode=ro")
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(f.ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		f.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			f.t.Fatal(err)
		}
		if !strings.HasPrefix(n, "job") && n != "work" && !strings.HasPrefix(n, "event") && n != "audit" && !strings.HasPrefix(n, "sqlite_") {
			tables = append(tables, n)
		}
	}
	_ = rows.Close()
	for _, n := range tables {
		r, err := db.QueryContext(f.ctx, `SELECT * FROM "`+n+`"`) // #nosec G202 -- a table name the database itself listed.
		if err != nil {
			f.t.Fatal(err)
		}
		cols, _ := r.Columns()
		var lines []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				f.t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%v", vals))
		}
		_ = r.Close()
		sort.Strings(lines)
		fmt.Fprintf(&b, "%s: %d rows\n%s\n", n, len(lines), strings.Join(lines, "\n"))
	}
	_ = filepath.WalkDir(f.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path) // #nosec G304 G122 -- the test's own storage, in its own temporary directory.
			fmt.Fprintf(&b, "%s %x\n", strings.TrimPrefix(path, f.root), sha256.Sum256(data))
		}
		return nil
	})
	return b.String()
}

// recorded counts how many times each instance was judged, by the judged
// seam.
type recorded struct {
	mu sync.Mutex
	n  map[string]int
}

// add counts one.
func (r *recorded) add(u string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[u]++
}

// count returns how many were judged.
func (r *recorded) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.n)
}
