package scrub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// studyCorpus are seven files of one CT study in two series, and three
// files of studies of their own.
var studyCorpus = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm",
	"ct5n-3353.dcm", "MR_small.dcm", "liver_1frame.dcm", "SC_rgb_rle_2frame.dcm"}

// quickClock runs quickRate times faster than the machine's, so a
// receiver's quiet period passes at once.
type quickClock struct{ start time.Time }

// quickRate is how much faster the clock runs.
const quickRate = 20

// Now answers the clock's time.
func (c quickClock) Now() time.Time { return c.start.Add(time.Since(c.start) * quickRate) }

// After answers a channel after d of the clock's time.
func (c quickClock) After(d time.Duration) <-chan time.Time { return time.After(d / quickRate) }

// errStop is the stop a test makes a correction take at one step, as a
// server stopping there would.
var errStop = errors.New("the server stopped here")

// stopAt is the tenant's database as a corrections engine sees it, failing
// the steps set: a publish (with the read of the correction an abandon
// makes first, so nothing is rolled back), or a purge's end.
type stopAt struct {
	tenant.Corrections
	commit, finish bool
}

// CommitCorrection fails when the publish is set to stop.
func (s stopAt) CommitCorrection(ctx context.Context, c tenant.CorrectionCommit) (tenant.Correction, error) {
	if s.commit {
		return tenant.Correction{}, errStop
	}
	return s.Corrections.CommitCorrection(ctx, c)
}

// Correction fails when the publish is set to stop.
func (s stopAt) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	if s.commit {
		return tenant.Correction{}, errStop
	}
	return s.Corrections.Correction(ctx, id)
}

// FinishPurge fails when the purge's end is set to stop.
func (s stopAt) FinishPurge(ctx context.Context, f tenant.PurgeFinish) (tenant.Correction, error) {
	if s.finish {
		return tenant.Correction{}, errStop
	}
	return s.Corrections.FinishPurge(ctx, f)
}

// failPut is a storage whose puts of the keys failing answers fail: a
// relabel stopped while it copies.
type failPut struct {
	blob.Store
	failing func(key string) bool
}

// Put fails for the keys failing answers.
func (s failPut) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if s.failing(key) {
		return blob.Written{}, errStop
	}
	return s.Store.Put(ctx, key, r)
}

// corrRig is the fixture with its corpus archived through the real
// pipeline (so each scan has its manifest and metadata), a scrub
// supervisor running on the rig's clock, and corrections made by the real
// engine.
type corrRig struct {
	*fixture
	clock quickClock
	sup   *Supervisor
	// reads counts the scrub's reads by key since readKeys last answered.
	mu    sync.Mutex
	reads map[string]int
}

// newCorrRig archives the corpus and starts the supervisor.
func newCorrRig(t *testing.T) *corrRig {
	t.Helper()
	r := &corrRig{fixture: newFixture(t), clock: quickClock{start: time.Now()}, reads: map[string]int{}}
	r.archiveCorpus(studyCorpus...)
	cfg := withRead(r.config(), func(key string, _ int) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reads[key]++
	})
	cfg.Now = r.clock.Now
	r.sup = r.start(cfg)
	return r
}

// readKeys returns the scrub's reads by key since it last answered.
func (r *corrRig) readKeys() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.reads
	r.reads = map[string]int{}
	return out
}

