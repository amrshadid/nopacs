package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files the tests archive.
const corpusDir = "../receive/testdata"

// studyFiles are files of four patients: a CT study in two series, and
// three studies of their own.
var studyFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "MR_small.dcm", "liver_1frame.dcm",
	"SC_rgb_rle_2frame.dcm"}

// tenantID is the id of the rig's one tenant.
const tenantID = "T0000000000001"

// fastClock runs fastClockRate times faster than the machine's, so a
// receiver's quiet period passes at once.
type fastClock struct{ start time.Time }

// fastClockRate is how much faster the clock runs.
const fastClockRate = 20

// Now answers the clock's time.
func (c fastClock) Now() time.Time { return c.start.Add(time.Since(c.start) * fastClockRate) }

// After answers a channel after d of the clock's time.
func (c fastClock) After(d time.Duration) <-chan time.Time { return time.After(d / fastClockRate) }

// rig is an installation in a data directory: its key file, its master
// (SQLite in the directory, or a Postgres schema), one tenant "lab" with a
// directory storage "main" under the root "archive", a receiver, two
// profiles' versions, two keys with grants.
type rig struct {
	t        testing.TB
	ctx      context.Context
	postgres bool
	dataDir  string
	keyPath  string
	keys     *secret.Keys
	id       string
	// masterAddr and tenantAddr are the databases' addresses; tenantAddr is
	// empty on SQLite.
	masterAddr, tenantAddr string
	m                      master.Store
	st                     tenant.Store
	p                      tenant.Pipeline
	// root is the storage root's directory; the tenant's storage is
	// root/lab.
	root    string
	storage blob.Store
	clock   fastClock
	seq     atomic.Int64
}

