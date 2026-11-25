package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestBackfillCopiesEverything guards a backfill: every scan of the primary
// is copied, each ends in step at its manifest, the two storages hold the
// same keys with the same bytes, and the scan's manifest is the last object
// of the scan put, after its sidecar.
func TestBackfillCopiesEverything(t *testing.T) {
	f := newFixture(t)
	a := f.archive(1, "scan-a", synthetic(1, 1, 5, 3000))
	b := f.archive(2, "scan-b", synthetic(2, 1, 3, 70000))
	f.attach()
	w := f.worker(f.config())
	p := f.runOnce(w)
	if p.Claimed != 2 || p.InStep != 2 || p.Failed != 0 {
		t.Fatalf("the pass is %+v, want two scans claimed and in step", p)
	}
	for _, s := range []*scanInfo{a, b} {
		r := f.row(s.path.Scan)
		if r.State != tenant.ReplicaInStep || r.ManifestSHA256 != s.manifestSHA256 || r.Backfill {
			t.Errorf("the row of %s is %+v, want in step at its manifest", s.path.Scan, r)
		}
	}
	f.wantSameTrees()
	puts := f.replica.putKeys()
	for _, s := range []*scanInfo{a, b} {
		mk, sk := mustKey(layout.ManifestKey(s.path)), mustKey(layout.MetadataKey(s.path))
		last, side := -1, slices.Index(puts, sk)
		for i, k := range puts {
			if strings.HasPrefix(k, mustKey(layout.InstancePrefix(s.path))) {
				last = i
			}
		}
		if mi := slices.Index(puts, mk); mi < 0 || side < 0 || last >= side || side >= mi {
			t.Errorf("the puts of %s are not its files, then its sidecar, then its manifest: %v", s.path.Scan, puts)
		}
	}
	if p := f.runOnce(w); p.Claimed != 0 {
		t.Errorf("a pass after everything is in step claimed %d scans", p.Claimed)
	}
}

// TestArrivalAfterInStepCopiesOnlyWhatIsNew guards a scan copied again
// after an arrival: it ends in step at the new manifest, and only the new
// file, the sidecar and the manifest are put, the files the replica holds
// at their size being taken as copied.
func TestArrivalAfterInStepCopiesOnlyWhatIsNew(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 4, 1000))
	f.attach()
	w := f.worker(f.config())
	f.runOnce(w)
	before := len(f.replica.putKeys())
	s := f.archive(1, "scan-a", synthetic(1, 10, 1, 1000))
	if r := f.row("scan-a"); r.State != tenant.ReplicaPending {
		t.Fatalf("a scan committed into after it was in step is %s, want pending", r.State)
	}
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	if r := f.row("scan-a"); r.State != tenant.ReplicaInStep || r.ManifestSHA256 != s.manifestSHA256 {
		t.Errorf("the row is %+v, want in step at the new manifest", r)
	}
	if n := len(f.replica.putKeys()) - before; n != 3 {
		t.Errorf("the copy after one arrival put %d objects, want 3 (the file, the sidecar, the manifest)", n)
	}
	f.wantSameTrees()
}

// TestRepublishDuringCopyNeverInStepAtOldManifest guards a scan committed
// into while its copy runs: the copy that began at the old manifest finds
// the primary's manifest changed when it reads it again after the sidecar,
// fails as a change and is never recorded in step, and the next pass
// copies the new manifest and ends in step at it.
func TestRepublishDuringCopyNeverInStepAtOldManifest(t *testing.T) {
	f := newFixture(t)
	old := f.archive(1, "scan-a", synthetic(1, 1, 3, 2000)).manifestSHA256
	f.attach()
	var once atomic.Bool
	f.replica.set(func(s *store) {
		s.onPut = func(string) {
			if once.CompareAndSwap(false, true) {
				f.archive(1, "scan-a", synthetic(1, 20, 2, 2000))
			}
		}
	})
	w := f.worker(f.config())
	p := f.runOnce(w)
	if p.InStep != 0 || p.Failed != 1 {
		t.Fatalf("the pass is %+v, want the copy failed as a change", p)
	}
	r := f.row("scan-a")
	if r.State == tenant.ReplicaInStep {
		t.Fatalf("a copy begun at the old manifest was recorded in step: %+v", r)
	}
	if r.ManifestSHA256 == old {
		t.Errorf("the row holds the old manifest as verified: %+v", r)
	}
	f.replica.set(func(s *store) { s.onPut = nil })
	f.clock.Add(DefaultChangedDelay)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the next pass is %+v, want the scan in step", p)
	}
	if r := f.row("scan-a"); r.ManifestSHA256 != f.scans["scan-a"].manifestSHA256 {
		t.Errorf("the row is %+v, want in step at the new manifest", r)
	}
	f.wantSameTrees()
}