// archiveCorpus runs the corpus files named through the receive pipeline
// into the receiver "recv" and waits until every reception has ended.
func (r *corrRig) archiveCorpus(names ...string) {
	r.t.Helper()
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		r.t.Fatal(err)
	}
	p, err := receive.New(receive.Config{Tenant: "lab", Store: r.p, Hasher: hasher, Clock: r.clock,
		OpenStorage:  func(context.Context, string) (blob.Store, error) { return r.store, nil },
		IncomingRoot: filepath.Join(r.t.TempDir(), "incoming"), Workers: 2, Lease: 30 * time.Second, PollInterval: time.Second,
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
		out := p.Accept(r.ctx, receive.Arrival{Receiver: "recv", Body: bytes.NewReader(b),
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
		ended := len(rs) > 0
		for _, rc := range rs {
			ended = ended && rc.State.Ended()
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

// engine returns a corrections engine over the rig's database as db
// answers it and its storage as store answers it.
func (r *corrRig) engine(db tenant.Corrections, store blob.Store) *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: fixtureTenantID, Keys: r.installationKeys, Store: db, Now: r.clock.Now,
		OpenStorage: func(context.Context, string) (blob.Store, error) { return store, nil },
		Grace:       func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// scans returns the catalog's scans.
func (r *corrRig) scans() []tenant.CatalogScan {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Items
}

// pathOf returns a catalog scan's labels.
func pathOf(s tenant.CatalogScan) label.Path {
	return label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
}

// held returns a scan's instances.
func (r *corrRig) held(p label.Path) []tenant.ScanInstance {
	r.t.Helper()
	ins, _, err := r.p.ListHeldInstances(r.ctx, p, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	return ins
}

// heldCount counts the instances the catalog holds.
func (r *corrRig) heldCount() int64 {
	var n int64
	for _, s := range r.scans() {
		n += s.Instances
	}
	return n
}

// scrubClean runs a scrub of "main" and fails unless it ended done, read
// every instance the catalog holds good, and holds and tallied no finding.
func (r *corrRig) scrubClean(when string) tenant.Job {
	r.t.Helper()
	j := r.scrubNow()
	if j.State != tenant.JobDone || j.Counts.InstancesSent != r.heldCount() || j.Counts.InstancesRefused != 0 {
		r.t.Errorf("%s: the scrub ended as %+v, want done with the %d instances held read good", when, j, r.heldCount())
	}
	if got := r.findings(false); len(got) != 0 {
		r.t.Errorf("%s: the scrub holds findings %v", when, got)
	}
	for k, n := range r.tallies(j.ID) {
		if strings.HasPrefix(k, "finding|") || k == tenant.TallySidecarMissing || k == tenant.TallyCorrectionRecordOnly {
			r.t.Errorf("%s: the scrub tallied %s %d", when, k, n)
		}
	}
	return j
}

// scrubNow runs a scrub of "main" to its end.
func (r *corrRig) scrubNow() tenant.Job {
	r.t.Helper()
	j, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: r.id("job"), Kind: tenant.JobScrub, Storage: "main", Created: r.clock.Now()},
		r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.waitEnded(r.sup, j.ID)
}

// keysUnder returns the storage's keys under the subject's data/ and meta/
// trees.
func (r *corrRig) keysUnder(subject string) []string {
	r.t.Helper()
	var out []string
	for _, prefix := range []string{layout.DataRoot + subject + "/", layout.MetaRoot + subject + "/"} {
		keys, err := listAll(r.ctx, r.store, prefix, "")
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, keys...)
	}
	return out
}

// deleteNow previews and deletes target with e.
func (r *corrRig) deleteNow(e *correct.Engine, target tenant.CorrectionTarget) tenant.Correction {
	r.t.Helper()
	p, err := r.p.PreviewDelete(r.ctx, target, r.clock.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		r.t.Fatalf("Delete = %+v, %v", c, err)
	}
	return c
}

// putStray puts an object no scan and no correction names at key.
func (r *corrRig) putStray(key string) {
	r.t.Helper()
	if _, err := r.store.Put(r.ctx, key, strings.NewReader("an object put by hand")); err != nil {
		r.t.Fatal(err)
	}
}

// TestScrubDuringADeleteFindsNothing guards that a scrub during a delete's
// grace, and during an instance delete's purge stopped before its end,
// calls nothing of what the delete keeps an orphan, missing, or a manifest
// or metadata that disagrees; and that after the purges nothing of the
// deleted subject is left on the storage and the scrub still finds
// nothing.
func TestScrubDuringADeleteFindsNothing(t *testing.T) {
	r := newCorrRig(t)
	e := r.engine(r.p, r.store)
	var two tenant.CatalogScan
	for _, s := range r.scans() {
		if s.Instances >= 2 {
			two = s
		}
	}
	other := ""
	for _, s := range r.scans() {
		if s.SubjectLabel != two.SubjectLabel {
			other = s.SubjectLabel
		}
	}
	if two.Label == "" || other == "" {
		t.Fatalf("the corpus gave no scan of two instances beside another subject: %+v", r.scans())
	}
	r.scrubClean("before any correction")

	whole := r.deleteNow(e, tenant.CorrectionTarget{Subject: other})
	path := pathOf(two)
	victim := r.held(path)[0]
	one := r.deleteNow(e, tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan,
		Instance: victim.SOPInstanceUID})
	if len(r.keysUnder(other)) == 0 {
		t.Fatal("the delete's objects are not kept for its grace")
	}
	r.scrubClean("in the deletes' grace")

	// The instance delete's purge removes the object and writes the scan's
	// manifest and metadata without it, then stops before its end: the
	// catalog still holds the old manifest's checksum.
	stopped := r.engine(stopAt{Corrections: r.p, finish: true}, r.store)
	if _, err := stopped.PurgeNow(r.ctx, one.ID, r.audit(), nil); !errors.Is(err, errStop) {
		t.Fatalf("the purge did not stop before its end: %v", err)
	}
	if c, err := r.p.Correction(r.ctx, one.ID); err != nil || c.State != record.StatePurging {
		t.Fatalf("the stopped purge is %+v, %v", c, err)
	}
	r.scrubClean("in an instance delete's purge stopped before its end")

	for _, id := range []string{whole.ID, one.ID} {
		if c, err := e.PurgeNow(r.ctx, id, r.audit(), nil); err != nil || c.State != record.StatePurged {
			t.Fatalf("PurgeNow(%s) = %+v, %v", id, c, err)
		}
	}
	if left := r.keysUnder(other); len(left) != 0 {
		t.Fatalf("after the purge the storage holds %q", left)
	}
	r.scrubClean("after the purges")
}

// TestScrubDuringARelabelFindsNothing guards that a scrub during a
// relabel, while it copies (stopped before its new manifest) and after its
// publish until its purge, calls neither tree's objects orphans; that an
// object put by hand under the new tree is still found as an orphan; and
// that after the purge the old tree is gone and the scrub finds nothing.
func TestScrubDuringARelabelFindsNothing(t *testing.T) {
	r := newCorrRig(t)
	subject := ""
	for _, s := range r.scans() {
		if s.Instances >= 2 {
			subject = s.SubjectLabel
		}
	}
	e := r.engine(r.p, r.store)
	c, err := e.Relabel(r.ctx, correct.RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "renamed-1",
		Reason: "wrong subject id at the scanner", Audit: r.audit()})
	if err != nil || c.State != record.StatePrepared {
		t.Fatalf("Relabel = %+v, %v", c, err)
	}
	copying := r.engine(r.p, failPut{Store: r.store, failing: func(k string) bool {
		return strings.HasPrefix(k, layout.MetaRoot+"renamed-1/") && strings.HasSuffix(k, layout.ManifestName)
	}})
	if _, err := copying.ContinueRelabel(r.ctx, c.ID); !errors.Is(err, errStop) {
		t.Fatalf("the relabel did not stop while it copied: %v", err)
	}
	copies := r.keysUnder("renamed-1")
	if len(copies) == 0 {
		t.Fatal("the stopped relabel copied nothing")
	}
	for _, k := range copies {
		if strings.HasSuffix(k, layout.ManifestName) {
			t.Fatalf("the stopped relabel wrote its manifest %s", k)
		}
	}
	r.scrubClean("while the relabel copies")

	// An object put by hand beside the copies is an orphan all the same.
	data := ""
	for _, k := range copies {
		if strings.HasPrefix(k, layout.DataRoot) {
			data = k[:strings.LastIndex(k, "/")+1]
		}
	}
	stray := data + "1.2.826.0.1.3680043.2.1125.999.1.dcm"
	r.putStray(stray)
	r.scrubNow()
	if got := r.findings(false); len(got) != 1 || got[0] != string(tenant.FindingOrphan)+" "+stray {
		t.Fatalf("beside a relabel's copies the scrub holds %v, want the object put by hand an orphan", got)
	}
	if err := r.store.Delete(r.ctx, stray); err != nil {
		t.Fatal(err)
	}

	if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil || c.State != record.StateCommitted {
		t.Fatalf("ContinueRelabel = %+v, %v", c, err)
	}
	if len(r.keysUnder(subject)) == 0 {
		t.Fatal("the old tree is not kept until the purge")
	}
	r.scrubClean("after the relabel's publish")

	if c, err = e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || c.State != record.StatePurged {
		t.Fatalf("PurgeNow = %+v, %v", c, err)
	}
	if left := r.keysUnder(subject); len(left) != 0 {
		t.Fatalf("after the purge the old tree holds %q", left)
	}
	r.scrubClean("after the relabel's purge")
}

// TestScrubDuringAReplaceFindsNothing guards that a scrub during a replace
// stopped before its publish (its new revision put, the manifest and
// metadata naming it) calls nothing a difference; that after a replace's
// publish its revision key is read good like any instance and the
// superseded revision is kept without being an orphan; and that after the
// purge the superseded object is gone and the scrub finds nothing.
func TestScrubDuringAReplaceFindsNothing(t *testing.T) {
	r := newCorrRig(t)
	var scans []tenant.CatalogScan
	for _, s := range r.scans() {
		if s.Instances >= 2 {
			scans = append(scans, s)
		}
	}
	if len(scans) < 2 {
		t.Fatalf("the corpus gave fewer than two scans of two instances: %+v", r.scans())
	}

	// A replace stopped at its publish: nothing is rolled back.
	first := pathOf(scans[0])
	in := r.held(first)[0]
	stopped := r.engine(stopAt{Corrections: r.p, commit: true}, r.store)
	if _, err := r.replace(stopped, scans[0], in, "qreplace0001"); !errors.Is(err, errStop) {
		t.Fatalf("the replace did not stop at its publish: %v", err)
	}
	key, _ := layout.RevisionKey(first, in.SOPInstanceUID, 2)
	if got := r.lineOf(first, in.SOPInstanceUID); got.Key != key {
		t.Fatalf("the stopped replace's metadata line is %+v, want %s", got, key)
	}
	r.scrubClean("with a replace stopped before its publish")

	second := pathOf(scans[1])
	in = r.held(second)[0]
	e := r.engine(r.p, r.store)
	c, err := r.replace(e, scans[1], in, "qreplace0002")
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("Replace = %+v, %v", c, err)
	}
	key, _ = layout.RevisionKey(second, in.SOPInstanceUID, 2)
	if h, err := r.p.HeldInstance(r.ctx, second, in.SOPInstanceUID); err != nil || h.Key != key {
		t.Fatalf("the replaced instance is %+v, %v; want %s", h, err, key)
	}
	r.readKeys()
	r.scrubClean("after a replace's publish")
	read := r.readKeys()
	if read[key] == 0 {
		t.Errorf("the scrub did not read the revision key %s", key)
	}
	if read[in.Key] != 0 {
		t.Errorf("the scrub read the superseded revision %s as an instance", in.Key)
	}

	if c, err = e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || c.State != record.StatePurged {
		t.Fatalf("PurgeNow = %+v, %v", c, err)
	}
	if _, err := r.store.Read(r.ctx, in.Key, 0, -1); err == nil {
		t.Fatal("the purge left the superseded revision")
	}
	r.scrubClean("after the replace's purge")
}

// TestScrubTrustsARecordWithNoRowOnlyForWhatItsManifestNames guards the
// fallback to a correction record whose correction has no row: a scan
// tree the catalog does not hold, which a committed delete's record on the
// storage names, is no orphan as far as its own manifest names its
// objects; an object beside them that the manifest does not name is still
// an orphan; and the record is tallied.
func TestScrubTrustsARecordWithNoRowOnlyForWhatItsManifestNames(t *testing.T) {
	r := newCorrRig(t)
	_, stray := r.ghostDeleted(func(c record.Correction) []byte {
		b, err := record.EncodeCorrection(c, fixtureTenantID, r.installationKeys)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})

	j := r.scrubNow()
	if got := r.findings(false); len(got) != 1 || got[0] != string(tenant.FindingOrphan)+" "+stray {
		t.Errorf("beside a record with no row the scrub holds %v, want the object its manifest does not name an orphan", got)
	}
	if n := r.tallies(j.ID)[tenant.TallyCorrectionRecordOnly]; n != 1 {
		t.Errorf("the record with no row is tallied %d times, want once", n)
	}
}

// replace replaces in, of the scan s, with its bytes and one byte more,
// from a quarantined file of that id.
func (r *corrRig) replace(e *correct.Engine, s tenant.CatalogScan, in tenant.ScanInstance, id string) (tenant.Correction, error) {
	r.t.Helper()
	rc, err := r.store.Read(r.ctx, in.Key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	old, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		r.t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0)
	recv, _, err := r.p.Reception(r.ctx, in.ReceptionID)
	if err != nil {
		r.t.Fatal(err)
	}
	q := tenant.QuarantinedFile{ID: id, Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: in.ReceptionID,
		Size: int64(len(corrected)), SHA256: tenant.SHA256(sha256.Sum256(corrected)), KeptSHA256: in.SHA256,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: in.SOPInstanceUID, StudyUID: recv.StudyUID, SeriesUID: s.SeriesUID,
		TransferSyntaxUID: "1.2.840.10008.1.2.1", Incoming: "quarantine/" + id, ContentKept: true,
		Quarantined: r.clock.Now().UTC().Truncate(time.Microsecond)}
	if _, err := r.p.HoldFiles(r.ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		r.t.Fatal(err)
	}
	p := pathOf(s)
	return e.Replace(r.ctx, correct.ReplaceRequest{Quarantine: id, Storage: "main", Path: p, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineOf(p, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
}

// lineOf returns the instance's line of the scan's metadata.
func (r *corrRig) lineOf(p label.Path, uid string) sidecar.Instance {
	r.t.Helper()
	sk, _ := layout.MetadataKey(p)
	rc, err := r.store.Read(r.ctx, sk, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	for {
		in, _, err := rd.Next()
		if err != nil {
			r.t.Fatalf("the scan's metadata holds no line of %s: %v", uid, err)
		}
		if in.SOPInstanceUID == uid {
			return in
		}
	}
}
