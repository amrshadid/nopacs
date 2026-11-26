package replicate

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAPurgingScanCopiesArrivalsWithoutItsManifest guards the copy of a
// scan whose instance delete is purging, which a hold can keep purging for
// years: a new arrival in the scan reaches the replica all the same, but no
// copy puts the scan's manifest or sidecar while the purge is unfinished,
// since the catalog's manifest is the one the purge is replacing and, put
// after the purge rewrote it, would name the deleted SOP Instance UID again
// on the replica. The row is not in step meanwhile and is not claimed again
// until the scan changes; once the purge ends, the next copy brings it in
// step at the scan's new manifest, which names the arrival and not the
// deleted instance, and the deleted instance's object is not on the
// replica.
func TestAPurgingScanCopiesArrivalsWithoutItsManifest(t *testing.T) {
	f := newFixture(t)
	files := synthetic(1, 1, 3, 900)
	s := f.archive(1, "scan-a", files)
	f.attach()
	w := f.worker(f.config())
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the backfill's pass is %+v, want the scan in step", p)
	}
	corr, ok := f.p.(tenant.Corrections)
	if !ok {
		t.Fatal("the fixture's store does not implement tenant.Corrections")
	}
	gone := files[1].uid
	target := tenant.CorrectionTarget{Subject: s.path.Subject, Session: s.path.Session, Scan: s.path.Scan, Instance: gone}
	pv, err := corr.PreviewDelete(f.ctx, target, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := tenant.NewCorrectionID(f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := corr.PrepareCorrection(f.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: f.clock.Now()}, f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corr.CommitCorrection(f.ctx, tenant.CorrectionCommit{ID: id, Now: f.clock.Now(), Grace: time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Second)
	a := f.audit()
	if _, err := corr.StartPurge(f.ctx, id, f.clock.Now(), &a, nil); err != nil {
		t.Fatal(err)
	}

	// The purge as the correction engine runs it on both copies, its end
	// not yet recorded: the instance's object removed, the manifest and
	// the sidecar written again without it.
	mk, sk := mustKey(layout.ManifestKey(s.path)), mustKey(layout.MetadataKey(s.path))
	var kept []manifest.File
	for _, l := range s.lines {
		if l.SOPInstanceUID != gone {
			kept = append(kept, l)
		}
	}
	enc, err := manifest.Encode(manifest.Manifest{Files: kept})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []*store{f.primary, f.replica} {
		if err := st.Delete(f.ctx, c.InstanceKey); err != nil {
			t.Fatal(err)
		}
		for key, b := range map[string][]byte{mk: enc, sk: fmt.Appendf(nil, "sidecar of scan-a with %d files\n", len(kept))} {
			if _, err := st.Store.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Any manifest or sidecar the replica is asked to put from here until
	// the purge's end is a failure.
	var mu sync.Mutex
	var putWhilePurging []string
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if key == mk || key == sk {
				mu.Lock()
				putWhilePurging = append(putWhilePurging, key)
				mu.Unlock()
			}
		}
	})

	// A new arrival in the scan, its manifest written from the one the
	// purge left.
	s.lines = kept
	arrival := synthetic(1, 10, 1, 700)
	s = f.archive(1, "scan-a", arrival)
	newKey := mustKey(layout.InstanceKey(s.path, arrival[0].uid))
	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.Again != 1 || p.InStep != 0 {
		t.Fatalf("the pass after the arrival is %+v; want the scan copied and left not in step", p)
	}
	if !f.replicaHas(newKey) {
		t.Errorf("the arrival's object %s did not reach the replica while the purge is unfinished", newKey)
	}
	if r := f.row("scan-a"); r.State == tenant.ReplicaInStep {
		t.Errorf("the replica's row is in step while the purge is unfinished: %+v", r)
	}
	f.wantReplicaManifest("while the purge is unfinished", mk, enc, gone)
	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.Again+p.InStep+p.Failed != 0 {
		t.Errorf("a later pass while the purge is unfinished is %+v; want nothing claimed", p)
	}
	mu.Lock()
	if len(putWhilePurging) > 0 {
		t.Errorf("the replica was asked to put %q while the purge was unfinished", putWhilePurging)
	}
	mu.Unlock()
	f.replica.set(func(st *store) { st.onPut = nil })

	if err := corr.RecordPurgedObject(f.ctx, tenant.PurgedObject{ID: id, Storage: "main", Key: c.InstanceKey, Size: c.InstanceSize,
		SHA256: sumOf(files[1]), Disposition: tenant.DispositionRemoved, Now: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := corr.FinishPurge(f.ctx, tenant.PurgeFinish{ID: id, Now: f.clock.Now(), Manifest: tenant.SHA256(sha256.Sum256(enc)),
		ManifestBefore: tenant.SHA256(sha256.Sum256(enc))}); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass after the purge is %+v, want the scan in step", p)
	}
	if r := f.row("scan-a"); r.ManifestSHA256 != s.manifestSHA256 {
		t.Errorf("the replica's row is in step at %s; want the scan's manifest with the arrival, %s", r.ManifestSHA256, s.manifestSHA256)
	}
	got := f.read(f.replica, mk)
	if strings.Contains(string(got), gone) || !strings.Contains(string(got), arrival[0].uid) {
		t.Errorf("after the purge the replica's manifest names the deleted instance, or not the arrival")
	}
	if f.replicaHas(c.InstanceKey) {
		t.Errorf("the replica still holds the deleted instance's object %s", c.InstanceKey)
	}
	if !slices.ContainsFunc(s.lines, func(l manifest.File) bool { return l.SOPInstanceUID == arrival[0].uid }) {
		t.Fatal("the fixture's scan does not list the arrival")
	}
}

// wantReplicaManifest fails unless the replica's manifest at key is want
// and names no instance uid.
func (f *fixture) wantReplicaManifest(when, key string, want []byte, uid string) {
	f.t.Helper()
	got := f.read(f.replica, key)
	if strings.Contains(string(got), uid) {
		f.t.Errorf("%s, the replica's manifest names the deleted instance %s", when, uid)
	}
	if !bytes.Equal(got, want) {
		f.t.Errorf("%s, the replica's manifest is not the one written without the instance", when)
	}
}
