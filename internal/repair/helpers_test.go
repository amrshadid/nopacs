package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testTenantID is the fixture's tenant's id.
const testTenantID = "T0000000000001"

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// Now answers the clock's time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Add moves the clock forward.
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// entry is one synthetic instance file: its SOP Instance UID and its bytes,
// made from seed and size by pattern, never held whole.
type entry struct {
	uid  string
	size int64
	seed byte
}

// pattern returns the bytes of a synthetic file, streamed, so a file of
// 64 MiB is never held in memory.
func pattern(seed byte, size int64) io.Reader {
	return io.LimitReader(&patternReader{seed: seed}, size)
}

// patternReader yields pattern's bytes without end.
type patternReader struct {
	seed byte
	at   int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	for i := range b {
		x := uint64(p.at) + uint64(i) // #nosec G115 -- a position, never negative.
		b[i] = p.seed ^ byte(x&0xff) ^ byte((x>>11)&0xff)
	}
	p.at += int64(len(b))
	return len(b), nil
}

// sumOf returns the SHA-256 of a synthetic file.
func sumOf(e entry) tenant.SHA256 {
	h := sha256.New()
	if _, err := io.Copy(h, pattern(e.seed, e.size)); err != nil {
		panic(err)
	}
	return tenant.SHA256(h.Sum(nil))
}

// synthetic returns n entries of size bytes, their UIDs numbered from 1.
func synthetic(n int, size int64) []entry {
	out := make([]entry, n)
	for i := range out {
		out[i] = entry{uid: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.7.1.%d", i+1), size: size, seed: byte(31 + i)}
	}
	return out
}

// fixture is a tenant's SQLite database with a directory storage "main"
// holding one archived scan, and a directory storage "copy" made its
// replica and brought in step.
type fixture struct {
	t       testing.TB
	ctx     context.Context
	p       tenant.Pipeline
	clock   *fakeClock
	primary *store
	replica *store
	seq     atomic.Int64
	path    label.Path
	study   string
	series  string
	files   []entry
	keys    []string
	// scrubFence is the scrub unit findings are recorded under; scrubJob
	// its job.
	scrubFence tenant.WorkFence
	scrubJob   string
	// made names the repair jobs whose rows the test made.
	made map[string]bool
}

// newFixture archives files into one scan on "main", makes "copy" its
// replica and copies the scan to it.
func newFixture(t testing.TB, files []entry) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), clock: &fakeClock{t: time.Now().UTC().Truncate(time.Second)}, files: files,
		made:  map[string]bool{},
		path:  label.Path{Subject: "sub-01", Session: "ses-01", Scan: "scan-01"},
		study: "1.2.826.0.1.3680043.2.1125.8.1", series: "1.2.826.0.1.3680043.2.1125.9.1"}
	d := t.TempDir()
	s, err := (driver.SQLiteTenants{DataDir: d}).Open(f.ctx, testTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
	f.primary = &store{Store: openDir(t, filepath.Join(d, "main"))}
	f.replica = &store{Store: openDir(t, filepath.Join(d, "copy"))}
	at := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"main", "copy"} {
		if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
			Directory: tenant.DirectoryStorage{Root: "archive", Path: name}}, at, f.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.archive()
	if _, _, _, err := f.p.AttachReplica(f.ctx, "copy", "main", f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.copyToReplica()
	f.startScrub()
	return f
}

// openDir opens a directory storage at root.
func openDir(t testing.TB, root string) blob.Store {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := dir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

// archive archives the fixture's files into its scan on "main" as a
// reception does: each file put, the sidecar and manifest written, the
// catalog rows committed.
func (f *fixture) archive() {
	t := f.t
	t.Helper()
	at := time.Now().Add(-time.Hour)
	r, _, err := f.p.OpenReception(f.ctx, "recv", f.study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	var received []tenant.ReceivedFile
	for _, e := range f.files {
		received = append(received, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: e.uid, SeriesUID: f.series, StudyUID: f.study,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: e.size, SHA256: sumOf(e),
			Incoming: r.ID + "/" + e.uid + ".dcm", Association: "assoc1",
			Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}})
	}
	if _, err := f.p.AddReceivedFiles(f.ctx, received, at); err != nil {
		t.Fatal(err)
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = f.path.Subject, f.path.Session, "main"
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
	if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims, r.StepTime, nil); err != nil {
		t.Fatal(err)
	}
	var confirmed []tenant.ConfirmedInstance
	var lines []manifest.File
	for _, e := range f.files {
		key, err := layout.InstanceKey(f.path, e.uid)
		if err != nil {
			t.Fatal(err)
		}
		w, err := f.primary.Store.Put(f.ctx, key, pattern(e.seed, e.size))
		if err != nil {
			t.Fatal(err)
		}
		sum := tenant.SHA256(w.SHA256)
		f.keys = append(f.keys, key)
		confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: e.uid, Key: key, Length: e.size, Size: e.size, ArchivedSHA256: sum})
		lines = append(lines, manifest.File{Key: key, Length: e.size, Size: e.size, SHA256: sum.String(), SOPInstanceUID: e.uid})
	}
	f.putPrimary(mustKey(layout.MetadataKey(f.path)), fmt.Appendf(nil, "sidecar with %d files\n", len(lines)))
	m, err := manifest.Encode(manifest.Manifest{Files: lines})
	if err != nil {
		t.Fatal(err)
	}
	f.putPrimary(mustKey(layout.ManifestKey(f.path)), m)
	c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: f.study, SeriesUID: f.series, SubjectLabel: f.path.Subject,
		SessionLabel: f.path.Session, ScanLabel: f.path.Scan, Storage: "main", ManifestSHA256: tenant.SHA256(sha256.Sum256(m)),
		At: r.StepTime, Instances: confirmed}
	if _, err := f.p.CommitScan(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
}

