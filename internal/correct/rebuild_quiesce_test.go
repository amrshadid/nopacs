package correct

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readHook is a storage that calls hook, once, before the first read of a
// key match accepts.
type readHook struct {
	blob.Store
	match func(key string) bool
	once  sync.Once
	hook  func()
}

// Read calls the hook before the first read it matches.
func (s *readHook) Read(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if s.match(key) {
		s.once.Do(s.hook)
	}
	return s.Store.Read(ctx, key, off, n)
}

// createReplace makes a replace rebuild job of the rig's tenant.
func (r *rig) createReplace(t *testing.T) tenant.Job {
	t.Helper()
	j, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: fmt.Sprintf("job%08d", r.seq.Add(1)), Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildReplace}, Created: time.Now()}, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// runRebuild runs the rig's queued rebuild j to its end through a
// supervisor reading st, with drain as its Config.Drain, and answers the
// job ended.
func (r *rig) runRebuild(t *testing.T, j tenant.Job, st blob.Store, drain func(context.Context, string) error) tenant.Job {
	t.Helper()
	return r.runRebuildWith(t, j, st, drain, nil)
}

// runRebuildWith is runRebuild with decide as the supervisor's
// Config.Decide.
func (r *rig) runRebuildWith(t *testing.T, j tenant.Job, st blob.Store, drain func(context.Context, string) error,
	decide func(context.Context, tenant.Pipeline, record.Correction) (record.Correction, bool, error)) tenant.Job {
	t.Helper()
	sup, err := rebuild.New(rebuild.Config{Tenants: oneTenant{p: r.p}, OpenStorages: func(string, tenant.Pipeline) rebuild.Storages {
		return storages{s: st}
	}, PollInterval: 20 * time.Millisecond, Interval: time.Second, Keys: r.keys,
		TenantID: func(context.Context, string) (string, error) { return rigTenantID, nil }, Drain: drain, Decide: decide})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sup.Stop(ctx)
	}()
	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := r.p.Job(r.ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State.Ended() {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rebuild did not end: %+v", got)
		}
		sup.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
}

// TestQuiesceWaitsForARequestInFlight guards what a rebuild's drain waits
// for: Quiesce does not return while a request holds the tenant's engine
// (Idle, which waits for background work alone, does), and returns once
// the request has let it go.
func TestQuiesceWaitsForARequestInFlight(t *testing.T) {
	r := newRig(t, false)
	es := r.engines(time.Hour, 24*time.Hour, nil)
	_, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	// Let go on every way out, so a failure does not hold the engines' Stop.
	defer release()
	if err := es.Idle(r.ctx, "lab"); err != nil {
		t.Fatalf("Idle with a request in flight: %v", err)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 200*time.Millisecond)
	err = es.Quiesce(ctx, "lab")
	cancel()
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Quiesce with a request in flight: %v, want unavailable once its bound ends", err)
	}
	release()
	if err := es.Quiesce(r.ctx, "lab"); err != nil {
		t.Fatalf("Quiesce once the request let the engine go: %v", err)
	}
}

// TestQuiesceWaitsForARequestOnAReplacedEngine guards a request still in
// flight on the engine of a database since opened anew: Quiesce waits for
// it too, not only for the requests of the engine now current.
func TestQuiesceWaitsForARequestOnAReplacedEngine(t *testing.T) {
	r := newRig(t, false)
	again, err := (driver.SQLiteTenants{DataDir: r.dataDir}).Open(r.ctx, rigTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	es := r.engines(time.Hour, 24*time.Hour, nil)
	_, releaseOld, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	// Let go on every way out, so a failure does not hold the engines' Stop.
	defer releaseOld()
	_, releaseNew, err := es.Use(r.ctx, "lab", again)
	if err != nil {
		t.Fatal(err)
	}
	releaseNew()
	ctx, cancel := context.WithTimeout(r.ctx, 200*time.Millisecond)
	err = es.Quiesce(ctx, "lab")
	cancel()
	if fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("Quiesce with a request on the replaced engine: %v, want unavailable once its bound ends", err)
	}
	releaseOld()
	if err := es.Quiesce(r.ctx, "lab"); err != nil {
		t.Fatalf("Quiesce once that request let it go: %v", err)
	}
}

