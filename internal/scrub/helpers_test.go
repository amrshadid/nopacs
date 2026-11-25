package scrub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files the tests archive.
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

// settingsOf is the settings a test gives: scrub.rate and scrub.interval.
type settingsOf struct {
	rate     int64
	interval time.Duration
}

// FailuresKept answers job.failures-kept.
func (settingsOf) FailuresKept(context.Context) (int, error) { return tenant.DefaultFailuresKept, nil }

// ScrubRate answers scrub.rate.
func (s settingsOf) ScrubRate(context.Context) (int64, error) { return s.rate, nil }

// ScrubInterval answers scrub.interval.
func (s settingsOf) ScrubInterval(context.Context) (time.Duration, error) { return s.interval, nil }

// oneTenant is the installation of one tenant, "lab".
type oneTenant struct{ p tenant.Pipeline }

// Names answers the one tenant.
func (o *oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database.
func (o *oneTenant) Store(string) (tenant.Store, error) { return o.p, nil }

// storages answers every storage name with one store, never written to by
// a scrub: the test counts the writes.
type storages struct{ s blob.Store }

// Get answers the storage.
func (s storages) Get(context.Context, string) (blob.Store, error) { return s.s, nil }

// Close keeps it open: the test closes it.
func (storages) Close() {}

// readOnly is a store that fails the test on any write: a scrub only reads
// and lists.
type readOnly struct {
	blob.Store
	writes *atomic.Int64
}

// Put counts a write.
func (r readOnly) Put(ctx context.Context, key string, rd io.Reader) (blob.Written, error) {
	r.writes.Add(1)
	return r.Store.Put(ctx, key, rd)
}

// Delete counts a write.
func (r readOnly) Delete(ctx context.Context, key string) error {
	r.writes.Add(1)
	return r.Store.Delete(ctx, key)
}

// fixture is a tenant's SQLite database with a directory storage and
// archived instances on it, each scan with its manifest.
type fixture struct {
	t      testing.TB
	ctx    context.Context
	p      tenant.Pipeline
	store  blob.Store
	reads  *checkedread.Slots
	seq    atomic.Int64
	writes atomic.Int64
	// files are the archived files by SOP Instance UID, keys their keys.
	files map[string][]byte
	keys  map[string]string
	// editManifest, when set, changes a scan's manifest lines before the
	// manifest is written and its checksum committed: a manifest whole by
	// its checksum that says another thing than the catalog.
	editManifest func(lines []manifest.File)
	// installationKeys authenticate the correction records on the storage.
	installationKeys *secret.Keys
}

// fixtureTenantID is the fixture's tenant's id, which its records are
// written for.
const fixtureTenantID = "T0000000000001"

// newFixture makes the tenant "lab" with a storage "main" and a receiver.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), reads: &checkedread.Slots{}, files: map[string][]byte{}, keys: map[string]string{},
		installationKeys: testkit.InstallationKeys(t)}
	d := t.TempDir()
	s, err := (driver.SQLiteTenants{DataDir: d}).Open(f.ctx, fixtureTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
	root := filepath.Join(d, "storage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	f.store = ds
	now := time.Now().Add(-48 * time.Hour)
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
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

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

// archive archives files into one scan of subject sub-01, in a session of
// its own named "ses-" and the scan's label (each scan's files may be of
// another study), through the tenant port, as a reception would: each file
// put under its
// key, the scan's manifest written, and its catalog rows committed with the
// manifest's checksum. The scan's series UID is the first file's.
func (f *fixture) archive(scan string, files ...[]byte) label.Path {
	f.t.Helper()
	study, series, _ := uidsOf(f.t, files[0])
	entries := make([]entry, len(files))
	for i, b := range files {
		_, _, u := uidsOf(f.t, b)
		entries[i] = entry{uid: u, b: b}
	}
	return f.archiveEntries(scan, study, series, entries)
}

// entry is one file a test archives: its SOP Instance UID and bytes.
type entry struct {
	uid string
	b   []byte
}

// synthetic returns n files of a few bytes each, under UIDs made from
// base: a scrub reads an instance's bytes and never parses them, so a
// scan of many instances needs no DICOM file per instance.
func synthetic(base string, n int) []entry {
	out := make([]entry, n)
	for i := range out {
		out[i] = entry{uid: fmt.Sprintf("%s.%d", base, i+1), b: fmt.Appendf(nil, "synthetic instance %s.%d", base, i+1)}
	}
	return out
}

// reception opens a reception of the study's files into the scan, records
// them, names it and claims every instance: a reception in flight, its
// files not yet put or committed.
func (f *fixture) reception(scan, study, series string, files []entry) (tenant.Reception, []tenant.ReceivedFile, func(from reception.State, ev reception.Event)) {
	f.t.Helper()
	t := f.t
	at := time.Now().Add(-time.Hour)
	r, _, err := f.p.OpenReception(f.ctx, "recv", study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	var received []tenant.ReceivedFile
	for _, e := range files {
		received = append(received, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: e.uid, SeriesUID: series, StudyUID: study,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: int64(len(e.b)),
			SHA256: tenant.SHA256(sha256.Sum256(e.b)), Incoming: r.ID + "/" + e.uid + ".dcm",
			Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}, Association: "assoc1"})
	}
	for start := 0; start < len(received); start += 500 {
		if _, err := f.p.AddReceivedFiles(f.ctx, received[start:min(start+500, len(received))], at); err != nil {
			t.Fatal(err)
		}
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", "ses-"+scan, "main"
		}
		if r, err = f.p.StepReception(f.ctx, st, nil); err != nil {
			t.Fatalf("step %s: %v", ev, err)
		}
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(received))
	for i, fl := range received {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: fl.SOPInstanceUID, StudyUID: fl.StudyUID, SeriesUID: fl.SeriesUID, SHA256: fl.SHA256}
	}
	for start := 0; start < len(claims); start += tenant.MaxClaimBatch {
		if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims[start:min(start+tenant.MaxClaimBatch, len(claims))], r.StepTime, nil); err != nil {
			t.Fatal(err)
		}
	}
	return r, received, step
}