// putPrimary puts b at key on "main".
func (f *fixture) putPrimary(key string, b []byte) {
	f.t.Helper()
	if _, err := f.primary.Store.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
		f.t.Fatal(err)
	}
}

// mustKey returns a key the layout made, panicking on an error a test's own
// labels never give.
func mustKey(k string, err error) string {
	if err != nil {
		panic(err)
	}
	return k
}

// opener returns an OpenFunc over the fixture's two storages.
func (f *fixture) opener() OpenFunc {
	return func(_ context.Context, name string) (blob.Store, error) {
		switch name {
		case "main":
			return keepOpen{f.primary}, nil
		case "copy":
			return keepOpen{f.replica}, nil
		}
		return nil, fmt.Errorf("no storage %q", name)
	}
}

// copyToReplica runs the replicator until the scan is in step on "copy".
func (f *fixture) copyToReplica() {
	f.t.Helper()
	w, err := replicate.New(replicate.Config{Queue: f.p, Catalog: f.p, Now: f.clock.Now, Owner: "replicator-test",
		Open: replicate.OpenFunc(f.opener())})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := w.RunOnce(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	if !f.inStep() {
		f.t.Fatal("the scan is not in step on the replica after its copy")
	}
}

// inStep reports whether "copy" is in step for the scan.
func (f *fixture) inStep() bool {
	f.t.Helper()
	m, err := f.p.InStepReplicas(f.ctx, []string{f.series})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(m[f.series]) == 1 && m[f.series][0] == "copy"
}

// startScrub makes a scrub job of "main", lists it and claims its scan's
// unit, under whose fence the test records findings.
func (f *fixture) startScrub() {
	t := f.t
	t.Helper()
	f.scrubJob = f.id("sj")
	now := f.clock.Now()
	if _, err := f.p.CreateJob(f.ctx, tenant.NewJob{ID: f.scrubJob, Kind: tenant.JobScrub, Storage: "main", Created: now}, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := f.p.ClaimWork(f.ctx, tenant.ScrubWorkKind, "lister", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.StartJobUnit(f.ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	for more := true; more; {
		if _, more, err = f.p.ListJobScans(f.ctx, w.Fence(), now); err != nil {
			t.Fatal(err)
		}
	}
	w, err = f.p.ClaimWork(f.ctx, tenant.ScrubWorkKind, "scrubber", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.StartJobUnit(f.ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	f.scrubFence = w.Fence()
}

// find records what a scrub finds of the instance numbered i (from 0) on
// "main" now: missing, or damaged with the bytes there.
func (f *fixture) find(i int) tenant.ScrubFinding {
	f.t.Helper()
	e := f.files[i]
	expected, sum := e.size, sumOf(e)
	fd := tenant.ScrubFinding{Kind: tenant.FindingMissing, Storage: "main", Key: f.keys[i], Path: f.path, SOPInstanceUID: e.uid,
		Revision: revisionOf(f.keys[i]), ExpectedSize: &expected, ExpectedSHA256: sum}
	if size, got, ok := f.measure(f.primary.Store, f.keys[i]); ok {
		fd.Kind, fd.FoundSize, fd.FoundSHA256 = tenant.FindingDamaged, &size, got
	}
	return f.record(fd)
}

// record records fd under the scrub unit's fence.
func (f *fixture) record(fd tenant.ScrubFinding) tenant.ScrubFinding {
	f.t.Helper()
	fd.ID = tenant.ScrubFindingID(f.scrubJob, fd.Storage, fd.Key, fd.Kind)
	got, _, err := f.p.RecordScrubFinding(f.ctx, tenant.ScrubFindingRecord{Fence: f.scrubFence, Finding: fd, Now: f.clock.Now()})
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// revisionOf returns the revision an instance's key names.
func revisionOf(key string) int {
	k, err := layout.Parse(key)
	if err != nil || k.Kind != layout.Instance {
		return 0
	}
	return k.Rev()
}

// measure returns the size and SHA-256 of the object at key on s, and
// whether there is one.
func (f *fixture) measure(s blob.Store, key string) (int64, tenant.SHA256, bool) {
	f.t.Helper()
	rc, err := s.Read(f.ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return 0, tenant.SHA256{}, false
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		f.t.Fatal(err)
	}
	return n, tenant.SHA256(h.Sum(nil)), true
}

// damage flips one byte of the object at key on s, in place.
func (f *fixture) damage(s blob.Store, key string) {
	f.t.Helper()
	rc, err := s.Read(f.ctx, key, 0, -1)
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		f.t.Fatal(err)
	}
	b[len(b)/2] ^= 0x40
	if _, err := s.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
		f.t.Fatal(err)
	}
}

// remove deletes the object at key on s.
func (f *fixture) remove(s blob.Store, key string) {
	f.t.Helper()
	if err := s.Delete(f.ctx, key); err != nil {
		f.t.Fatal(err)
	}
}

// wantGood fails unless the primary's object of the instance numbered i is
// its archived bytes.
func (f *fixture) wantGood(i int) {
	f.t.Helper()
	size, sum, ok := f.measure(f.primary.Store, f.keys[i])
	if !ok || size != f.files[i].size || sum != sumOf(f.files[i]) {
		f.t.Errorf("the primary's object %d reads (%d bytes, present %v), not its archived bytes", i, size, ok)
	}
}

// findingOf returns the finding of id, cleared or not.
func (f *fixture) findingOf(id string) tenant.ScrubFinding {
	f.t.Helper()
	list, _, err := f.p.ListScrubFindings(f.ctx, tenant.ScrubFindingFilter{IncludeCleared: true}, master.Page{Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == id {
			return x
		}
	}
	f.t.Fatalf("no finding %s", id)
	return tenant.ScrubFinding{}
}

// repairsOf returns the repairs of a finding.
func (f *fixture) repairsOf(id string) []tenant.Repair {
	f.t.Helper()
	r, err := f.p.FindingRepairs(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// marked reports whether the catalog answers the instance numbered i
// marked.
func (f *fixture) marked(i int) bool {
	f.t.Helper()
	in, err := f.p.HeldInstance(f.ctx, f.path, f.files[i].uid)
	if err != nil {
		f.t.Fatal(err)
	}
	return in.Damaged
}

// config returns a runner's configuration over the fixture.
func (f *fixture) config() Config {
	return Config{DB: f.p, Tenant: "lab", Open: f.opener(), Now: f.clock.Now, Owner: "repair-test", Lease: 10 * time.Second,
		PutMargin: 2 * time.Second}
}

// runner returns a runner over cfg.
func (f *fixture) runner(cfg Config) *Runner {
	f.t.Helper()
	r, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// manual returns a manual job of "main" mending the findings given.
func (f *fixture) manual(findings ...string) Job {
	return Job{ID: f.id("rj"), Storage: "main", Mode: ModeManual, Findings: findings}
}

// keepOpen is a storage whose Close does nothing: the test closes it.
type keepOpen struct{ blob.Store }

// Close keeps the storage open.
func (keepOpen) Close() error { return nil }

// store is a storage a test can make misbehave: flip a byte of what reads
// of some keys give, fail every put, or call a hook at each read.
type store struct {
	blob.Store
	mu sync.Mutex
	// flipRead flips the first byte of what a read of these keys gives.
	flipRead map[string]bool
	// failPut fails every put as a storage that did not answer; lie keeps
	// each put's bytes with one flipped while reporting those given.
	failPut, lie bool
	// onRead runs before each read; onPut before each put, with its
	// context.
	onRead func(key string)
	onPut  func(ctx context.Context, key string)
	// reads counts the reads made; puts lists the keys put.
	reads int
	puts  []string
}

// Put fails as the test says, or puts and lists the key.
func (s *store) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	s.mu.Lock()
	fail, lie, hook := s.failPut, s.lie, s.onPut
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, key)
	}
	if fail {
		return blob.Written{}, fault.New(fault.Unavailable, "the test's storage does not answer")
	}
	if lie {
		b, err := io.ReadAll(r)
		if err != nil {
			return blob.Written{}, err
		}
		given := blob.Written{Size: int64(len(b)), SHA256: sha256.Sum256(b)}
		b[0] ^= 0x01
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(b)); err != nil {
			return blob.Written{}, err
		}
		return given, nil
	}
	w, err := s.Store.Put(ctx, key, r)
	if err == nil {
		s.mu.Lock()
		s.puts = append(s.puts, key)
		s.mu.Unlock()
	}
	return w, err
}

// Read applies the test's misbehavior to the storage's own read.
func (s *store) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.reads++
	hook := s.onRead
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	s.mu.Lock()
	flip := s.flipRead[key]
	s.mu.Unlock()
	rc, err := s.Store.Read(ctx, key, offset, length)
	if err != nil || !flip {
		return rc, err
	}
	return &flipper{ReadCloser: rc}, nil
}

// set changes the store's misbehavior under its lock.
func (s *store) set(change func(s *store)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

// counts returns the reads made and the puts listed so far.
func (s *store) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, len(s.puts)
}

// flipper flips the first byte of a stream.
type flipper struct {
	io.ReadCloser
	done bool
}

func (f *flipper) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	if n > 0 && !f.done {
		p[0] ^= 0x01
		f.done = true
	}
	return n, err
}

// errTest is a failure a test makes.
var errTest = errors.New("the test's failure")
