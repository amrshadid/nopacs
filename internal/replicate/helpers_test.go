package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// entry is one synthetic instance file: its SOP Instance UID and its
// bytes, made from seed and size by pattern, never held whole.
type entry struct {
	uid  string
	size int64
	seed byte
}

// pattern returns the bytes of a synthetic file: size bytes made from seed,
// streamed, so a file of 64 MiB is never held in memory.
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

// scanInfo is what the fixture archived of one scan.
type scanInfo struct {
	path           label.Path
	study, series  string
	lines          []manifest.File
	confirmed      []tenant.ConfirmedInstance
	manifestSHA256 tenant.SHA256
}

// fixture is a tenant's SQLite database with a directory storage "main"
// that holds archived scans, and a directory storage "copy", its replica
// once attach is called.
type fixture struct {
	t       testing.TB
	ctx     context.Context
	p       tenant.Pipeline
	clock   *fakeClock
	primary *store
	replica *store
	seq     atomic.Int64
	scans   map[string]*scanInfo
	// editManifest, when set, changes a scan's next manifest lines before
	// the manifest is written and its checksum committed.
	editManifest func(lines []manifest.File) []manifest.File
	// sidecarDigest, when set, commits each scan with its sidecar's
	// SHA-256, as a reception does.
	sidecarDigest bool
}

// newFixture makes the tenant "lab" with the storages "main" and "copy"
// and a receiver into "main".
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), clock: &fakeClock{t: time.Now().UTC().Truncate(time.Second)}, scans: map[string]*scanInfo{}}
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

// synthetic returns n entries of the scan numbered scan, each of size
// bytes, their UIDs numbered from first.
func synthetic(scan, first, n int, size int64) []entry {
	out := make([]entry, n)
	for i := range out {
		k := first + i
		out[i] = entry{uid: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.7.%d.%d", scan, k), size: size, seed: byte((scan*31 + k) & 0xff)}
	}
	return out
}

// archive archives entries into the scan labeled scan (numbered n for its
// UIDs) of subject sub-01 on "main" as a reception does: each file put,
// the sidecar and manifest written, the catalog rows committed. A scan
// archived before is added to.
func (f *fixture) archive(n int, scan string, files []entry) *scanInfo {
	f.t.Helper()
	commit := f.stage(n, scan, files)
	commit()
	return f.scans[scan]
}

// stage is archive up to the commit: the files, the sidecar and the next
// manifest are on "main", the instances claimed by a reception, and the
// catalog not yet told. It returns the commit.
func (f *fixture) stage(n int, scan string, files []entry) (commit func()) {
	f.t.Helper()
	t := f.t
	si := f.scans[scan]
	if si == nil {
		si = &scanInfo{path: label.Path{Subject: "sub-01", Session: "ses-" + scan, Scan: scan},
			study: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.8.%d", n), series: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.9.%d", n)}
	}
	at := time.Now().Add(-time.Hour)
	r, _, err := f.p.OpenReception(f.ctx, "recv", si.study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	var received []tenant.ReceivedFile
	for _, e := range files {
		received = append(received, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: e.uid, SeriesUID: si.series, StudyUID: si.study,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: e.size, SHA256: sumOf(e),
			Incoming: r.ID + "/" + e.uid + ".dcm", Association: "assoc1",
			Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}})
	}
	if _, err := f.p.AddReceivedFiles(f.ctx, received, at, nil); err != nil {
		t.Fatal(err)
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = si.path.Subject, si.path.Session, "main"
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
	lines := slices.Clone(si.lines)
	for _, e := range files {
		key, err := layout.InstanceKey(si.path, e.uid)
		if err != nil {
			t.Fatal(err)
		}
		w, err := f.primary.Store.Put(f.ctx, key, pattern(e.seed, e.size))
		if err != nil {
			t.Fatal(err)
		}
		sum := tenant.SHA256(w.SHA256)
		confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: e.uid, Key: key, Length: e.size, Size: e.size, ArchivedSHA256: sum})
		lines = append(lines, manifest.File{Key: key, Length: e.size, Size: e.size, SHA256: sum.String(), SOPInstanceUID: e.uid})
	}
	sidecar := fmt.Appendf(nil, "sidecar of %s with %d files\n", scan, len(lines))
	f.putPrimary(mustKey(layout.MetadataKey(si.path)), sidecar)
	if f.editManifest != nil {
		lines = f.editManifest(lines)
	}
	sum := f.putManifest(si.path, lines)
	return func() {
		t.Helper()
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: si.study, SeriesUID: si.series, SubjectLabel: si.path.Subject,
			SessionLabel: si.path.Session, ScanLabel: si.path.Scan, Storage: "main", ManifestSHA256: sum, At: r.StepTime, Instances: confirmed}
		if f.sidecarDigest {
			c.SidecarSHA256 = tenant.SHA256(sha256.Sum256(sidecar))
		}
		if _, err := f.p.CommitScan(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		step(reception.Identified, reception.Archive)
		step(reception.Archived, reception.Verify)
		step(reception.Verified, reception.Finish)
		si.lines, si.manifestSHA256 = lines, sum
		si.confirmed = append(si.confirmed, confirmed...)
		f.scans[scan] = si
	}
}

