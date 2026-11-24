package receive

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestStorageThatFailsAPutIsTriedAgain guards a storage that refuses
// writes for a while: the archive step fails its attempts, is tried again
// after its delay, and archives every file once.
func TestStorageThatFailsAPutIsTriedAgain(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.putFails.Store(3)
		}
		return fs
	}})
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", rs[0])
	}
	if e.p.Stats().StepsFailed.Load() < 1 {
		t.Fatal("no step failed, so the failure was not exercised")
	}
	checkArchive(t, e, rs[0], sentMap(t, studyFiles...))
}

// TestStorageThatFailsAReadIsTriedAgain guards a storage that cannot be
// read back for a while: the verify step is tried again, nothing is held.
func TestStorageThatFailsAReadIsTriedAgain(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.readFails.Store(2)
		}
		return fs
	}})
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) || len(e.held()) != 0 || e.p.Stats().StepsFailed.Load() < 1 {
		t.Fatalf("reception %+v, held %d", rs[0], len(e.held()))
	}
}

// TestReadBackOfOtherBytesIsHeld guards the read-back before the commit: a
// storage that returns other bytes than were written has every such file
// held with the read-back reason and the checksum it read, never archived
// (the catalog holds none of the instances), its incoming copy kept, and the
// reception's counts adding up, quarantined since nothing went on.
func TestReadBackOfOtherBytesIsHeld(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.corrupt.Store(true)
		}
		return fs
	}})
	e.send(studyFiles...)
	rs := e.settle(1)
	held := e.held()
	if len(held) != 7 {
		t.Fatalf("%d held, want 7: %+v", len(held), rs[0])
	}
	for _, q := range held {
		if q.Reason != tenant.QuarantineReadBackMismatch || q.Details["storage"] != "main" || len(q.Details["read_sha256"]) != 64 || !q.ContentKept {
			t.Fatalf("held %+v", q)
		}
	}
	if n := e.incomingFiles(); n != 7 {
		t.Fatalf("the incoming area holds %d files, want every copy kept", n)
	}
	c := rs[0].Counts
	if rs[0].State != reception.Quarantined || c.FilesArchived != 0 || c.FilesQuarantined != 7 ||
		c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined {
		t.Fatalf("reception %+v", rs[0])
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	if totals.Instances != 0 {
		t.Fatalf("the catalog holds %d instances that did not read back", totals.Instances)
	}
	// The claims were let go when the reception ended: the same study,
	// from storage that answers truly, is archived.
	fs.corrupt.Store(false)
	e.clock.Advance(time.Minute)
	e.send(studyFiles...)
	rs = byArrival(e.settle(2))
	if !done(rs[1]) || rs[1].Counts.FilesArchived != 7 {
		t.Fatalf("second reception %+v", rs[1])
	}
	checkArchive(t, e, rs[1], sentMap(t, studyFiles...))
}

// TestSlowStorageKeepsItsLease guards a step that takes longer than its
// lease: its heartbeat renews the lease, no other worker takes the unit
// over, and the step finishes when the storage does.
func TestSlowStorageKeepsItsLease(t *testing.T) {
	fs := (*faultyStorage)(nil)
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
		}
		return fs
	}, workers: 3})
	if _, err := e.cfg.OpenStorage(e.ctx, "main"); err != nil {
		t.Fatal(err)
	}
	release := fs.setStall()
	e.send(studyFiles...)
	e.waitFor("the archive step to stall", 3*time.Second, func() bool { return len(fs.stalled) > 0 })
	// Twice the lease passes on the clock while the Put waits.
	for range 20 {
		e.clock.Advance(3 * time.Second)
		<-e.clock.After(30 * time.Millisecond)
	}
	release()
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 || e.p.Stats().StepsFailed.Load() != 0 {
		t.Fatalf("reception %+v, failed steps %d", rs[0], e.p.Stats().StepsFailed.Load())
	}
}

// TestDatabaseThatRefusesMidStepIsTriedAgain guards a database that refuses
// a commit after the files are on the storage: the step is tried again, its
// claims answer ClaimedBefore, the same objects are put again, and every
// file is archived once.
func TestDatabaseThatRefusesMidStepIsTriedAgain(t *testing.T) {
	var db *faultyDB
	e := newEnv(t, envOptions{db: func(s tenant.Pipeline) tenant.Pipeline {
		db = &faultyDB{Pipeline: s}
		db.commitFails.Store(2)
		return db
	}})
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", rs[0])
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	if totals.Instances != 7 {
		t.Fatalf("totals %+v", totals)
	}
	checkArchive(t, e, rs[0], sentMap(t, studyFiles...))
}

