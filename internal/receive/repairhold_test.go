package receive

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// repairHeldDB is the tenant's database with a repair holding every scan
// while held is set: ScanRepairHeld answers it, and CommitScan refuses as
// the driver does for a scan a repair holds. commits counts the commits it
// let through.
type repairHeldDB struct {
	tenant.Pipeline
	held    atomic.Bool
	commits atomic.Int64
}

// ScanRepairHeld answers held for every scan.
func (d *repairHeldDB) ScanRepairHeld(ctx context.Context, seriesUID string, now time.Time) (bool, error) {
	if d.held.Load() {
		return true, nil
	}
	return d.Pipeline.ScanRepairHeld(ctx, seriesUID, now)
}

// CommitScan refuses while held, as a driver refuses a commit to a scan a
// repair holds, and counts the commits it lets through.
func (d *repairHeldDB) CommitScan(ctx context.Context, c tenant.ScanCommit) (tenant.Scan, error) {
	if d.held.Load() {
		return tenant.Scan{}, tenant.ScanRepairingError()
	}
	d.commits.Add(1)
	return d.Pipeline.CommitScan(ctx, c)
}

// manifests returns the bytes of every scan manifest on the env's storage,
// by path.
func (e *env) manifests() map[string][]byte {
	e.t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(e.storeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != layout.ManifestName {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- a path under the test's own temporary storage, which only the test writes.
		out[p] = b
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// TestScanStepWaitsForRepairHold guards a reception into a scan a repair
// holds: the archive step looks for the hold before it puts the scan's
// sidecar and manifest, and fails, to be tried again, without writing
// them, so the manifest on the storage stays the one the catalog names
// rather than a new one whose commit the hold refuses. Once the repair
// ended, the step tried again archives the file.
func TestScanStepWaitsForRepairHold(t *testing.T) {
	var db *repairHeldDB
	e := newEnv(t, envOptions{db: func(s tenant.Pipeline) tenant.Pipeline {
		db = &repairHeldDB{Pipeline: s}
		return db
	}})
	e.send("ct5n-2062.dcm")
	if rs := e.settle(1); !done(rs[0]) {
		t.Fatalf("the first reception reads %+v", rs[0])
	}
	before := e.manifests()
	if len(before) != 1 {
		t.Fatalf("the storage holds %d manifests after one scan, want 1", len(before))
	}
	committed := db.commits.Load()
	db.held.Store(true)
	e.send("ct5n-2392.dcm")
	e.waitFor("the archive step to fail on the hold", time.Second, func() bool { return e.p.Stats().StepsFailed.Load() >= 1 })
	for p, b := range e.manifests() {
		if !bytes.Equal(b, before[p]) {
			t.Errorf("the manifest %s was replaced while a repair held its scan", p)
		}
	}
	if n := db.commits.Load(); n != committed {
		t.Errorf("%d commits went through while a repair held the scan", n-committed)
	}
	db.held.Store(false)
	rs := e.settle(2)
	for _, r := range rs {
		if !done(r) {
			t.Errorf("a reception reads %+v once the repair ended; want done", r)
		}
	}
	totals, err := e.store.CatalogTotals(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Instances != 2 {
		t.Errorf("the catalog holds %d instances, want 2", totals.Instances)
	}
	for p, b := range e.manifests() {
		if bytes.Equal(b, before[p]) {
			t.Errorf("the manifest %s was not written again once the repair ended", p)
		}
	}
}
