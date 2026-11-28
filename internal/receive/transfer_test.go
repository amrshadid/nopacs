package receive

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAcceptTransferTakesOnlyWhatTheReceiverMayArchive guards the receiver of
// type job: a file a transfer marks as one that must be de-identified is
// refused, with nothing landed, by a receiver that archives as received; the
// same file unmarked is taken; a file given to a job receiver by anything
// but a transfer is refused; and a transfer's file given to a receiver of
// another type is refused.
func TestAcceptTransferTakesOnlyWhatTheReceiverMayArchive(t *testing.T) {
	r := newQuotaRig(t, tenant.QuotaBounds{})
	if _, err := r.store.AddReceiver(r.ctx, tenant.Receiver{Name: "xfer", Type: tenant.ReceiverJob, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30}, r.clock.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	file := func() Arrival {
		return Arrival{Receiver: "xfer", Body: bytes.NewReader(corpus(t, studyFiles[0]))}
	}
	landed := r.p.stats.Landed.Load()
	marked := file()
	marked.RequireDeidentified = true
	if out := r.p.AcceptTransfer(context.Background(), marked); out.Status != Failed || out.Reason != ReasonNotDeidentified {
		t.Fatalf("a file that must be de-identified, to a receiver that archives as received: %+v", out)
	}
	if got := r.p.stats.Landed.Load(); got != landed {
		t.Errorf("a refused file landed: %d files landed before, %d after", landed, got)
	}
	if out := r.p.AcceptTransfer(context.Background(), file()); out.Status != Received {
		t.Errorf("a file that need not be de-identified: %+v, want received", out)
	}
	if out := r.p.Accept(context.Background(), file()); out.Status != Failed {
		t.Errorf("a file handed to a job receiver by a source that is no transfer: %+v, want failed", out)
	}
	other := file()
	other.Receiver = "scanner"
	if out := r.p.AcceptTransfer(context.Background(), other); out.Status != Failed {
		t.Errorf("a transfer's file handed to a receiver of another type: %+v, want failed", out)
	}
}

// jobReceiverDeidentifying adds a receiver of type job that de-identifies by
// the suite's test profile, and returns it.
func jobReceiverDeidentifying(e *env) tenant.Receiver {
	e.t.Helper()
	r, err := e.store.AddReceiver(e.ctx, tenant.Receiver{Name: "xfer", Type: tenant.ReceiverJob, Enabled: true, Storage: "main", Profile: testProfile,
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30}, t0, e.audit(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// TestTransferFileWaitingOnTheRetryListIsHeldWhenItsReceiverStopsDeidentifying
// guards a file a transfer marked as one that must be de-identified, accepted
// by a receiver that de-identifies, whose row could not be written yet and
// which waits on the retry list: the receiver is changed to archive as
// received meanwhile, and when the retry runs the file is held in quarantine
// (deid_failed, no_profile) with its bytes kept, never recorded and archived
// as received, and nothing reaches the storage or the catalog. Its record is
// protected: a release through the receiver that archives as received is
// refused, and one through a receiver that de-identifies archives it
// de-identified. A file not
// marked, waiting the same way, is archived as received.
func TestTransferFileWaitingOnTheRetryListIsHeldWhenItsReceiverStopsDeidentifying(t *testing.T) {
	for _, marked := range []bool{true, false} {
		var db *faultyDB
		e := newEnv(t, envOptions{deid: &deidOptions{}, db: func(s tenant.Pipeline) tenant.Pipeline { db = &faultyDB{Pipeline: s}; return db }})
		jobReceiverDeidentifying(e)
		e.clock.Advance(2 * time.Second)
		db.addFails.Store(1 << 30)
		out := e.p.AcceptTransfer(e.ctx, Arrival{Receiver: "xfer", Body: bytes.NewReader(corpus(t, studyFiles[0])), RequireDeidentified: marked})
		if out.Status != Received {
			t.Fatalf("marked %v: the file was not accepted: %+v", marked, out)
		}
		e.waitFor("the row of the file to be refused", time.Second, func() bool { return db.addTries.Load() > 0 })
		if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "xfer"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(2 * time.Second)
		if marked {
			// No row can be written meanwhile (every write is refused), so
			// the only way out of the retry list is the hold.
			e.waitFor("the file to be held", time.Second, func() bool { return len(e.held()) == 1 })
			q := e.held()[0]
			if q.Reason != tenant.QuarantineDeidFailed || q.Details["cause"] != "no_profile" || !q.ContentKept || q.Receiver != "xfer" || !q.Deidentified {
				t.Errorf("the file is held as %+v, want deid_failed, no_profile, protected, its bytes kept", q)
			}
			if got := e.p.stats.Recorded.Load(); got != 0 {
				t.Errorf("%d files recorded, want none", got)
			}
			db.addFails.Store(0)
			assertNothingArchived := func(when string) {
				t.Helper()
				c, err := e.store.CatalogTotals(e.ctx)
				if err != nil || c.Instances != 0 {
					t.Errorf("%s: the catalog holds %+v (%v), want no instance", when, c, err)
				}
				if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
					t.Errorf("%s: the storage holds %v (%v), want nothing", when, objs.Keys, err)
				}
			}
			assertNothingArchived("held")
			// A release through the receiver that archives as received is
			// refused: the file is never archived as received.
			_, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
			if fault.KindOf(err) != fault.Precondition {
				t.Fatalf("a release through a receiver that archives as received: %v, want a precondition failure", err)
			}
			assertNothingArchived("after the refused release")
			// Once the receiver de-identifies again, the release archives it
			// de-identified.
			if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "xfer", Profile: testProfile},
				[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(2 * time.Second)
			if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
				t.Fatalf("a release through a receiver that de-identifies: %v", err)
			}
			// A reception a retry opened before the change holds nothing and
			// ends on its own; the release's ends done.
			archived := false
			for _, r := range e.settle(1) {
				if done(r) && r.Counts.FilesArchived == 1 {
					noOriginal(t, e, r)
					archived = true
				}
			}
			if !archived {
				e.waitFor("the released file to be archived", time.Second, func() bool {
					for _, r := range e.receptions() {
						if done(r) && r.Counts.FilesArchived == 1 {
							noOriginal(t, e, r)
							return true
						}
					}
					return false
				})
			}
			continue
		}
		// An unmarked file: wait for two more refused writes, so one whose
		// check ran after the change is among them, then let it be written.
		n := db.addTries.Load()
		e.waitFor("a write attempt after the change", time.Second, func() bool { return db.addTries.Load() >= n+2 })
		db.addFails.Store(0)
		r := e.settle(1)[0]
		if !done(r) || r.Counts.FilesArchived != 1 {
			t.Errorf("an unmarked file ended in reception %s %+v, want it archived as received", r.State, r.Counts)
		}
	}
}

// TestArchiveStepHoldsAFileRecordedAsMustBeDeidentified guards the archive
// step on its own: a file whose row says its source required it
// de-identified (ReceivedFile.MustDeidentify) but was recorded as received,
// at a receiver that archives as received, is held (deid_failed, no_profile)
// with its bytes kept and is never put on the storage, whatever the row's
// other flag says.
func TestArchiveStepHoldsAFileRecordedAsMustBeDeidentified(t *testing.T) {
	var db *faultyDB
	e := newEnv(t, envOptions{deid: &deidOptions{asReceived: true}, db: func(s tenant.Pipeline) tenant.Pipeline { db = &faultyDB{Pipeline: s}; return db }})
	db.markMust.Store(true)
	e.send(studyFiles[0])
	r := e.settle(1)[0]
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "no_profile" || !qs[0].ContentKept {
		t.Fatalf("reception %s, held %+v; want the file held as deid_failed, no_profile", r.State, qs)
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
		t.Fatalf("the storage holds %v (%v), want nothing", objs.Keys, err)
	}
	if !qs[0].Deidentified {
		t.Fatal("the record is not protected: a release through a receiver that archives as received would archive the file as received")
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a release through a receiver that archives as received: %v, want a precondition failure", err)
	}
	if c, err := e.store.CatalogTotals(e.ctx); err != nil || c.Instances != 0 {
		t.Fatalf("the catalog holds %+v (%v) after the refused release, want no instance", c, err)
	}
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: testProfile},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatalf("a release through a receiver that de-identifies: %v", err)
	}
	for _, x := range e.settle(2) {
		if done(x) {
			noOriginal(t, e, x)
			return
		}
	}
	t.Fatal("no reception archived the released file")
}
