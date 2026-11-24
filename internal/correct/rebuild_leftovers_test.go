package correct

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The corrections a stop or a failure left half done when a replace is
// made, which no request holds and no Recover has taken up (the restarted
// server's pass and Recover wait for the rebuild): each must be read by
// the build as what the database says, never as its record says. Each case
// runs both ways the build is kept right, each alone: the drain's settle
// (QuiesceForRebuild, the record read as it is), and the build reading
// each record as its row says (rebuild.ByRow, no settle).

// leftoverWay is one way a replace is kept from applying what was left.
type leftoverWay struct {
	name   string
	drain  func(es *Engines) func(context.Context, string) error
	decide func(context.Context, tenant.Pipeline, record.Correction) (record.Correction, bool, error)
}

// leftoverWays are the drain's settle and the build's reading by row.
var leftoverWays = []leftoverWay{
	{name: "settle", drain: func(es *Engines) func(context.Context, string) error { return es.QuiesceForRebuild }},
	{name: "byRow", drain: func(*Engines) func(context.Context, string) error { return nil }, decide: rebuild.ByRow},
}

// sessionHeld reports whether the catalog holds a scan of the session.
func (r *rig) sessionHeld(subject, session string) bool {
	for _, x := range r.scans() {
		if x.SubjectLabel == subject && x.SessionLabel == session {
			return true
		}
	}
	return false
}

// replaceOnRestart makes a replace and runs it as a restarted server does:
// fresh engines, their pass running (it waits for the rebuild), and the
// rebuild kept right as way says; it fails unless the replace activated.
func (r *rig) replaceOnRestart(t *testing.T, way leftoverWay) {
	t.Helper()
	j := r.createReplace(t)
	es := r.engines(50*time.Millisecond, 24*time.Hour, nil)
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.runRebuildWith(t, j, r.storage, way.drain(es), way.decide)
	if got.Rebuild == nil || !got.Rebuild.Activated {
		t.Fatalf("the replace ended %s (%s), want activated", got.State, got.Reason)
	}
}

// TestReplaceKeepsADeleteAStopLeftPrepared guards a delete whose records
// were written and whose publish never ran (the server stopped): the
// replace made then keeps the session it never deleted in the catalog;
// with the settle, the delete is abandoned and its records say so before
// the build reads them.
func TestReplaceKeepsADeleteAStopLeftPrepared(t *testing.T) {
	for _, way := range leftoverWays {
		t.Run(way.name, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			scans := r.scans()
			s := scans[0]
			e := r.engine()
			e.SetCrashAt(crashAt("records"))
			del, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel})
			if err == nil {
				t.Fatal("the delete was not left prepared")
			}
			if rec := r.correctionRecord(del.ID); rec.State != record.StatePrepared {
				t.Fatalf("the delete's record says %s, want prepared", rec.State)
			}
			r.replaceOnRestart(t, way)
			if !r.sessionHeld(s.SubjectLabel, s.SessionLabel) || len(r.scans()) != len(scans) {
				t.Fatalf("session %s/%s, never deleted (correction %s), after the activation: %d scans of %d", s.SubjectLabel,
					s.SessionLabel, r.state(del.ID).State, len(r.scans()), len(scans))
			}
			if way.name == "settle" {
				if c := r.state(del.ID); c.State != record.StateAbandoned || r.correctionRecord(del.ID).State != record.StateAbandoned {
					t.Fatalf("after the settle the delete is %s, its record %s; want both abandoned", c.State, r.correctionRecord(del.ID).State)
				}
			}
		})
	}
}

// TestReplaceKeepsADeleteWhoseUndoRecordLags guards an undone delete whose
// records still say committed (the undo's rewrite of them never ran): the
// replace keeps its session in the catalog; with the settle, the records
// say undone before the build reads them.
func TestReplaceKeepsADeleteWhoseUndoRecordLags(t *testing.T) {
	for _, way := range leftoverWays {
		t.Run(way.name, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			scans := r.scans()
			s := scans[0]
			del, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel})
			if err != nil {
				t.Fatal(err)
			}
			e := r.engine()
			e.SetCrashAt(crashAt("undone"))
			if _, err := e.Undo(r.ctx, del.ID, "the wrong session", r.audit(), nil); err == nil {
				t.Fatal("the undo's records were rewritten")
			}
			if rec := r.correctionRecord(del.ID); rec.State != record.StateCommitted || r.state(del.ID).State != record.StateUndone {
				t.Fatalf("the delete is %s, its record %s; want undone, its record committed", r.state(del.ID).State, rec.State)
			}
			r.replaceOnRestart(t, way)
			if !r.sessionHeld(s.SubjectLabel, s.SessionLabel) || len(r.scans()) != len(scans) {
				t.Fatalf("session %s/%s, its delete undone, after the activation: %d scans of %d", s.SubjectLabel, s.SessionLabel,
					len(r.scans()), len(scans))
			}
			if way.name == "settle" && r.correctionRecord(del.ID).State != record.StateUndone {
				t.Fatalf("after the settle the delete's record says %s, want undone", r.correctionRecord(del.ID).State)
			}
		})
	}
}

