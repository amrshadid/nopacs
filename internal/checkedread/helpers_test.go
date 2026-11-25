package checkedread_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testTenantID is the fixture's tenant's id.
const testTenantID = "T0000000000001"

// roots answers the installation's directory roots: one, "fast".
type roots settings.DirectoryRoots

// DirectoryRoots answers the roots.
func (r roots) DirectoryRoots(context.Context) (settings.DirectoryRoots, error) {
	return settings.DirectoryRoots(r), nil
}

// S3Endpoints answers none.
func (roots) S3Endpoints(context.Context) (settings.S3Endpoints, error) { return nil, nil }

// fixture is the tenant "lab" in a SQLite database, with the directory
// storage "main" holding one archived scan and the directory storages
// "copy1" and "copy2", which a test makes replicas of "main".
type fixture struct {
	t    testing.TB
	ctx  context.Context
	p    tenant.Pipeline
	root string
	seq  atomic.Int64
	// path, study and series name the scan; files maps each instance's
	// UID to its bytes.
	path          label.Path
	study, series string
	files         map[string][]byte
}

// newFixture makes the tenant, its storages and receiver, and archives a
// scan of n instances of size bytes each into "main".
func newFixture(t testing.TB, n int, size int) *fixture {
	t.Helper()
	d := t.TempDir()
	f := &fixture{t: t, ctx: context.Background(), root: filepath.Join(d, "roots"),
		path:  label.Path{Subject: "sub-01", Session: "ses-01", Scan: "t1w"},
		study: "1.2.826.0.1.3680043.2.1125.8.1", series: "1.2.826.0.1.3680043.2.1125.9.1", files: map[string][]byte{}}
	s, err := (driver.SQLiteTenants{DataDir: d}).Open(f.ctx, testTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
	at := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"main", "copy1", "copy2"} {
		if err := os.MkdirAll(filepath.Join(f.root, "lab", name), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
			Directory: tenant.DirectoryStorage{Root: "fast", Path: name}}, at, f.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.archive(1, n, size)
	return f
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// dirOf opens the directory of the storage named name, as a test writes
// to it behind the archive.
func (f *fixture) dirOf(name string) blob.Store {
	f.t.Helper()
	s, err := dir.Open(filepath.Join(f.root, "lab", name))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	return s
}

// archive archives n more instances of size bytes, numbered from first,
// into the scan on "main" as a reception does: each file put, the catalog
// rows committed with a new manifest digest.
func (f *fixture) archive(first, n, size int) {
	f.t.Helper()
	t := f.t
	at := time.Now().Add(-time.Hour)
	r, _, err := f.p.OpenReception(f.ctx, "recv", f.study, fmt.Sprintf("rc%08d", f.seq.Add(1)), at)
	if err != nil {
		t.Fatal(err)
	}
	main := f.dirOf("main")
	var (
		received  []tenant.ReceivedFile
		confirmed []tenant.ConfirmedInstance
	)
	for i := range n {
		uid := fmt.Sprintf("1.2.826.0.1.3680043.2.1125.7.1.%d", first+i)
		b := bytes.Repeat([]byte{byte((first + i) & 0xff)}, size) // #nosec G115 -- masked to a byte.
		for j := range b {
			b[j] ^= byte((j >> 7) & 0xff) // #nosec G115 -- masked to a byte.
		}
		f.files[uid] = b
		sum := tenant.SHA256(sha256.Sum256(b))
		received = append(received, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: uid, SeriesUID: f.series, StudyUID: f.study,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: int64(size), SHA256: sum,
			Incoming: r.ID + "/" + uid + ".dcm", Association: "assoc1",
			Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}})
		key, err := layout.InstanceKey(f.path, uid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := main.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: uid, Key: key, Length: int64(size), Size: int64(size), ArchivedSHA256: sum})
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
	digest := tenant.SHA256(sha256.Sum256([]byte("manifest " + strconv.Itoa(first))))
	if _, err := f.p.CommitScan(f.ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: f.study, SeriesUID: f.series, SubjectLabel: f.path.Subject,
		SessionLabel: f.path.Session, ScanLabel: f.path.Scan, Storage: "main", ManifestSHA256: digest, At: r.StepTime, Instances: confirmed}); err != nil {
		t.Fatal(err)
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
}

// held returns the catalog's row of the instance uid.
func (f *fixture) held(uid string) tenant.ScanInstance {
	f.t.Helper()
	in, err := f.p.HeldInstance(f.ctx, f.path, uid)
	if err != nil {
		f.t.Fatal(err)
	}
	return in
}

// uid returns the UID of the n-th instance archived, from 1.
func uid(n int) string { return fmt.Sprintf("1.2.826.0.1.3680043.2.1125.7.1.%d", n) }

// attach makes the storage replica a replica of "main", every scan pending
// for it.
func (f *fixture) attach(replica string) {
	f.t.Helper()
	if _, _, _, err := f.p.AttachReplica(f.ctx, replica, "main", time.Now(), f.audit(), nil); err != nil {
		f.t.Fatal(err)
	}
}

