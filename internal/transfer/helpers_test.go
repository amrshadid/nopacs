package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// corpusDir holds the public test file the instances are made from.
const corpusDir = "../receive/testdata"

// generated returns n distinct instances made from MR_small.dcm.
func generated(t testing.TB, n int) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, "MR_small.dcm")) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	g := testkit.NewGenerator(t, b)
	out := make([][]byte, n)
	for i := range n {
		r, _ := g.File(i, 256)
		if out[i], err = io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

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

// fixture is the source tenant: a SQLite database with a directory storage
// and archived instances on it, as a transfer reads them.
type fixture struct {
	t     testing.TB
	ctx   context.Context
	dir   string
	p     tenant.Pipeline
	store blob.Store
	seq   atomic.Int64
	// files are the archived files by SOP Instance UID, keys their storage keys.
	files map[string][]byte
	keys  map[string]string
	// now is the fixture's clock, which a test moves.
	now atomic.Int64
}

// profile is the profile version the fixture records its instances with.
var fixtureProfile = tenant.ProfileRef{Name: "research", Version: 1}

// newFixture makes the tenant "lab" with a storage "main" and a receiver.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), dir: t.TempDir(), files: map[string][]byte{}, keys: map[string]string{}}
	f.now.Store(time.Now().UTC().Truncate(time.Microsecond).UnixMicro())
	s, err := (driver.SQLiteTenants{DataDir: f.dir}).Open(f.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p = s.(tenant.Pipeline)
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
	at := f.clock().Add(-48 * time.Hour)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "archive"}},
		at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.AddReceiver(f.ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true}}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	// The store keeps a version's texts as given and checks only the checksum:
	// a transfer reads no profile, only that one is recorded.
	expanded := []byte("expanded form of research")
	sum := sha256.Sum256(expanded)
	if _, _, err := f.p.(tenant.Profiles).CreateProfileVersion(f.ctx, tenant.NewProfileVersion{Name: fixtureProfile.Name,
		Source: []byte("source of research"), Expanded: expanded, Checksum: hex.EncodeToString(sum[:])}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// clock is the fixture's time.
func (f *fixture) clock() time.Time { return time.UnixMicro(f.now.Load()).UTC() }

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: f.clock(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// id returns a new id with prefix.
func (f *fixture) id(prefix string) string { return fmt.Sprintf("%s%08d", prefix, f.seq.Add(1)) }

// scanFiles are the files of one scan to archive, under its label.
type scanFiles struct {
	label string
	files [][]byte
}

// archive archives the scans into a session of subject sub-01 through the
// tenant port, as a reception would, and puts each file on the storage under
// its key. asReceived archives them with no profile version.
func (f *fixture) archive(session string, asReceived bool, scans ...scanFiles) {
	f.t.Helper()
	t := f.t
	at := f.clock().Add(-time.Hour)
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
	for si, sc := range scans {
		for _, b := range sc.files {
			u := uidOf(t, b)
			sum := tenant.SHA256(sha256.Sum256(b))
			all = append(all, held{scan: si, uid: u, b: b, sum: sum})
			files = append(files, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: u, SeriesUID: study + "." + strconv.Itoa(si+1),
				StudyUID: study, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
				Size: int64(len(b)), SHA256: sum, Incoming: r.ID + "/" + u + ".dcm",
				Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}, Association: "assoc1"})
		}
	}
	if _, err := f.p.AddReceivedFiles(f.ctx, files, at, nil); err != nil {
		t.Fatal(err)
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
	if _, err := f.p.ClaimInstances(f.ctx, r.ID, claims, r.StepTime, nil); err != nil {
		t.Fatal(err)
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
			ci := tenant.ConfirmedInstance{SOPInstanceUID: h.uid, Key: key, Length: int64(len(h.b)), Size: int64(len(h.b)), ArchivedSHA256: h.sum}
			if !asReceived {
				ci.Profile = fixtureProfile
			}
			confirmed = append(confirmed, ci)
		}
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: study, SeriesUID: study + "." + strconv.Itoa(si+1), SubjectLabel: p.Subject,
			SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "main", ManifestSHA256: tenant.SHA256(sha256.Sum256([]byte(p.Scan))), At: r.StepTime,
			Instances: confirmed}
		if _, err := f.p.CommitScan(f.ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	step(reception.Identified, reception.Archive)
	step(reception.Archived, reception.Verify)
	step(reception.Verified, reception.Finish)
}

// source is the fixture as the runner reads it: the real store, with the
// corrections a move's fake deleter made answered from its own record.
type source struct {
	tenant.Pipeline
	*fixture
	mu          sync.Mutex
	corrections map[string]record.State
	// made are the corrections the fake deleter made, as ListCorrections
	// answers them; gone says the source's subject is deleted, as the
	// catalog then answers it (not found), which the fake deleter sets.
	made []tenant.Correction
	gone atomic.Bool
	// failRecord makes the next RecordTransferDelete fail, as a stop between
	// a delete and its record does.
	failRecord atomic.Bool
}

// ListCorrections answers the corrections the fake deleter made.
func (s *source) ListCorrections(_ context.Context, _ tenant.CorrectionFilter, _ master.Page) ([]tenant.Correction, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tenant.Correction(nil), s.made...), "", nil
}

