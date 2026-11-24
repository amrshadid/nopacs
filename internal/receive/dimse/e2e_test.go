package dimse_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// manualClock is a pipeline clock a test moves by hand; its waits are real
// and short, so a worker polls without the test sleeping on a result.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	return time.After(max(d/1000, time.Millisecond))
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// archive is a tenant on the real SQLite driver with a directory storage,
// a receiver row and a running reception pipeline.
type archive struct {
	t       *testing.T
	ctx     context.Context
	dataDir string
	store   tenant.Pipeline
	storage blob.Store
	clock   *manualClock
	p       *receive.Pipeline
	row     tenant.Receiver
}

// newArchive makes the tenant, its storage and r, and starts a pipeline;
// each of mods may change the tenant and the pipeline's configuration
// before it starts.
func newArchive(t *testing.T, r tenant.Receiver, mods ...func(*archive, *receive.Config)) *archive {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the reception pipeline on a database; left out under -short")
	}
	a := &archive{t: t, ctx: context.Background(), dataDir: t.TempDir(), clock: &manualClock{now: t0}, row: r}
	s, err := (driver.SQLiteTenants{DataDir: a.dataDir}).Open(a.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(a.ctx); err != nil {
		t.Fatal(err)
	}
	a.store = s.(tenant.Pipeline)
	storeDir := filepath.Join(a.dataDir, "storage")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ds, err := dir.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	a.storage = ds
	if _, err := a.store.AddStorage(a.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, t0, a.audit("s"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.AddReceiver(a.ctx, r, t0, a.audit("r"), nil); err != nil {
		t.Fatal(err)
	}
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{7}, naming.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	cfg := receive.Config{
		Tenant: "lab", Store: a.store, Hasher: hasher, Clock: a.clock,
		OpenStorage:  func(context.Context, string) (blob.Store, error) { return a.storage, nil },
		IncomingRoot: filepath.Join(a.dataDir, "incoming"), Workers: 2, Lease: 30 * time.Second,
		PollInterval: 2 * time.Second, BatchInterval: 5 * time.Second, RetryInterval: 20 * time.Second,
	}
	for _, m := range mods {
		m(a, &cfg)
	}
	p, err := receive.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(a.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	a.p = p
	return a
}

// audit returns an audit entry for a change the test makes.
func (a *archive) audit(id string) master.AuditEntry {
	return master.AuditEntry{ID: "a-" + id, Time: t0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
		Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r-" + id}
}

// receptions lists the tenant's receptions.
func (a *archive) receptions() []tenant.Reception {
	a.t.Helper()
	rs, _, err := a.store.ListReceptions(a.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 100})
	if err != nil {
		a.t.Fatal(err)
	}
	return rs
}

// settle moves the pipeline's clock until n receptions exist and all ended.
func (a *archive) settle(n int) []tenant.Reception {
	a.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rs := a.receptions()
		ended := len(rs) >= n
		for _, r := range rs {
			ended = ended && r.State.Ended()
		}
		if ended {
			return rs
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("timed out waiting for %d receptions to end: %+v", n, rs)
		}
		a.clock.advance(3 * time.Second)
		time.Sleep(20 * time.Millisecond)
	}
}

// incomingFiles counts the regular files in the tenant's incoming area.
func (a *archive) incomingFiles() int {
	a.t.Helper()
	n := 0
	root := filepath.Join(a.dataDir, "incoming")
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		a.t.Fatal(err)
	}
	return n
}

// archived returns every file on the storage by SOP Instance UID, checking
// each object against its manifest entry's size and SHA-256.
func (a *archive) archived(r tenant.Reception) map[string][]byte {
	a.t.Helper()
	scans, _, err := a.store.ReceptionScans(a.ctx, r.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, sc := range scans {
		mk, err := layout.ManifestKey(label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label})
		if err != nil {
			a.t.Fatal(err)
		}
		m, err := manifest.Decode(bytes.NewReader(a.read(mk)))
		if err != nil {
			a.t.Fatal(err)
		}
		for _, f := range m.Files {
			obj := a.read(f.Key)
			s := sha256.Sum256(obj)
			if hex.EncodeToString(s[:]) != f.SHA256 || int64(len(obj)) != f.Size {
				a.t.Errorf("the object %s does not match its manifest entry", f.Key)
			}
			out[f.SOPInstanceUID] = obj
		}
	}
	return out
}

// read reads a whole object from the storage.
func (a *archive) read(key string) []byte {
	a.t.Helper()
	rc, err := a.storage.Read(a.ctx, key, 0, -1)
	if err != nil {
		a.t.Fatalf("read %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		a.t.Fatal(err)
	}
	return b
}

// uidOf returns a file's SOP Instance UID, read with the pipeline's own
// header reader.
func uidOf(t *testing.T, b []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := h.UID(header.SOPInstanceUID)
	return u
}

// TestAStudyArrivesArchivedAsSent guards the listener and the pipeline
// together on the real SQLite driver and a directory storage: a study sent
// by pynetdicom from its files ends done, and every archived object holds
// the data set sent, byte for byte, so its SHA-256 is the sent data set's;
// the whole object's SHA-256 is what its manifest records.
func TestAStudyArrivesArchivedAsSent(t *testing.T) {
	py := python(t)
	r := row()
	a := newArchive(t, r)
	s := listen(t, r, a.p, nil)
	paths := make([]string, len(studyFiles))
	for i, f := range studyFiles {
		paths[i] = corpusPath(f)
	}
	res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", paths...)
	for i, st := range res.Statuses {
		if !isStatus(st, statusSuccess) {
			t.Fatalf("%s answered %v", studyFiles[i], st)
		}
	}
	rs := a.settle(1)
	if len(rs) != 1 || rs[0].State != reception.Done || rs[0].Counts.FilesArchived != int64(len(studyFiles)) {
		t.Fatalf("receptions %+v", rs)
	}
	got := a.archived(rs[0])
	for _, f := range studyFiles {
		sent := corpusBytes(t, f)
		obj, ok := got[uidOf(t, sent)]
		if !ok {
			t.Fatalf("%s is not on the storage", f)
		}
		if sum(dataSet(t, obj)) != sum(dataSet(t, sent)) {
			t.Errorf("%s: the archived data set's SHA-256 is not the sent one's", f)
		}
	}
	if n := a.incomingFiles(); n != 0 {
		t.Fatalf("the incoming area holds %d files after the reception is done", n)
	}
}

// TestOverSizeLeavesNothingInTheIncomingArea guards the size bound with the
// real pipeline: a file past the receiver's largest is answered out of
// resources, held in quarantine as too large without its bytes, and the
// incoming area holds nothing.
func TestOverSizeLeavesNothingInTheIncomingArea(t *testing.T) {
	py := python(t)
	r := row(func(r *tenant.Receiver) { r.MaxFileBytes = 6000 })
	a := newArchive(t, r)
	s := listen(t, r, a.p, nil)
	res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
	if len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusOutOfResources) {
		t.Fatalf("%+v, want A700", res)
	}
	qs, _, err := a.store.ListQuarantined(a.ctx, tenant.QuarantineFilter{}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineTooLarge || qs[0].Caller.CallingAETitle != "SCANNER" {
		t.Fatalf("quarantine %+v", qs)
	}
	if n := a.incomingFiles(); n != 0 {
		t.Fatalf("the incoming area holds %d files", n)
	}
	if n := a.p.Stats().Landed.Load(); n != 0 {
		t.Fatalf("%d files landed", n)
	}
}

// watchedSink hands files to a Sink and tells a test once a body has
// passed a number of bytes, then records each outcome.
type watchedSink struct {
	next     receive.Sink
	at       int64
	reached  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	outcomes []receive.Outcome
}

// Accept wraps a's body and hands it on.
func (w *watchedSink) Accept(ctx context.Context, a receive.Arrival) receive.Outcome {
	a.Body = &countingReader{r: a.Body, at: w.at, reached: func() { w.once.Do(func() { close(w.reached) }) }}
	out := w.next.Accept(ctx, a)
	w.mu.Lock()
	w.outcomes = append(w.outcomes, out)
	w.mu.Unlock()
	return out
}

func (w *watchedSink) got() []receive.Outcome {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]receive.Outcome(nil), w.outcomes...)
}

