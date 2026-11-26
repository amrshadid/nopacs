package scrub

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// countingStore counts the reads of a store's objects.
type countingStore struct {
	blob.Store
	reads *atomic.Int64
}

// Read counts a read.
func (c countingStore) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	c.reads.Add(1)
	return c.Store.Read(ctx, key, offset, length)
}

// named answers each storage by its name.
type named map[string]blob.Store

// Get answers the storage of that name.
func (n named) Get(_ context.Context, name string) (blob.Store, error) {
	if s, ok := n[name]; ok {
		return s, nil
	}
	return nil, fault.New(fault.NotFound, "no storage of that name")
}

// Close keeps them open: the test closes them.
func (named) Close() {}

// replicaRig is the fixture with replicas of "main", each a store of its
// own.
type replicaRig struct {
	*fixture
	replicas map[string]blob.Store
	// replicaReads counts the scrub's reads of each replica's objects.
	replicaReads map[string]*atomic.Int64
}

// newReplicaRig makes the fixture, with no replica yet.
func newReplicaRig(t *testing.T) *replicaRig {
	t.Helper()
	return &replicaRig{fixture: newFixture(t), replicas: map[string]blob.Store{}, replicaReads: map[string]*atomic.Int64{}}
}

// addReplica adds a directory storage of that name and attaches it to
// "main" as its replica, which queues every scan of "main" for it.
func (r *replicaRig) addReplica(name string) blob.Store {
	r.t.Helper()
	root := filepath.Join(r.t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		r.t.Fatal(err)
	}
	s, err := dir.Open(root)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = s.Close() })
	at := time.Now().Add(-47 * time.Hour)
	if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "replicas", Path: name}}, at, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	if _, _, _, err := r.p.AttachReplica(r.ctx, name, "main", at, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	r.replicas[name] = s
	return s
}

// copyAll brings every scan due for a replica in step, as the replicator
// would: each object the primary's manifest names and the manifest put on
// the replica, then the copy ended at the manifest's digest.
func (r *replicaRig) copyAll() {
	r.t.Helper()
	claims, err := r.p.ClaimReplicaScans(r.ctx, "rig", time.Now(), time.Minute, tenant.MaxReplicaClaim)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, c := range claims {
		to := r.replicas[c.Fence.Replica]
		mk, _ := layout.ManifestKey(c.Path)
		data := r.read(r.store, mk)
		m, err := manifest.Decode(bytes.NewReader(data))
		if err != nil {
			r.t.Fatal(err)
		}
		for _, f := range m.Files {
			if slices.Contains(c.Purging, f.SOPInstanceUID) {
				continue // its purge began: it would outlive the purge
			}
			r.putOn(to, f.Key, r.read(r.store, f.Key))
		}
		r.putOn(to, mk, data)
		if st, err := r.p.EndReplicaScan(r.ctx, c.Fence, c.ManifestSHA256, time.Now()); err != nil || st != tenant.ReplicaInStep {
			r.t.Fatalf("EndReplicaScan = %v, %v", st, err)
		}
	}
}