// RecordTransferDelete fails once when a test asks, then records.
func (s *source) RecordTransferDelete(ctx context.Context, id, correction string) error {
	if s.failRecord.CompareAndSwap(true, false) {
		return fault.New(fault.Unavailable, "the database does not answer")
	}
	return s.Pipeline.RecordTransferDelete(ctx, id, correction)
}

// ListSessions is the catalog's, or not found once the subject is deleted.
func (s *source) ListSessions(ctx context.Context, f tenant.SessionFilter, p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
	if s.gone.Load() {
		return tenant.CatalogPage[tenant.CatalogSession]{}, fault.New(fault.NotFound, "the tenant holds no such subject")
	}
	return s.Pipeline.ListSessions(ctx, f, p)
}

// ListScans is the catalog's, or not found once the subject is deleted.
func (s *source) ListScans(ctx context.Context, f tenant.ScanFilter, p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
	if s.gone.Load() {
		return tenant.CatalogPage[tenant.CatalogScan]{}, fault.New(fault.NotFound, "the tenant holds no such session")
	}
	return s.Pipeline.ListScans(ctx, f, p)
}

// Correction answers the correction the fake deleter made, or the store's.
func (s *source) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	s.mu.Lock()
	st, ok := s.corrections[id]
	s.mu.Unlock()
	if ok {
		return tenant.Correction{ID: id, State: st}, nil
	}
	return s.Pipeline.Correction(ctx, id)
}

// storages reads the fixture's storage and counts the objects it opened.
type storages struct {
	s      blob.Store
	opened atomic.Int64
}

// Open reads the object from the storage.
func (s *storages) Open(ctx context.Context, obj checkedread.Object, offset, length int64) (checkedread.Opened, error) {
	if obj.Damaged {
		return checkedread.Opened{}, checkedread.ErrMarked
	}
	s.opened.Add(1)
	rc, err := s.s.Read(ctx, obj.Key, offset, length)
	return checkedread.Opened{Body: rc, Storage: obj.Storage}, err
}

// Mismatch records nothing: there is no replica.
func (*storages) Mismatch(context.Context, checkedread.Opened, error) error { return nil }

