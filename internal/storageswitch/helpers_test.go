package storageswitch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The fixture's tenant and installation ids.
const (
	testTenantID     = "T0000000000001"
	testInstallation = "inst-test-1"
)

// db is what the fixture uses of the tenant's database: the Switcher's
// port and the rest of the pipeline's.
type db interface {
	tenant.Pipeline
	DB
}

// fixture is a tenant's SQLite database with the directory storages "main"
// (a primary with a receiver) and "copy", each place a directory of its
// own under the test's data directory, named by its row's path; the
// installation's key file; and, once started, the replicator.
type fixture struct {
	t     testing.TB
	ctx   context.Context
	d     string
	p     db
	keys  *secret.Keys
	seq   atomic.Int64
	scans map[string]*scanInfo

	mu     sync.Mutex
	places map[string]blob.Store
	worker *replicate.Worker
}

// scanInfo is what the fixture archived of one scan.
type scanInfo struct {
	path          label.Path
	study, series string
	lines         []manifest.File
}

// newFixture makes the tenant, its storages and receiver, and the key file.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), d: t.TempDir(), scans: map[string]*scanInfo{}, places: map[string]blob.Store{}}
	s, err := (driver.SQLiteTenants{DataDir: f.d}).Open(f.ctx, testTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, ok := s.(db)
	if !ok {
		t.Fatal("the SQLite tenant store does not implement the Switcher's port")
	}
	f.p = p
	at := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"main", "copy"} {
		if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
			Directory: tenant.DirectoryStorage{Root: "archive", Path: name}}, at, f.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(t.TempDir(), "key")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keyDir, "nopacs.key")
	if err := secret.CreateKeyFile(path); err != nil {
		t.Fatal(err)
	}
	if f.keys, err = secret.LoadKeyFile(path); err != nil {
		t.Fatal(err)
	}
	return f
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

// placeAt returns the store of the directory a row's path names, opened
// once and kept open: two names may come to name it in turn.
func (f *fixture) placeAt(path string) (blob.Store, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.places[path]; ok {
		return s, nil
	}
	root := filepath.Join(f.d, "places", path)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s, err := dir.Open(root)
	if err != nil {
		return nil, err
	}
	f.t.Cleanup(func() { _ = s.Close() })
	f.places[path] = s
	return s, nil
}

// openRow opens the place a storage's row names; its Close does nothing.
func (f *fixture) openRow(_ context.Context, row tenant.Storage) (blob.Store, error) {
	s, err := f.placeAt(row.Directory.Path)
	if err != nil {
		return nil, err
	}
	return keepOpen{s}, nil
}

