package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files the tests archive.
const corpusDir = "../receive/testdata"

// studyFiles are seven files of one CT study in two series, and three files
// of studies of their own.
var studyFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm",
	"ct5n-3353.dcm", "MR_small.dcm", "liver_1frame.dcm", "SC_rgb_rle_2frame.dcm"}

// fastClock runs fastClockRate times faster than the machine's, so a
// receiver's quiet period passes at once.
type fastClock struct{ start time.Time }

// fastClockRate is how much faster the clock runs.
const fastClockRate = 20

// Now answers the clock's time.
func (c fastClock) Now() time.Time {
	return c.start.Add(time.Since(c.start) * fastClockRate)
}

// After answers a channel after d of the clock's time.
func (c fastClock) After(d time.Duration) <-chan time.Time { return time.After(d / fastClockRate) }

// rig is one tenant "lab" with a directory storage "main", a receiver
// "dimse", and the receive pipeline that archives into it.
type rig struct {
	t       testing.TB
	ctx     context.Context
	p       tenant.Pipeline
	storage blob.Store
	root    string
	dataDir string
	clock   fastClock
	seq     atomic.Int64
	// keys authenticate the correction records the engines write.
	keys *secret.Keys
}

// rigTenantID is the rig's tenant's id, which its records are written for.
const rigTenantID = "T0000000000001"

