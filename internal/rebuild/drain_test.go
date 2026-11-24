package rebuild

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// drainLog records, in order, what a unit asked of Config.Drain and
// Config.Resume and when it wrote its first scan, for the tests below.
type drainLog struct {
	mu     sync.Mutex
	events []string
	// fail, when set, is what every Drain answers.
	fail error
}

// add appends one event.
func (l *drainLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// list returns the events so far.
func (l *drainLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// wire gives cfg a Drain, a Resume and an after-scan seam that record into
// l, the first two with the tenant's name.
func (l *drainLog) wire(cfg Config) Config {
	cfg.Drain = func(_ context.Context, name string) error {
		l.add("drain " + name)
		return l.fail
	}
	cfg.Resume = func(name string) { l.add("resume " + name) }
	return withAfterScan(cfg, func(string) { l.add("scan") })
}

// index returns where e first is in events, -1 when it is not.
func index(events []string, e string) int {
	for i, x := range events {
		if x == e {
			return i
		}
	}
	return -1
}

// TestReplaceDrainsBeforeItBuilds guards the order of a replace: the
// tenant's writers are drained before its first scan is written into the
// shadow, so nothing archived meanwhile is missed by it and then dropped at
// the activation; the receivers are told to look again once the unit
// stops; and a compare, which changes no catalog, drains nothing. On
// SQLite and on Postgres.
func TestReplaceDrainsBeforeItBuilds(t *testing.T) {
	drivers(t, testReplaceDrainsBeforeItBuilds)
}

// testReplaceDrainsBeforeItBuilds is TestReplaceDrainsBeforeItBuilds on
// one driver.
func testReplaceDrainsBeforeItBuilds(t *testing.T, postgres bool) {
	r := newRig(t, postgres)
	r.archive(studyFiles...)
	var writes atomic.Int64
	l := &drainLog{}
	j, tallies := r.run(l.wire(r.config(&writes)), tenant.RebuildReplace)
	if !j.State.Ended() || tallies[tenant.TallyActivated] != 1 {
		t.Fatalf("the replace ended %+v with %v, want activated", j, tallies)
	}
	events := l.list()
	drain, scan, resume := index(events, "drain lab"), index(events, "scan"), index(events, "resume lab")
	if drain < 0 || scan < 0 || drain > scan {
		t.Fatalf("the replace's events are %v, want the drain before the first scan", events)
	}
	if resume < scan {
		t.Fatalf("the replace's events are %v, want the receivers told after its work", events)
	}
	l = &drainLog{}
	if j, _ = r.run(l.wire(r.config(&writes)), tenant.RebuildCompare); j.State != tenant.JobDone {
		t.Fatalf("the compare ended %+v", j)
	}
	if events := l.list(); index(events, "drain lab") >= 0 || index(events, "scan") < 0 {
		t.Fatalf("the compare's events are %v, want scans and no drain", events)
	}
	if writes.Load() != 0 {
		t.Fatalf("a rebuild wrote %d times to a storage", writes.Load())
	}
}

// TestReplaceResumedDrainsAgain guards a replace whose build is stopped
// between two scans, as by a crash: the job is not ended, so its pause
// holds, and the next supervisor, as after a restart, drains the tenant's
// writers again before it writes the next scan, then goes on from the
// shadow's cursor to the activation.
func TestReplaceResumedDrainsAgain(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	j := r.createRebuild(tenant.RebuildReplace)
	var writes atomic.Int64
	l1 := &drainLog{}
	var scans int
	hold, held := make(chan struct{}), make(chan struct{})
	cfg := withAfterScan(l1.wire(r.config(&writes)), func(string) {
		if scans++; scans == 2 {
			close(held)
			<-hold
		}
	})
	s := r.start(cfg)
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = s.Stop(ctx) // Cut short: the unit is held between two scans.
	cancel()
	close(hold)
	if replacing, err := r.p.RebuildReplacing(r.ctx); err != nil || !replacing {
		t.Fatalf("after the stop RebuildReplacing answers %v, %v, want the pause held", replacing, err)
	}
	l2 := &drainLog{}
	end := r.waitEnded(r.start(l2.wire(r.config(&writes))), j.ID)
	if tallies := r.tallies(j.ID); !end.State.Ended() || tallies[tenant.TallyActivated] != 1 {
		t.Fatalf("the resumed replace ended %+v with %v, want activated", end, tallies)
	}
	events := l2.list()
	if d, s := index(events, "drain lab"), index(events, "scan"); d < 0 || s < 0 || d > s {
		t.Fatalf("the resumed replace's events are %v, want a drain before its next scan", events)
	}
	if replacing, err := r.p.RebuildReplacing(r.ctx); err != nil || replacing {
		t.Fatalf("after the activation RebuildReplacing answers %v, %v, want the pause lifted", replacing, err)
	}
}

// TestReplaceWaitsForADrain guards a drain that does not end: the unit's
// attempt fails with a reason that says so, nothing is written into the
// shadow, the catalog in use is as it was, the job is not ended (so the
// pause it holds stays until it is), and the receivers are told to look
// again.
func TestReplaceWaitsForADrain(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	before := r.catalogText()
	var writes atomic.Int64
	l := &drainLog{fail: fault.Wrap(fault.Unavailable, errors.New("still running"), "the tenant's reception pipeline did not stop in time")}
	j := r.createRebuild(tenant.RebuildReplace)
	s := r.start(l.wire(r.config(&writes)))
	deadline := time.Now().Add(60 * time.Second)
	for {
		w, err := r.p.Work(r.ctx, tenant.JobUnitID(j.ID, ""))
		if err != nil {
			t.Fatal(err)
		}
		if w.LastError != "" {
			if w.LastError != reasonDrain {
				t.Fatalf("the unit's attempt failed with %q, want %q", w.LastError, reasonDrain)
			}
			break
		}
		if w.State == tenant.WorkDone || time.Now().After(deadline) {
			t.Fatalf("the unit's attempt did not fail: %+v", w)
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	events := l.list()
	if index(events, "scan") >= 0 || index(events, "drain lab") < 0 || index(events, "resume lab") < 0 {
		t.Fatalf("the events are %v, want a drain, no scan, and the receivers told", events)
	}
	got, err := r.p.Job(r.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Ended() {
		t.Fatalf("the replace whose drain did not end is %s, want it not ended", got.State)
	}
	if replacing, err := r.p.RebuildReplacing(r.ctx); err != nil || !replacing {
		t.Fatalf("RebuildReplacing answers %v, %v, want the pause held", replacing, err)
	}
	if r.catalogText() != before {
		t.Fatal("a replace whose drain did not end changed the catalog in use")
	}
}