// TestReplaceRollsBackAReplaceAStopLeft guards a correction's replace that
// wrote its new revision and the scan's manifest naming it, and never
// published (the server stopped): the drain's settle abandons it and writes
// the manifest back before the build, so the catalog after the activation
// names the revision the storage holds, not the one never published. (The
// build reading by row does not cover this: a replace's record hides
// nothing, and the manifest is what names a revision.)
func TestReplaceRollsBackAReplaceAStopLeft(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	req, newKey := r.replaceRequest()
	e := r.engine()
	e.SetCrashAt(crashAt("meta"))
	if _, err := e.Replace(r.ctx, req); err == nil {
		t.Fatal("the replace was not left prepared")
	}
	c := r.onlyCorrection()
	r.replaceOnRestart(t, leftoverWays[0])
	if s := r.state(c.ID); s.State != record.StateAbandoned {
		t.Fatalf("after the settle the replace is %s, want abandoned", s.State)
	}
	found := false
	for _, in := range r.held(req.Path) {
		if in.SOPInstanceUID != req.UID {
			continue
		}
		found = true
		if in.Key == newKey {
			t.Fatalf("after the activation the catalog names %s, the revision of a replace never published", in.Key)
		}
		if _, err := r.storage.Stat(r.ctx, in.Key); err != nil {
			t.Fatalf("after the activation the catalog names %s, which the storage does not hold: %v", in.Key, err)
		}
	}
	if !found {
		t.Fatal("after the activation the catalog lost the instance the replace never replaced")
	}
}

// TestReplaceStoppedMidBuildSettlesAgain guards a server stopped while a
// replace builds, with a delete a stop left prepared before it: the build
// goes on from where it stopped on the restarted server, whose drain
// settles again before it (finding nothing more to do), and the session
// never deleted is in the catalog after the activation.
func TestReplaceStoppedMidBuildSettlesAgain(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	scans := r.scans()
	s := scans[0]
	e := r.engine()
	e.SetCrashAt(crashAt("records"))
	if _, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}); err == nil {
		t.Fatal("the delete was not left prepared")
	}
	j := r.createReplace(t)
	es1 := r.engines(time.Hour, 24*time.Hour, nil)
	// The first server stops at the build's first read of a scan.
	stopped := make(chan struct{})
	var once atomic.Bool
	st := &readHook{Store: r.storage, match: func(k string) bool {
		return strings.HasPrefix(k, "meta/") && !strings.HasPrefix(k, "meta/.corrections")
	}, hook: func() {
		if once.CompareAndSwap(false, true) {
			close(stopped)
		}
	}}
	sup, err := rebuild.New(rebuild.Config{Tenants: oneTenant{p: r.p}, OpenStorages: func(string, tenant.Pipeline) rebuild.Storages {
		return storages{s: st}
	}, PollInterval: 20 * time.Millisecond, Interval: time.Second, Keys: r.keys,
		TenantID: func(context.Context, string) (string, error) { return rigTenantID, nil }, Drain: es1.QuiesceForRebuild})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(60 * time.Second):
		t.Fatal("the build did not start")
	}
	ctx, cancel := context.WithTimeout(r.ctx, 100*time.Millisecond)
	_ = sup.Stop(ctx) // Cut short, as a stop does.
	cancel()
	if err := es1.Stop(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.p.Job(r.ctx, j.ID); got.State.Ended() {
		t.Fatalf("the replace ended though its server stopped: %s", got.State)
	}
	es2 := r.engines(50*time.Millisecond, 24*time.Hour, nil)
	if err := es2.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	got := r.runRebuildWith(t, j, r.storage, es2.QuiesceForRebuild, rebuild.ByRow)
	if got.Rebuild == nil || !got.Rebuild.Activated {
		t.Fatalf("the resumed replace ended %s (%s), want activated", got.State, got.Reason)
	}
	if !r.sessionHeld(s.SubjectLabel, s.SessionLabel) || len(r.scans()) != len(scans) {
		t.Fatalf("session %s/%s, never deleted, after the resumed replace: %d scans of %d", s.SubjectLabel, s.SessionLabel,
			len(r.scans()), len(scans))
	}
}