// openName opens the place the storage named names now, as the server's
// opener does: the row read again at each open.
func (f *fixture) openName(ctx context.Context, name string) (blob.Store, error) {
	row, err := f.p.Storage(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.openRow(ctx, row)
}

// place returns the store of the directory named path.
func (f *fixture) place(path string) blob.Store {
	f.t.Helper()
	s, err := f.placeAt(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// keepOpen is a store whose Close does nothing: the fixture closes it.
type keepOpen struct{ blob.Store }

// Close keeps the store open.
func (keepOpen) Close() error { return nil }

// fileOf returns the bytes of the synthetic instance k of scan n.
func fileOf(n, k int) []byte {
	return fmt.Appendf(nil, "synthetic instance %d of scan %d\n%s", k, n, bytes.Repeat([]byte{"abcdefgh"[(n*31+k)%8]}, 512+k))
}

// uidOf returns the SOP Instance UID of instance k of scan n.
func uidOf(n, k int) string { return fmt.Sprintf("1.2.826.0.1.3680043.2.1125.7.%d.%d", n, k) }

// archive archives count instances into the scan labeled scan (numbered n)
// on "main" as a reception does: each file put on the place "main" names,
// its sidecar and manifest written, the catalog rows committed.
func (f *fixture) archive(n int, scan string, count int) {
	f.t.Helper()
	f.stage(n, scan, count)()
}

// stage is archive up to the commit, which it returns: the files, the
// sidecar and the manifest are on the place "main" names now.
func (f *fixture) stage(n int, scan string, count int) (commit func()) {
	f.t.Helper()
	t := f.t
	si := &scanInfo{path: label.Path{Subject: "sub-01", Session: "ses-" + scan, Scan: scan},
		study: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.8.%d", n), series: fmt.Sprintf("1.2.826.0.1.3680043.2.1125.9.%d", n)}
	store, err := f.openName(f.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	r, _, err := f.p.OpenReception(f.ctx, "recv", si.study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	var received []tenant.ReceivedFile
	for k := range count {
		b := fileOf(n, k)
		received = append(received, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: uidOf(n, k), SeriesUID: si.series,
			StudyUID: si.study, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: int64(len(b)),
			SHA256: tenant.SHA256(sha256.Sum256(b)), Incoming: r.ID + "/" + uidOf(n, k) + ".dcm", Association: "assoc1",
			Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}})
	}
	if _, err := f.p.AddReceivedFiles(f.ctx, received, at, nil); err != nil {
		t.Fatal(err)
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = si.path.Subject, si.path.Session, "main"
		}
		if r, err = f.p.StepReception(f.ctx, st, nil); err != nil {
			t.Fatalf("step %s: %v", ev, err)
		}
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(received))
	for i, fl := range received {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: fl.SOPInstanceUID, StudyUID: fl.StudyUID, SeriesUID: fl.SeriesUID, SHA256: fl.SHA256}
	}
	if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims, r.StepTime, nil); err != nil {
		t.Fatal(err)
	}
	var confirmed []tenant.ConfirmedInstance
	for k := range count {
		key, err := layout.InstanceKey(si.path, uidOf(n, k))
		if err != nil {
			t.Fatal(err)
		}
		w, err := store.Put(f.ctx, key, bytes.NewReader(fileOf(n, k)))
		if err != nil {
			t.Fatal(err)
		}
		sum := tenant.SHA256(w.SHA256)
		confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: uidOf(n, k), Key: key, Length: w.Size, Size: w.Size,
			ArchivedSHA256: sum})
		si.lines = append(si.lines, manifest.File{Key: key, Length: w.Size, Size: w.Size, SHA256: sum.String(), SOPInstanceUID: uidOf(n, k)})
	}
	put := func(key string, b []byte) {
		t.Helper()
		if _, err := store.Put(f.ctx, key, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	put(mustKey(layout.MetadataKey(si.path)), fmt.Appendf(nil, "sidecar of %s with %d files\n", scan, count))
	m, err := manifest.Encode(manifest.Manifest{Files: si.lines})
	if err != nil {
		t.Fatal(err)
	}
	put(mustKey(layout.ManifestKey(si.path)), m)
	sum := tenant.SHA256(sha256.Sum256(m))
	return func() {
		t.Helper()
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: si.study, SeriesUID: si.series, SubjectLabel: si.path.Subject,
			SessionLabel: si.path.Session, ScanLabel: si.path.Scan, Storage: "main", ManifestSHA256: sum, At: r.StepTime, Instances: confirmed}
		if _, err := f.p.CommitScan(f.ctx, c); err != nil {
			t.Fatal(err)
		}
		step(reception.Identified, reception.Archive)
		step(reception.Archived, reception.Verify)
		step(reception.Verified, reception.Finish)
		f.mu.Lock()
		f.scans[scan] = si
		f.mu.Unlock()
		if f.worker != nil {
			f.worker.Wake()
		}
	}
}

// mustKey returns a key the layout made, panicking on an error the
// fixture's own labels never give.
func mustKey(k string, err error) string {
	if err != nil {
		panic(err)
	}
	return k
}

// attach makes "copy" a replica of "main", queuing every scan.
func (f *fixture) attach() {
	f.t.Helper()
	if _, _, _, err := f.p.AttachReplica(f.ctx, "copy", "main", time.Now(), f.audit(), nil); err != nil {
		f.t.Fatal(err)
	}
}

// replicate starts the replicator over the fixture, stopped at the test's
// end.
func (f *fixture) replicate() {
	f.t.Helper()
	w, err := replicate.New(replicate.Config{Queue: f.p, Catalog: f.p, Now: time.Now, Owner: "replicator-test", Parallel: 2, Scans: 2,
		Idle: 20 * time.Millisecond, RetryMin: 20 * time.Millisecond, RetryMax: 100 * time.Millisecond, ChangedDelay: 10 * time.Millisecond,
		Open: f.openName})
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	f.t.Cleanup(func() {
		cancel()
		<-done
	})
	f.mu.Lock()
	f.worker = w
	f.mu.Unlock()
}