// put puts each file under its key in the scan p and returns its
// confirmed instance and manifest line.
func (f *fixture) put(p label.Path, files []entry) ([]tenant.ConfirmedInstance, []manifest.File) {
	f.t.Helper()
	var confirmed []tenant.ConfirmedInstance
	var lines []manifest.File
	for _, e := range files {
		key, err := layout.InstanceKey(p, e.uid)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err := f.store.Put(f.ctx, key, bytes.NewReader(e.b)); err != nil {
			f.t.Fatal(err)
		}
		f.files[e.uid], f.keys[e.uid] = e.b, key
		sum := tenant.SHA256(sha256.Sum256(e.b))
		confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: e.uid, Key: key, Length: int64(len(e.b)),
			Size: int64(len(e.b)), ArchivedSHA256: sum})
		lines = append(lines, manifest.File{Key: key, Length: int64(len(e.b)), Size: int64(len(e.b)), SHA256: sum.String(), SOPInstanceUID: e.uid})
	}
	return confirmed, lines
}

// putManifest writes the scan's manifest of lines and returns its SHA-256.
func (f *fixture) putManifest(p label.Path, lines []manifest.File) tenant.SHA256 {
	f.t.Helper()
	m, err := manifest.Encode(manifest.Manifest{Files: lines})
	if err != nil {
		f.t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(p)
	if _, err := f.store.Put(f.ctx, mk, bytes.NewReader(m)); err != nil {
		f.t.Fatal(err)
	}
	return tenant.SHA256(sha256.Sum256(m))
}

// archiveEntries archives files of the study and series into one scan, as
// archive does.
func (f *fixture) archiveEntries(scan, study, series string, files []entry) label.Path {
	f.t.Helper()
	t := f.t
	r, _, step := f.reception(scan, study, series, files)
	p := label.Path{Subject: "sub-01", Session: "ses-" + scan, Scan: scan}
	confirmed, lines := f.put(p, files)
	if f.editManifest != nil {
		f.editManifest(lines)
	}
	sum := f.putManifest(p, lines)
	for start := 0; start < len(confirmed); start += tenant.MaxScanCommit {
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: study, SeriesUID: series, SubjectLabel: p.Subject, SessionLabel: p.Session,
			ScanLabel: p.Scan, Storage: "main", ManifestSHA256: sum, At: r.StepTime,
			Instances: confirmed[start:min(start+tenant.MaxScanCommit, len(confirmed))]}
		if _, err := f.p.CommitScan(f.ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
	return p
}

// config returns the supervisor's configuration over the fixture: reads
// unpaced (64 GiB a second), no wait before a second look.
func (f *fixture) config() Config {
	c := Config{
		Tenants:        &oneTenant{p: f.p},
		TenantSettings: func(tenant.Store) TenantSettings { return settingsOf{rate: 64 << 30} },
		OpenStorages: func(string, tenant.Pipeline) Storages {
			return storages{s: readOnly{Store: f.store, writes: &f.writes}}
		},
		Reads: f.reads, NewID: func() string { return f.id("audit") }, Interval: time.Second, PollInterval: 20 * time.Millisecond,
		ScheduleInterval: time.Hour, Keys: f.installationKeys,
		TenantID: func(context.Context, string) (string, error) { return fixtureTenantID, nil },
	}
	return withSettle(withRenew(c, 200*time.Millisecond), -1)
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

// createScrub makes a scrub job of the storage.
func (f *fixture) createScrub(storage string) tenant.Job {
	f.t.Helper()
	j, err := f.p.CreateJob(f.ctx, tenant.NewJob{ID: f.id("job"), Kind: tenant.JobScrub, Storage: storage, Created: time.Now()},
		f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// waitEnded waits until the job has ended, poking the supervisor, and
// returns it; it fails the test after the bound.
func (f *fixture) waitEnded(s *Supervisor, id string) tenant.Job {
	f.t.Helper()
	return f.waitEndedWithin(s, id, 60*time.Second)
}

// waitEndedWithin waits as waitEnded does, failing the test after bound.
func (f *fixture) waitEndedWithin(s *Supervisor, id string, bound time.Duration) tenant.Job {
	f.t.Helper()
	deadline := time.Now().Add(bound)
	for {
		j, err := f.p.Job(f.ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		if j.State.Ended() {
			return j
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s did not end: %+v", id, j)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
}

// scrub runs a scrub of the storage "main" to its end with cfg.
func (f *fixture) scrub(cfg Config) tenant.Job {
	f.t.Helper()
	j := f.createScrub("main")
	s := f.start(cfg)
	return f.waitEnded(s, j.ID)
}

// findings returns the findings that hold, as "kind key" in order.
func (f *fixture) findings(includeCleared bool) []string {
	f.t.Helper()
	got, _, err := f.p.ListScrubFindings(f.ctx, tenant.ScrubFindingFilter{IncludeCleared: includeCleared}, master.Page{Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	out := make([]string, len(got))
	for i, g := range got {
		out[i] = string(g.Kind) + " " + g.Key
	}
	sort.Strings(out)
	return out
}

// tallies returns a job's tallies by key.
func (f *fixture) tallies(id string) map[string]int64 {
	f.t.Helper()
	got, err := f.p.JobTallies(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]int64{}
	for _, g := range got {
		out[g.Key] = g.Count
	}
	return out
}

// snapshot returns every key of the storage with the SHA-256 of its
// object, read straight from the store.
func (f *fixture) snapshot() map[string]tenant.SHA256 {
	f.t.Helper()
	keys, err := listAll(f.ctx, f.store, "", "")
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]tenant.SHA256{}
	for _, k := range keys {
		rc, err := f.store.Read(f.ctx, k, 0, -1)
		if err != nil {
			f.t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			f.t.Fatal(err)
		}
		out[k] = tenant.SHA256(sha256.Sum256(b))
	}
	return out
}

// marked returns whether the catalog's read of the instance answers it
// marked.
func (f *fixture) marked(p label.Path, uid string) bool {
	f.t.Helper()
	in, err := f.p.HeldInstance(f.ctx, p, uid)
	if err != nil {
		f.t.Fatal(err)
	}
	return in.Damaged
}
