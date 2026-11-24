package rebuild

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files the tests archive.
const corpusDir = "../receive/testdata"

// studyFiles are the seven files of one CT study in two series, and others
// files of studies of their own.
var studyFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm",
	"ct5n-3353.dcm", "MR_small.dcm", "liver_1frame.dcm", "SC_rgb_rle_2frame.dcm"}

// fastClock is the pipeline's clock, running fastClockRate times faster
// than the machine's, so a receiver's quiet period of five seconds passes
// in a quarter of a second and the tests archive through the whole
// pipeline at real speed.
type fastClock struct{ start time.Time }

// fastClockRate is how much faster the clock runs.
const fastClockRate = 20

// Now implements receive.Clock.
func (c fastClock) Now() time.Time {
	return c.start.Add(time.Since(c.start) * fastClockRate)
}

// After implements receive.Clock.
func (c fastClock) After(d time.Duration) <-chan time.Time { return time.After(d / fastClockRate) }

// rig is a tenant on a real driver, a directory storage, a receiver, and a
// pipeline that archived the corpus through every step.
type rig struct {
	t       testing.TB
	ctx     context.Context
	p       tenant.Pipeline
	storage blob.Store
	dataDir string
	dbPath  string // the SQLite file; empty on Postgres
	address string // the Postgres address; empty on SQLite
	clock   fastClock
	seq     atomic.Int64
	// keys authenticate the correction records the rig writes.
	keys *secret.Keys
}

// rigTenantID is the rig's tenant's id, which its records are written for.
const rigTenantID = "T0000000000001"

