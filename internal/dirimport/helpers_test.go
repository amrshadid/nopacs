package dirimport

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files the tree is built from: files of
// pydicom's test corpus (receive/testdata/README.md).
const corpusDir = "../receive/testdata"

// manualClock is the pipeline's clock, moved by the test: a reception
// completes only when the test moves it past the quiet period, so every
// file of a walk lands before any reception is named. Its waits are short.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's time.
func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives after a thousandth of d.
func (c *manualClock) After(d time.Duration) <-chan time.Time {
	return time.After(max(d/1000, time.Millisecond))
}

// advance moves the clock by d.
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// offsetClock is the supervisor's clock: the time now, moved on by what
// the test added, so a unit failed for now is due again without the test
// waiting out its delay.
type offsetClock struct{ by atomic.Int64 }

// Now returns the time now moved on by the offset.
func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.by.Load())) }

// skip moves the clock on by d.
func (c *offsetClock) skip(d time.Duration) { c.by.Add(int64(d)) }

// writeFiles writes files under e.tree, each path's body copied from the
// corpus file it names.
func (e *env) writeFiles(files map[string]string) {
	e.t.Helper()
	for p, corpus := range files {
		b, err := os.ReadFile(filepath.Join(corpusDir, corpus)) // #nosec G304 -- a test's own file names.
		if err != nil {
			e.t.Fatal(err)
		}
		full := filepath.Join(e.tree, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o600); err != nil { // #nosec G703 -- a test's own paths under its temporary directory.
			e.t.Fatal(err)
		}
	}
}