// seedIdentified lands the study, records it, and takes the reception to
// identified by running the quiet and identify steps by hand on a pipeline
// that was not started, and returns the reception.
func seedIdentified(t *testing.T, e *env) tenant.Reception {
	t.Helper()
	var files []tenant.ReceivedFile
	for _, n := range studyFiles {
		f, err := e.p.area.Land(e.ctx, "dimse", bytesReader(corpus(t, n)), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		d, err := e.p.check(e.ctx, landed{file: f, receiver: "dimse"})
		if err != nil || d.received == nil {
			t.Fatalf("check: %v %+v", err, d)
		}
		files = append(files, *d.received)
	}
	if _, err := e.store.AddReceivedFiles(e.ctx, files, e.p.now()); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := e.p.area.Commit(f.Incoming); err != nil {
			t.Fatal(err)
		}
	}
	rid := files[0].ReceptionID
	e.clock.Advance(time.Minute)
	for _, kind := range []string{kindQuiet, kindIdentify} {
		w, err := e.store.ClaimWork(e.ctx, kind, "seed", e.p.now(), time.Minute)
		if err != nil {
			t.Fatalf("claim %s: %v", kind, err)
		}
		if err := e.p.runStep(e.ctx, w); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	r, _, err := e.store.Reception(e.ctx, rid)
	if err != nil || r.State != reception.Identified {
		t.Fatalf("reception %+v %v", r, err)
	}
	return r
}

// TestFenceStopsAWorkerWhoseLeaseWasTakenOver guards the fence: a worker
// whose lease ended and was taken over by a second worker runs the archive
// step and changes nothing (its claim is refused), and the second worker
// archives the reception.
func TestFenceStopsAWorkerWhoseLeaseWasTakenOver(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	r := seedIdentified(t, e)
	first, err := e.store.ClaimWork(e.ctx, kindArchive, "first", e.p.now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	second, err := e.store.ClaimWork(e.ctx, kindArchive, "second", e.p.now(), time.Minute)
	if err != nil || second.Attempt != first.Attempt+1 {
		t.Fatalf("takeover: %+v %v", second, err)
	}
	if err := e.p.archive(e.ctx, first); !errors.Is(err, tenant.ErrLeaseLost) {
		t.Fatalf("the first worker's step gave %v, want ErrLeaseLost", err)
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	page, _ := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"})
	if totals.Instances != 0 || len(page.Keys) != 0 {
		t.Fatalf("the first worker changed something: %+v, %d objects", totals, len(page.Keys))
	}
	if err := e.p.archive(e.ctx, second); err != nil {
		t.Fatal(err)
	}
	got, _, _ := e.store.Reception(e.ctx, r.ID)
	if got.State != reception.Archived || got.Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", got)
	}
}

// TestAStaleWorkerHoldsNothing guards the fence on a worker's holds
// (HoldFilesFenced): with one received file's incoming copy gone, a worker
// whose lease was taken over runs the archive step, whose first write is
// that file's hold as incoming_lost, and holds nothing; the worker holding
// the lease holds it and archives the other six.
func TestAStaleWorkerHoldsNothing(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	r := seedIdentified(t, e)
	files, _, err := e.store.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	if err != nil || len(files) != 7 {
		t.Fatalf("received %d, %v", len(files), err)
	}
	if err := e.p.area.Remove(files[0].Incoming); err != nil {
		t.Fatal(err)
	}
	first, err := e.store.ClaimWork(e.ctx, kindArchive, "first", e.p.now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	second, err := e.store.ClaimWork(e.ctx, kindArchive, "second", e.p.now(), time.Minute)
	if err != nil || second.Attempt != first.Attempt+1 {
		t.Fatalf("takeover: %+v %v", second, err)
	}
	if err := e.p.archive(e.ctx, first); !errors.Is(err, tenant.ErrLeaseLost) {
		t.Fatalf("the stale worker's step gave %v, want ErrLeaseLost", err)
	}
	if held := e.held(); len(held) != 0 {
		t.Fatalf("the stale worker held %+v", held)
	}
	if err := e.p.archive(e.ctx, second); err != nil {
		t.Fatal(err)
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineIncomingLost || held[0].SOPInstanceUID != files[0].SOPInstanceUID {
		t.Fatalf("held %+v, want the file whose copy is gone as incoming_lost", held)
	}
	got, _, _ := e.store.Reception(e.ctx, r.ID)
	if got.State != reception.Archived || got.Counts.FilesArchived != 6 || got.Counts.FilesQuarantined != 1 {
		t.Fatalf("reception %+v", got)
	}
}

// TestLeaseLostMidStepStopsTheStep guards a lease lost while a step runs:
// the heartbeat's renewal is refused, the step is stopped, no failed
// attempt is recorded, and the unit is taken up again once its lease ends.
func TestLeaseLostMidStepStopsTheStep(t *testing.T) {
	var db *faultyDB
	var fs *faultyStorage
	e := newEnv(t, envOptions{
		db: func(s tenant.Pipeline) tenant.Pipeline { db = &faultyDB{Pipeline: s}; return db },
		storage: func(s blob.Store) blob.Store {
			if fs == nil {
				fs = newFaulty(s)
			}
			return fs
		},
	})
	if _, err := e.cfg.OpenStorage(e.ctx, "main"); err != nil {
		t.Fatal(err)
	}
	release := fs.setStall()
	lost := make(chan struct{}, 4)
	e.p.hook = func(point string) {
		if point == "step.lease-lost:"+kindArchive {
			lost <- struct{}{}
		}
		select {
		case e.notify <- struct{}{}:
		default:
		}
	}
	e.send(studyFiles...)
	e.waitFor("the archive step to stall", 3*time.Second, func() bool { return len(fs.stalled) > 0 })
	db.leaseLost.Store(true)
	e.waitFor("the step to stop on its lost lease", time.Second, func() bool { return len(lost) > 0 })
	db.leaseLost.Store(false)
	release()
	rs := e.settle(1)
	if !done(rs[0]) || e.p.Stats().StepsFailed.Load() != 0 {
		t.Fatalf("reception %+v, failed %d", rs[0], e.p.Stats().StepsFailed.Load())
	}
}

// TestWorkThatKeepsFailingFailsTheReception guards the end of retries: a
// storage that never takes a write fails the archive unit after its last
// attempt, the store fails the reception and lets its claims go, and every
// incoming copy stays for a retry or a discard.
func TestWorkThatKeepsFailingFailsTheReception(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.putFails.Store(1 << 40)
		}
		return fs
	}})
	e.send(studyFiles...)
	e.waitFor("the reception to fail", 10*time.Minute, e.allEnded(1))
	r := e.receptions()[0]
	if r.State != reception.Failed || r.Failure != tenant.FailureWorkGaveUp {
		t.Fatalf("reception %+v", r)
	}
	if n := e.incomingFiles(); n != 7 {
		t.Fatalf("%d incoming files, want 7 kept", n)
	}
	ds, err := e.store.CheckDuplicates(e.ctx, []tenant.DuplicateQuery{{SOPInstanceUID: uidOf(t, corpus(t, studyFiles[0])),
		StudyUID: r.StudyUID, SeriesUID: "1.2.3", SHA256: tenant.SHA256{1}}})
	if err != nil || ds[0].Kind != tenant.NotHeld {
		t.Fatalf("a claim outlived the failed reception: %+v %v", ds, err)
	}

	// Retry: the storage takes writes again, and the retry archives it all.
	fs.putFails.Store(0)
	_, retry, err := e.p.Retry(e.ctx, r.ID, "", "", e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the retry to end", 3*time.Second, func() bool {
		got, _, _ := e.store.Reception(e.ctx, retry.ID)
		return got.State.Ended()
	})
	got, _, _ := e.store.Reception(e.ctx, retry.ID)
	if !done(got) || got.Counts.FilesArchived != 7 || e.incomingFiles() != 0 {
		t.Fatalf("retry %+v, %d incoming", got, e.incomingFiles())
	}
}