// TestBackgroundWorkOnAReplacedEngineIsDropped guards work queued on an
// engine whose database is then closed and opened anew (the tenant removed
// and made again under its name): asking whether a rebuild runs fails on
// the closed database, and the work, no longer on the tenant's engine, is
// dropped rather than waiting for ever, so the replaced engine is let go
// and the tenant is quiet again.
func TestBackgroundWorkOnAReplacedEngineIsDropped(t *testing.T) {
	r := newRig(t, false)
	old, err := (driver.SQLiteTenants{DataDir: r.dataDir}).Open(r.ctx, rigTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	es := r.engines(50*time.Millisecond, 24*time.Hour, nil)
	block, started := make(chan struct{}), make(chan struct{})
	es.Go("lab", old, "first", func(context.Context, *Engine) error { close(started); <-block; return nil })
	<-started
	var ran atomic.Bool
	es.Go("lab", old, "queued", func(context.Context, *Engine) error { ran.Store(true); return nil })
	nr, err := es.acquire(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		close(block)
		t.Fatal(err)
	}
	nr.holds.Done()
	_ = old.Close()
	close(block)
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	if err := es.Quiesce(ctx, "lab"); err != nil {
		t.Fatalf("Quiesce with no rebuild, once the replaced engine's work ended: %v", err)
	}
	if ran.Load() {
		t.Fatal("work queued on a closed database ran")
	}
}

// TestACorrectionRequestDuringARebuildIsRefusedAtOnce guards a request on a
// restarted server while a rebuild is queued: Recover waits for the
// rebuild, and the request is answered the precondition at once, not after
// it waited out RecoverWait.
func TestACorrectionRequestDuringARebuildIsRefusedAtOnce(t *testing.T) {
	r := newRig(t, false)
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: time.Hour, RecoverWait: 10 * time.Second,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }}, nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	j := r.createReplace(t)
	start := time.Now()
	_, _, err = es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "rebuild") {
		t.Fatalf("a request while a rebuild is queued: %v, want the precondition that a rebuild runs", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("the request waited %v", waited)
	}
	if _, err := r.p.CancelJob(r.ctx, j.ID, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	_, release, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatalf("a request once the rebuild ended: %v", err)
	}
	release()
}

// TestALaggingRecordAStorageCannotTakeDoesNotHoldAReplace guards the settle
// before a replace when a record that lags its row cannot be rewritten (its
// storage cannot be opened): the drain goes on, since the build reads each
// record as its row says, and the session whose delete was undone, though
// its record still says committed, is in the catalog after the activation.
func TestALaggingRecordAStorageCannotTakeDoesNotHoldAReplace(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	scans := r.scans()
	s := scans[0]
	del, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel})
	if err != nil {
		t.Fatal(err)
	}
	e := r.engine()
	e.SetCrashAt(crashAt("undone"))
	if _, err := e.Undo(r.ctx, del.ID, "the wrong session", r.audit(), nil); err == nil {
		t.Fatal("the undo's records were rewritten")
	}
	es, err := NewEngines(EnginesConfig{Tenants: oneTenant{r.p}, Interval: time.Hour,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) {
				return nil, fault.New(fault.Unavailable, "the storage does not answer")
			}}, nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	j := r.createReplace(t)
	got := r.runRebuildWith(t, j, r.storage, es.QuiesceForRebuild, rebuild.ByRow)
	if got.Rebuild == nil || !got.Rebuild.Activated {
		t.Fatalf("the replace ended %s (%s), want activated", got.State, got.Reason)
	}
	if rec := r.correctionRecord(del.ID); rec.State != record.StateCommitted {
		t.Fatalf("the record says %s; the storage could not be written, so it should still lag at committed", rec.State)
	}
	if !r.sessionHeld(s.SubjectLabel, s.SessionLabel) || len(r.scans()) != len(scans) {
		t.Fatalf("session %s/%s, its delete undone, after the activation: %d scans of %d", s.SubjectLabel, s.SessionLabel,
			len(r.scans()), len(scans))
	}
}

// switchTenant is the installation of one tenant whose database can be
// swapped for one opened anew; asked marks the first ask for it.
type switchTenant struct {
	mu    sync.Mutex
	st    tenant.Store
	asked chan struct{}
	once  sync.Once
}

// Names answers the one tenant.
func (s *switchTenant) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers its database as it is now.
func (s *switchTenant) Store(string) (tenant.Store, error) {
	s.once.Do(func() { close(s.asked) })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st, nil
}

// TestQuiesceForRebuildFollowsAReplacedEngine guards the drain's wait when
// the tenant's engine is replaced while it waits (its database opened
// anew): the wait takes up the new engine, and returns once the request on
// the old one has let it go, rather than wait for the old engine, no longer
// the tenant's, until its bound.
func TestQuiesceForRebuildFollowsAReplacedEngine(t *testing.T) {
	r := newRig(t, false)
	again, err := (driver.SQLiteTenants{DataDir: r.dataDir}).Open(r.ctx, rigTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	ts := &switchTenant{st: r.p.(tenant.Store), asked: make(chan struct{})}
	es, err := NewEngines(EnginesConfig{Tenants: ts, Interval: time.Hour,
		Configure: func(context.Context, string, tenant.Corrections) (Config, func() error, error) {
			return Config{TenantID: rigTenantID, Keys: r.keys, OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil }}, nil, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	_, releaseOld, err := es.Use(r.ctx, "lab", r.p.(tenant.Store))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOld()
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- es.QuiesceForRebuild(ctx, "lab") }()
	<-ts.asked
	// The tenant's database is opened anew, and a request on it makes its
	// engine; then the request on the old engine ends.
	ts.mu.Lock()
	ts.st = again
	ts.mu.Unlock()
	_, releaseNew, err := es.Use(r.ctx, "lab", again)
	if err != nil {
		t.Fatal(err)
	}
	releaseNew()
	releaseOld()
	if err := <-done; err != nil {
		t.Fatalf("the drain's wait across an engine replaced: %v", err)
	}
}
