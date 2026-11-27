package send

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test files this package sends.
const corpusDir = "../receive/testdata"

// corpus reads a public test file.
func corpus(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, name)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// generated returns n distinct instances made from MR_small.dcm with
// pixelBytes of pixel data each (testkit.Generator).
func generated(t testing.TB, n int, pixelBytes int64) [][]byte {
	t.Helper()
	g := testkit.NewGenerator(t, corpus(t, "MR_small.dcm"))
	out := make([][]byte, n)
	for i := range n {
		r, _ := g.File(i, pixelBytes)
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// clock is a test's clock: the machine's, moved forward by what the test
// adds, so a unit's wait between attempts passes at once.
type clock struct{ off atomic.Int64 }

// Now returns the time.
func (c *clock) Now() time.Time { return time.Now().UTC().Add(time.Duration(c.off.Load())) }

// add moves the clock forward.
func (c *clock) add(d time.Duration) { c.off.Add(int64(d)) }

// installation is the installation's settings a test gives.
type installation struct {
	mu    sync.Mutex
	dests settings.Destinations
	plain bool
	max   int
}

// SenderDestinations answers sender.destinations.
func (i *installation) SenderDestinations(context.Context) (settings.Destinations, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.dests, nil
}

// AllowPlainHTTP answers sender.allow-plain-http.
func (i *installation) AllowPlainHTTP(context.Context) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.plain, nil
}

// MaxAssociations answers sender.max-associations.
func (i *installation) MaxAssociations(context.Context) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.max, nil
}

// tenantSettings is a tenant's settings a test gives.
type tenantSettings struct{ max, kept int }

// MaxAssociations answers sender.tenant-max-associations.
func (s tenantSettings) MaxAssociations(context.Context) (int, error) { return s.max, nil }

// FailuresKept answers job.failures-kept.
func (s tenantSettings) FailuresKept(context.Context) (int, error) { return s.kept, nil }

// oneTenant is the installation of one tenant, "lab".
type oneTenant struct {
	p        tenant.Pipeline
	notReady atomic.Bool
	// gone makes the list of tenants empty, as after the tenant's removal.
	gone atomic.Bool
}

// Names answers the one tenant, or none once it is gone.
func (o *oneTenant) Names(context.Context) ([]string, error) {
	if o.gone.Load() {
		return nil, nil
	}
	return []string{"lab"}, nil
}

// Store answers its database, or an error while it is not ready.
func (o *oneTenant) Store(string) (tenant.Store, error) {
	if o.notReady.Load() {
		return nil, fault.New(fault.Unavailable, "the tenant is not ready")
	}
	return o.p, nil
}

// storages is the tenant's one storage, opened by the test, with no
// replica.
type storages struct{ s blob.Store }

// Open reads the object from the storage.
func (s storages) Open(ctx context.Context, obj checkedread.Object, offset, length int64) (checkedread.Opened, error) {
	if obj.Damaged {
		return checkedread.Opened{}, checkedread.ErrMarked
	}
	rc, err := s.s.Read(ctx, obj.Key, offset, length)
	return checkedread.Opened{Body: rc, Storage: obj.Storage}, err
}

// Mismatch records nothing: there is no replica.
func (storages) Mismatch(context.Context, checkedread.Opened, error) error { return nil }

// Close keeps it open: the test closes it.
func (storages) Close() {}

// fixture is a tenant's SQLite database with a directory storage, archived
// instances on it, and the parts of a supervisor over them.
type fixture struct {
	t     testing.TB
	ctx   context.Context
	dir   string
	p     tenant.Pipeline
	store blob.Store
	clock *clock
	inst  *installation
	ten   *oneTenant
	reads *checkedread.Slots
	seq   atomic.Int64
	// files are the archived files by SOP Instance UID.
	files map[string][]byte
	// keys are the storage's key of each, by SOP Instance UID.
	keys map[string]string
	// wakes counts the supervisor's wakes of the tenant's feed.
	wakes atomic.Int64
	// reception, study, series and scanOf are what the last archive made:
	// its reception and study, each scan's series by label, and each
	// instance's scan label by SOP Instance UID.
	reception, study string
	series, scanOf   map[string]string
	// provenance is how archive records each instance, by SOP Instance UID:
	// nil records every one de-identified by the fixture's profile version,
	// with no pixel risk accepted, the default rule lets out.
	provenance func(uid string) tenant.InstanceProvenance
}

// fixtureProfile is the profile version newFixture stores and archive
// records by default.
var fixtureProfile = tenant.ProfileRef{Name: "research", Version: 1}