// TestDiscardRemovesTheBytesAfterTheReason guards a discard: the store
// records it, then the bytes go, and a quarantined file's bytes stay.
func TestDiscardRemovesTheBytesAfterTheReason(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.putFails.Store(1 << 40)
		}
		return fs
	}})
	e.send(studyFiles...)
	e.waitFor("the reception to fail", 10*time.Minute, e.allEnded(1))
	r := e.receptions()[0]
	if _, err := e.store.DiscardReception(e.ctx, r.ID, "the study was sent to the wrong archive", e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.p.RemoveDiscarded(e.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ := e.store.Reception(e.ctx, r.ID)
	if got.DiscardTime.IsZero() || e.incomingFiles() != 0 {
		t.Fatalf("after discard: %+v, %d incoming", got, e.incomingFiles())
	}
}

// TestRejectRemovesTheBytesAfterTheReason guards a rejection: the record is
// rejected with its reason first, then its bytes leave the incoming area.
func TestRejectRemovesTheBytesAfterTheReason(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.sendBytes([]byte("not a DICOM file at all"), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	out, err := e.store.RejectQuarantined(e.ctx, q.ID, "a stray text file", e.p.now(), e.audit(), nil)
	if err != nil || out.State != tenant.QuarantineRejected || out.ContentKept {
		t.Fatalf("reject %+v %v", out, err)
	}
	if err := e.p.RemoveRejected(q.Incoming); err != nil {
		t.Fatal(err)
	}
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d incoming files after the rejection", n)
	}
}

// TestReleasedFileIsArchived guards a release: a file held because its
// receiver did not take its SOP class is released once the receiver takes
// it, joins a reception, and is archived.
func TestReleasedFileIsArchived(t *testing.T) {
	e := newEnv(t, envOptions{sopClasses: []string{"1.2.840.10008.5.1.4.1.1.2"}})
	e.send("MR_small.dcm")
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDIMSESOPClasses},
		e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	out, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
	if err != nil || out.ReleaseReception == "" {
		t.Fatalf("release %+v %v", out, err)
	}
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 1 || rs[0].Counts.FilesReleased != 1 {
		t.Fatalf("reception %+v", rs[0])
	}
	checkArchive(t, e, rs[0], sentMap(t, "MR_small.dcm"))
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d incoming files", n)
	}
}