// newRig makes the rig on SQLite, or on Postgres when postgres is set
// (skipping when there is none).
func newRig(t testing.TB, postgres bool) *rig {
	t.Helper()
	r := &rig{t: t, ctx: context.Background(), dataDir: t.TempDir(), clock: fastClock{start: time.Now()}, keys: testkit.InstallationKeys(t)}
	var st tenant.Store
	var err error
	if postgres {
		addr, ok := testkit.PostgresSchema(t)
		if !ok {
			t.Skip("no Postgres to correct a tenant on")
		}
		st, err = (driver.PostgresTenants{}).Open(r.ctx, rigTenantID, "lab", addr)
	} else {
		st, err = (driver.SQLiteTenants{DataDir: r.dataDir}).Open(r.ctx, rigTenantID, "lab", "")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.p = st.(tenant.Pipeline)
	r.root = filepath.Join(r.dataDir, "storage")
	if err := os.MkdirAll(r.root, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	r.storage = ds
	now := time.Now()
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

// engine returns an engine over the rig, with a grace period of a day.
func (r *rig) engine() *Engine {
	r.t.Helper()
	return r.engineWith(nil)
}

// engineWith returns an engine over the rig as engine does, its
// configuration changed by change first.
func (r *rig) engineWith(change func(*Config)) *Engine {
	r.t.Helper()
	cfg := Config{Tenant: "lab", TenantID: rigTenantID, Keys: r.keys, Store: r.p, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		Grace: func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }}
	if change != nil {
		change(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// quarantined returns the quarantine's records of reason.
func (r *rig) quarantined(reason tenant.QuarantineReason) []tenant.QuarantinedFile {
	r.t.Helper()
	q, _, err := r.p.ListQuarantined(r.ctx, tenant.QuarantineFilter{Reason: reason}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return q
}

// incomingFiles returns the files left in the incoming area, by path under
// it.
func (r *rig) incomingFiles() []string {
	r.t.Helper()
	var out []string
	root := filepath.Join(r.dataDir, "incoming")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.t.Fatal(err)
	}
	return out
}

// dirsNaming returns the storage's directories whose path holds s.
func (r *rig) dirsNaming(s string) []string {
	r.t.Helper()
	var out []string
	err := filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && strings.Contains(filepath.ToSlash(path[len(r.root):]), "/"+s) {
			out = append(out, filepath.ToSlash(path[len(r.root):]))
		}
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// archive runs the corpus files named through the real pipeline and waits
// until every reception has ended.
func (r *rig) archive(names ...string) {
	r.t.Helper()
	p := r.pipeline()
	defer func() { _ = p.Stop(context.Background()) }()
	r.accept(p, names...)
	r.waitEnded()
}

// pipeline starts the real reception pipeline over the rig; the caller
// stops it.
func (r *rig) pipeline() *receive.Pipeline {
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
	return p
}

// accept hands the corpus files named to a running pipeline.
func (r *rig) accept(p *receive.Pipeline, names ...string) {
	r.t.Helper()
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
}

// waitEnded waits until every reception has ended.
func (r *rig) waitEnded() {
	r.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		rs, _, err := r.p.ListReceptions(r.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
		if err != nil {
			r.t.Fatal(err)
		}
		ended := len(rs) > 0
		for _, rc := range rs {
			if !rc.State.Ended() {
				ended = false
			}
		}
		if ended {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the receptions did not all end: %+v", rs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// scans returns the catalog's scans, by labels.
func (r *rig) scans() []tenant.CatalogScan {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Items
}

// scanWith returns a scan of at least n instances.
func (r *rig) scanWith(n int64) tenant.CatalogScan {
	r.t.Helper()
	for _, s := range r.scans() {
		if s.Instances >= n {
			return s
		}
	}
	r.t.Fatalf("no scan holds %d instances", n)
	return tenant.CatalogScan{}
}

// held returns a scan's instances.
func (r *rig) held(p label.Path) []tenant.ScanInstance {
	r.t.Helper()
	ins, _, err := r.p.ListHeldInstances(r.ctx, p, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return ins
}

// objects returns every file of the storage by key, with its SHA-256,
// read from the directory itself.
func (r *rig) objects() map[string][sha256.Size]byte {
	r.t.Helper()
	out := map[string][sha256.Size]byte{}
	err := filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) // #nosec G304 G122 -- a file under the test's own directory, which nothing else writes.
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.root, path)
		out[filepath.ToSlash(rel)] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// under returns the keys of objs that begin with any of the prefixes, in
// order.
func under(objs map[string][sha256.Size]byte, prefixes ...string) []string {
	var out []string
	for k := range objs {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				out = append(out, k)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// wantUnchanged fails unless every object of before but those under the
// prefixes is in after with the same bytes.
func (r *rig) wantUnchanged(before, after map[string][sha256.Size]byte, gone ...string) {
	r.t.Helper()
	for k, sum := range before {
		if len(under(map[string][sha256.Size]byte{k: sum}, gone...)) > 0 {
			continue
		}
		if after[k] != sum {
			r.t.Errorf("%s changed or went, and is not the correction's", k)
		}
	}
}

// correctionRecord reads a correction's record from the storage.
func (r *rig) correctionRecord(id string) record.Correction {
	r.t.Helper()
	key, err := (record.Correction{ID: id}).Key()
	if err != nil {
		r.t.Fatal(err)
	}
	rc, err := r.storage.Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	c, err := record.DecodeCorrection(rc, rigTenantID, r.keys)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// deleteNow previews and deletes target.
func (r *rig) deleteNow(e *Engine, target tenant.CorrectionTarget) (tenant.Correction, error) {
	r.t.Helper()
	p, err := r.p.PreviewDelete(r.ctx, target, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	return e.Delete(r.ctx, DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
}

// onBoth runs fn on SQLite and on Postgres (skipped when there is none).
func onBoth(t *testing.T, fn func(t *testing.T, pg bool)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, false) })
	t.Run("postgres", func(t *testing.T) { fn(t, true) })
}

// errCrash is the crash a seam makes.
var errCrash = errors.New("the process stopped here")

// crashAt returns a seam that crashes at the step named, once.
func crashAt(step string) func(string) error {
	var done atomic.Bool
	return func(s string) error {
		if s == step && done.CompareAndSwap(false, true) {
			return errCrash
		}
		return nil
	}
}

// state reads a correction's state.
func (r *rig) state(id string) tenant.Correction {
	r.t.Helper()
	c, err := r.p.Correction(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// pathOf returns a catalog scan's labels.
func pathOf(s tenant.CatalogScan) label.Path {
	return label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
}

// receptionsByState counts the receptions by their state.
func (r *rig) receptionsByState() map[reception.State]int {
	r.t.Helper()
	rs, _, err := r.p.ListReceptions(r.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[reception.State]int{}
	for _, x := range rs {
		out[x.State]++
	}
	return out
}