// read reads the whole object at key on s.
func (r *replicaRig) read(s blob.Store, key string) []byte {
	r.t.Helper()
	rc, err := s.Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// putOn puts b at key on s.
func (r *replicaRig) putOn(s blob.Store, key string, b []byte) {
	r.t.Helper()
	if _, err := s.Put(r.ctx, key, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
}

// config returns the supervisor's configuration, each storage by its
// name, each replica's reads counted.
func (r *replicaRig) config() Config {
	c := r.fixture.config()
	stores := named{"main": readOnly{Store: r.store, writes: &r.writes}}
	for name, s := range r.replicas {
		n := r.replicaReads[name]
		if n == nil {
			n = &atomic.Int64{}
			r.replicaReads[name] = n
		}
		stores[name] = readOnly{Store: countingStore{Store: s, reads: n}, writes: &r.writes}
	}
	c.OpenStorages = func(string, tenant.Pipeline) Storages { return stores }
	return c
}

// scrubAll runs a scrub of every storage of the tenant to its end.
func (r *replicaRig) scrubAll(cfg Config) tenant.Job {
	r.t.Helper()
	j := r.createScrub("")
	s := r.start(cfg)
	got := r.waitEnded(s, j.ID)
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(stopCtx); err != nil {
		r.t.Fatal(err)
	}
	return got
}

// row returns the replica's row of the series.
func (r *replicaRig) row(replica, series string) tenant.ReplicaScan {
	r.t.Helper()
	row, err := r.p.ReplicaScan(r.ctx, replica, series)
	if err != nil {
		r.t.Fatal(err)
	}
	return row
}

// archiveTwo archives a scan of two synthetic instances and returns its
// labels, its series UID and its instances' keys.
func (r *replicaRig) archiveTwo(scan, base string) (label.Path, string, []string) {
	r.t.Helper()
	files := synthetic(base, 2)
	p := r.archiveEntries(scan, base+".9", base+".8", files)
	return p, base + ".8", []string{r.keys[files[0].uid], r.keys[files[1].uid]}
}

// TestReplicaDamageIsTheReplicasNeverTheInstances guards that a scrub of
// the whole tenant reads a replica in step like the primary: an object
// changed on the replica becomes the replica's damage (its row damaged,
// and a finding on the replica's name), never a mark on the instance nor a
// finding on the primary, and none of the replica's objects is taken for
// an orphan; and that a later scrub that
// reads it good clears the damage and puts the row in step again.
func TestReplicaDamageIsTheReplicasNeverTheInstances(t *testing.T) {
	r := newReplicaRig(t)
	p, series, keys := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.501")
	copy1 := r.addReplica("copy")
	r.copyAll()
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep {
		t.Fatalf("the replica's row before the scrub is %+v", row)
	}
	good := r.read(copy1, keys[0])
	r.putOn(copy1, keys[0], []byte("bytes that are not what was archived"))

	j := r.scrubAll(r.config())
	row := r.row("copy", series)
	if row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("after a scrub that read a changed object the replica's row is %s with %d damaged; want damaged with 1", row.State, row.Damaged)
	}
	for _, in := range r.held(p) {
		if in.Damaged {
			t.Errorf("the instance %s is marked; a replica's damage marks no instance", in.SOPInstanceUID)
		}
	}
	if got := r.findingsOn("copy"); len(got) != 1 || got[0] != "damaged "+keys[0] {
		t.Errorf("the scrub holds findings %v on the replica; want its damage alone: its objects are no orphans", got)
	}
	if got := r.findingsOn("main"); len(got) != 0 {
		t.Errorf("the scrub holds findings %v on the primary; a replica's damage is its own", got)
	}
	if got := r.tallies(j.ID)[TallyReplicaDamaged]; got != 1 {
		t.Errorf("the scrub tallied %d objects damaged on a replica; want 1", got)
	}
	if j.Counts.InstancesSent != 2 || j.Counts.InstancesRefused != 0 {
		t.Errorf("the scrub counted %d sent, %d refused; want both instances read good on their primary", j.Counts.InstancesSent,
			j.Counts.InstancesRefused)
	}
	if r.replicaReads["copy"].Load() == 0 {
		t.Error("the scrub read nothing on the replica")
	}

	r.putOn(copy1, keys[0], good)
	j = r.scrubAll(r.config())
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after a scrub that read the object good again the replica's row is %s with %d damaged; want in step with none",
			row.State, row.Damaged)
	}
	if got := r.tallies(j.ID)[TallyReplicaDamaged]; got != 0 {
		t.Errorf("the second scrub tallied %d objects damaged on a replica; want none", got)
	}
	if r.writes.Load() != 0 {
		t.Errorf("the scrubs wrote %d times to a storage", r.writes.Load())
	}
}

// held returns a scan's instances.
func (r *replicaRig) held(p label.Path) []tenant.ScanInstance {
	r.t.Helper()
	ins, _, err := r.p.ListHeldInstances(r.ctx, p, pageOf(100))
	if err != nil {
		r.t.Fatal(err)
	}
	return ins
}