// newFixture makes the tenant "lab" with a storage "main" and a receiver.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), dir: t.TempDir(), clock: &clock{},
		inst: &installation{max: 32, dests: mustDestinations(t, "127.0.0.0/8\n::1/128")}, reads: &checkedread.Slots{},
		files: map[string][]byte{}, keys: map[string]string{}, series: map[string]string{}, scanOf: map[string]string{}}
	s, err := (driver.SQLiteTenants{DataDir: f.dir}).Open(f.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
	f.ten = &oneTenant{p: f.p}
	root := filepath.Join(f.dir, "storage")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	f.store = ds
	now := f.clock.Now().Add(-48 * time.Hour)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, now, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, now, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	// The store keeps a version's texts as given and checks only the
	// checksum: the send reads no profile, only that one is recorded.
	expanded := []byte("expanded form of research")
	sum := sha256.Sum256(expanded)
	if _, _, err := f.p.(tenant.Profiles).CreateProfileVersion(f.ctx, tenant.NewProfileVersion{Name: fixtureProfile.Name,
		Source: []byte("source of research"), Expanded: expanded, Checksum: hex.EncodeToString(sum[:])}, now, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// mustDestinations reads a value of sender.destinations.
func mustDestinations(t testing.TB, v string) settings.Destinations {
	t.Helper()
	d, err := settings.ParseDestinations(v)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: f.clock.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

// uidOf returns the SOP Instance UID a file holds.
func uidOf(t testing.TB, b []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	u, ok := h.UID(header.SOPInstanceUID)
	if !ok {
		t.Fatal("a test file holds no SOP Instance UID")
	}
	return u
}

// scanFiles are the files of one scan to archive, under its label.
type scanFiles struct {
	label string
	files [][]byte
}

// archive archives the scans into a session of subject sub-01 through the
// tenant port, as a reception would, and puts each file on the storage
// under its key; the reception ends done an hour before the clock's now,
// so a job made now sends every instance.
func (f *fixture) archive(session string, scans ...scanFiles) {
	f.t.Helper()
	t := f.t
	at := f.clock.Now().Add(-time.Hour)
	study := "1.2.826.0.1.3680043.10.543." + strconv.FormatInt(f.seq.Add(1), 10)
	r, _, err := f.p.OpenReception(f.ctx, "recv", study, f.id("rc"), at)
	if err != nil {
		t.Fatal(err)
	}
	type held struct {
		scan int
		uid  string
		b    []byte
		sum  tenant.SHA256
	}
	var all []held
	var files []tenant.ReceivedFile
	f.reception, f.study = r.ID, study
	for si, sc := range scans {
		f.series[sc.label] = study + "." + strconv.Itoa(si+1)
		for _, b := range sc.files {
			u := uidOf(t, b)
			f.scanOf[u] = sc.label
			sum := tenant.SHA256(sha256.Sum256(b))
			all = append(all, held{scan: si, uid: u, b: b, sum: sum})
			files = append(files, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: u, SeriesUID: study + "." + strconv.Itoa(si+1),
				StudyUID: study, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
				Size: int64(len(b)), SHA256: sum, Incoming: r.ID + "/" + u + ".dcm",
				Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}, Association: "assoc1"})
		}
	}
	for i := 0; i < len(files); i += 500 {
		if _, err := f.p.AddReceivedFiles(f.ctx, files[i:min(i+500, len(files))], at); err != nil {
			t.Fatal(err)
		}
	}
	step := func(from reception.State, ev reception.Event) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Second)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", session, "main"
		}
		if r, err = f.p.StepReception(f.ctx, st, nil); err != nil {
			t.Fatalf("step %s: %v", ev, err)
		}
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(files))
	for i, fl := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: fl.SOPInstanceUID, StudyUID: fl.StudyUID, SeriesUID: fl.SeriesUID, SHA256: fl.SHA256}
	}
	for i := 0; i < len(claims); i += 500 {
		if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims[i:min(i+500, len(claims))], r.StepTime, nil); err != nil {
			t.Fatal(err)
		}
	}
	for si, sc := range scans {
		p := label.Path{Subject: "sub-01", Session: session, Scan: sc.label}
		var confirmed []tenant.ConfirmedInstance
		for _, h := range all {
			if h.scan != si {
				continue
			}
			key, err := layout.InstanceKey(p, h.uid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Put(f.ctx, key, bytes.NewReader(h.b)); err != nil {
				t.Fatal(err)
			}
			f.files[h.uid], f.keys[h.uid] = h.b, key
			prov := tenant.InstanceProvenance{Profile: fixtureProfile}
			if f.provenance != nil {
				prov = f.provenance(h.uid)
			}
			confirmed = append(confirmed, tenant.ConfirmedInstance{SOPInstanceUID: h.uid, Key: key, Length: int64(len(h.b)),
				Size: int64(len(h.b)), ArchivedSHA256: h.sum, Profile: prov.Profile, PixelRiskAccepted: prov.PixelRiskAccepted})
		}
		for i := 0; i < len(confirmed); i += 500 {
			c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: study, SeriesUID: study + "." + strconv.Itoa(si+1),
				SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "main",
				ManifestSHA256: tenant.SHA256(sha256.Sum256([]byte(p.Scan))), At: r.StepTime,
				Instances: confirmed[i:min(i+500, len(confirmed))]}
			if _, err := f.p.CommitScan(f.ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
}

// addDIMSE adds an enabled DIMSE sender named name to the peer at addr.
func (f *fixture) addDIMSE(name, addr string, mods ...func(*tenant.Sender)) tenant.Sender {
	f.t.Helper()
	ap := netip.MustParseAddrPort(addr)
	s := tenant.Sender{Name: name, Type: tenant.SenderDIMSE, Enabled: true, DIMSE: tenant.DIMSESender{
		Host: ap.Addr().String(), Port: int(ap.Port()), CalledAETitle: "PEER", CallingAETitle: "NOPACS"}}
	for _, m := range mods {
		m(&s)
	}
	out, err := f.p.AddSender(f.ctx, s, f.clock.Now(), f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// createJob makes a job sending the session to the sender.
func (f *fixture) createJob(sender, session string) tenant.Job {
	f.t.Helper()
	j, err := f.p.CreateJob(f.ctx, tenant.NewJob{ID: f.id("job"), Sender: sender,
		Target: tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: session}, Created: f.clock.Now()}, f.audit(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// config returns the supervisor's configuration over the fixture.
func (f *fixture) config() Config {
	return Config{
		Tenants: f.ten, Installation: f.inst,
		TenantSettings: func(tenant.Store) TenantSettings { return tenantSettings{max: 8, kept: tenant.DefaultFailuresKept} },
		OpenStorages:   func(string, tenant.Pipeline) Storages { return storages{s: f.store} },
		Credential: func(_ context.Context, _, _ string, sealed []byte) (string, error) {
			return string(sealed), nil
		},
		Reads: f.reads, TLSDir: func(string) string { return filepath.Join(f.dir, "tls", "lab") },
		Wake: func(string) { f.wakes.Add(1) }, Now: f.clock.Now,
		Interval: time.Second, PollInterval: 20 * time.Millisecond,
		seams: seams{renew: 200 * time.Millisecond},
	}
}

// start starts a supervisor over the fixture, stopped at the test's end.
func (f *fixture) start(cfg Config) *Supervisor {
	f.t.Helper()
	s, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := s.Start(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	return s
}

// waitJob waits until the job is in a state ok accepts, poking the
// supervisor, and returns it; it fails the test after the bound.
func (f *fixture) waitJob(s *Supervisor, id string, bound time.Duration, ok func(tenant.Job) bool) tenant.Job {
	f.t.Helper()
	deadline := time.Now().Add(bound)
	for {
		j, err := f.p.Job(f.ctx, id)
		if err != nil {
			f.t.Fatal(err)
		}
		if ok(j) {
			return j
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s did not reach the state awaited in %s: %+v", id, bound, j)
		}
		if s != nil {
			s.Poke("lab")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ended accepts a job that has ended.
func ended(j tenant.Job) bool { return j.State.Ended() }

// failures returns every failure of a job, warnings included.
func (f *fixture) failures(id string) []tenant.JobFailure {
	f.t.Helper()
	out, _, err := f.p.ListJobFailures(f.ctx, id, true, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// sendUnits returns the units of the sender's jobs, by listing the work
// the store holds for them.
func (f *fixture) unit(id string) tenant.Work {
	f.t.Helper()
	w, err := f.p.Work(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// freePort returns a TCP address on the loopback nothing listens on.
func freePort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// stepOnEach waits until done holds, moving the clock forward by step each
// time count grows, and only then: a wait between attempts passes once an
// attempt has happened, never while a unit is held, so no lease runs out
// under a busy machine. It fails the test after bound.
func (f *fixture) stepOnEach(s *Supervisor, what string, bound, step time.Duration, count func() int, done func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(bound)
	seen := count()
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("%s did not happen within %s", what, bound)
		}
		if n := count(); n > seen {
			seen = n
			f.clock.add(step)
			s.Poke("lab")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// failedConnections counts the sender's connections in a row that failed.
func failedConnections(s *Supervisor, sender string) func() int {
	return func() int { h, _ := s.Health("lab", sender); return h.FailedConnections }
}