// inStep waits, up to ten seconds, until every scan of "main" is in step on
// "copy".
func (f *fixture) inStep() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		facts, err := f.p.SwitchFacts(f.ctx, "main", "copy")
		if err != nil {
			f.t.Fatal(err)
		}
		if facts.ScansNotInStep == 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the replica is not in step: %d scans", facts.ScansNotInStep)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// config returns a Switcher's configuration over the fixture.
func (f *fixture) config() Config {
	f.t.Helper()
	key, err := f.keys.Derive(secret.ConfirmToken)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg := Config{DB: f.p, Tenant: place.TenantOf(testTenantID, "lab", f.p), Installation: testInstallation, OwnerKeys: f.keys,
		OpenPlace: f.openRow, ConfirmKey: key, Poll: 20 * time.Millisecond, GateWait: -1, GateRowTTL: 10 * time.Millisecond}
	cfg.Reseal = func(ctx context.Context, primary, replica tenant.Storage) ([]byte, []byte, error) {
		return place.SwapSecrets(ctx, f.keys, cfg.Tenant, primary, replica)
	}
	// The replicator a test starts later is woken too.
	cfg.Wake = func() {
		f.mu.Lock()
		w := f.worker
		f.mu.Unlock()
		if w != nil {
			w.Wake()
		}
	}
	return cfg
}

// switcher returns a Switcher over cfg, recovered and stopped at the
// test's end.
func (f *fixture) switcher(cfg Config) *Switcher {
	f.t.Helper()
	s, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.Recover(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = s.Stop(context.Background()) })
	return s
}

// wait waits for the switch's end, up to thirty seconds.
func (f *fixture) wait(s *Switcher, id string) tenant.StorageSwitch {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	sw, err := s.Wait(ctx, id)
	if err != nil {
		f.t.Fatalf("waiting for the switch: %v", err)
	}
	return sw
}

// catalog returns every instance the catalog holds, as rows, of every scan
// archived, in order.
func (f *fixture) catalog() []tenant.ScanInstance {
	f.t.Helper()
	f.mu.Lock()
	names := make([]string, 0, len(f.scans))
	for name := range f.scans {
		names = append(names, name)
	}
	f.mu.Unlock()
	slices.Sort(names)
	var out []tenant.ScanInstance
	for _, name := range names {
		rows, _, err := f.p.ListHeldInstances(f.ctx, f.scans[name].path, master.Page{Limit: master.MaxPageLimit})
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, rows...)
	}
	return out
}

// readAll returns the SHA-256 of the object key on s.
func (f *fixture) readSum(s blob.Store, key string) (tenant.SHA256, error) {
	rc, err := s.Read(f.ctx, key, 0, -1)
	if err != nil {
		return tenant.SHA256{}, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return tenant.SHA256{}, err
	}
	return tenant.SHA256(h.Sum(nil)), nil
}

// wantServed fails the test unless every instance the catalog holds reads,
// from the place "main" names now, with the SHA-256 it was archived with.
func (f *fixture) wantServed(rows []tenant.ScanInstance) {
	f.t.Helper()
	store, err := f.openName(f.ctx, "main")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, in := range rows {
		got, err := f.readSum(store, in.Key)
		if err != nil || got != in.ArchivedSHA256 {
			f.t.Errorf("the instance at %s reads %v (%v) from the primary's place, want %v", in.Key, got, err, in.ArchivedSHA256)
		}
	}
}

// preview returns a preview of the switch of "main" to "copy".
func (f *fixture) preview(s *Switcher, skip bool) Preview {
	f.t.Helper()
	p, err := s.Validate(f.ctx, "main", "copy", skip)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// request returns a request to switch "main" to "copy" with token, passing
// the checks of the replica over.
func (f *fixture) request(token string) Request {
	return Request{Primary: "main", Replica: "copy", Confirm: token, SkipVerification: true, SkipReason: "checked by hand",
		DrainTimeout: 10 * time.Second, Audit: f.audit()}
}