// copyTo puts every object of "main" on replica, byte for byte.
func (f *fixture) copyTo(replica string) {
	f.t.Helper()
	from, to := f.dirOf("main"), f.dirOf(replica)
	page, err := from.List(f.ctx, blob.ListOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, k := range page.Keys {
		rc, err := from.Read(f.ctx, k, 0, -1)
		if err != nil {
			f.t.Fatal(err)
		}
		_, err = to.Put(f.ctx, k, rc)
		_ = rc.Close()
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

// inStep copies "main" to replica and makes the replica's row of the scan
// in step at the catalog's manifest digest, as the replicator does once it
// verified the copy; a claim of another replica is put back pending.
func (f *fixture) inStep(replica string) {
	f.t.Helper()
	f.copyTo(replica)
	now := time.Now()
	claims, err := f.p.ClaimReplicaScans(f.ctx, "test", now, time.Minute, tenant.MaxReplicaClaim)
	if err != nil {
		f.t.Fatal(err)
	}
	ended := false
	for _, c := range claims {
		if c.Fence.Replica != replica {
			if err := f.p.FailReplicaScan(f.ctx, tenant.ReplicaFailure{Fence: c.Fence, Fault: tenant.FaultUnreachable, Reason: "not this test's replica", Retry: now, Now: now}); err != nil {
				f.t.Fatal(err)
			}
			continue
		}
		state, err := f.p.EndReplicaScan(f.ctx, c.Fence, c.ManifestSHA256, now)
		if err != nil || state != tenant.ReplicaInStep {
			f.t.Fatalf("ending the copy to %s: %s, %v", replica, state, err)
		}
		ended = true
	}
	if !ended {
		f.t.Fatalf("no row of %s to end in step", replica)
	}
}

// stores returns the tenant's Stores over the roots, each storage wrapped
// as a misbehaving store whose faults the test sets, and those stores by
// name.
func (f *fixture) stores() (*checkedread.Stores, map[string]*faulty) {
	f.t.Helper()
	s := checkedread.NewStores(place.Opener{Values: roots{{Name: "fast", Path: f.root}}},
		func(context.Context, string) (string, error) { return testTenantID, nil }, f.p, "lab")
	f.t.Cleanup(s.Close)
	all := map[string]*faulty{}
	for _, n := range []string{"main", "copy1", "copy2"} {
		all[n] = &faulty{}
	}
	checkedread.WrapStores(s, func(name string, st blob.Store) blob.Store {
		fs := all[name]
		fs.Store = st
		return fs
	})
	return s, all
}

// readAll reads in through s whole, checked against the catalog as every
// reader checks it, and returns the bytes and the storage that served.
func readAll(ctx context.Context, s *checkedread.Stores, obj checkedread.Object, in tenant.ScanInstance) ([]byte, checkedread.Opened, error) {
	o, err := s.Open(ctx, obj, in.Offset, in.Length)
	if err != nil {
		return nil, o, err
	}
	defer func() { _ = o.Body.Close() }()
	var out []byte
	sum := in.ArchivedSHA256
	_, err = checkedread.CopyChecked(o.Body, in.Length, &sum, [2][]byte{make([]byte, 4096), make([]byte, 4096)}, func(b []byte) error {
		out = append(out, b...)
		return nil
	})
	return out, o, err
}

// faulty is a storage a test makes misbehave, and counts what is asked of
// it.
type faulty struct {
	blob.Store
	mu sync.Mutex
	// fail is answered to every Read and Stat instead of the storage's
	// answer; offline makes Stat say not online; flip flips the first byte
	// of each read.
	fail          error
	offline, flip bool
	reads, stats  int
}

// set changes the store's faults under its lock.
func (s *faulty) set(change func(s *faulty)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

// asked returns how many reads and stats were asked.
func (s *faulty) asked() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.stats
}

// Read answers the fault, or the storage's read.
func (s *faulty) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	s.mu.Lock()
	s.reads++
	fail, flip := s.fail, s.flip
	s.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	rc, err := s.Store.Read(ctx, key, offset, length)
	if err != nil || !flip {
		return rc, err
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		b[0] ^= 0x01
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Stat answers the fault, or the storage's stat.
func (s *faulty) Stat(ctx context.Context, key string) (blob.Info, error) {
	s.mu.Lock()
	s.stats++
	fail, offline := s.fail, s.offline
	s.mu.Unlock()
	if fail != nil {
		return blob.Info{}, fail
	}
	info, err := s.Store.Stat(ctx, key)
	if offline {
		info.State = blob.Archived
	}
	return info, err
}

// notAnswering is a storage that does not answer.
var notAnswering = fault.New(fault.Unavailable, "the test's storage does not answer")

// notOnline is an object held in a tier its storage cannot read at once.
var notOnline = fault.Wrap(fault.Unavailable, blob.ErrNotOnline, "the test's object is in a cold tier")

// countingReplicas counts the lookups of replicas a read asks.
type countingReplicas struct {
	checkedread.Replicas
	lookups atomic.Int64
}

// InStepReplicas counts, then asks.
func (c *countingReplicas) InStepReplicas(ctx context.Context, uids []string) (map[string][]string, error) {
	c.lookups.Add(1)
	return c.Replicas.InStepReplicas(ctx, uids)
}

// CatalogScan counts, then asks.
func (c *countingReplicas) CatalogScan(ctx context.Context, p label.Path) (tenant.CatalogScan, error) {
	c.lookups.Add(1)
	return c.Replicas.CatalogScan(ctx, p)
}
