package dirimport

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// importBound bounds how long one import of the test tree may take.
const importBound = 90 * time.Second

// wantSkipped is what the test tree's traps are skipped as.
var wantSkipped = map[tenant.ImportSkipCode]int64{
	tenant.SkipOutsideLayout: 2, tenant.SkipDirectoryTooWide: 1, tenant.SkipTooLarge: 1, tenant.SkipNotRegular: 1,
	tenant.SkipNotPart10: 1, tenant.SkipUnmappedSession: 1, tenant.SkipPathTooLong: 1,
}

// TestAnImportOfATreeReconcilesOnSQLite guards the whole walk on SQLite:
// every trap of the tree is skipped with its code, every DICOM file is
// handed in and archived under the labels its path and the map give, byte
// for byte, the instance in two folders is archived once and held once,
// the counts reconcile, and nothing under the root changed.
func TestAnImportOfATreeReconcilesOnSQLite(t *testing.T) {
	_, _ = runImport(t, "")
}

// TestAnImportOfATreeReconcilesOnPostgres guards the same on Postgres.
func TestAnImportOfATreeReconcilesOnPostgres(t *testing.T) {
	_, _ = runImport(t, postgresAddress(t))
}

// runImport imports the test tree into a tenant on the database given,
// checks what became of it, and returns the env and the tree's files.
func runImport(t *testing.T, postgres string) (*env, []file) {
	e := newEnv(t, postgres)
	files := e.buildTree()
	before := treeSums(t, e.tree)
	j := e.createJob()
	s := e.start(e.config())
	ended := e.waitEnded(s, j.ID, importBound)
	e.checkImport(files, ended)
	if after := treeSums(t, e.tree); !maps.Equal(before, after) {
		t.Errorf("the tree under the root changed: %d files before, %d after", len(before), len(after))
	}
	if twice := e.sink.twice(); len(twice) > 0 {
		t.Errorf("files handed in more than once: %v", twice)
	}
	return e, files
}

// TestAKilledImportResumesWithoutHandingAFileTwiceOnSQLite guards resuming
// on SQLite: the supervisor is stopped hard in the middle of the walk, its
// units' leases left held as a crashed server leaves them, and a new one
// takes the units back and finishes from their cursors; no file is handed
// in twice and the job ends as an import never stopped would.
func TestAKilledImportResumesWithoutHandingAFileTwiceOnSQLite(t *testing.T) {
	runKilledImport(t, "")
}

// TestAKilledImportResumesWithoutHandingAFileTwiceOnPostgres guards the
// same on Postgres.
func TestAKilledImportResumesWithoutHandingAFileTwiceOnPostgres(t *testing.T) {
	runKilledImport(t, postgresAddress(t))
}

// runKilledImport kills the supervisor after a few entries and starts a
// new one.
func runKilledImport(t *testing.T, postgres string) {
	e := newEnv(t, postgres)
	files := e.buildTree()
	j := e.createJob()
	cfg := e.config()
	cfg.Interval = time.Hour // the first supervisor reads its tenant once
	reached := make(chan struct{})
	var judged atomic.Int64
	cfg.seams.judged = func(string) {
		if judged.Add(1) == 4 {
			close(reached)
		}
	}
	cfg.seams.renew = time.Hour // a lease the killed process held stays held
	e.settings.maxUnits.Store(1)
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(importBound):
		t.Fatal("the first walk did not reach its fourth entry")
	}
	killed, cancel := context.WithCancel(context.Background())
	cancel()
	_ = first.Stop(killed)
	mid, err := e.store.ImportJob(e.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.State.Ended() || mid.Import.Counts.Found == 0 {
		t.Fatalf("the kill did not land mid-walk: %s, found %d", mid.State, mid.Import.Counts.Found)
	}
	e.settings.maxUnits.Store(4)
	second := e.start(e.config())
	ended := e.waitEnded(second, j.ID, importBound)
	e.checkImport(files, ended)
	if twice := e.sink.twice(); len(twice) > 0 {
		t.Errorf("files handed in more than once across the kill: %v", twice)
	}
}