// TestReleasedFileThatIsStillNotDICOMIsHeldAgain guards a release of a file
// that joins nothing: it is checked again, held again under a new record
// that names the one released, and its bytes stay in the incoming area.
func TestReleasedFileThatIsStillNotDICOMIsHeldAgain(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.sendBytes([]byte("not a DICOM file at all"), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	out, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
	if err != nil || out.State != tenant.QuarantineReleased || out.ReleaseReception != "" {
		t.Fatalf("release %+v %v", out, err)
	}
	e.waitFor("the second hold", time.Second, func() bool { return len(e.held()) == 2 && e.p.batch.idle() })
	var again tenant.QuarantinedFile
	for _, h := range e.held() {
		if h.ID != q.ID {
			again = h
		}
	}
	if again.ReleasedFrom != q.ID || again.State != tenant.QuarantineHeld || !again.ContentKept || again.Incoming != q.Incoming {
		t.Fatalf("the second record %+v", again)
	}
	// A restart keeps the bytes: a held record keeps them.
	if _, err := e.p.Recover(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.incomingFiles(); n != 1 {
		t.Fatalf("%d incoming files, want the one held", n)
	}
}

// TestVerifyThatFindsAnObjectGoneKeepsTheCopies guards the verify step's
// check: an instance confirmed and then not found on its storage fails the
// step until its unit gives up, the reception fails, and every incoming copy
// stays for a retry.
func TestVerifyThatFindsAnObjectGoneKeepsTheCopies(t *testing.T) {
	var fs *faultyStorage
	e := newEnv(t, envOptions{storage: func(s blob.Store) blob.Store {
		if fs == nil {
			fs = newFaulty(s)
			fs.missing.Store(true)
		}
		return fs
	}})
	e.send(studyFiles...)
	e.waitFor("the reception to fail", 10*time.Minute, e.allEnded(1))
	r := e.receptions()[0]
	if r.State != reception.Failed || r.Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", r)
	}
	if n := e.incomingFiles(); n != 7 {
		t.Fatalf("%d incoming files, want every copy kept", n)
	}
}