// TestManifestInFlightIsNotCopied guards a reception in flight, its next
// manifest and file on the primary and its rows not committed: nothing it
// wrote reaches the replica, the scan is not in step, and once it commits
// the scan is copied at the new manifest.
func TestManifestInFlightIsNotCopied(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 500))
	f.attach()
	next := synthetic(1, 30, 1, 500)
	commit := f.stage(1, "scan-a", next)
	w := f.worker(f.config())
	if p := f.runOnce(w); p.Failed != 1 {
		t.Fatalf("the pass is %+v, want the copy failed as a change", p)
	}
	r := f.row("scan-a")
	if r.State == tenant.ReplicaInStep || !strings.Contains(r.LastError, "not the one claimed") {
		t.Errorf("the row is %+v, want pending with the change as its reason", r)
	}
	if !r.Due.Equal(f.clock.Now().Add(DefaultChangedDelay)) {
		t.Errorf("a change is due again at %v, want after the short delay", r.Due)
	}
	path := f.scans["scan-a"].path
	if f.replicaHas(mustKey(layout.InstanceKey(path, next[0].uid))) || f.replicaHas(mustKey(layout.ManifestKey(path))) {
		t.Error("what a reception in flight wrote reached the replica")
	}
	commit()
	f.clock.Add(DefaultChangedDelay)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass after the commit is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// TestHiddenInstanceIsCopiedAndInStep guards a manifest naming an instance
// the catalog does not hold (as after a delete hid it, until its purge):
// the replica is a copy of the storage, so the scan ends in step in one
// pass, holding the same objects as the primary, the hidden one with them,
// and no pass after claims it again.
func TestHiddenInstanceIsCopiedAndInStep(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 2, 800))
	hidden := synthetic(1, 40, 1, 800)[0]
	key := mustKey(layout.InstanceKey(s.path, hidden.uid))
	w0, err := f.primary.Store.Put(f.ctx, key, pattern(hidden.seed, hidden.size))
	if err != nil {
		t.Fatal(err)
	}
	// The scan's next manifest also names the hidden instance, and is
	// committed as the scan's digest with one more instance of its own.
	f.editManifest = func(lines []manifest.File) []manifest.File {
		lines = append(lines, manifest.File{Key: key, Length: hidden.size, Size: hidden.size,
			SHA256: tenant.SHA256(w0.SHA256).String(), SOPInstanceUID: hidden.uid})
		slices.SortFunc(lines, func(a, b manifest.File) int { return strings.Compare(a.Key, b.Key) })
		return lines
	}
	f.archive(1, "scan-a", synthetic(1, 41, 1, 800))
	f.attach()
	w := f.worker(f.config())
	if p := f.runOnce(w); p.InStep != 1 || p.Failed != 0 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	if r := f.row("scan-a"); r.State != tenant.ReplicaInStep || r.ManifestSHA256 != f.scans["scan-a"].manifestSHA256 {
		t.Errorf("the row is %+v, want in step at the manifest naming the hidden instance", r)
	}
	if !f.replicaHas(key) {
		t.Error("the object of a hidden instance, still on the primary, is not on the replica")
	}
	f.wantSameTrees()
	f.clock.Add(time.Hour)
	if p := f.runOnce(w); p.Claimed != 0 {
		t.Errorf("a pass after the scan was in step claimed it again: %+v", p)
	}
}

