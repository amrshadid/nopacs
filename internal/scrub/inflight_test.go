package scrub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestScrubSeesReceptionsInFlight guards that what a reception in flight
// has put and not yet committed is no finding: a scan's next manifest,
// listing the catalog's instances and one more the reception claims, is
// not called damaged though its SHA-256 is not the catalog's; a new scan's
// objects and manifest, under labels the catalog does not hold yet, are no
// orphans; and the scan whose metadata file is not there is counted in the
// job's tally sidecar|missing.
func TestScrubSeesReceptionsInFlight(t *testing.T) {
	f := newFixture(t)
	held := synthetic("2.25.20", 2)
	p := f.archiveEntries("scan-a", "2.25.200", "2.25.201", held)
	_, oldLines := f.linesOf(p, held)

	// Into the scan the catalog holds: the object and the next manifest
	// put, the rows not committed.
	next := synthetic("2.25.21", 1)
	f.reception("scan-a", "2.25.200", "2.25.201", next)
	_, newLines := f.put(p, next)
	f.putManifest(p, append(oldLines, newLines...))

	// A new scan: its object and its manifest put, the scan not committed.
	fresh := synthetic("2.25.22", 1)
	f.reception("scan-n", "2.25.220", "2.25.221", fresh)
	q := label.Path{Subject: "sub-01", Session: "ses-scan-n", Scan: "scan-n"}
	_, freshLines := f.put(q, fresh)
	f.putManifest(q, freshLines)

	j := f.scrub(f.config())
	if got := f.findings(true); len(got) != 0 {
		t.Errorf("receptions in flight made findings %v", got)
	}
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 {
		t.Errorf("the scrub ended as %s %+v", j.State, j.Counts)
	}
	if n := f.tallies(j.ID)[tenant.TallySidecarMissing]; n != 1 {
		t.Errorf("the job tallied %d scans without a metadata file, want 1", n)
	}
}

// linesOf returns the confirmed instances and manifest lines of files
// archived in the scan p, as archiveEntries made them, putting the same
// bytes under the same keys again.
func (f *fixture) linesOf(p label.Path, files []entry) ([]tenant.ConfirmedInstance, []manifest.File) {
	f.t.Helper()
	return f.put(p, files)
}

// unreachable is a set of storages of which one does not answer.
type unreachable struct {
	storages
	name string
}

// Get answers the storage named name as not answering.
func (u unreachable) Get(ctx context.Context, name string) (blob.Store, error) {
	if name == u.name {
		return nil, fault.New(fault.Unavailable, "the storage does not answer")
	}
	return u.storages.Get(ctx, name)
}

// TestScrubPassesOverAStorageOutOfReach guards a scrub of every storage
// while one of them does not answer: its look for orphans is passed over
// and tallied unreadable|storage, the others' looks and every scan's unit
// go on, and the job ends rather than failing for that one storage.
func TestScrubPassesOverAStorageOutOfReach(t *testing.T) {
	f := newFixture(t)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "cold", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "cold"}}, time.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.archiveEntries("scan-a", "2.25.300", "2.25.301", synthetic("2.25.30", 2))
	stray := "data/stray.txt"
	if _, err := f.store.Put(f.ctx, stray, strings.NewReader("not archived")); err != nil {
		t.Fatal(err)
	}
	cfg := f.config()
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages {
		return unreachable{storages: storages{s: readOnly{Store: f.store, writes: &f.writes}}, name: "cold"}
	}
	j := f.createScrub("")
	s := f.start(cfg)
	got := f.waitEnded(s, j.ID)
	if got.State != tenant.JobDone || got.Counts.InstancesSent != 2 {
		t.Errorf("the scrub ended as %s %+v", got.State, got.Counts)
	}
	if n := f.tallies(j.ID)[tenant.TallyStorageUnreadable]; n != 1 {
		t.Errorf("the job tallied %d storages out of reach, want 1", n)
	}
	if fs := f.findings(false); len(fs) != 1 || fs[0] != "orphan "+stray {
		t.Errorf("the storage that answered was looked at with findings %v, want its orphan", fs)
	}
}