// fakeTarget is the tenant a job copies into, as far as the runner sees it:
// a receiver of type job that reads each body whole and answers Received
// only when it did, and a catalog that holds what it received. It maps UIDs
// and checksums as a receiver that de-identifies does, with a fixed rule a
// test can compute, so the runner is shown to use the target's mapping.
type fakeTarget struct {
	mu   sync.Mutex
	rcv  tenant.Receiver
	deid bool
	// accepted counts the files taken, by the SOP Instance UID they carry.
	accepted map[string]int
	// held is the catalog: the target's UID to what it holds.
	held map[string]tenant.TransferFound
	prov map[string]tenant.TransferProvenance
	// bodies are the files taken, by the target's UID.
	bodies map[string][]byte
	// hold makes the catalog show a file only after that many lookups of it.
	finds map[string]int
	delay int
	// wrongSum makes the catalog hold another checksum for the UIDs listed.
	wrongSum map[string]bool
	// refuse answers Failed to every file; busy answers Refused.
	refuse, busy atomic.Bool
	// afterAccept, when set, is called after each file taken with how many
	// have been taken.
	afterAccept func(n int)
	taken       int
	// afterProvenance, when set, is called with the lock held after
	// provenance rows are recorded.
	afterProvenance func()
}

// newFakeTarget returns a target with an enabled receiver of type job.
func newFakeTarget(deid bool) *fakeTarget {
	rcv := tenant.Receiver{Name: "xfer", Type: tenant.ReceiverJob, Enabled: true}
	if deid {
		rcv.Profile = tenant.ProfileRef{Name: "target", Version: 1}
	}
	return &fakeTarget{rcv: rcv, deid: deid, accepted: map[string]int{}, held: map[string]tenant.TransferFound{}, prov: map[string]tenant.TransferProvenance{},
		bodies: map[string][]byte{}, finds: map[string]int{}, wrongSum: map[string]bool{}}
}

// Receiver answers the receiver.
func (t *fakeTarget) Receiver(_ context.Context, name string) (tenant.Receiver, error) {
	if name != t.rcv.Name {
		return tenant.Receiver{}, fault.New(fault.NotFound, "no such receiver")
	}
	return t.rcv, nil
}

// TransferRow is the target's mapping: a receiver that de-identifies gives an
// instance another UID and a keyed checksum.
func (t *fakeTarget) TransferRow(_ context.Context, receiver, uid string, sum tenant.SHA256) (receive.TransferRow, error) {
	if receiver != t.rcv.Name {
		return receive.TransferRow{}, fault.New(fault.NotFound, "no such receiver")
	}
	if !t.deid {
		return receive.TransferRow{SOPInstanceUID: uid, SHA256: sum}, nil
	}
	return receive.TransferRow{SOPInstanceUID: "2.25.7." + strconv.FormatUint(uint64(sum[0])<<8|uint64(sum[1]), 10) + "." + uid[len(uid)-6:],
		SHA256: tenant.SHA256(sha256.Sum256(append([]byte("keyed"), sum[:]...)))}, nil
}

// AcceptTransfer reads the body whole, as the pipeline's landing does, and
// takes it only when it was read to its end without an error.
func (t *fakeTarget) AcceptTransfer(ctx context.Context, a receive.Arrival) receive.Outcome {
	if a.Receiver != t.rcv.Name {
		return receive.Outcome{Status: receive.Failed, Reason: "the receiver does not exist or does not take files"}
	}
	t.mu.Lock()
	notDeid := a.RequireDeidentified && !t.rcv.Deidentifies()
	t.mu.Unlock()
	if notDeid {
		// What the pipeline's Accept does before it lands a byte.
		return receive.Outcome{Status: receive.Failed, Reason: receive.ReasonNotDeidentified}
	}
	b, err := io.ReadAll(a.Body)
	switch {
	case err != nil:
		return receive.Outcome{Status: receive.Refused, Reason: "the file could not be kept"}
	case t.refuse.Load():
		return receive.Outcome{Status: receive.Failed, Reason: "the file is larger than the receiver's largest file"}
	case t.busy.Load():
		return receive.Outcome{Status: receive.Refused, Reason: "the archive is stopping"}
	}
	src := tenant.SHA256(sha256.Sum256(b))
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return receive.Outcome{Status: receive.Failed, Reason: "the file is not DICOM"}
	}
	uid, _ := h.UID(header.SOPInstanceUID)
	row, _ := t.TransferRow(ctx, a.Receiver, uid, src)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.accepted[uid]++
	f := tenant.TransferFound{SHA256: row.SHA256, Path: label.Path{Subject: "t-sub", Session: "t-ses", Scan: "t-scan"}}
	if t.wrongSum[uid] {
		f.SHA256[0] ^= 0xff
	}
	t.held[row.SOPInstanceUID] = f
	t.bodies[row.SOPInstanceUID] = b
	t.taken++
	if t.afterAccept != nil {
		t.afterAccept(t.taken)
	}
	return receive.Outcome{Status: receive.Received}
}