// TestManifestLineTheCatalogContradictsFails guards the check of the
// manifest against the catalog: a line of an instance the catalog holds
// with another SHA-256 than the catalog's row fails the copy, the scan is
// not in step, and the object is not put.
func TestManifestLineTheCatalogContradictsFails(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 2, 800))
	f.attach()
	victim := s.confirmed[0]
	cfg := f.config()
	cfg.Catalog = catalog{Catalog: f.p, edit: func(in *tenant.ScanInstance) {
		if in.SOPInstanceUID == victim.SOPInstanceUID {
			in.ArchivedSHA256[0] ^= 0x01
		}
	}}
	if p := f.runOnce(f.worker(cfg)); p.Failed != 1 || p.InStep != 0 {
		t.Fatalf("the pass is %+v, want the copy failed", p)
	}
	if r := f.row("scan-a"); r.State == tenant.ReplicaInStep || !strings.Contains(r.LastError, "catalog's row") {
		t.Errorf("the row is %+v, want not in step with the contradiction as its reason", r)
	}
	if f.replicaHas(victim.Key) {
		t.Error("an object whose manifest line the catalog contradicts was put")
	}
}

// TestDamagedInstancesArePutAgain guards the copy of a scan with damage
// recorded on the replica: the damaged object, there at its size but with
// other bytes, is put again from the primary, and the scan ends in step.
func TestDamagedInstancesArePutAgain(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 3, 1500))
	f.attach()
	w := f.worker(f.config())
	f.runOnce(w)
	victim := s.confirmed[1]
	bad := make([]byte, victim.Size)
	if _, err := f.replica.Store.Put(f.ctx, victim.Key, bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.RecordReplicaDamage(f.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: s.series,
		Damage: tenant.ReplicaDamage{SOPInstanceUID: victim.SOPInstanceUID, Revision: 1, FoundSize: &victim.Size,
			FoundSHA256: tenant.SHA256(sha256.Sum256(bad)), Found: f.clock.Now()}}); err != nil {
		t.Fatal(err)
	}
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
	if r := f.row("scan-a"); r.Damaged != 0 {
		t.Errorf("the row still counts damage: %+v", r)
	}
}

// TestPlanRecopiesEverythingWhenAsked guards a claim that says to copy
// every object (more damage than a claim names): every object is put
// again, and a claim naming damage puts exactly the damaged instances'.
func TestPlanRecopiesEverythingWhenAsked(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 3, 100))
	held := map[string]tenant.ScanInstance{}
	for _, c := range s.confirmed {
		held[c.SOPInstanceUID] = tenant.ScanInstance{SOPInstanceUID: c.SOPInstanceUID, Key: c.Key, Length: c.Length, Size: c.Size,
			ArchivedSHA256: c.ArchivedSHA256}
	}
	m := manifest.Manifest{Files: s.lines}
	all, err := plan(m, held, tenant.ReplicaClaim{RecopyAll: true})
	if err != nil {
		t.Fatal(err)
	}
	some, err := plan(m, held, tenant.ReplicaClaim{Damaged: []tenant.ReplicaDamage{{SOPInstanceUID: s.confirmed[2].SOPInstanceUID}}})
	if err != nil {
		t.Fatal(err)
	}
	var forcedAll, forcedSome []string
	for i := range all {
		if all[i].force {
			forcedAll = append(forcedAll, all[i].key)
		}
		if some[i].force {
			forcedSome = append(forcedSome, some[i].key)
		}
	}
	if len(forcedAll) != 3 || !slices.Equal(forcedSome, []string{s.confirmed[2].Key}) {
		t.Errorf("forced with everything asked: %v; with one damaged: %v", forcedAll, forcedSome)
	}
}

// TestFlippedSourceByteIsRefused guards the put against the catalog's
// checksum on a replica that confirms checksums itself (so nothing is read
// back): a byte flipped on the way from the primary fails the put, the
// replica keeps nothing, the scan fails and is due later, never in step,
// and once the primary reads true the copy ends in step.
func TestFlippedSourceByteIsRefused(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 3, 4000))
	f.attach()
	victim := s.confirmed[0].Key
	f.primary.set(func(st *store) { st.flipRead = map[string]bool{victim: true} })
	f.replica.set(func(st *store) { st.confirm = true })
	w := f.worker(f.config())
	if p := f.runOnce(w); p.Failed != 1 || p.InStep != 0 {
		t.Fatalf("the pass is %+v, want the copy failed", p)
	}
	r := f.row("scan-a")
	if r.State == tenant.ReplicaInStep || !r.Due.After(f.clock.Now()) || r.LastError == "" {
		t.Errorf("the row after a refused put is %+v, want pending, due later, with a reason", r)
	}
	if f.replicaHas(victim) {
		t.Error("the replica kept an object whose bytes were not the catalog's")
	}
	f.primary.set(func(st *store) { st.flipRead = nil })
	f.clock.Add(DefaultRetryMin)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the retry is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// TestReadBackCatchesAReplicaThatKeepsOtherBytes guards the read back: a