// newRig makes the tenant "lab" on SQLite, or on Postgres when postgres is
// set (skipping when there is none to use), with the storage "main" and the
// receiver "dimse".
func newRig(t testing.TB, postgres bool) *rig {
	t.Helper()
	r := &rig{t: t, ctx: context.Background(), dataDir: t.TempDir(), clock: fastClock{start: time.Now()}, keys: testkit.InstallationKeys(t)}
	var st tenant.Store
	var err error
	if postgres {
		addr, ok := testkit.PostgresSchema(t)
		if !ok {
			t.Skip("no Postgres to rebuild a tenant on")
		}
		r.address = addr
		st, err = (driver.PostgresTenants{}).Open(r.ctx, rigTenantID, "lab", addr)
	} else {
		d := driver.SQLiteTenants{DataDir: r.dataDir}
		if r.dbPath, err = d.Path("lab"); err != nil {
			t.Fatal(err)
		}
		st, err = d.Open(r.ctx, rigTenantID, "lab", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.p = st.(tenant.Pipeline)
	root := filepath.Join(r.dataDir, "storage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	r.storage = ds
	now := r.clock.Now()
	if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "archive"}},
		now, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.AddReceiver(r.ctx, tenant.Receiver{Name: "dimse", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, now, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// audit returns a new audit entry for a change the test makes.
func (r *rig) audit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("a%08d", r.seq.Add(1)), Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// archive runs the corpus files named through the real pipeline and waits
// until every reception is done.
func (r *rig) archive(names ...string) {
	r.t.Helper()
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		r.t.Fatal(err)
	}
	p, err := receive.New(receive.Config{Tenant: "lab", Store: r.p, Hasher: hasher, Clock: r.clock,
		OpenStorage:  func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		IncomingRoot: filepath.Join(r.dataDir, "incoming"), Workers: 2, Lease: 30 * time.Second, PollInterval: time.Second,
		BatchInterval: time.Second, RetryInterval: 5 * time.Second})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := p.Start(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(corpusDir, n)) // #nosec G304 -- a test's own file names.
		if err != nil {
			r.t.Fatal(err)
		}
		out := p.Accept(r.ctx, receive.Arrival{Receiver: "dimse", Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
		if out.Status != receive.Received {
			r.t.Fatalf("%s: outcome %v (%s)", n, out.Status, out.Reason)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		rs, _, err := r.p.ListReceptions(r.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
		if err != nil {
			r.t.Fatal(err)
		}
		done := len(rs) > 0
		for _, rc := range rs {
			if rc.State != reception.Done {
				done = false
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the receptions did not all end: %+v", rs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pageOf returns the first page of n items.
func pageOf(n int) master.Page { return master.Page{Limit: n} }

// rigTenant answers the rig's tenant's id (Config.TenantID).
func rigTenant(context.Context, string) (string, error) { return rigTenantID, nil }

// oneTenant is the installation of one tenant, "lab".
type oneTenant struct{ p tenant.Pipeline }

// Names answers the one tenant.
func (o oneTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database.
func (o oneTenant) Store(string) (tenant.Store, error) { return o.p, nil }

// readOnly is a store that fails the test on any write: a rebuild only
// reads and lists.
type readOnly struct {
	blob.Store
	writes *atomic.Int64
}

// Put counts a write.
func (s readOnly) Put(ctx context.Context, key string, rd io.Reader) (blob.Written, error) {
	s.writes.Add(1)
	return s.Store.Put(ctx, key, rd)
}

// Delete counts a write.
func (s readOnly) Delete(ctx context.Context, key string) error {
	s.writes.Add(1)
	return s.Store.Delete(ctx, key)
}

// storages answers every storage name with the rig's one store.
type storages struct{ s blob.Store }

// Get answers the storage.
func (s storages) Get(context.Context, string) (blob.Store, error) { return s.s, nil }

// Close keeps it open: the rig closes it.
func (storages) Close() {}

// config returns a supervisor's configuration over the rig, counting any
// write to the storage into writes.
func (r *rig) config(writes *atomic.Int64) Config {
	c := Config{Tenants: oneTenant{p: r.p}, OpenStorages: func(string, tenant.Pipeline) Storages {
		return storages{s: readOnly{Store: r.storage, writes: writes}}
	}, PollInterval: 20 * time.Millisecond, Interval: time.Second, Keys: r.keys, TenantID: rigTenant}
	return withRenew(c, 200*time.Millisecond)
}

// start starts a supervisor over cfg, stopped at the test's end.
func (r *rig) start(cfg Config) *Supervisor {
	r.t.Helper()
	s, err := New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := s.Start(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

// createRebuild makes a rebuild job of the mode, its scratch schema made
// on Postgres.
func (r *rig) createRebuild(mode tenant.RebuildMode) tenant.Job {
	r.t.Helper()
	scratch := ""
	if r.address != "" {
		var ok bool
		if scratch, ok = testkit.PostgresScratch(r.t); !ok {
			r.t.Skip("no Postgres to make a scratch schema in")
		}
	}
	j, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: fmt.Sprintf("job%08d", r.seq.Add(1)), Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: mode, Scratch: scratch}, Created: time.Now()}, r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

// waitEnded waits until the job has ended and its unit is done, poking the
// supervisor, and returns it.
func (r *rig) waitEnded(s *Supervisor, id string) tenant.Job {
	r.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		j, err := r.p.Job(r.ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		// The job ends first; its unit is done once its shadow is dropped.
		if w, err := r.p.Work(r.ctx, tenant.JobUnitID(id, "")); err == nil && j.State.Ended() && w.State == tenant.WorkDone {
			return j
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("job %s did not end: %+v", id, j)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
}

// run runs a rebuild of the mode to its end with cfg and returns the job
// and its tallies.
func (r *rig) run(cfg Config, mode tenant.RebuildMode) (tenant.Job, map[string]int64) {
	r.t.Helper()
	j := r.createRebuild(mode)
	s := r.start(cfg)
	j = r.waitEnded(s, j.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		r.t.Fatal(err)
	}
	return j, r.tallies(j.ID)
}

// tallies returns a job's tallies by key.
func (r *rig) tallies(id string) map[string]int64 {
	r.t.Helper()
	got, err := r.p.JobTallies(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]int64{}
	for _, g := range got {
		out[g.Key] = g.Count
	}
	return out
}

// scanPaths returns the catalog's scans' labels, in order.
func (r *rig) scanPaths() []label.Path {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderNewest}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	var out []label.Path
	for _, s := range page.Items {
		out = append(out, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	}
	key := func(p label.Path) string { return p.Subject + "/" + p.Session + "/" + p.Scan }
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// catalogText is the catalog in use as its reads answer it, one line a
// row: every subject, session, scan and instance with every value a read
// gives, and the totals.
func (r *rig) catalogText() string {
	r.t.Helper()
	var lines []string
	subs, err := r.p.ListSubjects(r.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, s := range subs.Items {
		lines = append(lines, fmt.Sprintf("subject %+v", s))
	}
	for _, p := range r.scanPaths() {
		se, err := r.p.CatalogSession(r.ctx, p.Subject, p.Session)
		if err != nil {
			r.t.Fatal(err)
		}
		sc, err := r.p.CatalogScan(r.ctx, p)
		if err != nil {
			r.t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("session %+v", se), fmt.Sprintf("scan %+v", sc))
		ins, _, err := r.p.ListHeldInstances(r.ctx, p, master.Page{Limit: 1000})
		if err != nil {
			r.t.Fatal(err)
		}
		for _, in := range ins {
			lines = append(lines, fmt.Sprintf("instance %+v", in))
		}
	}
	totals, err := r.p.CatalogTotals(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	lines = append(lines, fmt.Sprintf("totals %+v", totals))
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// forgetCatalog removes every row of the catalog from the tenant's
// database, its storage left as it is: what a rebuild from the storages
// alone must make again.
func (r *rig) forgetCatalog() {
	r.t.Helper()
	stmts := []string{"DELETE FROM instances", "DELETE FROM reception_scans", "DELETE FROM session_modalities", "DELETE FROM scans",
		"DELETE FROM sessions", "DELETE FROM subjects", "DELETE FROM storage_totals",
		"UPDATE catalog_totals SET subjects = 0, sessions = 0, scans = 0, instances = 0"}
	if r.address != "" {
		testkit.PostgresExec(r.t, r.address, stmts...)
		return
	}
	sqliteExec(r.t, r.dbPath, stmts...)
}