// putManifest writes the manifest of lines on "main" and returns its
// SHA-256.
func (f *fixture) putManifest(p label.Path, lines []manifest.File) tenant.SHA256 {
	f.t.Helper()
	m, err := manifest.Encode(manifest.Manifest{Files: lines})
	if err != nil {
		f.t.Fatal(err)
	}
	f.putPrimary(mustKey(layout.ManifestKey(p)), m)
	return tenant.SHA256(sha256.Sum256(m))
}

// putPrimary puts b at key on "main".
func (f *fixture) putPrimary(key string, b []byte) {
	f.t.Helper()
	if _, err := f.primary.Store.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
		f.t.Fatal(err)
	}
}

// mustKey returns a key the layout made, panicking on an error a test's
// own labels never give.
func mustKey(k string, err error) string {
	if err != nil {
		panic(err)
	}
	return k
}

// attach makes "copy" a replica of "main", queuing every scan.
func (f *fixture) attach() {
	f.t.Helper()
	if _, _, _, err := f.p.AttachReplica(f.ctx, "copy", "main", f.clock.Now(), f.audit(), nil); err != nil {
		f.t.Fatal(err)
	}
}

// config returns a worker's configuration over the fixture.
func (f *fixture) config() Config {
	return Config{Queue: f.p, Catalog: f.p, Now: f.clock.Now, Owner: "replicator-test", Parallel: 2, Scans: 2,
		Open: func(_ context.Context, name string) (blob.Store, error) {
			switch name {
			case "main":
				return keepOpen{f.primary}, nil
			case "copy":
				return keepOpen{f.replica}, nil
			}
			return nil, fmt.Errorf("no storage %q", name)
		}}
}

