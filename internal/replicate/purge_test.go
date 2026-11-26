package replicate

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPurgingScanIsNotCopiedOverItsRewrite guards a replica against the
// manifest an instance delete's purge replaces: until the purge ends, the
// catalog names the old manifest, so a copy made while the purge runs
// would put that manifest, which names the deleted SOP Instance UID, on
// the replica after the purge wrote it again there without the instance.
// The test's purge rewrites both copies at the moment such a copy would
// put its manifest (or after the pass, when no copy was made); after the
// purge ends, and after the next copy, the replica's manifest is the one
// written without the instance, and the instance's object is gone.
func TestPurgingScanIsNotCopiedOverItsRewrite(t *testing.T) {
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

	// The purge as the correction engine runs it on both copies: the
	// instance's object removed, the manifest and the sidecar written again
	// without it.
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
	after := tenant.SHA256(sha256.Sum256(enc))
	var once sync.Once
	purge := func() {
		once.Do(func() {
			for _, st := range []*store{f.primary, f.replica} {
				if err := st.Delete(f.ctx, c.InstanceKey); err != nil {
					t.Error(err)
				}
				for key, b := range map[string][]byte{mk: enc, sk: fmt.Appendf(nil, "sidecar of scan-a with %d files\n", len(kept))} {
					if _, err := st.Store.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
						t.Error(err)
					}
				}
			}
		})
	}
	f.replica.set(func(st *store) {
		st.onPut = func(key string) {
			if key == mk {
				purge()
			}
		}
	})
	f.runOnce(w)
	purge()
	f.replica.set(func(st *store) { st.onPut = nil })
	if err := corr.RecordPurgedObject(f.ctx, tenant.PurgedObject{ID: id, Storage: "main", Key: c.InstanceKey, Size: c.InstanceSize,
		SHA256: sumOf(files[1]), Disposition: tenant.DispositionRemoved, Now: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := corr.FinishPurge(f.ctx, tenant.PurgeFinish{ID: id, Now: f.clock.Now(), Manifest: after,
		ManifestBefore: s.manifestSHA256}); err != nil {
		t.Fatal(err)
	}
	f.wantReplicaManifest("after the purge", mk, enc, gone)

	f.clock.Add(time.Second)
	if p := f.runOnce(w); p.InStep != 1 {
		t.Fatalf("the pass after the purge is %+v, want the scan in step", p)
	}
	f.wantReplicaManifest("after the next copy", mk, enc, gone)
	if r := f.row("scan-a"); r.ManifestSHA256 != after {
		t.Errorf("the replica's row is in step at %s; want the manifest written without the instance, %s", r.ManifestSHA256, after)
	}
	if f.replicaHas(c.InstanceKey) {
		t.Errorf("the replica still holds the deleted instance's object %s", c.InstanceKey)
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