// TestReplaceWaitsForADeleteInFlight guards a delete in flight when a
// replace is made: it has written its records, prepared, and its publish
// is then refused. The rebuild's drain (Quiesce) waits for the request to
// end, abandoned, before the build reads a scan, so the build reads its
// records abandoned and the session it never deleted is still in the
// catalog after the activation.
func TestReplaceWaitsForADeleteInFlight(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	scans := r.scans()
	s := scans[0]
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}
	es := r.engines(time.Hour, 24*time.Hour, nil)
	e, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	// The delete is held after its records until the build reads its first
	// object from meta/ (which a drain that waits for nothing lets happen at
	// once), or, a bounded time after the drain began, as a drain that waits
	// for the request would otherwise wait for ever.
	atRecords, go1 := make(chan struct{}), make(chan struct{})
	goOn := sync.OnceFunc(func() { close(go1) })
	var once sync.Once
	e.SetCrashAt(func(step string) error {
		if step == "records" {
			once.Do(func() { close(atRecords); <-go1 })
		}
		return nil
	})
	var ended atomic.Bool
	var del tenant.Correction
	var delErr error
	go func() {
		// As a request does: the engine is let go once the call returns.
		del, delErr = r.deleteNow(e, target)
		release()
		ended.Store(true)
	}()
	<-atRecords
	j := r.createReplace(t)
	var inFlight atomic.Bool
	st := &readHook{Store: r.storage, match: func(k string) bool { return strings.HasPrefix(k, "meta/") }, hook: func() {
		inFlight.Store(!ended.Load())
		goOn()
	}}
	got := r.runRebuild(t, j, st, func(ctx context.Context, name string) error {
		go func() {
			select {
			case <-time.After(200 * time.Millisecond):
				goOn()
			case <-go1:
			}
		}()
		return es.Quiesce(ctx, name)
	})
	if inFlight.Load() {
		t.Fatal("the build read the storage's meta/ while a correction request was in flight")
	}
	if delErr == nil || del.State == record.StateCommitted {
		t.Fatalf("the delete in flight was answered %s, %v; want its publish refused", del.State, delErr)
	}
	if c := r.state(del.ID); c.State != record.StateAbandoned {
		t.Fatalf("the delete in flight is %s, want abandoned", c.State)
	}
	if got.Rebuild == nil || !got.Rebuild.Activated {
		t.Fatalf("the replace ended %+v, want activated", got)
	}
	found := false
	for _, x := range r.scans() {
		if x.SubjectLabel == s.SubjectLabel && x.SessionLabel == s.SessionLabel {
			found = true
		}
	}
	if !found || len(r.scans()) != len(scans) {
		t.Fatalf("after the activation the catalog holds %d scans of %d, session %s/%s there: %v", len(r.scans()), len(scans),
			s.SubjectLabel, s.SessionLabel, found)
	}
}