// pageOf returns the first page of at most n rows.
func pageOf(n int) master.Page { return master.Page{Limit: n} }

// sorted returns s in order.
func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestPendingReplicaScanIsSkipped guards that a scan a replica has not yet
// copied (its row pending) is not read there: nothing of it is called
// missing, its row stays pending, and the scan is counted skipped.
func TestPendingReplicaScanIsSkipped(t *testing.T) {
	r := newReplicaRig(t)
	_, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.502")
	r.addReplica("copy")
	j := r.scrubAll(r.config())
	row := r.row("copy", series)
	if row.State != tenant.ReplicaPending || row.Damaged != 0 {
		t.Errorf("after a scrub the pending scan's row is %s with %d damaged; want pending with none", row.State, row.Damaged)
	}
	tallies := r.tallies(j.ID)
	if tallies[TallyReplicaSkipped] != 1 || tallies[TallyReplicaMissing] != 0 {
		t.Errorf("the scrub tallied %v; want the scan skipped once and nothing missing", tallies)
	}
	if got := r.findings(false); len(got) != 0 {
		t.Errorf("the scrub holds findings %v", got)
	}
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 {
		t.Errorf("the scrub ended as %+v; want done with both instances read good on the primary", j)
	}
}

// TestStrayOnReplicaIsRecordedNeverRemoved guards that an object on a
// replica that no manifest names, under a scan's keys or under no scan of
// the catalog, is recorded as an orphan on the replica and left where it
// is; and that once a person removes it, the next scrub clears the
// finding.
func TestStrayOnReplicaIsRecordedNeverRemoved(t *testing.T) {
	r := newReplicaRig(t)
	p, series, _ := r.archiveTwo("s1", "1.2.826.0.1.3680043.10.503")
	copy1 := r.addReplica("copy")
	r.copyAll()
	inScan, err := layout.InstanceKey(p, "1.2.826.0.1.3680043.10.503.77")
	if err != nil {
		t.Fatal(err)
	}
	outside, err := layout.InstanceKey(label.Path{Subject: "sub-99", Session: "ses-x", Scan: "x"}, "1.2.826.0.1.3680043.10.503.78")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{inScan, outside} {
		r.putOn(copy1, k, []byte("a copy that landed after its purge"))
	}

	j := r.scrubAll(r.config())
	got := r.findingsOn("copy")
	want := []string{"orphan " + inScan, "orphan " + outside}
	if strings.Join(got, ",") != strings.Join(sorted(want), ",") {
		t.Errorf("the findings on the replica are %v; want %v", got, sorted(want))
	}
	for _, k := range []string{inScan, outside} {
		if _, err := copy1.Stat(r.ctx, k); err != nil {
			t.Errorf("the stray %s is gone after the scrub (%v); a scrub never removes an object", k, err)
		}
	}
	if row := r.row("copy", series); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("a stray made the replica's row %s with %d damaged; want in step", row.State, row.Damaged)
	}
	if r.tallies(j.ID)[tenant.TallyOrphanObjects] != 2 {
		t.Errorf("the scrub tallied %v; want two orphan objects", r.tallies(j.ID))
	}

	for _, k := range []string{inScan, outside} {
		if err := copy1.Delete(r.ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	r.scrubAll(r.config())
	if got := r.findingsOn("copy"); len(got) != 0 {
		t.Errorf("after the strays were removed the findings on the replica are %v; want none", got)
	}
}

// findingsOn returns the findings that hold on the storage, as "kind key"
// in order.
func (r *replicaRig) findingsOn(storage string) []string {
	r.t.Helper()
	got, _, err := r.p.ListScrubFindings(r.ctx, tenant.ScrubFindingFilter{Storage: storage}, pageOf(100))
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]string, len(got))
	for i, g := range got {
		out[i] = string(g.Kind) + " " + g.Key
	}
	return sorted(out)
}
