package servecmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// workCorpus holds the public test files the background work's tests
// archive (pydicom's test files, copied unchanged).
const workCorpus = "../receive/testdata"

// noSettings is an installation that sets nothing: every setting is its
// built-in default unless the tenant sets its own.
type noSettings struct{}

// Setting answers that nothing is set.
func (noSettings) Setting(context.Context, string) (string, error) {
	return "", fault.New(fault.NotFound, "not set")
}

// quickClock runs twenty times faster than the machine's, so a receiver's
// quiet period passes at once.
type quickClock struct{ start time.Time }

// Now answers the clock's time.
func (c quickClock) Now() time.Time { return c.start.Add(time.Since(c.start) * 20) }

// After answers a channel after d of the clock's time.
func (c quickClock) After(d time.Duration) <-chan time.Time { return time.After(d / 20) }

// workRig is one tenant "lab" on SQLite with the directory storages "main"
// (a primary, under <root>/lab/main) and "copy" (its replica, under
// <root>/lab/copy), a DIMSE receiver into main, and the server's background
// work for it as serve builds it, over the real storage port.
type workRig struct {
	t    *testing.T
	ctx  context.Context
	root string
	data string
	keys *secret.Keys
	db   tenantDB
	w    *tenantWork
	seq  atomic.Int64
}

// newWorkRig makes the rig; its work is stopped at the test's end.
func newWorkRig(t *testing.T) *workRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &workRig{t: t, ctx: context.Background(), root: root, data: t.TempDir(), keys: testkit.InstallationKeys(t)}
	for _, p := range []string{"main", "copy"} {
		if err := os.MkdirAll(filepath.Join(root, "lab", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := (driver.SQLiteTenants{DataDir: r.data}).Open(r.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.db, err = dbOf(st); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	for _, name := range []string{"main", "copy"} {
		if _, err := r.db.AddStorage(r.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
			Directory: tenant.DirectoryStorage{Root: "fast", Path: name}}, at, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := r.db.AttachReplica(r.ctx, "copy", "main", at, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.AddReceiver(r.ctx, tenant.Receiver{Name: "dimse", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, at, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	key, err := r.keys.Derive(secret.ConfirmToken)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.w = &tenantWork{d: Deps{Keys: r.keys, Master: oneTenant{}, Log: log}, values: settingvalue.Reader{Master: noSettings{}},
		opener:       place.Opener{Values: roots{{Name: "fast", Path: root}}, Keys: r.keys, Cleanups: &place.Cleanups{}},
		installation: server.NewID(), confirmKey: key, owners: &ownerLocks{}, owner: workerName("test.host", 1), log: log, interval: time.Hour,
		now: time.Now, switchers: map[string]*tenantSwitcher{}, sets: map[string]*tenantSet{}, ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), time.Minute)
		defer scancel()
		_ = r.w.Stop(sctx)
	})
	return r
}

// audit returns a new audit entry for a change the test makes.
func (r *workRig) audit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("a%08d", r.seq.Add(1)), Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// start starts the tenant's background work as the server's reading of the
// tenants does.
func (r *workRig) start() {
	r.t.Helper()
	if err := r.w.startSet(r.ctx, "lab", r.db, r.w.epochOf("lab")); err != nil {
		r.t.Fatal(err)
	}
}

// pipeline starts the real reception pipeline over the rig as serve wires
// it: what it is given of the tenant's background work is serve's
// (receptionWrites), its storages are opened by the rows as they stand. It
// is stopped at the test's end.
func (r *workRig) pipeline() *receive.Pipeline {
	r.t.Helper()
	pause, committed, err := r.w.receptionWrites(r.ctx, "lab", r.db, func(string) {})
	if err != nil {
		r.t.Fatal(err)
	}
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		r.t.Fatal(err)
	}
	storages := newTenantStorages(r.w.opener, oneTenant{}, "lab", r.db)
	p, err := receive.New(receive.Config{Tenant: "lab", Store: r.db, Hasher: hasher, Clock: quickClock{start: time.Now()},
		OpenStorage: storages.Open, IncomingRoot: filepath.Join(r.data, "incoming"), Workers: 2, Lease: 30 * time.Second,
		PollInterval: time.Second, BatchInterval: time.Second, RetryInterval: 5 * time.Second,
		Pause: pause, Wake: committed})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := p.Start(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		_ = p.Stop(context.Background())
		_ = storages.Close()
	})
	return p
}

// accept hands the corpus files named to a running pipeline.
func (r *workRig) accept(p *receive.Pipeline, names ...string) {
	r.t.Helper()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(workCorpus, n)) // #nosec G304 -- a test's own file names.
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

// waitFor polls cond until it holds, failing the test with what it waited
// for after d.
func (r *workRig) waitFor(what string, d time.Duration, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// archived reports whether want receptions have all reached done.
func (r *workRig) archived(want int) bool {
	r.t.Helper()
	rs, _, err := r.db.ListReceptions(r.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	done := 0
	for _, rc := range rs {
		if rc.State.Ended() {
			done++
		}
	}
	return done == want && len(rs) == want
}

// inStep reports whether every scan of main is in step on copy.
func (r *workRig) inStep() bool {
	r.t.Helper()
	f, err := r.db.SwitchFacts(r.ctx, "main", "copy")
	if err != nil {
		r.t.Fatal(err)
	}
	return f.ScansNotInStep == 0
}

// files returns every file under the place at path below the tenant's
// directory, by key, and its SHA-256, read straight from the disk.
func (r *workRig) files(path, prefix string) map[string]string {
	r.t.Helper()
	base := filepath.Join(r.root, "lab", path)
	out := map[string]string{}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		key := filepath.ToSlash(strings.TrimPrefix(p, base+string(filepath.Separator)))
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the test's own storage, which nothing else changes.
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[key] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

// catalogKeys returns the key, size and SHA-256 of every instance the
// catalog holds, by key.
func (r *workRig) catalogKeys() map[string]tenant.ScanInstance {
	r.t.Helper()
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]tenant.ScanInstance{}
	for _, s := range scans.Items {
		ins, _, err := r.db.ListHeldInstances(r.ctx, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label},
			master.Page{Limit: 1000})
		if err != nil {
			r.t.Fatal(err)
		}
		for _, in := range ins {
			out[in.Key] = in
		}
	}
	return out
}

// readWhole reads the object at key from the store named, opened as the
// server opens it by the row as it stands, and returns its SHA-256.
func (r *workRig) readWhole(storage, key string) string {
	r.t.Helper()
	row, err := r.db.Storage(r.ctx, storage)
	if err != nil {
		r.t.Fatal(err)
	}
	st, err := r.w.opener.OpenForReading(r.ctx, place.TenantOf("T0000000000001", "lab", r.db), row)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	rc, err := st.Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatalf("%s on %s: %v", key, storage, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		r.t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// place0 returns the rig's tenant as the storage port opens its storages.
func place0(r *workRig) place.Tenant { return place.TenantOf("T0000000000001", "lab", r.db) }

// sortedKeys returns m's keys in order, for a failure's message.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