// replica that keeps other bytes than it is given, while reporting those
// given, is caught when the copy is read back; the object is recorded as
// damage, the scan fails and is never in step, and a retry against a true
// replica puts the object again and ends in step.
func TestReadBackCatchesAReplicaThatKeepsOtherBytes(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 3000))
	f.attach()
	f.replica.set(func(st *store) { st.lie = true })
	w := f.worker(f.config())
	if p := f.runOnce(w); p.Failed != 1 || p.InStep != 0 {
		t.Fatalf("the pass is %+v, want the copy failed", p)
	}
	r := f.row("scan-a")
	if r.State != tenant.ReplicaDamaged || r.Damaged == 0 {
		t.Errorf("the row after a wrong read back is %+v, want damaged", r)
	}
	f.replica.set(func(st *store) { st.lie = false })
	f.clock.Add(DefaultRetryMin)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the retry is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// TestLostLeaseWritesNothing guards a lease taken over while a scan copies:
// the copy notices at its next renewal, puts nothing more, and writes
// nothing to the row, which stays the new holder's.
func TestLostLeaseWritesNothing(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 6, 2000))
	f.attach()
	cfg := f.config()
	cfg.Parallel, cfg.Lease = 1, time.Minute
	var stolen atomic.Bool
	var putBefore atomic.Int64
	var thief []tenant.ReplicaClaim
	f.primary.set(func(st *store) {
		st.onRead = func(key string) {
			if key == s.confirmed[2].Key && stolen.CompareAndSwap(false, true) {
				putBefore.Store(int64(len(f.replica.putKeys())))
				f.clock.Add(2 * time.Minute)
				c, err := f.p.ClaimReplicaScans(f.ctx, "another", f.clock.Now(), time.Hour, 1)
				if err != nil || len(c) != 1 {
					t.Errorf("the takeover claimed %v, %v", c, err)
				}
				thief = c
			}
		}
	})
	w := f.worker(cfg)
	if p := f.runOnce(w); p.Stopped != 1 {
		t.Fatalf("the pass is %+v, want the scan stopped", p)
	}
	if !stolen.Load() {
		t.Fatal("the lease was never taken over")
	}
	if n := int64(len(f.replica.putKeys())) - putBefore.Load(); n != 0 {
		t.Errorf("%d objects were put after the lease was taken over", n)
	}
	r := f.row("scan-a")
	if r.State != tenant.ReplicaCopying || r.Attempt != 2 || r.LastError != "" {
		t.Errorf("the row after a lost lease is %+v, want copying under the takeover's attempt, untouched", r)
	}
	if len(thief) == 1 {
		if err := f.p.RenewReplicaScan(f.ctx, thief[0].Fence, f.clock.Now(), time.Minute); err != nil {
			t.Errorf("the takeover's lease does not hold: %v", err)
		}
	}
}

// TestStopMidCopyThenRestartFinishes guards a stop while a scan copies:
// nothing is recorded, the scan is not in step, and once its lease has run
// out a new worker copies it to the end.
func TestStopMidCopyThenRestartFinishes(t *testing.T) {
	f := newFixture(t)
	s := f.archive(1, "scan-a", synthetic(1, 1, 6, 2000))
	f.attach()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if key == s.confirmed[2].Key {
				cancel()
			}
		}
	})
	cfg := f.config()
	cfg.Parallel = 1
	w := f.worker(cfg)
	p, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stopped != 1 {
		t.Fatalf("the pass is %+v, want the scan stopped", p)
	}
	r := f.row("scan-a")
	if r.State == tenant.ReplicaInStep || r.LastError != "" {
		t.Errorf("the row after a stop is %+v, want not in step and no failure recorded", r)
	}
	if f.replicaHas(mustKey(layout.ManifestKey(s.path))) {
		t.Error("the manifest reached the replica before the scan's files")
	}
	f.replica.set(func(st *store) { st.onPut = nil })
	f.clock.Add(cfg.Lease + DefaultLease)
	w2 := f.worker(f.config())
	if p := f.runOnce(w2); p.InStep != 1 {
		t.Fatalf("the restart's pass is %+v, want the scan in step", p)
	}
	f.wantSameTrees()
}