// FindTransferred answers what the catalog holds, a file showing only after
// delay lookups of it.
func (t *fakeTarget) FindTransferred(_ context.Context, uids []string) (map[string]tenant.TransferFound, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]tenant.TransferFound{}
	for _, u := range uids {
		f, ok := t.held[u]
		if !ok {
			continue
		}
		t.finds[u]++
		if t.finds[u] > t.delay {
			out[u] = f
		}
	}
	return out, nil
}

// RecordTransferProvenance records the rows.
func (t *fakeTarget) RecordTransferProvenance(_ context.Context, rows []tenant.TransferProvenance) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range rows {
		if _, held := t.prov[r.SOPInstanceUID]; !held {
			t.prov[r.SOPInstanceUID] = r
		}
	}
	if t.afterProvenance != nil {
		t.afterProvenance()
	}
	return nil
}

// TransferProvenanceOf answers a row.
func (t *fakeTarget) TransferProvenanceOf(_ context.Context, uid string) (tenant.TransferProvenance, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.prov[uid]
	return p, ok, nil
}

// ListTransferProvenance is not used by the runner.
func (t *fakeTarget) ListTransferProvenance(context.Context, string, master.Page) ([]tenant.TransferProvenance, string, error) {
	return nil, "", nil
}

// timesAccepted returns how many files the target took in all.
func (t *fakeTarget) timesAccepted() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, c := range t.accepted {
		n += c
	}
	return n
}

// fakeDeleter is the source's corrections engine as a move uses it: it
// records each call and, when a delete is made, checks that the job's items
// are all confirmed at that moment.
type fakeDeleter struct {
	t       testing.TB
	src     *source
	job     string
	holds   []string
	minimum int64
	// failOnce makes the first Delete answer an error that passes by itself.
	failOnce atomic.Bool
	previews atomic.Int64
	deletes  atomic.Int64
	reasons  []string
	// beforePreview, when set, runs once before the first preview answers.
	beforePreview func()
}

// Preview answers a delete's preview.
func (d *fakeDeleter) Preview(ctx context.Context, t tenant.CorrectionTarget) (correct.Preview, error) {
	d.previews.Add(1)
	if d.beforePreview != nil {
		f := d.beforePreview
		d.beforePreview = nil
		f()
	}
	// What the catalog holds now, as the real preview counts it.
	pv, err := PreviewOf(ctx, d.src, tenant.JobTarget{SubjectLabel: t.Subject, SessionLabel: t.Session}, false)
	if err != nil {
		return correct.Preview{}, err
	}
	return correct.Preview{DeletePreview: tenant.DeletePreview{Holds: d.holds, SessionsUnderMinimum: d.minimum, Confirm: "token",
		Counts: tenant.CorrectionCounts{Scans: pv.Scans, Instances: pv.Instances}}}, nil
}