// newRig makes the installation, on Postgres when postgres is set
// (skipping when there is none).
func newRig(t testing.TB, postgres bool) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{t: t, ctx: context.Background(), postgres: postgres, dataDir: filepath.Join(base, "data"), clock: fastClock{start: time.Now()}}
	if err := server.EnsurePrivateDir(r.dataDir); err != nil {
		t.Fatal(err)
	}
	r.keyPath = filepath.Join(r.dataDir, "key")
	if err := secret.CreateKeyFile(r.keyPath); err != nil {
		t.Fatal(err)
	}
	var err error
	if r.keys, err = secret.LoadKeyFile(r.keyPath); err != nil {
		t.Fatal(err)
	}
	r.masterAddr = driver.SQLiteScheme + filepath.Join(r.dataDir, "master.db")
	if postgres {
		var ok bool
		if r.masterAddr, ok = testkit.PostgresSchema(t); !ok {
			t.Skip("no Postgres to back up an installation on")
		}
		r.tenantAddr, _ = testkit.PostgresSchema(t)
	}
	r.openMaster()
	r.id = server.NewID()
	if err := r.m.SetSetting(r.ctx, server.InstallationIDSetting, r.id, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	check, err := server.SealKeyCheck(r.keys, r.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.SetSetting(r.ctx, server.KeyCheckSetting, check, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	if err := server.WriteInstallationID(r.dataDir, r.id); err != nil {
		t.Fatal(err)
	}
	r.root = filepath.Join(base, "roots")
	if err := os.MkdirAll(filepath.Join(r.root, "lab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.m.SetSetting(r.ctx, settings.StorageDirectoryRoots, "archive="+r.root, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	drv := master.SQLiteDriver
	if postgres {
		drv = master.PostgresDriver
	}
	sealed, err := r.keys.Seal(registry.AddressContext(tenantID), []byte(r.tenantAddr))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.m.CreateTenant(r.ctx, master.Tenant{ID: tenantID, Name: "lab", Driver: drv, SealedAddress: sealed, State: master.StateReady,
		Created: time.Now().UTC()}, r.masterAudit(), master.Server()); err != nil {
		t.Fatal(err)
	}
	r.openTenant()
	r.configure()
	ds, err := dir.Open(filepath.Join(r.root, "lab"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	r.storage = ds
	return r
}

// openMaster opens and migrates the master.
func (r *rig) openMaster() {
	r.t.Helper()
	m, err := driver.OpenMaster(r.ctx, r.masterAddr)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	r.m = m
}

// openTenant opens and migrates the tenant's database.
func (r *rig) openTenant() {
	r.t.Helper()
	row, err := r.m.Tenant(r.ctx, tenantID)
	if err != nil {
		r.t.Fatal(err)
	}
	st, err := OpenTenant(r.ctx, r.keys, r.dataDir, row)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	r.st, r.p = st, st.(tenant.Pipeline)
}

// configure gives the tenant its storage, receiver, profiles and a
// setting, and the installation two keys with grants.
func (r *rig) configure() {
	r.t.Helper()
	now := time.Now()
	if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "archive"}},
		now, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.p.AddReceiver(r.ctx, tenant.Receiver{Name: "dimse", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, now, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	profiles, ok := r.st.(tenant.Profiles)
	if !ok {
		r.t.Fatal("the tenant's store keeps no profiles")
	}
	for i, name := range []string{"research", "research", "export"} {
		expanded := []byte(fmt.Sprintf("profile %s text %d", name, i))
		sum := sha256.Sum256(expanded)
		if _, _, err := profiles.CreateProfileVersion(r.ctx, tenant.NewProfileVersion{Name: name, Source: []byte("source of " + name),
			Expanded: expanded, Checksum: hex.EncodeToString(sum[:]), Note: "made by the test"}, now, r.audit(), nil); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.st.SetSetting(r.ctx, settings.EventsRetention, "90d", r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	for i, holder := range []string{"pipeline robot", "an admin"} {
		k := master.Key{ID: fmt.Sprintf("K%011d", i+1), Hash: bytes.Repeat([]byte{byte(i + 1)}, 32), Holder: holder, Created: now.UTC()}
		if err := r.m.CreateKey(r.ctx, k, r.masterAudit(), master.Server()); err != nil {
			r.t.Fatal(err)
		}
	}
	if _, err := r.m.SetGrant(r.ctx, master.Grant{KeyID: "K00000000001", Scope: master.TenantScope(tenantID), Role: master.RoleWriter},
		r.masterAudit(), master.Server()); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.m.SetGrant(r.ctx, master.Grant{KeyID: "K00000000002", Role: master.RoleAdmin}, r.masterAudit(), master.Server()); err != nil {
		r.t.Fatal(err)
	}
}

// audit returns a new audit entry for a change of the tenant's.
func (r *rig) audit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("a%08d", r.seq.Add(1)), Time: time.Now(), LocalUser: "tester", Method: "Test", TenantName: "lab",
		Outcome: master.OutcomeDone}
}

// masterAudit returns a new audit entry for a change of the master's.
func (r *rig) masterAudit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("m%08d", r.seq.Add(1)), Time: time.Now(), LocalUser: "tester", Method: "Test",
		Outcome: master.OutcomeDone}
}

// archive runs the corpus files named through the real pipeline and waits
// until every reception has ended.
func (r *rig) archive(names ...string) {
	r.t.Helper()
	var bodies [][]byte
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(corpusDir, n)) // #nosec G304 -- a test's own file names.
		if err != nil {
			r.t.Fatal(err)
		}
		bodies = append(bodies, b)
	}
	r.arrive(bodies...)
}

// arrive runs each body through the real pipeline and waits until every
// reception has ended.
func (r *rig) arrive(bodies ...[]byte) {
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
	for i, b := range bodies {
		out := p.Accept(r.ctx, receive.Arrival{Receiver: "dimse", Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
		if out.Status != receive.Received {
			r.t.Fatalf("arrival %d: outcome %v (%s)", i, out.Status, out.Reason)
		}
	}
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

// engine returns a corrections engine over the rig, with a grace period of
// two days.
func (r *rig) engine() *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: tenantID, Keys: r.keys, Store: r.p, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		Grace: func(context.Context) (time.Duration, error) { return 48 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// subjects returns the catalog's subjects' labels, in order.
func (r *rig) subjects(p tenant.Pipeline) []string {
	r.t.Helper()
	page, err := p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, s := range page.Items {
		if len(out) == 0 || out[len(out)-1] != s.SubjectLabel {
			out = append(out, s.SubjectLabel)
		}
	}
	return out
}

// deleteSubject previews and deletes a subject, in its grace period.
func (r *rig) deleteSubject(e *correct.Engine, label string) tenant.Correction {
	r.t.Helper()
	target := tenant.CorrectionTarget{Subject: label}
	p, err := r.p.PreviewDelete(r.ctx, target, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// backup writes a backup of the rig's installation to a file and returns
// its path and summary.
func (r *rig) backup() (string, Summary) {
	r.t.Helper()
	dbs, closeAll, err := OpenTenants(r.ctx, r.m, r.keys, r.dataDir)
	if err != nil {
		r.t.Fatal(err)
	}
	defer closeAll()
	path := filepath.Join(filepath.Dir(r.dataDir), "backup.nopacs")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- the test's own directory.
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sum, err := Create(r.ctx, CreateConfig{Installation: r.id, Keys: r.keys, Master: r.m.(snapshot.Database), Tenants: dbs, Out: f})
	if err != nil {
		r.t.Fatal(err)
	}
	return path, sum
}

// schemaOf returns the search_path of a Postgres address.
func schemaOf(t testing.TB, address string) string {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("search_path")
}

// emptyPostgres drops and makes again the rig's two Postgres schemas, so a
// restore finds the empty databases the master names.
func (r *rig) emptyPostgres() {
	r.t.Helper()
	for _, a := range []string{r.masterAddr, r.tenantAddr} {
		s := schemaOf(r.t, a)
		testkit.PostgresExec(r.t, a, "DROP SCHEMA "+s+" CASCADE", "CREATE SCHEMA "+s)
	}
}
