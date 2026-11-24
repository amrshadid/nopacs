package auditseal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// testInstallation is the installation id every test seals under.
const testInstallation = "inst-test-1"

// t0 is when the tests' entries start: a time with microseconds.
var t0 = time.Date(2026, 11, 21, 8, 0, 0, 123456000, time.UTC)

// backend is one database type the tests run on: a master and two tenants,
// each a migrated database of its own, and where they live for a tool that
// reads them from outside.
type backend struct {
	name    string
	master  master.Store
	tenants map[string]master.AuditSeals // by tenant name
	ids     map[string]string            // tenant name to id
	// where holds each log's location: a SQLite file's path, or a Postgres
	// address whose search_path names the schema.
	where map[string]string
}

// tenantNames are the two tenants every backend has, by name.
var tenantNames = []string{"lab", "clinic"}

// backends returns SQLite, and Postgres when NOPACS_TEST_POSTGRES names a
// server.
func backends(t *testing.T) []*backend {
	t.Helper()
	out := []*backend{sqliteBackend(t)}
	if b, ok := postgresBackend(t); ok {
		out = append(out, b)
	}
	return out
}

// sqliteBackend makes a SQLite master and two SQLite tenants.
func sqliteBackend(t *testing.T) *backend {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	b := &backend{name: "sqlite", tenants: map[string]master.AuditSeals{}, ids: map[string]string{}, where: map[string]string{}}
	path := filepath.Join(dir, "master.db")
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b.master, b.where[master.MasterAuditLog] = m, path
	tenants := driver.SQLiteTenants{DataDir: dir}
	for i, name := range tenantNames {
		id := fmt.Sprintf("tenant-%d-%s", i+1, name)
		p, err := tenants.Path(name)
		if err != nil {
			t.Fatal(err)
		}
		st, err := tenants.Open(ctx, id, name, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		b.tenants[name], b.ids[name], b.where[id] = st, id, p
	}
	return b
}

// postgresBackend makes a Postgres master and two Postgres tenants, each in
// a schema of its own.
func postgresBackend(t *testing.T) (*backend, bool) {
	t.Helper()
	ctx := context.Background()
	address, ok := testkit.PostgresSchema(t)
	if !ok {
		return nil, false
	}
	b := &backend{name: "postgres", tenants: map[string]master.AuditSeals{}, ids: map[string]string{}, where: map[string]string{}}
	m, err := driver.OpenMaster(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b.master, b.where[master.MasterAuditLog] = m, address
	for i, name := range tenantNames {
		id := fmt.Sprintf("tenant-%d-%s", i+1, name)
		a, _ := testkit.PostgresSchema(t)
		st, err := driver.PostgresTenants{}.Open(ctx, id, name, a)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		b.tenants[name], b.ids[name], b.where[id] = st, id, a
	}
	return b, true
}

// list implements Tenants over the backend's tenants.
func (b *backend) List(context.Context) ([]TenantLog, error) {
	var out []TenantLog
	for _, name := range tenantNames {
		out = append(out, TenantLog{ID: b.ids[name], Name: name})
	}
	return out, nil
}

// Log implements Tenants.
func (b *backend) Log(_ context.Context, name string) (master.AuditSeals, error) {
	st, ok := b.tenants[name]
	if !ok {
		return nil, fault.New(fault.NotFound, "no such tenant")
	}
	return st, nil
}

// appender is what the tests write entries with: both stores have it.
type appender interface {
	AppendAudit(ctx context.Context, entries ...master.AuditEntry) error
}

// ids makes entry ids unique across a test.
var ids struct {
	sync.Mutex
	n int
}

// newID returns a new id.
func newID() string {
	ids.Lock()
	defer ids.Unlock()
	ids.n++
	return fmt.Sprintf("e%07d", ids.n)
}

// write appends n entries at t0 plus offset, a millisecond apart, with
// fields that exercise the encoding: Unicode, details, empty values.
func write(t *testing.T, st any, n int, offset time.Duration) []master.AuditEntry {
	t.Helper()
	a, ok := st.(appender)
	if !ok {
		t.Fatalf("%T cannot append", st)
	}
	out := make([]master.AuditEntry, n)
	for i := range out {
		id := newID()
		e := master.AuditEntry{
			ID: id, Time: t0.Add(offset + time.Duration(i)*time.Millisecond), RemoteAddress: "192.0.2.7:4100",
			Method: "/nopacs.v1.SettingsService/SetSetting", TenantName: "lab", Object: "setting/" + id,
			Outcome: master.OutcomeDone, RequestID: "req-" + id,
		}
		switch i % 3 {
		case 0:
			e.KeyID = "K" + strings.ToUpper(id)
			e.Details = map[string]string{"scope": "tenant", "név": "értéké"}
		case 1:
			e.LocalUser = "501"
			e.Outcome, e.Reason = master.OutcomeRefused, "pas d'accès — 拒否"
		default:
			e.RemoteAddress, e.Details = "", map[string]string{}
		}
		out[i] = e
	}
	if err := a.AppendAudit(context.Background(), out...); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	return out
}

// keyFile writes a key file with versions 1 to n and returns the keys.
func keyFile(t *testing.T, n int) *secret.Keys {
	t.Helper()
	var text strings.Builder
	for v := 1; v <= n; v++ {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		text.WriteString("nopacs-key-v1 " + strconv.Itoa(v) + " " + base64.StdEncoding.EncodeToString(key) + "\n")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nopacs.key")
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// signerOf returns the audit signer of one key version.
func signerOf(t *testing.T, keys *secret.Keys, version uint32) *secret.Signer {
	t.Helper()
	d, err := keys.DeriveVersion(secret.AuditSeal, version)
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.Signer()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// clock is a clock a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// now returns the clock's time.
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// set moves the clock to t.
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// sealer returns a sealer of b's logs with signer, the clock and retention.
func (b *backend) sealer(t *testing.T, signer *secret.Signer, c *clock, retention time.Duration) *Sealer {
	t.Helper()
	s, err := New(Config{
		Installation: testInstallation, Master: b.master, Tenants: b, Signer: signer,
		Retention: func(context.Context) (time.Duration, error) { return retention, nil },
		Now:       c.now, NewID: newID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// logs returns each log of b by its name in the checkpoints.
func (b *backend) logs() map[string]master.AuditSeals {
	out := map[string]master.AuditSeals{master.MasterAuditLog: b.master}
	for name, st := range b.tenants {
		out[b.ids[name]] = st
	}
	return out
}

// verify verifies the whole of one log with keys and the anchors the other
// logs keep of it, and fails the test unless it is intact.
func (b *backend) verify(t *testing.T, log string, keys []PublicKey) VerifyResult {
	t.Helper()
	return b.verifyAt(t, log, keys, time.Time{})
}

// verifyAt is verify made at the time now (zero for the clock's).
func (b *backend) verifyAt(t *testing.T, log string, keys []PublicKey, now time.Time) VerifyResult {
	t.Helper()
	r, err := b.verifyAll(t, log, keys, now)
	if err != nil {
		t.Fatalf("Verify(%s): %v", log, err)
	}
	if !r.Intact {
		t.Fatalf("%s: log %s is not intact: %+v", b.name, log, r.Problems)
	}
	return r
}

// verifyAll verifies the whole of one log with keys and the anchors the
// other logs keep of it, at the time now (zero for the clock's), and
// returns what it found, intact or not.
func (b *backend) verifyAll(t *testing.T, log string, keys []PublicKey, now time.Time) (VerifyResult, error) {
	t.Helper()
	return Verify(context.Background(), b.logs()[log], VerifyOptions{Installation: testInstallation, Log: log, Keys: keys, Anchors: b.anchorsOf(t, log), Now: now})
}

// tamper runs statements on one log's database directly, as someone with
// the database alone (no key file) would: SQLite through its file, Postgres
// through a connection of its own to the log's schema.
func (b *backend) tamper(t *testing.T, log string, statements ...string) {
	t.Helper()
	if b.name == "sqlite" {
		tamper(t, b.where[log], statements...)
		return
	}
	testkit.PostgresExec(t, b.where[log], statements...)
}

// anchorsOf returns the anchors the other logs keep of log.
func (b *backend) anchorsOf(t *testing.T, log string) []master.AuditAnchor {
	t.Helper()
	var out []master.AuditAnchor
	for name, st := range b.logs() {
		if name == log {
			continue
		}
		as, err := st.AuditAnchors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Log == log {
				out = append(out, a)
			}
		}
	}
	return out
}

// sha returns SHA-256(prev || b), one step of the chain written out.
func sha(prev [32]byte, b []byte) [32]byte {
	return sha256.Sum256(append(prev[:], b...))
}

// latestOf returns a log's latest checkpoint.
func latestOf(t *testing.T, st master.AuditSealReads) master.AuditCheckpoint {
	t.Helper()
	c, ok, err := latest(context.Background(), st)
	if err != nil || !ok {
		t.Fatalf("latest checkpoint: %v, %v", ok, err)
	}
	return c
}