// Delete records the call and checks every item was confirmed when it came.
func (d *fakeDeleter) Delete(ctx context.Context, r correct.DeleteRequest) (tenant.Correction, error) {
	d.t.Helper()
	tj, err := d.src.TransferJob(ctx, d.job)
	if err != nil {
		d.t.Errorf("TransferJob: %v", err)
	} else if tj.Items.Confirmed != tj.Items.Total() || tj.Items.Total() == 0 || tj.Step != tenant.TransferDeleting {
		d.t.Errorf("the source's delete came with the items %+v at the step %q, want every item confirmed at the step deleting", tj.Items, tj.Step)
	}
	if d.failOnce.CompareAndSwap(true, false) {
		return tenant.Correction{}, fault.New(fault.Unavailable, "the storage does not answer")
	}
	d.deletes.Add(1)
	d.reasons = append(d.reasons, r.Reason)
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		return tenant.Correction{}, err
	}
	d.src.mu.Lock()
	d.src.corrections[id] = record.StateCommitted
	d.src.made = append(d.src.made, tenant.Correction{ID: id, Kind: record.KindDelete, State: record.StateCommitted, Reason: r.Reason, Target: r.Target})
	d.src.mu.Unlock()
	d.src.gone.Store(true)
	return tenant.Correction{ID: id, State: record.StateCommitted}, nil
}

// rig is a source, a target and a runner over them.
type rig struct {
	t       testing.TB
	src     *source
	tgt     *fakeTarget
	stores  *storages
	deleter *fakeDeleter
	cfg     Config
}

// newRig archives two scans of one session of sub-01 (three instances), and
// a second session of one, and returns the rig.
func newRig(t testing.TB, deid bool) *rig {
	t.Helper()
	f := newFixture(t)
	files := generated(t, 4)
	f.archive("ses-1", false, scanFiles{label: "scan-a", files: files[:2]}, scanFiles{label: "scan-b", files: files[2:3]})
	f.archive("ses-2", false, scanFiles{label: "scan-a", files: files[3:]})
	r := &rig{t: t, src: &source{Pipeline: f.p, fixture: f, corrections: map[string]record.State{}}, tgt: newFakeTarget(deid), stores: &storages{s: f.store}}
	r.deleter = &fakeDeleter{t: t, src: r.src}
	r.cfg = Config{Source: r.src, SourceID: "T0000000000001",
		Storages: func(context.Context) (Storages, func(), error) { return r.stores, func() {}, nil },
		Target: func(_ context.Context, name string) (Target, error) {
			if name != "other" {
				return nil, fault.New(fault.NotFound, "no such tenant")
			}
			return r.tgt, nil
		},
		Deleter:     func(context.Context) (Deleter, func(), error) { return r.deleter, func() {}, nil },
		ConfirmWait: time.Hour, Now: f.clock, Workers: 2}
	return r
}

// queue queues a transfer of the session (or the subject, for an empty
// session) to the tenant "other", at the rig's receiver.
func (r *rig) queue(session string, move bool) string {
	r.t.Helper()
	id := r.src.id("job")
	_, err := r.src.QueueTransfer(r.src.ctx, tenant.TransferQueue{ID: id, Target: tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: session},
		TargetTenant: "other", TargetTenantID: "T0000000000002", Receiver: "xfer", SourceTenant: "lab", Move: move, Reason: "moving a study",
		KeyID: "k-admin", Now: r.src.clock()}, r.src.audit(), nil)
	if err != nil {
		r.t.Fatalf("QueueTransfer: %v", err)
	}
	r.deleter.job = id
	return id
}

// runner returns a new runner over the rig's config, as a restarted server
// makes one.
func (r *rig) runner() *Runner {
	r.t.Helper()
	run, err := New(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

// runUntilEnded runs passes until the job ends, failing after many.
func (r *rig) runUntilEnded(id string) tenant.TransferJob {
	r.t.Helper()
	run := r.runner()
	for range 50 {
		res, err := run.Run(r.src.ctx, id)
		if err != nil {
			r.t.Fatalf("Run: %v", err)
		}
		if res.Ended {
			break
		}
	}
	tj, err := r.src.TransferJob(r.src.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	if !tj.Job.State.Ended() {
		r.t.Fatalf("the job did not end: %+v", tj)
	}
	return tj
}