// countingReader calls reached once its reads pass at bytes.
type countingReader struct {
	r       io.Reader
	n, at   int64
	reached func()
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n >= c.at {
		c.reached()
	}
	return n, err
}

// TestAKilledSenderLeavesNothingCounted guards a data set cut short: dcmtk's
// storescu, sending a large file, is killed once part of it has reached the
// pipeline; the store is not answered success, nothing landed, nothing is
// recorded, the incoming area holds no file, and the listener goes on.
func TestAKilledSenderLeavesNothingCounted(t *testing.T) {
	py := python(t)
	storescu := tool(t, "storescu")
	r := row()
	a := newArchive(t, r)
	w := &watchedSink{next: a.p, at: 4 << 20, reached: make(chan struct{})}
	s := listen(t, r, w, nil)
	big := writeLarge(t, 64<<20)
	cmd := command(t, storescu, "-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port, big) // #nosec G204 -- the test's own arguments.
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.reached:
	case <-time.After(time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("the large file never reached the pipeline")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitFor(t, "the cut store to be answered by the pipeline", func() bool { return len(w.got()) == 1 })
	if out := w.got()[0]; out.Status == receive.Received {
		t.Fatalf("a store cut short was received: %+v", out)
	}
	if n := a.p.Stats().Landed.Load(); n != 0 {
		t.Fatalf("%d files landed", n)
	}
	if n := a.incomingFiles(); n != 0 {
		t.Fatalf("the incoming area holds %d files", n)
	}
	if rs := a.receptions(); len(rs) != 0 {
		t.Fatalf("receptions %+v", rs)
	}
	if res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE"); res.Assoc != "accepted" {
		t.Fatalf("after a killed sender: %+v", res)
	}
}

// writeLarge writes a valid DICOM file of about size bytes to a temporary
// file: MR_small.dcm's header with its pixel data made size bytes of zeros
// (generator), and returns its path.
func writeLarge(t *testing.T, size int64) string {
	t.Helper()
	g := newGenerator(t)
	path := filepath.Join(t.TempDir(), fmt.Sprintf("large-%d.dcm", size))
	f, err := os.Create(path) // #nosec G304 -- a test's temporary file.
	if err != nil {
		t.Fatal(err)
	}
	r, _ := g.File(0, size)
	if _, err := io.Copy(f, r); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