// worker returns a worker over cfg.
func (f *fixture) worker(cfg Config) *Worker {
	f.t.Helper()
	w, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// runOnce runs one pass of w and fails the test on a claim's error.
func (f *fixture) runOnce(w *Worker) Pass {
	f.t.Helper()
	p, err := w.RunOnce(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// row returns the replica's row of the scan.
func (f *fixture) row(scan string) tenant.ReplicaScan {
	f.t.Helper()
	r, err := f.p.ReplicaScan(f.ctx, "copy", f.scans[scan].series)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// tree returns every key of s with the SHA-256 of its object.
func (f *fixture) tree(s blob.Store) map[string]tenant.SHA256 {
	f.t.Helper()
	out := map[string]tenant.SHA256{}
	opts := blob.ListOptions{}
	for {
		page, err := s.List(f.ctx, opts)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, k := range page.Keys {
			rc, err := s.Read(f.ctx, k, 0, -1)
			if err != nil {
				f.t.Fatal(err)
			}
			h := sha256.New()
			_, err = io.Copy(h, rc)
			_ = rc.Close()
			if err != nil {
				f.t.Fatal(err)
			}
			out[k] = tenant.SHA256(h.Sum(nil))
		}
		if page.Next == "" {
			return out
		}
		opts.Token = page.Next
	}
}

// wantSameTrees fails unless both storages hold the same keys with the
// same bytes.
func (f *fixture) wantSameTrees() {
	f.t.Helper()
	a, b := f.tree(f.primary.Store), f.tree(f.replica.Store)
	if len(a) == 0 {
		f.t.Fatal("the primary holds nothing")
	}
	for k, s := range a {
		if b[k] != s {
			f.t.Errorf("the replica's %s differs from the primary's (or is missing)", k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			f.t.Errorf("the replica holds %s, which the primary does not", k)
		}
	}
}

// replicaHas reports whether the replica holds key.
func (f *fixture) replicaHas(key string) bool {
	_, err := f.replica.Stat(f.ctx, key)
	return err == nil
}

// queue is the tenant's queue with failures a test sets: a claim changed
// before the worker sees it, a renewal or a damage record that fails, and
// a count of the renewals made.
type queue struct {
	Queue
	mu sync.Mutex
	// claim changes each claim answered; renewErr and damageErr, when set,
	// are answered instead of renewing or recording.
	claim     func(c *tenant.ReplicaClaim)
	renewErr  error
	damageErr error
	renewed   atomic.Int64
	// lost is closed once a renewal is answered tenant.ErrLeaseLost.
	lost     chan struct{}
	lostOnce sync.Once
}

// newQueue returns a queue over the fixture's database.
func (f *fixture) newQueue() *queue { return &queue{Queue: f.p, lost: make(chan struct{})} }

// set changes the queue's failures under its lock.
func (q *queue) set(change func(q *queue)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	change(q)
}

// ClaimReplicaScans claims, then applies the test's change to each claim.
func (q *queue) ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error) {
	cs, err := q.Queue.ClaimReplicaScans(ctx, owner, now, lease, limit)
	q.mu.Lock()
	change := q.claim
	q.mu.Unlock()
	if change != nil {
		for i := range cs {
			change(&cs[i])
		}
	}
	return cs, err
}

// RenewReplicaScan answers the test's failure, or renews and counts it.
func (q *queue) RenewReplicaScan(ctx context.Context, fence tenant.ReplicaFence, now time.Time, lease time.Duration) error {
	q.mu.Lock()
	fail := q.renewErr
	q.mu.Unlock()
	if fail != nil {
		return fail
	}
	err := q.Queue.RenewReplicaScan(ctx, fence, now, lease)
	if err == nil {
		q.renewed.Add(1)
	}
	if errors.Is(err, tenant.ErrLeaseLost) {
		q.lostOnce.Do(func() { close(q.lost) })
	}
	return err
}

// RecordReplicaDamage answers the test's failure, or records.
func (q *queue) RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error) {
	q.mu.Lock()
	fail := q.damageErr
	q.mu.Unlock()
	if fail != nil {
		return false, fail
	}
	return q.Queue.RecordReplicaDamage(ctx, d)
}

// catalog is the tenant's catalog with each instance it answers changed by
// the test.
type catalog struct {
	Catalog
	edit func(in *tenant.ScanInstance)
}

// ListHeldInstances lists, then applies the test's change.
func (c catalog) ListHeldInstances(ctx context.Context, path label.Path, page master.Page) ([]tenant.ScanInstance, string, error) {
	rows, next, err := c.Catalog.ListHeldInstances(ctx, path, page)
	for i := range rows {
		c.edit(&rows[i])
	}
	return rows, next, err
}

// keepOpen is a storage whose Close does nothing: the test closes it.
type keepOpen struct{ blob.Store }

// Close keeps the storage open.
func (keepOpen) Close() error { return nil }

// store is a storage a test can make misbehave: flip a byte of what it
// reads, keep other bytes than it is given while reporting those given,
// say it confirmed a checksum, or call a hook at each put.
type store struct {
	blob.Store
	mu sync.Mutex
	// flipRead flips the first byte of what a read of these keys gives;
	// failRead fails a read of these keys as a storage that did not
	// answer.
	flipRead, failRead map[string]bool
	// lie keeps each put's bytes with one flipped and reports the bytes
	// given; confirm reports each put confirmed by the storage.
	lie, confirm bool
	// onPut runs before each put; onRead before each read.
	onPut  func(key string)
	onRead func(key string)
	// puts lists the keys put, in order.
	puts []string
}

// Put applies the test's misbehavior around the storage's own put.
func (s *store) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	s.mu.Lock()
	hook, lie, confirm := s.onPut, s.lie, s.confirm
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	var w blob.Written
	var err error
	if lie {
		b, rerr := io.ReadAll(r)
		if rerr != nil {
			return blob.Written{}, rerr
		}
		given := blob.Written{Size: int64(len(b)), SHA256: sha256.Sum256(b)}
		if len(b) > 0 {
			b[0] ^= 0x01
		}
		if _, err = s.Store.Put(ctx, key, bytes.NewReader(b)); err == nil {
			w = given
		}
	} else {
		w, err = s.Store.Put(ctx, key, r)
	}
	if err != nil {
		return blob.Written{}, err
	}
	w.Confirmed = confirm
	s.mu.Lock()
	s.puts = append(s.puts, key)
	s.mu.Unlock()
	return w, nil
}

// Read applies the test's misbehavior to the storage's own read.
func (s *store) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	s.mu.Lock()
	flip, fail, hook := s.flipRead[key], s.failRead[key], s.onRead
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	if fail {
		return nil, fault.New(fault.Unavailable, "the test's storage does not answer")
	}
	rc, err := s.Store.Read(ctx, key, offset, length)
	if err != nil || !flip {
		return rc, err
	}
	return &flipper{ReadCloser: rc}, nil
}

// put returns the keys put so far.
func (s *store) putKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.puts)
}

// set changes the store's misbehavior under its lock.
func (s *store) set(change func(s *store)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
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