// TestReplaceRefusesAPurgeDuringItsBuild guards an automatic purge, due,
// asked while a replace builds, between its read of a scan's manifest and
// of its metadata: the store refuses its start, so it rewrites nothing the
// build reads torn, the scan is in the catalog whole after the activation,
// and the delete, still committed, is purged once the rebuild has ended.
func TestReplaceRefusesAPurgeDuringItsBuild(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	o := r.scanWith(2)
	in := r.held(pathOf(o))[0]
	e := r.engine()
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: o.SubjectLabel, Session: o.SessionLabel, Scan: o.Label,
		Instance: in.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	later := r.engineWith(func(c *Config) { c.Now = func() time.Time { return time.Now().Add(72 * time.Hour) } })
	j := r.createReplace(t)
	mk, err := layout.MetadataKey(pathOf(o))
	if err != nil {
		t.Fatal(err)
	}
	var purged int
	var perr error
	st := &readHook{Store: r.storage, match: func(k string) bool { return k == mk }, hook: func() { purged, perr = later.PurgeDue(r.ctx) }}
	got := r.runRebuild(t, j, st, nil)
	if purged != 0 || fault.KindOf(perr) != fault.Precondition {
		t.Fatalf("a purge due during the build purged %d (%v), want its start refused", purged, perr)
	}
	if got.Rebuild == nil || !got.Rebuild.Activated {
		t.Fatalf("the replace ended %+v, want activated", got)
	}
	var after *tenant.CatalogScan
	for _, x := range r.scans() {
		if x.SeriesUID == o.SeriesUID {
			after = &x
		}
	}
	if after == nil || after.Instances != o.Instances-1 {
		t.Fatalf("after the activation the scan is %+v, want it with the %d instances its delete left", after, o.Instances-1)
	}
	if n, err := later.PurgeDue(r.ctx); err != nil || n != 1 {
		t.Fatalf("once the rebuild ended the purge due purged %d (%v), want 1", n, err)
	}
	if s := r.state(c.ID); s.State != record.StatePurged {
		t.Fatalf("the delete is %s, want purged", s.State)
	}
}

// TestEnginesPassWaitsForARebuild guards the pass while a rebuild is queued
// or running: it neither recovers nor abandons nor purges, so it writes no
// correction record and removes nothing the build reads; once the rebuild
// has ended the next pass takes up what it left.
func TestEnginesPassWaitsForARebuild(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	left := r.engine()
	left.SetCrashAt(crashAt("prepared"))
	if _, err := r.deleteNow(left, tenant.CorrectionTarget{Subject: r.scanWith(2).SubjectLabel}); err == nil {
		t.Fatal("the delete was not left prepared")
	}
	es := r.engines(time.Hour, 0, nil)
	j := r.createReplace(t)
	before := r.objects()
	es.pass()
	if c := r.onlyCorrection(); c.State != record.StatePrepared {
		t.Fatalf("a pass during a rebuild left the delete %s, want it prepared", c.State)
	}
	r.wantUnchanged(before, r.objects())
	if _, err := r.p.CancelJob(r.ctx, j.ID, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	es.pass()
	if c := r.onlyCorrection(); c.State != record.StateAbandoned {
		t.Fatalf("the pass after the rebuild left the delete %s, want it abandoned", c.State)
	}
}

// TestBackgroundWorkWaitsForARebuild guards work a request left to the
// background (a relabel's copy, a purge an admin started) asked while a
// rebuild is queued: it does not run, and does not hold the work lock, so
// a rebuild's drain is not held behind it; once the rebuild has ended it
// runs.
func TestBackgroundWorkWaitsForARebuild(t *testing.T) {
	r := newRig(t, false)
	es := r.engines(20*time.Millisecond, 24*time.Hour, nil)
	j := r.createReplace(t)
	ran := make(chan struct{})
	var during atomic.Bool
	es.Go("lab", r.p.(tenant.Store), "test", func(ctx context.Context, _ *Engine) error {
		running, err := r.p.RebuildRunning(ctx)
		during.Store(running || err != nil)
		close(ran)
		return nil
	})
	// Work that waits as it should is still waiting after this bound; work
	// that does not has run well within it.
	select {
	case <-ran:
	case <-time.After(500 * time.Millisecond):
	}
	if err := es.Quiesce(r.ctx, "lab"); err != nil {
		t.Fatalf("Quiesce with background work waiting for a rebuild: %v", err)
	}
	if _, err := r.p.CancelJob(r.ctx, j.ID, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("background work did not run once the rebuild ended")
	}
	if during.Load() {
		t.Fatal("background work ran while a rebuild was queued")
	}
}
