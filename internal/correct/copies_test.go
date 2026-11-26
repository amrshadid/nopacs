package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// flaky is a replica's storage a test can make stop answering: every call
// (down), or only its purges (purgeDown). Close leaves it open: the test
// closes it.
type flaky struct {
	blob.Store
	down, purgeDown atomic.Bool
	// beforePut, when set before the storage is used, is called with the
	// key of each Put before it is made.
	beforePut func(key string)
}

// errDown is what a storage that does not answer says.
func errDown() error { return fault.New(fault.Unavailable, "the test's replica does not answer") }

// Put fails while the storage is down.
func (f *flaky) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if f.down.Load() {
		return blob.Written{}, errDown()
	}
	if f.beforePut != nil {
		f.beforePut(key)
	}
	return f.Store.Put(ctx, key, r)
}

// Read fails while the storage is down.
func (f *flaky) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if f.down.Load() {
		return nil, errDown()
	}
	return f.Store.Read(ctx, key, offset, length)
}

// Stat fails while the storage is down.
func (f *flaky) Stat(ctx context.Context, key string) (blob.Info, error) {
	if f.down.Load() {
		return blob.Info{}, errDown()
	}
	return f.Store.Stat(ctx, key)
}

// Delete fails while the storage is down.
func (f *flaky) Delete(ctx context.Context, key string) error {
	if f.down.Load() {
		return errDown()
	}
	return f.Store.Delete(ctx, key)
}

// Purge fails while the storage is down or its purges are.
func (f *flaky) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	if f.down.Load() || f.purgeDown.Load() {
		return nil, errDown()
	}
	return f.Store.Purge(ctx, key)
}

// Close keeps the storage open.
func (*flaky) Close() error { return nil }

// keepOpen is a storage whose Close does nothing: the test closes it.
type keepOpen struct{ blob.Store }

// Close keeps the storage open.
func (keepOpen) Close() error { return nil }

// replicaRig is the rig with a second directory storage "copy", attached
// as a replica of "main" once attach is called, which the test may make
// stop answering.
type replicaRig struct {
	*rig
	copyRoot string
	copy     *flaky
}