// listedSkips returns the job's listed skips, by the path each is listed
// under.
func (e *env) listedSkips(id string) map[string]tenant.ImportSkipCode {
	e.t.Helper()
	listed, _, err := e.store.ListImportFiles(e.ctx, id, tenant.ImportFileFilter{Kinds: []tenant.ImportFileKind{tenant.ImportFileSkipped}},
		master.Page{Limit: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	got := map[string]tenant.ImportSkipCode{}
	for _, f := range listed {
		got[f.Path] = f.SkipCode
	}
	return got
}

// fakeSettings answers the settings the supervisor reads.
type fakeSettings struct {
	maxEntries atomic.Int64
	maxUnits   atomic.Int64
}

// ImportServerMaxUnits answers import.server-max-units.
func (*fakeSettings) ImportServerMaxUnits(context.Context) (int, error) { return 8, nil }

// ImportMaxDirEntries answers import.max-dir-entries, as the test set it.
func (s *fakeSettings) ImportMaxDirEntries(context.Context) (int, error) {
	return int(s.maxEntries.Load()), nil
}

// ImportMaxUnits answers import.max-units, as the test set it.
func (s *fakeSettings) ImportMaxUnits(context.Context) (int, error) {
	return int(s.maxUnits.Load()), nil
}

// oneTenant is the installation of one tenant, "lab".
type oneTenant struct{ p tenant.Pipeline }

// Names answers the one tenant.
func (o *oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database.
func (o *oneTenant) Store(string) (tenant.Store, error) { return o.p, nil }

// treeRoot is the one root the test's installation names, "old".
type treeRoot struct{ path string }

// Root answers the root "old", resolved as SettingRoots resolves one.
func (r treeRoot) Root(_ context.Context, _ string, _ tenant.Store, name string) (string, error) {
	if name != "old" {
		return "", fmt.Errorf("no root %s", name)
	}
	return resolveDir(r.path, name)
}

// countingSink is the pipeline, counting each path it took.
type countingSink struct {
	p  *receive.Pipeline
	mu sync.Mutex
	// taken counts the files the pipeline answered received, by path.
	taken map[string]int
}

// Accept hands a to the pipeline and counts it when it was taken.
func (s *countingSink) Accept(ctx context.Context, a receive.Arrival) receive.Outcome {
	out := s.p.Accept(ctx, a)
	if out.Status == receive.Received && a.Import != nil {
		s.mu.Lock()
		s.taken[a.Import.Path]++
		s.mu.Unlock()
	}
	return out
}

// twice returns the paths taken more than once.
func (s *countingSink) twice() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p, n := range s.taken {
		if n > 1 {
			out = append(out, p)
		}
	}
	return out
}

// env is a tenant on a real driver with a directory storage, an import
// receiver over a tree under a temporary root, and a running reception
// pipeline.
type env struct {
	t        *testing.T
	ctx      context.Context
	dataDir  string
	tree     string
	store    tenant.Pipeline
	storeDir string
	storage  blob.Store
	clock    *manualClock
	p        *receive.Pipeline
	sink     *countingSink
	settings *fakeSettings
	seq      atomic.Int64
}

// The receiver's layout: project, session and scan folders, then the file;
// one unit per session folder.
const (
	testPattern   = `^(?P<project>[^/]+)/(?P<session>[^/]+)/(?P<scan>[^/]+)/[^/]+$`
	testUnitDepth = 2
	// testMaxFile is the receiver's largest file: jpeg_3frame_nobot.dcm
	// (53380 bytes) passes it.
	testMaxFile = 40000
)

// newEnv makes the tenant, on Postgres when postgres is an address and on
// SQLite otherwise, its storage, the import receiver "old-import", and a
// started pipeline.
func newEnv(t *testing.T, postgres string) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the reception pipeline and an import on a database; left out under -short")
	}
	e := &env{t: t, ctx: context.Background(), dataDir: t.TempDir(), clock: &manualClock{now: time.Now().UTC()},
		settings: &fakeSettings{}}
	e.settings.maxEntries.Store(8)
	e.settings.maxUnits.Store(4)
	e.tree = filepath.Join(e.dataDir, "tree")
	var s tenant.Store
	var err error
	if postgres != "" {
		s, err = (driver.PostgresTenants{}).Open(e.ctx, "T0000000000001", "lab", postgres)
	} else {
		s, err = (driver.SQLiteTenants{DataDir: e.dataDir}).Open(e.ctx, "T0000000000001", "lab", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.store = s.(tenant.Pipeline)
	e.storeDir = filepath.Join(e.dataDir, "storage")
	if err := os.MkdirAll(e.storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(e.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	e.storage = ds
	now := e.clock.Now()
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, now, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddReceiver(e.ctx, testReceiver(), now, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{7}, naming.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	p, err := receive.New(receive.Config{
		Tenant: "lab", Store: e.store, Hasher: hasher, Clock: e.clock,
		OpenStorage:  func(context.Context, string) (blob.Store, error) { return e.storage, nil },
		IncomingRoot: filepath.Join(e.dataDir, "incoming"), Workers: 2, Lease: 30 * time.Second,
		PollInterval: 2 * time.Second, BatchInterval: 5 * time.Second, RetryInterval: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(e.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	e.p = p
	e.sink = &countingSink{p: p, taken: map[string]int{}}
	return e
}

// testReceiver is the import receiver: the pattern layout, a map that
// gives each session folder's subject, and a naming rule over the parts.
func testReceiver() tenant.Receiver {
	return tenant.Receiver{Name: "old-import", Type: tenant.ReceiverImport, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: testMaxFile,
		Naming: &naming.Spec{Subject: "{project}_{subject}", Session: "{project}_{session}", Scan: "{scan}"},
		Import: tenant.ImportReceiver{Root: "old", Layout: importpath.Pattern, Pattern: testPattern, UnitDepth: testUnitDepth,
			Map: tenant.ImportMap{KeyColumns: []string{"project", "label"}, ValueColumn: "subject_label",
				KeyParts: []string{"project", "session"}, Gives: "subject"}}}
}

// testMap is the job's map: every session folder of the tree but sessGone,
// whose files are so skipped as unmapped.
const testMap = "project,label,subject_label\nproj1,sessA,subj1\nproj1,sessB,subj2\nproj1,sessC,subj3\nproj1,sessL,subj4\nproj1,wide,subj5\n"

// audit returns a new audit entry for a change the test makes.
func (e *env) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(e.seq.Add(1), 10) + "x", Time: e.clock.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// config returns the supervisor's configuration over the env.
func (e *env) config() Config {
	return Config{
		Tenants: &oneTenant{p: e.store}, Installation: e.settings,
		TenantSettings: func(tenant.Store) TenantSettings { return e.settings },
		Roots:          treeRoot{path: e.tree},
		Sink:           func(string) receive.Sink { return e.sink },
		Interval:       time.Second, PollInterval: 20 * time.Millisecond,
		seams: seams{renew: 200 * time.Millisecond},
	}
}

// start starts a supervisor over cfg, stopped at the test's end.
func (e *env) start(cfg Config) *Supervisor {
	e.t.Helper()
	s, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := s.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

// createJob makes the import job of proj1 with the inline map.
func (e *env) createJob() tenant.Job {
	e.t.Helper()
	j, err := e.store.CreateImportJob(e.ctx, tenant.NewImportJob{ID: "job" + strconv.FormatInt(e.seq.Add(1), 10) + "imp",
		Receiver: "old-import", Path: "proj1", MapCSV: []byte(testMap), FailuresKept: tenant.MinFailuresKept,
		Created: e.clock.Now()}, e.audit(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// waitEnded waits for the import job to end, poking the supervisor, and
// moving the pipeline's clock once every walk has ended, so its
// receptions complete; it fails the test after the bound.
func (e *env) waitEnded(s *Supervisor, id string, bound time.Duration) tenant.Job {
	e.t.Helper()
	deadline := time.Now().Add(bound)
	for {
		j, err := e.store.ImportJob(e.ctx, id)
		if err != nil {
			e.t.Fatal(err)
		}
		if j.State.Ended() && j.Import.Counts.Processing == 0 {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the import job did not end in %s: %+v %+v", bound, j, j.Import.Counts)
		}
		if j.Import.Listed && j.Import.Counts.UnitsWalked == j.Import.Counts.Units {
			e.clock.advance(10 * time.Second)
		}
		if s != nil {
			s.Poke("lab")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// file is one file of the tree, with what the import makes of it.
type file struct {
	// path is its path under the root; corpus the corpus file it copies,
	// or "" for a file of its own bytes.
	path, corpus string
	body         []byte
	// labels is the subject, session and scan it is archived under, empty
	// for a file not archived from this path.
	labels [3]string
}

// testTree is the tree the import reads. Its traps: a text file
// (not_part10), a link (not_regular), a file deeper than the layout and
// one at the listing's level (outside_layout), a session the map does not
// list (unmapped_session), a file larger than the receiver's largest
// (too_large), a folder of nine files past the lowered
// import.max-dir-entries of 8 (directory_too_wide), a path past 2048 bytes
// (path_too_long), and one instance in two session folders (archived once,
// already held once).
func testTree() []file {
	a := [3]string{"proj1_subj1", "proj1_sessA", ""}
	scan := func(l [3]string, s string) [3]string { l[2] = s; return l }
	out := []file{
		{path: "proj1/readme.txt", body: []byte("notes about the project\n")},
		{path: "proj1/sessA/ct2/ct2n-6293.dcm", corpus: "ct2n-6293.dcm", labels: scan(a, "ct2")},
		{path: "proj1/sessA/ct2/ct2n-6924.dcm", corpus: "ct2n-6924.dcm", labels: scan(a, "ct2")},
		{path: "proj1/sessA/big/jpeg_3frame_nobot.dcm", corpus: "jpeg_3frame_nobot.dcm"},
		{path: "proj1/sessA/ct5/notes.txt", body: bytes.Repeat([]byte("x"), 300)},
		{path: "proj1/sessB/mr/MR_small.dcm", corpus: "MR_small.dcm", labels: [3]string{"proj1_subj2", "proj1_sessB", "mr"}},
		{path: "proj1/sessB/mr/extra/deep.dcm", corpus: "liver_1frame.dcm"},
		{path: "proj1/sessC/dup/MR_small.dcm", corpus: "MR_small.dcm"},
		{path: "proj1/sessC/sc/SC_rgb_rle_2frame.dcm", corpus: "SC_rgb_rle_2frame.dcm", labels: [3]string{"proj1_subj3", "proj1_sessC", "sc"}},
		{path: "proj1/sessGone/x/liver_1frame.dcm", corpus: "liver_1frame.dcm"},
	}
	for _, n := range []string{"2062", "2392", "2693", "3023", "3353"} {
		out = append(out, file{path: "proj1/sessA/ct5/ct5n-" + n + ".dcm", corpus: "ct5n-" + n + ".dcm", labels: scan(a, "ct5")})
	}
	for i := range 9 {
		out = append(out, file{path: "proj1/wide/f" + strconv.Itoa(i) + ".dcm", corpus: "MR_small_RLE.dcm"})
	}
	return out
}

// longSegment is one segment of the path past 2048 bytes.
var longSegment = strings.Repeat("d", 200)

// buildTree writes the tree under e.tree: testTree's files, a link at
// proj1/sessA/ct5/link.dcm to a file beside it, and under proj1/sessL a
// chain of directories of 200-byte names whose path passes 2048 bytes, with
// a file at its end. The chain is made through os.Root, one directory at a
// time, since its whole path is longer than some systems open at once.
func (e *env) buildTree() []file {
	e.t.Helper()
	files := testTree()
	for i, f := range files {
		body := f.body
		if f.corpus != "" {
			b, err := os.ReadFile(filepath.Join(corpusDir, f.corpus)) // #nosec G304 -- a test's own file names.
			if err != nil {
				e.t.Fatal(err)
			}
			body = b
		}
		files[i].body = body
		full := filepath.Join(e.tree, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil { // #nosec G703 -- a test's own paths under its temporary directory.
			e.t.Fatal(err)
		}
	}
	if err := os.Symlink("ct5n-2062.dcm", filepath.Join(e.tree, "proj1", "sessA", "ct5", "link.dcm")); err != nil {
		e.t.Fatal(err)
	}
	r, err := os.OpenRoot(e.tree)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := r.MkdirAll("proj1/sessL", 0o700); err != nil {
		e.t.Fatal(err)
	}
	cur, err := r.OpenRoot("proj1/sessL")
	if err != nil {
		e.t.Fatal(err)
	}
	for range 11 {
		if err := cur.Mkdir(longSegment, 0o700); err != nil {
			e.t.Fatal(err)
		}
		next, err := cur.OpenRoot(longSegment)
		_ = cur.Close()
		if err != nil {
			e.t.Fatal(err)
		}
		cur = next
	}
	if err := cur.WriteFile("MR_small.dcm", files[5].body, 0o600); err != nil {
		e.t.Fatal(err)
	}
	_ = cur.Close()
	return files
}

// postgresAddress returns a schema of the test Postgres, or skips.
func postgresAddress(t *testing.T) string {
	t.Helper()
	address, ok := testkit.PostgresSchema(t)
	if !ok {
		t.Skipf("%s is not set; the Postgres import test is skipped", testkit.PostgresEnv)
	}
	return address
}
