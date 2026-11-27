package correct

import (
	"bytes"
	"crypto/sha256"
	"io"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// arriveAt returns a seam that, at the step named, once, archives the
// corpus file named through the reception pipeline (a reception of the
// scan committing while a correction of it is prepared) and stops
// nothing.
func (r *rig) arriveAt(step, name string) func(string) error {
	var done atomic.Bool
	return func(s string) error {
		if s == step && done.CompareAndSwap(false, true) {
			r.archive(name)
		}
		return nil
	}
}

// wantDigestsOfStorage fails unless the catalog keeps, for the scan at p,
// the digests of the manifest and metadata file the storage holds, and the
// scan holds n instances, each at its first revision.
func (r *rig) wantDigestsOfStorage(t *testing.T, what string, p label.Path, n int) {
	t.Helper()
	sc, err := r.p.CatalogScan(r.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	mk, _ := layout.ManifestKey(p)
	sk, _ := layout.MetadataKey(p)
	objs := r.objects()
	if sc.ManifestSHA256 != tenant.SHA256(objs[mk]) {
		t.Fatalf("after %s the catalog keeps the manifest digest %s, the storage's manifest is %s", what, sc.ManifestSHA256,
			tenant.SHA256(objs[mk]))
	}
	if !sc.SidecarSHA256.IsZero() && sc.SidecarSHA256 != tenant.SHA256(objs[sk]) {
		t.Fatalf("after %s the catalog keeps the metadata digest %s, the storage's file is %s", what, sc.SidecarSHA256,
			tenant.SHA256(objs[sk]))
	}
	held := r.held(p)
	if len(held) != n {
		t.Fatalf("after %s the scan holds %d instances, want %d", what, len(held), n)
	}
	for _, in := range held {
		if plain, _ := layout.InstanceKey(p, in.SOPInstanceUID); in.Key != plain {
			t.Fatalf("after %s an instance is at %s, want its first revision", what, in.Key)
		}
	}
}

// TestEditRolledBackAfterAReceptionKeepsTheDigestsItWrote guards an edit
// whose scan a reception commits between the edit's write of the scan's
// manifest and metadata and its publish: the publish is refused (the
// scan's instances changed), the rollback writes the files naming the old
// revisions and the reception's instance, and the catalog keeps the
// digests of what the rollback wrote, not of the reception's manifest,
// which the rollback wrote over.
func TestEditRolledBackAfterAReceptionKeepsTheDigestsItWrote(t *testing.T) {
	onBoth(t, testEditRolledBackAfterAReception)
}

// testEditRolledBackAfterAReception is
// TestEditRolledBackAfterAReceptionKeepsTheDigestsItWrote on one database.
func testEditRolledBackAfterAReception(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:6]...)
	s := r.scanWith(4)
	e := r.editEngine()
	e.SetCrashAt(r.arriveAt("meta", studyFiles[6]))
	c, err := r.editNow(e, scanTarget(s), setDescription("Scout view"))
	if err == nil || c.State != record.StateAbandoned || c.StateReason != reasonChanged {
		t.Fatalf("the edit whose scan a reception changed = %s (%q), %v; want abandoned as changed", c.State, c.StateReason, err)
	}
	r.wantDigestsOfStorage(t, "an edit's rollback", pathOf(s), 5)
}

// TestReplaceRolledBackAfterAReceptionKeepsTheDigestsItWrote guards the
// same window of a replace: a reception of its scan commits between the
// replace's write of the manifest and metadata and its publish, the
// publish is refused, and the catalog keeps the digests of what the
// rollback wrote.
func TestReplaceRolledBackAfterAReceptionKeepsTheDigestsItWrote(t *testing.T) {
	onBoth(t, testReplaceRolledBackAfterAReception)
}

// testReplaceRolledBackAfterAReception is
// TestReplaceRolledBackAfterAReceptionKeepsTheDigestsItWrote on one
// database.
func testReplaceRolledBackAfterAReception(t *testing.T, pg bool) {
	r := newRig(t, pg)
	r.archive(studyFiles[:6]...)
	s := r.scanWith(4)
	path := pathOf(s)
	in := r.held(path)[0]
	old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	e := r.engine()
	e.SetCrashAt(r.arriveAt("meta", studyFiles[6]))
	c, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
	if err == nil || c.State != record.StateAbandoned {
		t.Fatalf("the replace whose scan a reception changed = %s (%q), %v; want abandoned", c.State, c.StateReason, err)
	}
	r.wantDigestsOfStorage(t, "a replace's rollback", path, 5)
	if got := r.objects()[in.Key]; got != sha256.Sum256(old) {
		t.Fatalf("after the rollback the instance's first revision is not its old bytes")
	}
}
