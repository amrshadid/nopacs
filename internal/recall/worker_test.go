package recall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const keyA = "data/s1/a.dcm"

// TestColdObjectIsAskedOnceThenPolledUntilOnline guards the whole life of a
// recall: the store is asked once with the row's days and speed, the row
// waits in progress while the object is restoring (no second ask), and goes
// online with the store's own time when the object reads.
func TestColdObjectIsAskedOnceThenPolledUntilOnline(t *testing.T) {
	f := newFixture(t)
	w := f.worker(nil)
	f.store.set(func(s *coldStore) { s.answer = blob.Recall{State: blob.RecallStarted} })
	f.ask(keyA)

	if p := f.once(w); p.Asked != 1 || p.Due != 1 {
		t.Fatalf("first pass = %+v, want one ask", p)
	}
	r := f.row(keyA)
	if r.State != tenant.RecallInProgress || !r.Due.Equal(f.clock.Now().Add(time.Minute)) {
		t.Fatalf("after the ask the row reads %+v, want in progress due a poll later", r)
	}
	if got := f.store.last; got != (blob.RecallRequest{Days: 3, Speed: blob.RecallBulk}) {
		t.Fatalf("the store was asked %+v, want the row's days and speed", got)
	}
	if p := f.once(w); p.Due != 0 {
		t.Fatalf("a pass before the poll is due read %+v, want nothing due", p)
	}

	f.clock.Add(time.Minute)
	f.store.set(func(s *coldStore) { s.state = blob.Restoring })
	if p := f.once(w); p.Polled != 1 || p.Asked != 0 {
		t.Fatalf("poll of a restoring object = %+v, want a poll and no ask", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallInProgress {
		t.Fatalf("a restoring object's row reads %+v", r)
	}

	f.clock.Add(time.Minute)
	until := f.clock.Now().Add(72 * time.Hour).Truncate(time.Second)
	f.store.set(func(s *coldStore) { s.state, s.until = blob.Online, until })
	if p := f.once(w); p.Online != 1 {
		t.Fatalf("poll of an online object = %+v, want online", p)
	}
	r = f.row(keyA)
	if r.State != tenant.RecallOnline || !r.OnlineUntil.Equal(until) || !r.Readable(f.clock.Now()) {
		t.Fatalf("an online object's row reads %+v, want online until %v", r, until)
	}
	if n := f.store.recalls.Load(); n != 1 {
		t.Fatalf("the store was asked %d times, want 1", n)
	}
	if p := f.once(w); p.Due != 0 {
		t.Fatalf("a pass after the object is online read %+v, want nothing due", p)
	}
}

// TestObjectReadingAtOnceIsRecordedOnline guards a store that answers a
// recall with RecallOnline (the object never left, or was already back): the
// row is online with the answer's time and no poll follows.
func TestObjectReadingAtOnceIsRecordedOnline(t *testing.T) {
	f := newFixture(t)
	until := f.clock.Now().Add(24 * time.Hour)
	f.store.set(func(s *coldStore) { s.answer = blob.Recall{State: blob.RecallOnline, OnlineUntil: until} })
	f.ask(keyA)
	if p := f.once(f.worker(nil)); p.Online != 1 {
		t.Fatalf("pass = %+v, want online", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallOnline || !r.OnlineUntil.Equal(until) {
		t.Fatalf("row reads %+v", r)
	}
}

// TestSharedRecallIsAskedOnce guards that however many requests wait for an
// object, the store is asked once: the port joins them in one row, and the
// worker asks only for a row not yet asked.
func TestSharedRecallIsAskedOnce(t *testing.T) {
	f := newFixture(t)
	f.store.set(func(s *coldStore) { s.answer = blob.Recall{State: blob.RecallStarted} })
	f.ask(keyA, "data/s1/b.dcm")
	if got := f.ask(keyA); got.Joined != 1 {
		t.Fatalf("a second request answered %+v, want it joined", got)
	}
	f.once(f.worker(nil))
	if got := f.ask(keyA); got.Joined != 1 {
		t.Fatalf("a request after the ask answered %+v, want it joined", got)
	}
	if n := f.store.recalls.Load(); n != 2 {
		t.Fatalf("the store was asked %d times for two objects, want 2", n)
	}
}

// TestTemporaryFailureIsRetriedLater guards the wait after a store that
// cannot answer now: the row stays live, due after a wait that doubles with
// each failure in a row, and the ask is made again once the store answers,
// without the store's text on the row.
func TestTemporaryFailureIsRetriedLater(t *testing.T) {
	f := newFixture(t)
	w := f.worker(nil)
	f.ask(keyA)
	f.store.set(func(s *coldStore) {
		s.recallErr = fault.Wrap(fault.Unavailable, errors.New("503 secret-store-text"), "the storage is unavailable")
	})
	if p := f.once(w); p.Retried != 1 {
		t.Fatalf("pass = %+v, want a retry", p)
	}
	r := f.row(keyA)
	if !r.Live() || !r.Due.Equal(f.clock.Now().Add(30*time.Second)) || r.Reason != "" {
		t.Fatalf("after a temporary failure the row reads %+v, want live, due in 30s", r)
	}
	f.clock.Add(30 * time.Second)
	f.store.set(func(s *coldStore) { s.statErr = fault.New(fault.Unavailable, "down") })
	f.once(w)
	if r := f.row(keyA); !r.Due.Equal(f.clock.Now().Add(time.Minute)) {
		t.Fatalf("after a second failure the row is due %v from now, want the wait doubled", r.Due.Sub(f.clock.Now()))
	}
	f.clock.Add(time.Minute)
	f.store.set(func(s *coldStore) {
		s.statErr, s.recallErr, s.answer = nil, nil, blob.Recall{State: blob.RecallStarted}
	})
	f.once(w)
	r = f.row(keyA)
	if r.State != tenant.RecallInProgress || !r.Due.Equal(f.clock.Now().Add(time.Minute)) {
		t.Fatalf("after the store answered the row reads %+v", r)
	}
	if n := f.store.recalls.Load(); n != 2 {
		t.Fatalf("the store was asked %d times (one failed), want 2: the object was cold, so the lost ask is made again", n)
	}
}

// TestRetryWaitIsBounded guards the cap on the wait after repeated failures.
func TestRetryWaitIsBounded(t *testing.T) {
	f := newFixture(t)
	w := f.worker(func(c *Config) { c.RetryMin, c.RetryMax = time.Second, 4*time.Second })
	f.ask(keyA)
	f.store.set(func(s *coldStore) { s.recallErr = fault.New(fault.Unavailable, "down") })
	var last time.Duration
	for i := 0; i < 6; i++ {
		f.once(w)
		last = f.row(keyA).Due.Sub(f.clock.Now())
		f.clock.Add(last)
		f.store.set(func(s *coldStore) { s.statErr = fault.New(fault.Unavailable, "down") })
	}
	if last != 4*time.Second {
		t.Fatalf("the wait after many failures is %v, want the 4s cap", last)
	}
}

// TestRefusalFailsWithFixedReason guards a store that will not take the
// recall as asked: the row is failed with the worker's own phrase, never the
// store's text, and the store is not asked again.
func TestRefusalFailsWithFixedReason(t *testing.T) {
	f := newFixture(t)
	f.ask(keyA)
	f.store.set(func(s *coldStore) {
		s.recallErr = fault.Wrap(fault.Unavailable, errors.New("expedited retrievals are not available for DEEP_ARCHIVE bucket-xyz"), "refused")
		s.recallErr = errors.Join(s.recallErr, blob.ErrRecallRefused)
	})
	w := f.worker(nil)
	if p := f.once(w); p.Failed != 1 {
		t.Fatalf("pass = %+v, want a failure", p)
	}
	r := f.row(keyA)
	if r.State != tenant.RecallFailed || r.Reason != ReasonRefused || strings.Contains(r.Reason, "DEEP") {
		t.Fatalf("row reads %+v, want failed with the fixed reason", r)
	}
	f.clock.Add(time.Hour)
	if p := f.once(w); p.Due != 0 || f.store.recalls.Load() != 1 {
		t.Fatalf("after a failure the store was asked again: pass %+v, %d asks", p, f.store.recalls.Load())
	}
}

// TestStoreWithoutRecallIsFailed guards a store that cannot recall: the row
// is failed with a fixed reason rather than retried for ever.
func TestStoreWithoutRecallIsFailed(t *testing.T) {
	f := newFixture(t)
	f.open = func(context.Context, string) (blob.Store, error) { return plainStore{}, nil }
	f.ask(keyA)
	if p := f.once(f.worker(nil)); p.Failed != 1 {
		t.Fatalf("pass = %+v, want a failure", p)
	}
	if r := f.row(keyA); r.State != tenant.RecallFailed || r.Reason != ReasonNotRecaller {
		t.Fatalf("row reads %+v", r)
	}
}

// TestMissingObjectIsFailed guards an object the store does not hold: a
// recall cannot bring it back, so the row fails with a fixed reason.
func TestMissingObjectIsFailed(t *testing.T) {
	f := newFixture(t)
	f.ask(keyA)
	f.store.set(func(s *coldStore) { s.recallErr = fault.New(fault.NotFound, "no such object") })
	f.once(f.worker(nil))
	if r := f.row(keyA); r.State != tenant.RecallFailed || r.Reason != ReasonMissing {
		t.Fatalf("row reads %+v", r)
	}
}

// TestUnopenableStorage guards the opener's failures: a storage that is not
// configured any more fails its recalls; one that cannot be reached now is
// retried.
func TestUnopenableStorage(t *testing.T) {
	f := newFixture(t)
	f.ask(keyA)
	w := f.worker(nil)
	f.open = func(context.Context, string) (blob.Store, error) {
		return nil, fault.New(fault.Unavailable, "no route")
	}
	if p := f.once(w); p.Retried != 1 {
		t.Fatalf("pass = %+v, want a retry", p)
	}
	f.clock.Add(time.Minute)
	f.open = func(context.Context, string) (blob.Store, error) {
		return nil, fault.New(fault.NotFound, "no such storage")
	}
	f.once(w)
	if r := f.row(keyA); r.State != tenant.RecallFailed || r.Reason != ReasonUnopenable {
		t.Fatalf("row reads %+v", r)
	}
}

// TestStopLeavesRowsForTheNextPass guards a stop in the middle of a pass:
// the call that was cut short records nothing, so its row stays as it was
// and is asked by the next worker; rows not reached stay due.
func TestStopLeavesRowsForTheNextPass(t *testing.T) {
	f := newFixture(t)
	f.ask(keyA, "data/s1/b.dcm", "data/s1/c.dcm")
	ctx, cancel := context.WithCancel(f.ctx)
	f.store.set(func(s *coldStore) {
		s.onRecall = func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }
		s.answer = blob.Recall{State: blob.RecallStarted}
	})
	p, err := f.worker(nil).RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stopped != 3 || p.Retried != 0 || p.Failed != 0 {
		t.Fatalf("pass = %+v, want the three rows stopped", p)
	}
	for _, k := range []string{keyA, "data/s1/b.dcm", "data/s1/c.dcm"} {
		if r := f.row(k); r.State != tenant.RecallRequested {
			t.Fatalf("after a stop %s reads %+v, want it still requested", k, r)
		}
	}
	f.store.set(func(s *coldStore) { s.onRecall = nil })
	if p := f.once(f.worker(nil)); p.Asked != 3 {
		t.Fatalf("the next pass = %+v, want the three asked", p)
	}
}

// TestColdAgainWhileInProgressAsksAgain guards an object found cold while
// its row says a restore runs (the store lost it): the store is asked
// again, and a store that already runs one answers with it.
func TestColdAgainWhileInProgressAsksAgain(t *testing.T) {
	f := newFixture(t)
	f.store.set(func(s *coldStore) { s.answer = blob.Recall{State: blob.RecallStarted} })
	f.ask(keyA)
	w := f.worker(nil)
	f.once(w)
	f.clock.Add(time.Minute)
	f.store.set(func(s *coldStore) { s.state, s.answer = blob.Archived, blob.Recall{State: blob.RecallInProgress} })
	f.once(w)
	if n := f.store.recalls.Load(); n != 2 {
		t.Fatalf("the store was asked %d times, want 2", n)
	}
	if r := f.row(keyA); r.State != tenant.RecallInProgress {
		t.Fatalf("row reads %+v", r)
	}
}

// TestRunAnswersAWakeAndStops guards the loop: a request woken is answered
// without waiting for the idle poll, and Run returns nil when ctx ends.
func TestRunAnswersAWakeAndStops(t *testing.T) {
	f := newFixture(t)
	f.store.set(func(s *coldStore) { s.answer = blob.Recall{State: blob.RecallStarted} })
	w := f.worker(nil)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	f.ask(keyA)
	deadline := time.Now().Add(10 * time.Second)
	for f.store.recalls.Load() == 0 {
		w.Wake()
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("a woken worker did not ask the store")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// TestNewRefusesBadConfig guards the required parts and the bounds.
func TestNewRefusesBadConfig(t *testing.T) {
	f := newFixture(t)
	good := Config{Queue: f.rc.(Queue), Open: f.open, Now: f.clock.Now}
	if _, err := New(good); err != nil {
		t.Fatalf("a good config was refused: %v", err)
	}
	for name, c := range map[string]Config{
		"queue":    {Open: good.Open, Now: good.Now},
		"opener":   {Queue: good.Queue, Now: good.Now},
		"clock":    {Queue: good.Queue, Open: good.Open},
		"negative": {Queue: good.Queue, Open: good.Open, Now: good.Now, Poll: -1},
		"batch":    {Queue: good.Queue, Open: good.Open, Now: good.Now, Batch: tenant.MaxRecallBatch + 1},
	} {
		if _, err := New(c); fault.KindOf(err) != fault.Invalid {
			t.Fatalf("a config without a valid %s answered %v", name, err)
		}
	}
}
