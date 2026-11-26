package correct

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A replicator's copy under way while a delete is published and purged:
// its puts cannot share a transaction with the catalog, so bytes it was
// putting may land after the purge's removals. Each test lets a put of the
// copy wait while the correction runs, then lets it land.

// movable is a clock that reads the machine's time moved by an offset a
// test sets.
type movable struct{ offset atomic.Int64 }

// now answers the machine's time moved by the offset.
func (m *movable) now() time.Time { return time.Now().Add(time.Duration(m.offset.Load())) }

// to moves the clock to read t now.
func (m *movable) to(t time.Time) { m.offset.Store(int64(time.Until(t))) }

// inFlightEngine returns an engine over both storages of the rig, by the
// clock given.
func (r *replicaRig) inFlightEngine(clock *movable) *Engine {
	r.t.Helper()
	return r.engineWith(func(c *Config) { c.OpenStorage = r.open; c.Now = clock.now })
}

// replicatorAt returns a replicator over the rig's two storages, by the
// clock given, its configuration changed by change when not nil.
func (r *replicaRig) replicatorAt(now func() time.Time, change func(*replicate.Config)) *replicate.Worker {
	r.t.Helper()
	cfg := replicate.Config{Queue: r.p, Catalog: r.p, Owner: "replicator-test", Now: now, Open: r.open}
	if change != nil {
		change(&cfg)
	}
	w, err := replicate.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// runCopy runs one pass of w, failing the test on a claim's error.
func (r *replicaRig) runCopy(w *replicate.Worker) replicate.Pass {
	r.t.Helper()
	p, err := w.RunOnce(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

// wantWaitingForCopies fails unless the correction is purging, waiting for
// copies in flight until a moment still ahead, and answers that moment.
func (r *replicaRig) wantWaitingForCopies(what string, id string, err error) time.Time {
	r.t.Helper()
	if fault.KindOf(err) != fault.Precondition {
		r.t.Fatalf("%s answered %v; want the purge waiting for the copies in flight", what, err)
	}
	c := r.state(id)
	if c.State != record.StatePurging || c.StateReason != tenant.StateReasonCopiesInFlight {
		r.t.Fatalf("%s left the correction %s (%q); want purging, %q", what, c.State, c.StateReason, tenant.StateReasonCopiesInFlight)
	}
	if !c.CopiesInFlightUntil.After(time.Now()) {
		r.t.Fatalf("%s kept copies in flight until %v; want a moment ahead", what, c.CopiesInFlightUntil)
	}
	return c.CopiesInFlightUntil
}

// purgeAcross runs the purge of id once just before until, where it must
// still wait, and once just after, where it must end purged no earlier
// than until; it answers the purged correction.
func (r *replicaRig) purgeAcross(e *Engine, clock *movable, id string, until time.Time) tenant.Correction {
	r.t.Helper()
	clock.to(until.Add(-time.Second))
	_, err := e.ContinuePurge(r.ctx, id)
	r.wantWaitingForCopies("the purge a second before the copies in flight can no longer land", id, err)
	clock.to(until.Add(time.Second))
	p, err := e.ContinuePurge(r.ctx, id)
	if err != nil || p.State != record.StatePurged {
		r.t.Fatalf("the purge once the copies in flight can no longer land = %s, %v", p.State, err)
	}
	if p.Ended.Before(until) {
		r.t.Errorf("the correction was purged at %v, before the copies in flight could no longer land, %v", p.Ended, until)
	}
	return p
}

// TestPurgeWaitsForCopiesInFlight guards the purge's wait: a delete whose
// scan a replicator has claimed is purged, and the purge removes nothing
// from any copy while a put of that claim may still land; the put that
// lands then is removed with the rest once the purge runs after that
// moment, and the correction is purged only then.
func TestPurgeWaitsForCopiesInFlight(t *testing.T) {
	r := newReplicaRig(t)
	clock := &movable{}
	e := r.inFlightEngine(clock)
	c, s, victim, key := r.instanceDelete(e, true)
	if claims, err := r.p.ClaimReplicaScans(r.ctx, "copier", time.Now(), time.Minute, 10); err != nil || len(claims) != 1 {
		t.Fatalf("the claim of the scan after the delete's publish = %+v, %v", claims, err)
	}
	body := readKey(t, r.storage, key)
	_, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	until := r.wantWaitingForCopies("the purge of a scan being copied", c.ID, err)
	if _, ok := r.objects()[key]; !ok {
		t.Error("the purge removed the instance from the primary while a copy's put could still land")
	}
	// The claimed copy's put lands now, on a replica the purge has not
	// touched.
	if _, err := r.copy.Put(r.ctx, key, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	r.purgeAcross(e, clock, c.ID, until)
	if _, ok := r.objects()[key]; ok {
		t.Errorf("the primary keeps %s after the purge", key)
	}
	r.wantGoneFromCopy(s, victim.SOPInstanceUID, key)
}

// TestPurgeStartStopsACopysPuts guards the lease a purge's start ends: a
// copy of the scan claimed before it, whose first put waits while the
// purge starts, puts nothing more once its next renewal is refused (its
// other objects, its sidecar and its manifest stay unput), and does not
// end in step.
func TestPurgeStartStopsACopysPuts(t *testing.T) {
	r := newReplicaRig(t)
	clock := &movable{}
	e := r.inFlightEngine(clock)
	c, s, _, key := r.instanceDelete(e, true)
	for _, in := range r.held(pathOf(s)) {
		r.removeFromCopy(in.Key)
	}
	r.removeFromCopy(key)
	mk, _ := layout.ManifestKey(pathOf(s))
	r.removeFromCopy(mk)
	copyClock := &movable{}
	var started atomic.Bool
	var after atomic.Int64
	r.copy.beforePut = func(string) {
		if started.Load() {
			after.Add(1)
			return
		}
		started.Store(true)
		if _, err := e.StartPurge(r.ctx, c.ID, r.audit(), nil); err != nil {
			t.Errorf("the purge's start = %v", err)
		}
		// Past a third of the lease by the copier's clock: its next step
		// renews the lease.
		copyClock.offset.Add(int64(replicate.DefaultLease / 2))
	}
	p := r.runCopy(r.replicatorAt(copyClock.now, func(cfg *replicate.Config) { cfg.Parallel = 1 }))
	r.copy.beforePut = nil
	if !started.Load() {
		t.Fatal("the copy put nothing")
	}
	if n := after.Load(); n != 0 {
		t.Errorf("the copy made %d puts after the purge's start ended its lease; want none", n)
	}
	if p.InStep != 0 || p.Stopped != 1 {
		t.Errorf("the pass is %+v; want the copy stopped, not in step", p)
	}
	if _, ok := r.copyObjects()[mk]; ok {
		t.Error("the copy put the scan's manifest after its lease was ended")
	}
}

// TestPurgeDuringAPutOfAnInstance guards an instance's delete purged while
// a copy claimed before streams the instance to the replica: the purge
// waits rather than remove it from both copies at once, the put lands, and
// once no put of that copy can land the purge removes the instance from
// every copy and ends purged, the replica's manifest and metadata no
// longer naming it.
func TestPurgeDuringAPutOfAnInstance(t *testing.T) {
	r := newReplicaRig(t)
	clock := &movable{}
	e := r.inFlightEngine(clock)
	c, s, victim, key := r.instanceDelete(e, true)
	r.removeFromCopy(key)
	var once sync.Once
	var purgeErr error
	r.copy.beforePut = func(k string) {
		if k == key {
			once.Do(func() { _, purgeErr = e.PurgeNow(r.ctx, c.ID, r.audit(), nil) })
		}
	}
	r.runCopy(r.replicatorAt(time.Now, nil))
	r.copy.beforePut = nil
	until := r.wantWaitingForCopies("the purge run while the copy put the instance", c.ID, purgeErr)
	if _, ok := r.copyObjects()[key]; !ok {
		t.Fatal("the put the purge waited on did not land")
	}
	if row, err := r.p.ReplicaScan(r.ctx, "copy", s.SeriesUID); err != nil || row.State == tenant.ReplicaInStep {
		t.Errorf("the copy whose lease the purge ended is %s, %v; want it not in step", row.State, err)
	}
	r.purgeAcross(e, clock, c.ID, until)
	for name, objs := range map[string]map[string][32]byte{"main": r.objects(), "copy": r.copyObjects()} {
		if _, ok := objs[key]; ok {
			t.Errorf("%s keeps the purged instance %s", name, key)
		}
	}
	r.replicate()
	r.wantGoneFromCopy(s, victim.SOPInstanceUID, key)
}

// deleteDuringBackfill archives the study, attaches the replica with its
// backfill queued, and makes the first put of the scan of three instances
// wait while during runs; it answers that scan and the replicator's pass,
// run with every object of the scan put in parallel.
func (r *replicaRig) deleteDuringBackfill(during func(s tenant.CatalogScan)) (tenant.CatalogScan, replicate.Pass) {
	r.t.Helper()
	r.archive(studyFiles[:7]...)
	s := r.scanWith(3)
	if _, _, _, err := r.p.AttachReplica(r.ctx, "copy", "main", time.Now(), r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	prefix, err := layout.InstancePrefix(pathOf(s))
	if err != nil {
		r.t.Fatal(err)
	}
	var once sync.Once
	var waiting sync.WaitGroup
	waiting.Add(1)
	r.copy.beforePut = func(k string) {
		if !strings.HasPrefix(k, prefix) {
			return
		}
		once.Do(func() {
			defer waiting.Done()
			during(s)
		})
		waiting.Wait()
	}
	p := r.runCopy(r.replicatorAt(time.Now, func(cfg *replicate.Config) { cfg.Parallel = 4 }))
	r.copy.beforePut = nil
	return s, p
}

// TestCopyRemovesWhatItPutOfADeletedScan guards the copy's own guard: a
// backfill's puts of a scan, each under way when the scan is deleted,
// land, and the copy, whose lease the delete's publish ended, removes
// every one of them, its sidecar and its manifest too, since the catalog
// no longer names the scan; the other scan of the study stays.
func TestCopyRemovesWhatItPutOfADeletedScan(t *testing.T) {
	r := newReplicaRig(t)
	e := r.inFlightEngine(&movable{})
	s, _ := r.deleteDuringBackfill(func(s tenant.CatalogScan) {
		if _, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}); err != nil {
			r.t.Error(err)
		}
	})
	if left := scanKeys(t, r.copyObjects(), pathOf(s)); len(left) != 0 {
		t.Errorf("the replica keeps %v, put by a copy of a scan deleted while it streamed", left)
	}
	for _, other := range r.scans() {
		if left := scanKeys(t, r.copyObjects(), pathOf(other)); len(left) == 0 {
			t.Errorf("the replica holds nothing of the scan %s, which was not deleted", other.Label)
		}
	}
}

// TestPurgeDuringABackfillOfAScan guards a scan deleted and purged while a
// backfill's parallel puts of its objects stream to the replica: the purge
// waits until none of them can land, then removes the scan from every
// copy and ends purged, and no object of the scan is left on any copy.
func TestPurgeDuringABackfillOfAScan(t *testing.T) {
	r := newReplicaRig(t)
	clock := &movable{}
	e := r.inFlightEngine(clock)
	var c tenant.Correction
	var purgeErr error
	s, _ := r.deleteDuringBackfill(func(s tenant.CatalogScan) {
		var err error
		c, err = r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
		if err != nil {
			r.t.Error(err)
			return
		}
		_, purgeErr = e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	})
	if c.ID == "" {
		t.Fatal("the scan was not deleted")
	}
	until := r.wantWaitingForCopies("the purge run while the backfill put the scan", c.ID, purgeErr)
	r.purgeAcross(e, clock, c.ID, until)
	if left := append(scanKeys(t, r.objects(), pathOf(s)), scanKeys(t, r.copyObjects(), pathOf(s))...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge of the scan", left)
	}
	if _, err := e.ContinuePurge(context.Background(), c.ID); err != nil {
		t.Errorf("the purged correction asked again = %v", err)
	}
}