// newReplicaRig makes the rig on SQLite with the storage "copy".
func newReplicaRig(t *testing.T) *replicaRig {
	t.Helper()
	r := &replicaRig{rig: newRig(t, false)}
	r.copyRoot = filepath.Join(r.dataDir, "copy")
	if err := os.MkdirAll(r.copyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(r.copyRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	r.copy = &flaky{Store: ds}
	if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "copy", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "replicas", Path: "copy"}}, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// open opens a storage of the rig by name.
func (r *replicaRig) open(_ context.Context, name string) (blob.Store, error) {
	switch name {
	case "main":
		return keepOpen{r.storage}, nil
	case "copy":
		return r.copy, nil
	}
	return nil, fault.New(fault.NotFound, "the rig has no storage "+name)
}

// engine returns an engine over both storages.
func (r *replicaRig) engine() *Engine {
	r.t.Helper()
	return r.engineWith(func(c *Config) { c.OpenStorage = r.open })
}

// attach makes "copy" a replica of "main" and copies every scan to it.
func (r *replicaRig) attach() {
	r.t.Helper()
	if _, _, _, err := r.p.AttachReplica(r.ctx, "copy", "main", time.Now(), r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	r.replicate()
}

// replicate runs the replicator until it claims nothing, failing the test
// unless every scan of "main" is in step on "copy" then.
func (r *replicaRig) replicate() {
	r.t.Helper()
	r.runReplicator()
	facts, err := r.p.SwitchFacts(r.ctx, "main", "copy")
	if err != nil {
		r.t.Fatal(err)
	}
	if facts.ScansNotInStep != 0 {
		r.t.Fatalf("%d scans of main are not in step on copy after the replicator ran", facts.ScansNotInStep)
	}
}

// runReplicator runs the replicator until it claims nothing and returns
// the scans it claimed in all.
func (r *replicaRig) runReplicator() int {
	r.t.Helper()
	w, err := replicate.New(replicate.Config{Queue: r.p, Catalog: r.p, Owner: "replicator-test",
		Now: func() time.Time { return time.Now().Add(time.Hour) }, Open: r.open})
	if err != nil {
		r.t.Fatal(err)
	}
	claimed := 0
	for range 20 {
		p, err := w.RunOnce(r.ctx)
		if err != nil {
			r.t.Fatal(err)
		}
		if p.Claimed == 0 {
			break
		}
		claimed += p.Claimed
	}
	return claimed
}

// copyObjects returns every file of the replica by key, with its SHA-256.
func (r *replicaRig) copyObjects() map[string][sha256.Size]byte {
	r.t.Helper()
	out := map[string][sha256.Size]byte{}
	err := filepath.WalkDir(r.copyRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 G122 -- a file under the test's own directory, which nothing else writes.
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.copyRoot, path)
		out[filepath.ToSlash(rel)] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// scanKeys returns the keys of objs under the scan at p, its objects and
// its manifest and metadata.
func scanKeys(t testing.TB, objs map[string][sha256.Size]byte, p label.Path) []string {
	t.Helper()
	prefix, err := layout.InstancePrefix(p)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(p)
	sk, _ := layout.MetadataKey(p)
	keys := under(objs, prefix)
	for _, k := range []string{mk, sk} {
		if _, ok := objs[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// recordOn reads a correction's record from a storage, or answers false
// when it has none.
func (r *replicaRig) recordOn(st blob.Store, id string) (record.Correction, bool) {
	r.t.Helper()
	key, err := (record.Correction{ID: id}).Key()
	if err != nil {
		r.t.Fatal(err)
	}
	rc, err := st.Read(r.ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return record.Correction{}, false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	c, err := record.DecodeCorrection(rc, rigTenantID, r.keys)
	if err != nil {
		r.t.Fatal(err)
	}
	return c, true
}

// disposition returns what the correction says the storage named did.
func disposition(c tenant.Correction, storage string) tenant.StorageDisposition {
	for _, d := range c.Dispositions {
		if d.Storage == storage {
			return d
		}
	}
	return tenant.StorageDisposition{}
}

// deleteScanOf archives the study, copies it to the replica, and deletes
// its scan of the most instances; it answers the delete and the scan.
func (r *replicaRig) deleteScanOf(e *Engine) (tenant.Correction, tenant.CatalogScan) {
	r.t.Helper()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(3)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		r.t.Fatal(err)
	}
	return c, s
}

// TestPurgeRemovesFromEveryCopy guards the purge of a delete on a primary
// with a replica: its record is on both before anything is removed; every
// object of the scan leaves both, and the correction is purged with each
// storage's disposition counting every object removed, the replica naming
// its primary. Then an instance's delete: its object leaves both, and the
// replica's manifest and metadata no longer name it.
func TestPurgeRemovesFromEveryCopy(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	if _, ok := r.recordOn(r.copy, c.ID); !ok {
		t.Fatal("the replica holds no record of the delete after its publish")
	}
	if got := r.state(c.ID); len(got.RecordsOwed) != 0 {
		t.Fatalf("a replica that answered is still owed the record: %v", got.RecordsOwed)
	}
	owned := len(scanKeys(t, r.copyObjects(), path))
	if owned != int(s.Instances)+2 {
		t.Fatalf("the replica holds %d objects of the scan, want its %d instances, manifest and metadata", owned, s.Instances)
	}
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != record.StatePurged {
		t.Fatalf("the purge ended %s (%s), want purged", p.State, p.StateReason)
	}
	if left := scanKeys(t, r.objects(), path); len(left) != 0 {
		t.Errorf("the primary keeps %v after the purge", left)
	}
	if left := scanKeys(t, r.copyObjects(), path); len(left) != 0 {
		t.Errorf("the replica keeps %v after the purge said purged", left)
	}
	m, cp := disposition(p, "main"), disposition(p, "copy")
	if m.Removed != int64(owned) || cp.Removed != int64(owned) || cp.ReplicaOf != "main" || m.ReplicaOf != "" || cp.Unknown+m.Unknown != 0 {
		t.Errorf("the dispositions are %+v, want %d removed from each copy, the replica naming main", p.Dispositions, owned)
	}
	if rec, ok := r.recordOn(r.copy, c.ID); !ok || rec.State != record.StatePurged || rec.Purge == nil || rec.Purge.Removed != int64(owned) {
		t.Errorf("the replica's record after the purge is %+v (%v), want purged with its own %d removed", rec, ok, owned)
	}

	// An instance's delete: the replica's manifest and metadata name it as
	// the primary's did, and are written again without it.
	other := r.scanWith(2)
	victim := r.held(pathOf(other))[0]
	ik, _ := layout.InstanceKey(pathOf(other), victim.SOPInstanceUID)
	ic, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: other.SubjectLabel, Session: other.SessionLabel, Scan: other.Label,
		Instance: victim.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	if ip, err := e.PurgeNow(r.ctx, ic.ID, r.audit(), nil); err != nil || ip.State != record.StatePurged {
		t.Fatalf("the instance's purge = %s, %v", ip.State, err)
	}
	if _, ok := r.copyObjects()[ik]; ok {
		t.Errorf("the replica keeps the deleted instance's object %s", ik)
	}
	mk, _ := layout.ManifestKey(pathOf(other))
	sk, _ := layout.MetadataKey(pathOf(other))
	for _, k := range []string{mk, sk} {
		b := readKey(t, r.copy, k)
		if bytes.Contains(b, []byte(victim.SOPInstanceUID)) {
			t.Errorf("the replica's %s still names the deleted instance", k)
		}
	}
	if raw := readKey(t, r.copy, mk); sha256.Sum256(raw) != sha256.Sum256(readKey(t, r.storage, mk)) {
		t.Error("the replica's manifest after the instance's purge is not the primary's")
	}
	if _, err := manifest.Decode(bytes.NewReader(readKey(t, r.copy, mk))); err != nil {
		t.Errorf("the replica's manifest after the purge does not read: %v", err)
	}
}

// readKey reads an object whole.
func readKey(t testing.TB, st blob.Store, key string) []byte {
	t.Helper()
	rc, err := st.Read(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPurgeWaitsForAReplicaThatDoesNotAnswer guards a purge whose replica
// does not answer its removals: the correction stays purging, saying a
// storage does not answer, the objects stay on the replica, and nothing is
// claimed purged; once the replica answers, a later run ends it, each
// copy's objects counted once.
func TestPurgeWaitsForAReplicaThatDoesNotAnswer(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	owned := len(scanKeys(t, r.copyObjects(), path))
	r.copy.purgeDown.Store(true)
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err == nil {
		t.Fatal("a purge whose replica does not answer was answered without an error")
	}
	if p = r.state(c.ID); p.State != record.StatePurging || p.StateReason != tenant.StateReasonStorage {
		t.Fatalf("the purge is %s (%q), want purging, waiting for a storage", p.State, p.StateReason)
	}
	if left := scanKeys(t, r.copyObjects(), path); len(left) != owned {
		t.Fatalf("the replica that does not answer holds %d of the scan's %d objects", len(left), owned)
	}
	if n, _ := e.PurgeDue(r.ctx); n != 0 || r.state(c.ID).State != record.StatePurging {
		t.Fatalf("a later run while the replica does not answer purged %d, the correction %s", n, r.state(c.ID).State)
	}
	r.copy.purgeDown.Store(false)
	if n, err := e.PurgeDue(r.ctx); err != nil || n != 1 {
		t.Fatalf("the run once the replica answers purged %d, %v", n, err)
	}
	p = r.state(c.ID)
	if p.State != record.StatePurged || p.StateReason != "" {
		t.Fatalf("the purge ended %s (%q)", p.State, p.StateReason)
	}
	if left := append(scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge", left)
	}
	if m, cp := disposition(p, "main"), disposition(p, "copy"); m.Removed != int64(owned) || cp.Removed != int64(owned) {
		t.Errorf("the dispositions are %+v, want %d removed on each copy, none counted twice", p.Dispositions, owned)
	}
}

// TestPurgeHeldStopsOnEveryCopy guards a hold seen while a purge removes an
// object: the purge stops before the next copy, so the replica keeps the
// object the primary gave up, and nothing is claimed purged; released, the
// purge ends on both.
func TestPurgeHeldStopsOnEveryCopy(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	before := scanKeys(t, r.copyObjects(), path)
	var asked atomic.Int64
	var released atomic.Bool
	e.SetHeld(func(context.Context, tenant.Correction) bool {
		// Held from the second question on: after the primary removed the
		// first object, before the replica does.
		return !released.Load() && asked.Add(1) > 1
	})
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err == nil {
		t.Fatal("a held purge was answered without an error")
	}
	p := r.state(c.ID)
	if p.State != record.StatePurging || p.StateReason != tenant.StateReasonHeld {
		t.Fatalf("the held purge is %s (%q), want purging and held", p.State, p.StateReason)
	}
	onMain, onCopy := scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)
	if len(onMain) != len(before)-1 || !slices.Equal(onCopy, before) {
		t.Fatalf("after the hold the primary keeps %d of %d objects and the replica %d; want one gone from the primary only",
			len(onMain), len(before), len(onCopy))
	}
	released.Store(true)
	if p, err := e.ContinuePurge(r.ctx, c.ID); err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge once released = %s, %v", p.State, err)
	}
	if left := append(scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge", left)
	}
}

// TestRecordOwedBlocksPurgeAndSwitch guards a correction whose replica did
// not take its record: the delete is published all the same and owes the
// record; its purge removes nothing from any copy while the record is
// owed, and a switch to the replica is not swapped; once the replica
// answers, the record is written there, the switch's count of records owed
// is none, and the purge ends on both.
func TestRecordOwedBlocksPurgeAndSwitch(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(3)
	path := pathOf(s)
	before := scanKeys(t, r.objects(), path)
	r.copy.down.Store(true)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("a delete whose replica does not answer = %s, %v; want it published", c.State, err)
	}
	if got := r.state(c.ID); !slices.Equal(got.RecordsOwed, []string{"copy"}) {
		t.Fatalf("the delete owes its record to %v, want [copy]", got.RecordsOwed)
	}
	r.copy.down.Store(false)
	r.replicate()
	if _, _, err := r.p.BeginSwitch(r.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "main", Replica: "copy", Token: "tok-1",
		Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	_, err = r.p.SwapStorages(r.ctx, tenant.SwitchSwap{ID: "sw-1", Now: time.Now()}, r.audit())
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), tenant.RefusalRecordsOwed) {
		t.Fatalf("a swap while a record is owed to the replica = %v, want refused: %s", err, tenant.RefusalRecordsOwed)
	}
	if _, err := r.p.RefuseSwitch(r.ctx, "sw-1", tenant.RefusalRecordsOwed, time.Now(), r.audit()); err != nil {
		t.Fatal(err)
	}
	r.copy.down.Store(true)
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err == nil {
		t.Fatal("a purge whose record is owed to a replica was answered without an error")
	}
	if p := r.state(c.ID); p.State != record.StatePurging || p.StateReason != tenant.StateReasonStorage {
		t.Fatalf("the purge is %s (%q), want purging, waiting for a storage", p.State, p.StateReason)
	}
	if left := scanKeys(t, r.objects(), path); !slices.Equal(left, before) {
		t.Fatalf("the primary lost %d objects while the record was owed to the replica", len(before)-len(left))
	}
	r.copy.down.Store(false)
	if n, err := e.WriteOwedRecords(r.ctx); err != nil || n != 1 {
		t.Fatalf("the records owed written = %d, %v; want 1", n, err)
	}
	if rec, ok := r.recordOn(r.copy, c.ID); !ok || rec.State != record.StatePurging {
		t.Fatalf("the replica's record once written is %+v (%v), want the purge's", rec, ok)
	}
	facts, err := r.p.SwitchFacts(r.ctx, "main", "copy")
	if err != nil || facts.RecordsOwed != 0 {
		t.Fatalf("the switch's records owed after they were written = %d, %v", facts.RecordsOwed, err)
	}
	if p, err := e.ContinuePurge(r.ctx, c.ID); err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge once the record is written = %s, %v", p.State, err)
	}
	if left := append(scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge", left)
	}
}

// TestRelabelCopiesToReplicaInStep guards a relabel on a primary with a
// replica in step: the new keys are on the replica before the publish, its
// row stays in step at the new manifest after it (a read may turn to it at
// once), and the purge removes the old keys from both.
func TestRelabelCopiesToReplicaInStep(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(3)
	from := pathOf(s)
	to := label.Path{Subject: from.Subject, Session: from.Session, Scan: "renamed"}
	c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: from.Subject, Session: from.Session, Scan: from.Scan},
		ToLabel: "renamed", Reason: "wrong series name", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(crashAt("copied"))
	if _, err := e.ContinueRelabel(r.ctx, c.ID); err == nil {
		t.Fatal("the relabel was not stopped before its publish")
	}
	if got := r.state(c.ID); got.State != record.StatePrepared {
		t.Fatalf("the relabel stopped before its publish is %s", got.State)
	}
	newOnCopy := scanKeys(t, r.copyObjects(), to)
	if len(newOnCopy) != int(s.Instances)+2 {
		t.Fatalf("before the publish the replica holds %d objects under the new labels, want %d", len(newOnCopy), s.Instances+2)
	}
	e.SetCrashAt(func(string) error { return nil })
	p, err := e.ContinueRelabel(r.ctx, c.ID)
	if err != nil || p.State != record.StateCommitted {
		t.Fatalf("the relabel's publish = %s, %v", p.State, err)
	}
	row, err := r.p.ReplicaScan(r.ctx, "copy", s.SeriesUID)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(to)
	if row.State != tenant.ReplicaInStep || row.ManifestSHA256 != tenant.SHA256(sha256.Sum256(readKey(t, r.storage, mk))) {
		t.Errorf("the replica's row after the publish is %s, want in step at the new manifest", row.State)
	}
	if in, err := r.p.InStepReplicas(r.ctx, []string{s.SeriesUID}); err != nil || !slices.Equal(in[s.SeriesUID], []string{"copy"}) {
		t.Errorf("a read of the relabeled scan turns to %v, %v; want copy", in, err)
	}
	if pp, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || pp.State != record.StatePurged {
		t.Fatalf("the relabel's purge = %s, %v", pp.State, err)
	}
	if left := append(scanKeys(t, r.objects(), from), scanKeys(t, r.copyObjects(), from)...); len(left) != 0 {
		t.Errorf("the old keys %v are left after the relabel's purge", left)
	}
	if n := len(scanKeys(t, r.copyObjects(), to)); n != int(s.Instances)+2 {
		t.Errorf("after the purge the replica holds %d objects under the new labels, want %d", n, s.Instances+2)
	}
}

// TestReplaceReachesEveryCopy guards a replace on a primary with a
// replica: its publish queues the scan for the replica, which then copies
// the new revision; its purge removes the old revision from both and keeps
// the new one on both.
func TestReplaceReachesEveryCopy(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	r.archive(studyFiles[:7]...)
	r.attach()
	s := r.scanWith(2)
	path := pathOf(s)
	in := r.held(path)[0]
	old := readKey(t, r.storage, in.Key)
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	c, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the replace = %s, %v", c.State, err)
	}
	if row, err := r.p.ReplicaScan(r.ctx, "copy", s.SeriesUID); err != nil || row.State != tenant.ReplicaPending {
		t.Fatalf("the replica's row after the replace's publish is %s, %v; want pending", row.State, err)
	}
	r.replicate()
	key, _ := layout.RevisionKey(path, in.SOPInstanceUID, 2)
	if r.copyObjects()[key] != sha256.Sum256(corrected) {
		t.Fatal("the replica does not hold the new revision once in step")
	}
	if p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the replace's purge = %s, %v", p.State, err)
	}
	for name, objs := range map[string]map[string][sha256.Size]byte{"primary": r.objects(), "replica": r.copyObjects()} {
		if _, ok := objs[in.Key]; ok || objs[key] != sha256.Sum256(corrected) {
			t.Errorf("after the purge the %s keeps the old revision or lost the new one", name)
		}
	}
}

