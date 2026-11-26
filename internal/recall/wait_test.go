package recall

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRecallNotOnlineWithinWaitIsGivenUp guards recall.wait: a recall the
// store works on is polled while younger than the wait; once the wait since
// its request has passed and the object is still not online, it is recorded
// failed with the fixed reason and polled no more; the wait is read at each
// pass, so lengthening it keeps the next recall going; and an object that
// is online at the look past the wait is recorded online, not given up.
func TestRecallNotOnlineWithinWaitIsGivenUp(t *testing.T) {
	f := newFixture(t)
	var wait atomic.Int64
	wait.Store(int64(2 * time.Hour))
	w := f.worker(func(c *Config) {
		c.Wait = func(context.Context) (time.Duration, error) { return time.Duration(wait.Load()), nil }
	})
	f.store.set(func(s *coldStore) { s.answer, s.state = blob.Recall{State: blob.RecallStarted}, blob.Restoring })
	f.ask(keyA)
	f.once(w)
	f.clock.Add(119 * time.Minute)
	if p := f.once(w); p.Polled != 1 || p.Failed != 0 {
		t.Fatalf("a poll within the wait = %+v, want a poll and no failure", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallInProgress {
		t.Fatalf("within the wait the row reads %+v", r)
	}
	f.clock.Add(time.Minute)
	if p := f.once(w); p.Failed != 1 {
		t.Fatalf("a poll at the wait = %+v, want the recall failed", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallFailed || r.Reason != ReasonTimedOut {
		t.Fatalf("past the wait the row reads %+v, want failed: %q", r, ReasonTimedOut)
	}
	f.clock.Add(time.Hour)
	if p := f.once(w); p.Due != 0 {
		t.Fatalf("a recall given up is still due: %+v", p)
	}

	// Asked again (a failed row becomes requested), with the wait now
	// longer: the same age no longer gives it up.
	f.ask(keyA)
	wait.Store(int64(10 * time.Hour))
	f.clock.Add(3 * time.Hour)
	if p := f.once(w); p.Failed != 0 {
		t.Fatalf("with the wait lengthened the pass = %+v, want no failure", p)
	}
	f.clock.Add(8 * time.Hour)
	f.store.set(func(s *coldStore) { s.state = blob.Online })
	if p := f.once(w); p.Online != 1 || p.Failed != 0 {
		t.Fatalf("an object online past the wait = %+v, want it online", p)
	}
}

// TestTemporaryFailurePastWaitIsGivenUp guards a store that keeps failing
// for a reason that would pass (it does not answer): the recall is retried
// while younger than recall.wait and recorded failed with the fixed reason
// once the wait has passed.
func TestTemporaryFailurePastWaitIsGivenUp(t *testing.T) {
	f := newFixture(t)
	w := f.worker(func(c *Config) {
		c.Wait = func(context.Context) (time.Duration, error) { return time.Hour, nil }
	})
	f.store.set(func(s *coldStore) { s.recallErr = fault.New(fault.Unavailable, "the test's storage does not answer") })
	f.ask(keyA)
	if p := f.once(w); p.Retried != 1 {
		t.Fatalf("a store that does not answer = %+v, want a retry", p)
	}
	f.clock.Add(time.Hour)
	if p := f.once(w); p.Failed != 1 || p.Retried != 0 {
		t.Fatalf("past the wait = %+v, want the recall failed", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallFailed || r.Reason != ReasonTimedOut {
		t.Fatalf("the row reads %+v", r)
	}
}
