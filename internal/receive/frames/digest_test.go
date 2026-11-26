package frames

import (
	"crypto/sha256"
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPassRecordsTheSidecarItRewrote guards that the pass tells the catalog
// of each metadata file it rewrites, with the scan's series, its storage
// and the SHA-256 of the file now on the storage, so the catalog keeps the
// new file's digest and queues the scan's replicas, after dropping the
// digest of the file it replaces; a scan whose file it leaves as it is and
// whose digest the catalog does not keep is told with its file's digest;
// and a pass run again, every digest kept, tells nothing.
func TestPassRecordsTheSidecarItRewrote(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	a.scan(scanB, false, file{name: "MR_small.dcm", uid: "1.2.4.1"})
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	dataA, _, _ := a.sidecarOf(scanA)
	dataB, _, _ := a.sidecarOf(scanB)
	want := []string{seriesUIDOf(0) + " main " + tenant.SHA256(sha256.Sum256(dataA)).String(),
		seriesUIDOf(1) + " main " + tenant.SHA256(sha256.Sum256(dataB)).String()}
	if !slices.Equal(a.cat.rewrites, want) {
		t.Errorf("the catalog was told %v; want %v", a.cat.rewrites, want)
	}
	if !slices.Equal(a.cat.forgets, []string{seriesUIDOf(0)}) {
		t.Errorf("the pass dropped the digests of %v; want only the rewritten scan's", a.cat.forgets)
	}
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	if len(a.cat.rewrites) > 2 {
		t.Errorf("a pass run again told the catalog %v; want nothing more", a.cat.rewrites[2:])
	}
}

// TestPassRecordsALostDigestLater guards a rewrite whose record fails after
// its put: the digest of the file it replaced is gone (the new file is
// unchecked, never checked against the old one), and the next pass, which
// leaves the complete file as it is, records the new file's digest.
func TestPassRecordsALostDigestLater(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	key, _ := layout.MetadataKey(scanA)
	a.cat.digests = map[string]tenant.SHA256{seriesUIDOf(0): {9}}
	a.cat.failRecord = func() bool { return true }
	if _, err := Run(a.ctx, a.config()); err == nil {
		t.Fatal("a pass whose record failed answered no failure")
	}
	if a.store.puts[key] != 1 {
		t.Fatalf("the file was put %d times; want once", a.store.puts[key])
	}
	if d, kept := a.cat.digests[seriesUIDOf(0)]; kept {
		t.Errorf("after the record failed the catalog keeps digest %v; want none", d)
	}
	a.cat.failRecord = nil
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	data, _, _ := a.sidecarOf(scanA)
	if got, want := a.cat.digests[seriesUIDOf(0)], tenant.SHA256(sha256.Sum256(data)); got != want {
		t.Errorf("after the next pass the catalog keeps digest %v; want the file's %v", got, want)
	}
	if a.store.puts[key] != 1 {
		t.Errorf("the next pass put the file again (%d puts); want only its digest recorded", a.store.puts[key])
	}
}

// TestPassLeavesAKeptDigestUnread guards the cost of a pass over complete
// files: one whose digest the catalog keeps, on a storage that keeps no
// SHA-256 of its own, is not read whole, and nothing is recorded.
func TestPassLeavesAKeptDigestUnread(t *testing.T) {
	a := newArchive(t)
	a.scan(scanB, false, file{name: "MR_small.dcm", uid: "1.2.4.1"})
	a.cat.digests = map[string]tenant.SHA256{seriesUIDOf(0): {7}}
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	key, _ := layout.MetadataKey(scanB)
	for _, n := range a.store.reads[key] {
		if n < 0 {
			t.Errorf("the complete file was read whole (reads %v)", a.store.reads[key])
		}
	}
	if len(a.cat.rewrites) != 0 {
		t.Errorf("the catalog was told %v; want nothing", a.cat.rewrites)
	}
}
