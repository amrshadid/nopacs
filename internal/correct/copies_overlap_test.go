package correct

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What keeps a purge's copies exact while other work runs beside it: a
// switch, an attach, a replicator's copy, a record owed to a storage that
// is no longer a copy.

// instanceDelete archives the study (attaching the replica first when
// attach is set) and deletes one instance of a scan of two; it answers the
// delete, the instance's scan and its key.
func (r *replicaRig) instanceDelete(e *Engine, attach bool) (tenant.Correction, tenant.CatalogScan, tenant.ScanInstance, string) {
	r.t.Helper()
	r.archive(studyFiles[:7]...)
	if attach {
		r.attach()
	}
	s := r.scanWith(2)
	victim := r.held(pathOf(s))[0]
	key, err := layout.InstanceKey(pathOf(s), victim.SOPInstanceUID)
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label,
		Instance: victim.SOPInstanceUID})
	if err != nil {
		r.t.Fatal(err)
	}
	return c, s, victim, key
}

// wantGoneFromCopy fails unless the replica holds no object at key and its
// manifest and metadata of the scan do not name the instance.
func (r *replicaRig) wantGoneFromCopy(s tenant.CatalogScan, uid, key string) {
	r.t.Helper()
	if _, ok := r.copyObjects()[key]; ok {
		r.t.Errorf("the replica keeps the deleted instance's object %s", key)
	}
	mk, _ := layout.ManifestKey(pathOf(s))
	sk, _ := layout.MetadataKey(pathOf(s))
	for _, k := range []string{mk, sk} {
		if bytes.Contains(readKey(r.t, r.copy, k), []byte(uid)) {
			r.t.Errorf("the replica's %s still names the deleted instance", k)
		}
	}
}

// TestSwitchWaitsForUnfinishedPurge guards a purge stopped by a replica
// that blinked, its first object removed from the primary and not from
// the replica: a switch of the two is refused until every object is
// settled, since a swap would point the names the purge recorded at the
// other places, and the purge run again would ask only a place already
// empty, leave the other holding the object, and say purged.
func TestSwitchWaitsForUnfinishedPurge(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	owned := len(scanKeys(t, r.copyObjects(), path))
	r.copy.purgeDown.Store(true)
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err == nil {
		t.Fatal("a purge whose replica does not answer was answered without an error")
	}
	if left := scanKeys(t, r.objects(), path); len(left) != owned-1 {
		t.Fatalf("the primary keeps %d of %d objects, want one removed before the replica stopped answering", len(left), owned)
	}
	r.copy.purgeDown.Store(false)
	_, _, err := r.p.BeginSwitch(r.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "main", Replica: "copy", Token: "tok-1",
		Now: time.Now()}, r.audit(), nil)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), tenant.BlockerPurgeUnfinished) {
		t.Fatalf("a switch while a purge of its storages is unfinished = %v, want refused: %s", err, tenant.BlockerPurgeUnfinished)
	}
	p, err := e.ContinuePurge(r.ctx, c.ID)
	if err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge once the replica answers = %s, %v", p.State, err)
	}
	if left := append(scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge said purged", left)
	}
	if m, cp := disposition(p, "main"), disposition(p, "copy"); m.Removed != int64(owned) || cp.Removed != int64(owned) {
		t.Errorf("the dispositions are %+v, want %d removed on each copy", p.Dispositions, owned)
	}
	if _, _, err := r.p.BeginSwitch(r.ctx, tenant.SwitchStart{ID: "sw-2", Primary: "main", Replica: "copy", Token: "tok-2",
		Now: time.Now()}, r.audit(), nil); err != nil {
		t.Errorf("a switch once the purge ended = %v", err)
	}
}

// TestPurgeWaitsWhileSwitchRuns guards a purge started while a switch of
// its storage runs: it removes nothing from any copy and stays purging;
// once the switch is refused, it ends on both.
func TestPurgeWaitsWhileSwitchRuns(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s := r.deleteScanOf(e)
	path := pathOf(s)
	before := len(scanKeys(t, r.objects(), path))
	if _, _, err := r.p.BeginSwitch(r.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "main", Replica: "copy", Token: "tok-1",
		Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a purge while a switch of its storage runs = %v, want a precondition failure", err)
	}
	if p := r.state(c.ID); p.State != record.StatePurging {
		t.Fatalf("the purge is %s, want purging", p.State)
	}
	if m, cp := len(scanKeys(t, r.objects(), path)), len(scanKeys(t, r.copyObjects(), path)); m != before || cp != before {
		t.Fatalf("while the switch ran the copies kept %d and %d of %d objects", m, cp, before)
	}
	if _, err := r.p.RefuseSwitch(r.ctx, "sw-1", tenant.RefusalRestarted, time.Now(), r.audit()); err != nil {
		t.Fatal(err)
	}
	if p, err := e.ContinuePurge(r.ctx, c.ID); err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge once the switch ended = %s, %v", p.State, err)
	}
	if left := append(scanKeys(t, r.objects(), path), scanKeys(t, r.copyObjects(), path)...); len(left) != 0 {
		t.Errorf("the copies keep %v after the purge", left)
	}
}