// TestRunStopsWhenItsContextEnds guards Run: it copies what is pending,
// then returns nil once its context ends.
func TestRunStopsWhenItsContextEnds(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 2, 100))
	f.attach()
	cfg := f.config()
	cfg.Idle = 10 * time.Millisecond
	w := f.worker(cfg)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for f.row("scan-a").State != tenant.ReplicaInStep {
		if time.Now().After(deadline) {
			t.Fatal("Run did not copy the pending scan")
		}
		w.Wake()
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v after its context ended", err)
	}
}

// TestMemoryStaysBoundedOn64MiBInstance guards the streaming: copying a
// scan whose one instance is 64 MiB keeps the live heap well under the
// instance's size.
func TestMemoryStaysBoundedOn64MiBInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 128 MiB of files")
	}
	f := newFixture(t)
	f.archive(1, "scan-a", synthetic(1, 1, 1, 64<<20))
	f.attach()
	w := f.worker(f.config())
	stop := testkit.HeapPeak()
	p := f.runOnce(w)
	peak := stop()
	if p.InStep != 1 || p.Bytes < 64<<20 {
		t.Fatalf("the pass is %+v, want the scan in step", p)
	}
	if peak > 24<<20 {
		t.Errorf("the live heap reached %d MiB copying a 64 MiB instance", peak>>20)
	}
	t.Logf("live heap peak %d MiB", peak>>20)
}

// TestNewRefusesAnIncompleteConfig guards New: a worker without its queue,
// catalog, opener, clock or owner, or with a negative bound, is refused.
func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	f := newFixture(t)
	good := f.config()
	for name, change := range map[string]func(c *Config){
		"no queue": func(c *Config) { c.Queue = nil }, "no clock": func(c *Config) { c.Now = nil },
		"no owner": func(c *Config) { c.Owner = "" }, "negative lease": func(c *Config) { c.Lease = -time.Second },
		"batch too large": func(c *Config) { c.Batch = tenant.MaxReplicaClaim + 1 },
	} {
		c := good
		change(&c)
		if _, err := New(c); err == nil {
			t.Errorf("New with %s was accepted", name)
		}
	}
	if _, err := New(good); err != nil {
		t.Errorf("New with a whole config: %v", err)
	}
}

// TestRetryBacksOffAndIsBounded guards the wait after failures in a row on
// a replica: it doubles from RetryMin, stops at RetryMax, and starts again
// after a copy ends in step.
func TestRetryBacksOffAndIsBounded(t *testing.T) {
	w := &Worker{cfg: Config{RetryMin: time.Second, RetryMax: 10 * time.Second}, failures: map[string]int{}}
	now := time.Unix(1_700_000_000, 0)
	var got []time.Duration
	for range 6 {
		got = append(got, w.retryAfter("copy", now).Sub(now))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("the waits are %v, want %v", got, want)
	}
	w.succeeded("copy")
	if d := w.retryAfter("copy", now).Sub(now); d != time.Second {
		t.Errorf("the wait after a success is %v, want RetryMin", d)
	}
}

// TestFaultOfReadsKindsNotWords guards the mapping of a storage's error to
// the contract's fault.
func TestFaultOfReadsKindsNotWords(t *testing.T) {
	if got := faultOf(context.DeadlineExceeded); got != tenant.FaultUnreachable {
		t.Errorf("a time out is %s", got)
	}
	if got := faultOf(errors.New("anything")); got != tenant.FaultUnreachable {
		t.Errorf("an unmarked error is %s", got)
	}
}
