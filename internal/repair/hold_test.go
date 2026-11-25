package repair

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// create makes job's row, which queues its unit, once.
func (f *fixture) create(job Job) {
	f.t.Helper()
	if f.made[job.ID] {
		return
	}
	if _, err := f.p.CreateJob(f.ctx, job.NewJob(f.clock.Now()), f.audit(), nil); err != nil {
		f.t.Fatalf("CreateJob of the repair: %v", err)
	}
	f.made[job.ID] = true
}

// job returns the row of the job of id.
func (f *fixture) job(id string) tenant.Job {
	f.t.Helper()
	j, err := f.p.Job(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// deletion is a delete of one instance taken as far as it goes: prepared,
// published, its purge started and, with manifest, its object removed and
// the scan's manifest rewritten without it.
type deletion struct {
	// err is the first refusal met; committed says the delete was
	// published (the instance hidden), purged that its purge ended.
	err       error
	committed bool
	purged    bool
	// manifest is the digest of the manifest the purge wrote.
	manifest tenant.SHA256
}

// deleteInstance deletes the instance numbered i as far as it goes, under
// the correction id given; with rewrite, it purges the instance's object
// and writes the scan's manifest again without it, as a purge does.
func (f *fixture) deleteInstance(id string, i int, rewrite bool) deletion {
	f.t.Helper()
	var d deletion
	target := tenant.CorrectionTarget{Subject: f.path.Subject, Session: f.path.Session, Scan: f.path.Scan, Instance: f.files[i].uid}
	now := f.clock.Now()
	before, err := f.p.CatalogScan(f.ctx, f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := f.p.PreviewDelete(f.ctx, target, now)
	if err != nil {
		f.t.Fatal(err)
	}
	c, _, err := f.p.PrepareCorrection(f.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: now.Add(time.Second)}, f.audit(), nil)
	if err != nil {
		d.err = err
		return d
	}
	if _, err := f.p.CommitCorrection(f.ctx, tenant.CorrectionCommit{ID: c.ID, Now: now.Add(2 * time.Second), Grace: time.Second}); err != nil {
		d.err = err
		return d
	}
	d.committed = true
	if _, err := f.p.StartPurge(f.ctx, c.ID, now.Add(3*time.Second), &master.AuditEntry{ID: f.id("a"), Time: now, Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil); err != nil {
		d.err = err
		return d
	}
	if !rewrite {
		return d
	}
	f.remove(f.primary.Store, f.keys[i])
	if err := f.p.RecordPurgedObject(f.ctx, tenant.PurgedObject{ID: c.ID, Storage: "main", Key: f.keys[i], Size: f.files[i].size,
		SHA256: sumOf(f.files[i]), Disposition: tenant.DispositionRemoved, Now: now.Add(4 * time.Second)}); err != nil {
		d.err = err
		return d
	}
	b := []byte("a manifest without the instance\n")
	f.putPrimary(mustKey(layout.ManifestKey(f.path)), b)
	d.manifest = tenant.SHA256(sha256.Sum256(b))
	if _, err := f.p.FinishPurge(f.ctx, tenant.PurgeFinish{ID: c.ID, Now: now.Add(5 * time.Second), Manifest: d.manifest,
		ManifestBefore: before.ManifestSHA256}); err != nil {
		d.err = err
		return d
	}
	d.purged = true
	return d
}

// onSecondRead runs fn at the second read of key on the replica: the
// stream of the put to the primary, after the record of the repair.
func (f *fixture) onSecondRead(key string, fn func()) {
	n := 0
	f.replica.set(func(s *store) {
		s.onRead = func(k string) {
			if k != key {
				return
			}
			if n++; n == 2 {
				fn()
			}
		}
	})
}

// TestHoldRefusesDeleteDuringPut guards a delete of an instance whose
// object a repair is putting back: while the put streams, the delete's
// prepare is refused as retryable (tenant.ErrScanRepairing), so the object
// of an instance a delete hid is never put back; once the repair ended,
// the delete is taken.
func TestHoldRefusesDeleteDuringPut(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	var during deletion
	f.onSecondRead(f.keys[0], func() { during = f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzz", 0, false) })
	res, err := f.run(f.config(), f.manual(fd.ID))
	if err != nil {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	_, _, present := f.measure(f.primary.Store, f.keys[0])
	if during.committed && present {
		t.Fatal("the repair put back the object of an instance a delete hid while it streamed")
	}
	if !errors.Is(during.err, tenant.ErrScanRepairing) || fault.KindOf(during.err) != fault.Precondition {
		t.Errorf("the delete during the put = %v; want refused as the scan being repaired", during.err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	f.replica.set(func(s *store) { s.onRead = nil })
	if after := f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzy", 0, false); after.err != nil || !after.committed {
		t.Errorf("the delete once the repair ended = %+v; want published and its purge started", after)
	}
}

// TestHoldKeepsNewManifest guards a scan's manifest a repair is putting
// back while a purge would rewrite it: the purge's correction is refused
// while the put streams, so the old manifest is never put over the one the
// catalog names after the purge.
func TestHoldKeepsNewManifest(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	key := mustKey(layout.ManifestKey(f.path))
	scan, err := f.p.CatalogScan(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.damage(f.primary.Store, key)
	size, sum, _ := f.measure(f.primary.Store, key)
	man := f.record(tenant.ScrubFinding{Kind: tenant.FindingManifestDamaged, Storage: "main", Key: key, Path: f.path,
		ExpectedSHA256: scan.ManifestSHA256, FoundSize: &size, FoundSHA256: sum})
	var during deletion
	f.onSecondRead(key, func() { during = f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzx", 2, true) })
	res, err := f.run(f.config(), f.manual(man.ID))
	if err != nil {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	now, err := f.p.CatalogScan(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, got, _ := f.measure(f.primary.Store, key)
	if got != now.ManifestSHA256 {
		t.Fatalf("the primary's manifest reads %s, the catalog names %s (purged %v): an old manifest over a new one",
			got, now.ManifestSHA256, during.purged)
	}
	if !errors.Is(during.err, tenant.ErrScanRepairing) {
		t.Errorf("the purge's delete during the put = %v; want refused as the scan being repaired", during.err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
}

// cutOff is the tenant's database as a worker that lost it sees it: once
// cut, no hold is let go.
type cutOff struct {
	DB
	cut atomic.Bool
}

// ReleaseRepair fails as a database that does not answer once cut.
func (c *cutOff) ReleaseRepair(ctx context.Context, fence tenant.WorkFence, id string) error {
	if c.cut.Load() {
		return fault.New(fault.Unavailable, "the test's database does not answer")
	}
	return c.DB.ReleaseRepair(ctx, fence, id)
}

// TestHoldOutlastsCutOffWorker guards a repair whose worker stopped in its
// put and could not reach the database to let its hold go: the hold stands
// past its unit's due, for the grace a put cut then may still take to land,
// so a delete of the scan is refused, and no longer once the grace too ran
// out; the job taken over then finds the correction under way and leaves
// the object as it is.
func TestHoldOutlastsCutOffWorker(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	db := &cutOff{DB: f.p}
	f.onSecondRead(f.keys[0], func() { db.cut.Store(true); cancel() })
	job := f.manual(fd.ID)
	f.create(job)
	cfg := f.config()
	cfg.DB = db
	if _, err := f.runner(cfg).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stopped Run = %v; want canceled", err)
	}
	f.replica.set(func(s *store) { s.onRead = nil })
	if held := f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzw", 1, false); !errors.Is(held.err, tenant.ErrScanRepairing) {
		t.Errorf("a delete while the stopped repair's lease holds = %+v; want refused as the scan being repaired", held)
	}
	f.clock.Add(cfg.Lease + time.Second)
	if held := f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzq", 1, false); !errors.Is(held.err, tenant.ErrScanRepairing) {
		t.Errorf("a delete past the lease's due, within the grace = %+v (%v); want refused as the scan being repaired", held, held.err)
	}
	f.clock.Add(tenant.RepairHoldGrace)
	if freed := f.deleteInstance("01hzzzzzzzzzzzzzzzzzzzzzzv", 1, false); freed.err != nil || !freed.committed {
		t.Fatalf("a delete once the hold ran out = %+v; want published", freed)
	}
	res, err := f.run(f.config(), job)
	if err != nil || !res.Done {
		t.Fatalf("the Run taken over = %+v, %v", res, err)
	}
	// The delete's publish queued the scan's copies again (the replica not
	// in step: no good copy) or, were it in step, the record is refused
	// (skipped); either way nothing is put.
	if res.Counts[Repaired] != 0 || res.Counts[Skipped]+res.Counts[NoGoodCopy] != 1 {
		t.Errorf("the Run taken over counted %v; want the object skipped or refused", res.Counts)
	}
	if _, puts := f.primary.counts(); puts != 0 {
		t.Errorf("the primary was put %d times while a correction of the scan was under way", puts)
	}
	if !f.findingOf(fd.ID).Holds() {
		t.Error("the finding was cleared though nothing was put")
	}
}

// gate is a write pause that counts what it admits and releases, or refuses
// every write.
type gate struct {
	mu                sync.Mutex
	entered, released int
	storages          []string
	refuse            bool
}

// Enter admits a write, or refuses it as a switch running.
func (g *gate) Enter(_ context.Context, storage string) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse {
		return nil, fault.New(fault.Unavailable, "the storage is being switched to its replica")
	}
	g.entered++
	g.storages = append(g.storages, storage)
	return func() {
		g.mu.Lock()
		g.released++
		g.mu.Unlock()
	}, nil
}

// TestPutsThroughGate guards the write gate: each put to the primary enters
// it, naming the storage, and releases it after; a gate that refuses (a
// switch holding the storage's writes) puts nothing and leaves the object
// for the next attempt, the reason naming the error's kind alone.
func TestPutsThroughGate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(2, 4096))
	f.remove(f.primary.Store, f.keys[0])
	fd := f.find(0)
	g := &gate{refuse: true}
	cfg := f.config()
	cfg.Pause = g
	var reasons []string
	cfg.Outcome = func(o Outcome) { reasons = append(reasons, o.Reason) }
	job := f.manual(fd.ID)
	res, err := f.run(cfg, job)
	if !errors.Is(err, ErrTryAgain) {
		t.Fatalf("Run through a refusing gate = %+v, %v; want tried again", res, err)
	}
	if _, puts := f.primary.counts(); puts != 0 {
		t.Errorf("the primary was put %d times through a refusing gate", puts)
	}
	if len(reasons) != 1 || reasons[0] != "the storage's writes are paused (unavailable)" {
		t.Errorf("the reasons are %q; want the gate's phrase and the error's kind alone", reasons)
	}
	g.mu.Lock()
	g.refuse = false
	g.mu.Unlock()
	f.clock.Add(time.Hour)
	res, err = f.run(cfg, job)
	if err != nil || res.Counts[Repaired] != 1 {
		t.Fatalf("Run through an open gate = %+v, %v", res, err)
	}
	f.wantGood(0)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entered != 1 || g.released != 1 || len(g.storages) != 1 || g.storages[0] != "main" {
		t.Errorf("the gate was entered %d and released %d times, for %v; want once each, for main", g.entered, g.released, g.storages)
	}
}

// TestOwnObject guards the guard of an object other files may share: an
// instance's key is mended only when it is the layout's key of that very
// instance in its scan, of any revision.
func TestOwnObject(t *testing.T) {
	t.Parallel()
	p := label.Path{Subject: "sub-01", Session: "ses-01", Scan: "scan-01"}
	own := mustKey(layout.InstanceKey(p, "1.2.3.4"))
	other := mustKey(layout.InstanceKey(p, "1.2.3.5"))
	elsewhere := mustKey(layout.InstanceKey(label.Path{Subject: "sub-02", Session: "ses-01", Scan: "scan-01"}, "1.2.3.4"))
	for key, want := range map[string]bool{own: true, other: false, elsewhere: false, "data/packed.bin": false,
		mustKey(layout.ManifestKey(p)): false} {
		if got := ownObject(key, p, "1.2.3.4"); got != want {
			t.Errorf("ownObject(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestNamedFindingsAlone guards a job naming findings: it reads those
// findings alone, never the storage's list of every finding, and answers
// one the tenant does not have as skipped.
func TestNamedFindingsAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, synthetic(3, 4096))
	f.remove(f.primary.Store, f.keys[0])
	f.remove(f.primary.Store, f.keys[1])
	first := f.find(0)
	second := f.find(1)
	cfg := f.config()
	counting := &listCounter{DB: f.p}
	cfg.DB = counting
	var skipped []string
	cfg.Outcome = func(o Outcome) {
		if o.Kind == Skipped {
			skipped = append(skipped, o.Reason)
		}
	}
	job := f.manual(first.ID)
	res, err := f.run(cfg, job)
	if err != nil || !res.Done {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	wantCounts(t, res, map[OutcomeKind]int64{Repaired: 1})
	if counting.lists != 0 {
		t.Errorf("a job naming its findings listed the storage's findings %d times", counting.lists)
	}
	if !f.findingOf(second.ID).Holds() {
		t.Error("a finding the job did not name was mended")
	}
	if len(skipped) != 0 {
		t.Errorf("skipped %q", skipped)
	}
}

// listCounter is the tenant's database counting the lists of findings.
type listCounter struct {
	DB
	lists int
}

// ListScrubFindings counts, then lists.
func (c *listCounter) ListScrubFindings(ctx context.Context, filter tenant.ScrubFindingFilter, page master.Page) ([]tenant.ScrubFinding, string, error) {
	c.lists++
	return c.DB.ListScrubFindings(ctx, filter, page)
}

// TestRetryReasonKind guards the reason of an object left for the next
// attempt: the phrase and the fault kind of the error, never the error's
// own text.
func TestRetryReasonKind(t *testing.T) {
	t.Parallel()
	err := fault.Wrap(fault.Unavailable, errors.New("dial tcp 10.0.0.9:9000: secret-bucket refused"), "the storage did not answer")
	got := retryReason("the storage did not take the object", err)
	if got != "the storage did not take the object (unavailable)" || strings.Contains(got, "10.0.0.9") {
		t.Errorf("the reason is %q", got)
	}
}