// TestAUnitWhoseRootIsGoneFails guards the root check: a root that no
// longer resolves to a directory fails the listing unit with the reason
// "root unavailable", and the job fails, with nothing found.
func TestAUnitWhoseRootIsGoneFails(t *testing.T) {
	e := newEnv(t, "")
	e.buildTree()
	j := e.createJob()
	cfg := e.config()
	cfg.Roots = treeRoot{path: filepath.Join(e.dataDir, "not-there")}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	if ended.State != tenant.JobFailed || ended.Reason != tenant.ImportReasonRootUnavailable {
		t.Fatalf("job %s (%q), want failed with %q", ended.State, ended.Reason, tenant.ImportReasonRootUnavailable)
	}
	if ended.Import.Counts.Found != 0 {
		t.Errorf("found %d, want 0", ended.Import.Counts.Found)
	}
	units, _, err := e.store.ListImportUnits(e.ctx, j.ID, tenant.ImportUnitFilter{}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 || units[0].State != tenant.ImportUnitFailed || units[0].Reason != tenant.ImportReasonRootUnavailable {
		t.Fatalf("units %+v, want the listing unit failed with the root's reason", units)
	}
}

// checkImport checks the ended job's counts, its listed skips, and what
// the storage holds.
func (e *env) checkImport(files []file, j tenant.Job) {
	t := e.t
	t.Helper()
	c := j.Import.Counts
	if msg := c.Reconcile(); msg != "" {
		t.Errorf("the counts do not reconcile: %s: %+v", msg, c)
	}
	if j.State != tenant.JobPartial {
		t.Errorf("job %s (%q), want partial: a directory too wide and a path too long may hide DICOM", j.State, j.Reason)
	}
	if c.Found != 18 || c.HandedIn != 10 || c.HandedAgain != 0 || c.Archived != 9 || c.AlreadyHeld != 1 || c.Processing != 0 ||
		c.QuarantinedTotal() != 0 || c.Units != 6 || c.UnitsWalked != 6 {
		t.Errorf("counts %+v, want 18 found, 10 handed in, 9 archived, 1 already held, 6 units walked", c)
	}
	if !maps.Equal(c.Skipped, wantSkipped) {
		t.Errorf("skipped %v, want %v", c.Skipped, wantSkipped)
	}
	listed, _, err := e.store.ListImportFiles(e.ctx, j.ID, tenant.ImportFileFilter{Kinds: []tenant.ImportFileKind{tenant.ImportFileSkipped}},
		master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]tenant.ImportSkipCode{}
	for _, f := range listed {
		got[f.Path] = f.SkipCode
	}
	long := "proj1/sessL" + strings.Repeat("/"+longSegment, 11)
	want := map[string]tenant.ImportSkipCode{
		"proj1/readme.txt": tenant.SkipOutsideLayout, "proj1/wide": tenant.SkipDirectoryTooWide,
		"proj1/sessA/big/jpeg_3frame_nobot.dcm": tenant.SkipTooLarge, "proj1/sessA/ct5/link.dcm": tenant.SkipNotRegular,
		"proj1/sessA/ct5/notes.txt": tenant.SkipNotPart10, "proj1/sessB/mr/extra/deep.dcm": tenant.SkipOutsideLayout,
		"proj1/sessGone/x/liver_1frame.dcm": tenant.SkipUnmappedSession, importpath.Fitting(long): tenant.SkipPathTooLong,
	}
	if !maps.Equal(got, want) {
		t.Errorf("listed skips %v, want %v", got, want)
	}
	// Every file archived from the tree is in the storage under the labels
	// its path and the map gave, as received, byte for byte; nothing else.
	expected := map[string]bool{}
	for _, f := range files {
		if f.labels[0] != "" {
			sum := sha256.Sum256(f.body)
			expected[f.labels[0]+"/"+f.labels[1]+"/"+f.labels[2]+"/"+string(sum[:])] = true
		}
	}
	stored := map[string]bool{}
	data := filepath.Join(e.storeDir, filepath.FromSlash(layout.DataRoot))
	err = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, layout.InstanceExt) {
			return err
		}
		rel, err := filepath.Rel(data, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the test's own storage, which nothing else changes.
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		stored[filepath.ToSlash(filepath.Dir(rel))+"/"+string(sum[:])] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(stored, expected) {
		t.Errorf("the storage holds %d files where %d were expected, or under other labels: %v", len(stored), len(expected),
			slices.Sorted(maps.Keys(labelsOf(stored))))
	}
}

// labelsOf returns the label directories of a set of stored files, for a
// message.
func labelsOf(stored map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range stored {
		out[k[:strings.LastIndexByte(k, '/')]] = true
	}
	return out
}

// treeSums returns the checksum of every regular file under root, and the
// target of every link, by path, read through os.Root.
func treeSums(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	out := map[string][32]byte{}
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.Type()&fs.ModeSymlink != 0:
			target, err := r.Readlink(p)
			out[p] = sha256.Sum256([]byte("link:" + target))
			return err
		case d.Type().IsRegular():
			b, err := r.ReadFile(p)
			out[p] = sha256.Sum256(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEachUnitJudgesItsEntriesInTheWalksOrder guards the order of a walk,
// on which resuming after a cursor rests: with one unit at a time, the
// entries each unit records come in importpath.Compare's strictly rising
// order, a directory's whole subtree before the next name beside it.
func TestEachUnitJudgesItsEntriesInTheWalksOrder(t *testing.T) {
	e := newEnv(t, "")
	e.buildTree()
	j := e.createJob()
	e.settings.maxUnits.Store(1)
	cfg := e.config()
	var mu sync.Mutex
	var seen []string
	cfg.seams.judged = func(p string) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	}
	s := e.start(cfg)
	e.waitEnded(s, j.ID, importBound)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 18 {
		t.Fatalf("%d entries recorded, want 18: %v", len(seen), seen)
	}
	unitOf := func(p string) string {
		if strings.Count(p, "/") < 2 {
			return "proj1"
		}
		return p[:strings.Index(p[len("proj1/"):], "/")+len("proj1/")]
	}
	for i := 1; i < len(seen); i++ {
		if unitOf(seen[i]) == unitOf(seen[i-1]) && importpath.Compare(seen[i-1], seen[i]) >= 0 {
			t.Errorf("entry %d comes before entry %d in the walk's order, and was recorded after it", i, i-1)
		}
	}
}