// TestDetachAbandonsAPurgesCopy guards a detach while a delete counts the
// replica: refused without abandon_purges; with it, the delete names the
// replica among its copies abandoned and its objects there unknown, and
// its purge then ends on the primary alone, leaving the detached storage
// as it is.
func TestDetachAbandonsAPurgesCopy(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	owned := len(scanKeys(t, r.copyObjects(), path))
	_, _, err := r.p.DetachReplica(r.ctx, tenant.ReplicaDetach{Replica: "copy", Now: time.Now()}, r.audit(), nil)
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a detach while a delete counts the replica = %v, want refused", err)
	}
	if _, _, err := r.p.DetachReplica(r.ctx, tenant.ReplicaDetach{Replica: "copy", Abandon: true, Reason: "the disk is gone",
		Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	got := r.state(c.ID)
	if cp := disposition(got, "copy"); !slices.Equal(got.CopiesAbandoned, []string{"copy"}) || cp.Unknown != int64(owned) || cp.Removed != 0 {
		t.Fatalf("after the detach the delete abandons %v, the replica's disposition %+v; want copy, %d unknown", got.CopiesAbandoned, cp, owned)
	}
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge after the detach = %s, %v", p.State, err)
	}
	if m := disposition(p, "main"); m.Removed != int64(owned) {
		t.Errorf("the primary's disposition is %+v, want %d removed", m, owned)
	}
	if left := scanKeys(t, r.copyObjects(), path); len(left) != owned {
		t.Errorf("the detached storage holds %d of the %d objects; a purge must not touch a storage that is no copy", len(left), owned)
	}
}