// TestAttachRefusedWhilePurging guards a replica attached while an
// instance's purge runs: the attach is refused, since the purge took its
// copies when it began and the backfill would copy the hidden instance;
// attached once the purge ended, the replica never holds it.
func TestAttachRefusedWhilePurging(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s, victim, key := r.instanceDelete(e, false)
	var attachErr error
	var once sync.Once
	// Asked before the purge removes its first object, after it took its
	// copies.
	e.SetHeld(func(context.Context, tenant.Correction) bool {
		once.Do(func() {
			_, _, _, attachErr = r.p.AttachReplica(r.ctx, "copy", "main", time.Now(), r.audit(), nil)
			if attachErr == nil {
				r.replicate()
			}
		})
		return false
	})
	if p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the instance's purge = %s, %v", p.State, err)
	}
	if fault.KindOf(attachErr) != fault.Precondition || !strings.Contains(fault.Message(attachErr), tenant.RefusalAttachPurging) {
		t.Errorf("an attach while the purge ran = %v, want refused: %s", attachErr, tenant.RefusalAttachPurging)
	}
	if fault.KindOf(attachErr) == fault.Precondition {
		r.attach()
	}
	r.wantGoneFromCopy(s, victim.SOPInstanceUID, key)
}

// replicator returns a replicator over the rig's two storages.
func (r *replicaRig) replicator() *replicate.Worker {
	r.t.Helper()
	w, err := replicate.New(replicate.Config{Queue: r.p, Catalog: r.p, Owner: "replicator-test",
		Now: func() time.Time { return time.Now().Add(time.Hour) }, Open: r.open})
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// removeFromCopy removes the replica's object at key, as a copy would have
// lost it, so the next copy of its scan puts it.
func (r *replicaRig) removeFromCopy(key string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.copyRoot, filepath.FromSlash(key))); err != nil {
		r.t.Fatal(err)
	}
}

// TestCopyInFlightAcrossPurgeStart guards a replicator's copy claimed
// before an instance's purge began, which puts the hidden instance on the
// replica after the purge removed it there: the copy does not end in step,
// since the purge's start counted a change on its row, and the purge
// checks every removed key on every copy before it says purged, so the
// object found again is removed.
func TestCopyInFlightAcrossPurgeStart(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s, victim, key := r.instanceDelete(e, true)
	r.removeFromCopy(key)
	var once sync.Once
	var purgeErr error
	r.copy.beforePut = func(k string) {
		if k != key {
			return
		}
		// The purge runs, while the copy's put waits, up to the rewrite of
		// the scan's manifest: the object is gone from both copies, and the
		// manifest is still the one the copy was claimed at.
		once.Do(func() {
			e.SetCrashAt(crashAt("meta-read"))
			_, purgeErr = e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
		})
	}
	if _, err := r.replicator().RunOnce(r.ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(purgeErr, errCrash) {
		t.Fatalf("the purge run while the copy put the object = %v, want it stopped before the manifest's rewrite", purgeErr)
	}
	if _, ok := r.copyObjects()[key]; !ok {
		t.Fatal("the copy claimed before the purge did not put the hidden instance")
	}
	if row, err := r.p.ReplicaScan(r.ctx, "copy", s.SeriesUID); err != nil || row.State == tenant.ReplicaInStep {
		t.Errorf("a copy claimed before the purge began ended %s, %v; want it not in step", row.State, err)
	}
	e.SetCrashAt(func(string) error { return nil })
	p, err := e.ContinuePurge(r.ctx, c.ID)
	if err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge run again = %s, %v", p.State, err)
	}
	if _, ok := r.copyObjects()[key]; ok {
		t.Errorf("the replica keeps %s after the purge said purged", key)
	}
	r.replicate()
	r.wantGoneFromCopy(s, victim.SOPInstanceUID, key)
}

// TestReplicatorSkipsWhatAPurgeHides guards a copy claimed while an
// instance's purge is under way: the hidden instance is not put on the
// replica, whose purge copies were taken when it began.
func TestReplicatorSkipsWhatAPurgeHides(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, s, victim, key := r.instanceDelete(e, true)
	r.removeFromCopy(key)
	if _, err := e.StartPurge(r.ctx, c.ID, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.replicate()
	if _, ok := r.copyObjects()[key]; ok {
		t.Errorf("a copy claimed while the purge was under way put the hidden instance %s", key)
	}
	if p, err := e.ContinuePurge(r.ctx, c.ID); err != nil || p.State != record.StatePurged {
		t.Fatalf("the purge = %s, %v", p.State, err)
	}
	r.replicate()
	r.wantGoneFromCopy(s, victim.SOPInstanceUID, key)
}

// TestAttachAfterPublishGetsRecord guards a replica attached after a
// delete was published: it is owed the delete's record, which is written
// there, so a rebuild from it alone knows the delete.
func TestAttachAfterPublishGetsRecord(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	r.archive(studyFiles[:7]...)
	s := r.scanWith(3)
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		t.Fatal(err)
	}
	r.attach()
	if got := r.state(c.ID); len(got.RecordsOwed) != 1 || got.RecordsOwed[0] != "copy" {
		t.Fatalf("a replica attached after the delete's publish is owed %v, want [copy]", got.RecordsOwed)
	}
	if n, err := e.WriteOwedRecords(r.ctx); err != nil || n != 1 {
		t.Fatalf("the records owed written = %d, %v; want 1", n, err)
	}
	if rec, ok := r.recordOn(r.copy, c.ID); !ok || rec.State != record.StateCommitted {
		t.Errorf("the replica's record of the delete is %+v (%v), want committed", rec, ok)
	}
}

// TestOwedRecordToNoCopyDropped guards a record owed to a storage that is
// not a copy of the correction's storages: writing the records owed drops
// it, writing nothing there, rather than keep it owed for ever.
func TestOwedRecordToNoCopyDropped(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	c, _ := r.deleteScanOf(e)
	if err := r.p.OweRecord(r.ctx, c.ID, "elsewhere", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := e.WriteOwedRecords(r.ctx); err != nil || n != 0 {
		t.Fatalf("the records owed written = %d, %v; want none", n, err)
	}
	if got := r.state(c.ID); len(got.RecordsOwed) != 0 {
		t.Errorf("after the records owed were written the delete is owed by %v, want none", got.RecordsOwed)
	}
}
