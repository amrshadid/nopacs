package dirimport

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAnEntryWithABadNameIsSkippedUnderItsFolder guards a name the rule of
// a segment refuses (here a folder named with ":"): it is skipped whole as
// bad_name, counted once as found, listed under the folder that holds it
// (the name kept nowhere), and makes the job partial, since what is under
// it may be DICOM the archive lacks. Passed over silently, its subtree
// would leave the job done with its files uncounted. On SQLite.
func TestAnEntryWithABadNameIsSkippedUnderItsFolder(t *testing.T) {
	runBadName(t, "")
}

// TestAnEntryWithABadNameIsSkippedUnderItsFolderOnPostgres guards the same
// on Postgres, whose cursor then holds a name no path rule takes.
func TestAnEntryWithABadNameIsSkippedUnderItsFolderOnPostgres(t *testing.T) {
	runBadName(t, postgresAddress(t))
}

// runBadName imports a tree with a folder named with ":" into a tenant on
// the database given and checks what became of it.
func runBadName(t *testing.T, postgres string) {
	if runtime.GOOS == "windows" {
		t.Skip("\":\" cannot be in a name on Windows")
	}
	e := newEnv(t, postgres)
	e.writeFiles(map[string]string{
		"proj1/sessA/ct2/ct2n-6293.dcm":  "ct2n-6293.dcm",
		"proj1/sessA/ct:9/ct2n-6924.dcm": "ct2n-6924.dcm",
	})
	j := e.createJob()
	s := e.start(e.config())
	ended := e.waitEnded(s, j.ID, importBound)
	c := ended.Import.Counts
	if msg := c.Reconcile(); msg != "" {
		t.Errorf("the counts do not reconcile: %s: %+v", msg, c)
	}
	if ended.State != tenant.JobPartial {
		t.Errorf("job %s (%q), want partial: a name the archive cannot keep may hide DICOM", ended.State, ended.Reason)
	}
	if c.Found != 2 || c.HandedIn != 1 || c.Skipped[tenant.SkipBadName] != 1 {
		t.Errorf("counts %+v, want 2 found, 1 handed in, 1 skipped as bad_name", c)
	}
	if got := e.listedSkips(j.ID); len(got) != 1 || got["proj1/sessA"] != tenant.SkipBadName {
		t.Errorf("listed skips %v, want one bad_name row under proj1/sessA", got)
	}
}

// TestAFileWithAnotherNameIsSkipped guards hard links: a file with two
// names, one of which may lie outside the root, is skipped as not_regular
// under each name, never handed in.
func TestAFileWithAnotherNameIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the count of a file's names is not read on Windows")
	}
	e := newEnv(t, "")
	e.writeFiles(map[string]string{"proj1/sessA/ct2/ct2n-6293.dcm": "ct2n-6293.dcm"})
	dir := filepath.Join(e.tree, "proj1", "sessA", "ct2")
	if err := os.Link(filepath.Join(dir, "ct2n-6293.dcm"), filepath.Join(dir, "again.dcm")); err != nil {
		t.Fatal(err)
	}
	j := e.createJob()
	s := e.start(e.config())
	ended := e.waitEnded(s, j.ID, importBound)
	c := ended.Import.Counts
	if c.Found != 2 || c.HandedIn != 0 || c.Skipped[tenant.SkipNotRegular] != 2 {
		t.Errorf("counts %+v, want 2 found, both skipped as not_regular", c)
	}
	got := e.listedSkips(j.ID)
	if got["proj1/sessA/ct2/again.dcm"] != tenant.SkipNotRegular || got["proj1/sessA/ct2/ct2n-6293.dcm"] != tenant.SkipNotRegular {
		t.Errorf("listed skips %v, want both names as not_regular", got)
	}
}

// TestAWalkHoldsOneDirectorysNames guards the memory of a deep walk: with
// wide directories at every level, the names the walk holds are at most
// one directory's whole and a bounded part of each directory above it,
// not every level's whole list, and every entry is still judged once.
func TestAWalkHoldsOneDirectorysNames(t *testing.T) {
	const (
		depth = 5
		wide  = 40
		keep  = 1024
	)
	e := newEnv(t, "")
	e.settings.maxEntries.Store(1000)
	// Each level holds the next one first ("0" sorts before the names)
	// and wide files of 100-byte names after it.
	level := filepath.Join(e.tree, "proj1", "sessA")
	var names []string
	for i := range wide {
		names = append(names, strconv.Itoa(1000+i)+strings.Repeat("n", 96))
	}
	for range depth {
		if err := os.MkdirAll(level, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(level, n), []byte("not DICOM\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		level = filepath.Join(level, "0")
	}
	j := e.createJob()
	cfg := e.config()
	cfg.seams.keep = keep
	var mu sync.Mutex
	most := 0
	cfg.seams.held = func(n int) {
		mu.Lock()
		most = max(most, n)
		mu.Unlock()
	}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	if c := ended.Import.Counts; c.Found != depth*wide || c.Reconcile() != "" {
		t.Errorf("counts %+v, want %d found, each once", c, depth*wide)
	}
	one := namesBytes(append(names, "0"))
	mu.Lock()
	defer mu.Unlock()
	if bound := one + depth*keep; most > bound {
		t.Errorf("the walk held %d bytes of names at most, want at most %d: one directory's %d and %d above each of %d levels",
			most, bound, one, keep, depth)
	}
}
