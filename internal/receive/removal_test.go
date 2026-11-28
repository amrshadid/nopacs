package receive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// removalDone reports whether the unit w ended done.
func (e *env) removalDone(w tenant.Work) func() bool {
	return func() bool {
		got, err := e.store.Work(e.ctx, w.ID)
		return err == nil && got.State == tenant.WorkDone
	}
}

// heldRecord sends bytes the receiver quarantines as not DICOM and returns
// the record of the held file.
func (e *env) heldRecord(body string) tenant.QuarantinedFile {
	e.t.Helper()
	before := len(e.held())
	e.sendBytes([]byte(body), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == before+1 })
	for _, q := range e.held() {
		if q.State == tenant.QuarantineHeld && q.ContentKept && !q.Deidentified {
			if b, _ := e.readIncoming(q.Incoming); string(b) == body {
				return q
			}
		}
	}
	e.t.Fatal("the hold of the sent bytes was not found")
	return tenant.QuarantinedFile{}
}

// readIncoming returns the bytes at an incoming path, nil when none.
func (e *env) readIncoming(path string) ([]byte, error) {
	f, size, err := e.p.area.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, size)
	_, err = f.Read(b)
	return b, err
}

// TestAQueuedRemovalRemovesWhatARejectionLetGo guards the removal of a
// rejected file's bytes as a unit of work: queued after the rejection is
// recorded, a running pipeline's worker removes the bytes and ends the unit
// done, with nobody calling the pipeline.
func TestAQueuedRemovalRemovesWhatARejectionLetGo(t *testing.T) {
	e := newEnv(t, envOptions{})
	q := e.heldRecord("a stray text file")
	if _, err := e.store.RejectQuarantined(e.ctx, q.ID, "a stray text file", e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := QueueRemoval(e.ctx, e.store, RejectedRemoval(q.ID, q.Incoming), e.p.now())
	if err != nil {
		t.Fatal(err)
	}
	e.p.WakeWork()
	e.waitFor("the removal", time.Second, e.removalDone(w))
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d incoming files after the removal", n)
	}
}

// TestARemovalNeverDeletesWithoutItsDecision guards that a removal unit
// deletes bytes only when the decision it names stands: a record that is
// still held, a rejected record whose checksum is not the bytes at the path,
// and a reception that is not discarded each end the unit and leave the
// bytes where they are.
func TestARemovalNeverDeletesWithoutItsDecision(t *testing.T) {
	e := newEnv(t, envOptions{})
	a := e.heldRecord("the first stray file")
	b := e.heldRecord("the second stray file")
	units := []tenant.Work{}
	queue := func(r Removal) {
		w, err := QueueRemoval(e.ctx, e.store, r, e.p.now())
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, w)
	}
	// Held, not rejected.
	queue(RejectedRemoval(a.ID, a.Incoming))
	// Rejected, but the path names the other file's bytes.
	if _, err := e.store.RejectQuarantined(e.ctx, b.ID, "a stray text file", e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	queue(RejectedRemoval(b.ID, a.Incoming))
	// A reception nobody discarded.
	queue(DiscardedRemoval("R0000000000000000000000000000001"))
	// A record that is not there.
	queue(RejectedRemoval("Q0000000000000000000000000000001", a.Incoming))
	e.p.WakeWork()
	for _, w := range units {
		e.waitFor("a removal unit to end", time.Second, e.removalDone(w))
	}
	if got, err := e.readIncoming(a.Incoming); err != nil || string(got) != "the first stray file" {
		t.Fatalf("the held file's bytes after removals that had no decision: %q %v", got, err)
	}
	// With its own decision and path the second file goes.
	w, err := QueueRemoval(e.ctx, e.store, RejectedRemoval(b.ID, b.Incoming), e.p.now())
	if err != nil {
		t.Fatal(err)
	}
	e.p.WakeWork()
	e.waitFor("the removal", time.Second, e.removalDone(w))
	if n := e.incomingFiles(); n != 1 {
		t.Fatalf("%d incoming files, want the held one only", n)
	}
}

// TestARemovalOwedWhileNoPipelineRunsIsDoneByTheNext guards that a removal
// owed after a decision survives a stop: queued while the pipeline is
// stopped it is a unit in the database, and the next pipeline over the same
// database runs it. A removal queued twice is one unit.
func TestARemovalOwedWhileNoPipelineRunsIsDoneByTheNext(t *testing.T) {
	e := newEnv(t, envOptions{})
	q := e.heldRecord("a stray text file")
	if err := e.p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RejectQuarantined(e.ctx, q.ID, "a stray text file", e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := QueueRemoval(e.ctx, e.store, RejectedRemoval(q.ID, q.Incoming), e.p.now())
	if err != nil {
		t.Fatal(err)
	}
	again, err := QueueRemoval(e.ctx, e.store, RejectedRemoval(q.ID, q.Incoming), e.p.now())
	if err != nil || again.ID != w.ID {
		t.Fatalf("the same removal queued twice: %v, unit %q then %q", err, w.ID, again.ID)
	}
	if got, _ := e.store.Work(e.ctx, w.ID); got.State != tenant.WorkQueued {
		t.Fatalf("the owed removal is %q, want queued", got.State)
	}
	e.start()
	e.waitFor("the owed removal", time.Second, e.removalDone(w))
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d incoming files after the next pipeline's start", n)
	}
}

// The ids of the corrections the tests name, in the form a correction's id
// takes.
const (
	corrDelete  = "01arz3ndektsv4rrffq69g5fa1"
	corrRelabel = "01arz3ndektsv4rrffq69g5fa2"
	corrNone    = "01arz3ndektsv4rrffq69g5fa3"
)

// relabelOrDelete is a database that answers two corrections of its own
// (corrDelete a delete, corrRelabel a relabel) and the real store's
// answer for any other id.
type relabelOrDelete struct{ tenant.Pipeline }

func (s relabelOrDelete) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	switch id {
	case corrDelete:
		return tenant.Correction{ID: id, Kind: record.KindDelete}, nil
	case corrRelabel:
		return tenant.Correction{ID: id, Kind: record.KindRelabel}, nil
	}
	return s.Pipeline.Correction(ctx, id)
}

// landOrphan lands bytes in the incoming area that no row names and returns
// their place.
func (e *env) landOrphan(body string) string {
	e.t.Helper()
	f, err := e.p.area.Land(e.ctx, "dimse", strings.NewReader(body), 1<<20)
	if err != nil {
		e.t.Fatal(err)
	}
	return f.Path
}

// TestARejectedRemovalNeverDeletesAnotherFilesBytes guards the binding of a
// rejected removal to its record's own place: a record made at a receiver
// that de-identifies keeps a keyed checksum, which the bytes are not
// compared with, so a unit queued with another file's path must still end
// and leave that file where it is.
func TestARejectedRemovalNeverDeletesAnotherFilesBytes(t *testing.T) {
	e := newEnv(t, envOptions{})
	a := e.heldRecord("the first stray file")
	other := incoming.PathOf("dimse", incoming.NewID())
	c := tenant.QuarantinedFile{ID: "q-deid", Reason: tenant.QuarantineNotDICOM, Details: map[string]string{"offset": "128"},
		Receiver: "dimse", Caller: tenant.Caller{CallingAETitle: "SCANNER1", Address: "192.0.2.7:51234"},
		Size: a.Size, Incoming: other, ContentKept: true, Quarantined: e.p.now(), Deidentified: true}
	if _, err := e.store.HoldFiles(e.ctx, []tenant.QuarantinedFile{c}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RejectQuarantined(e.ctx, c.ID, "a stray file", e.p.now(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	w, err := QueueRemoval(e.ctx, e.store, RejectedRemoval(c.ID, a.Incoming), e.p.now())
	if err != nil {
		t.Fatal(err)
	}
	e.p.WakeWork()
	e.waitFor("the removal unit to end", time.Second, e.removalDone(w))
	if got, err := e.readIncoming(a.Incoming); err != nil || string(got) != "the first stray file" {
		t.Fatalf("another record's bytes after a removal queued with their path: %q %v", got, err)
	}
}

// TestAWithdrawnRemovalDeletesOnlyWhatNoRecordKeeps guards the removal after
// a purged delete: a correction that is not a delete, one that is not there,
// and a path a live quarantine record still keeps each delete nothing; a
// delete correction and a path no row keeps delete the bytes.
func TestAWithdrawnRemovalDeletesOnlyWhatNoRecordKeeps(t *testing.T) {
	e := newEnv(t, envOptions{db: func(p tenant.Pipeline) tenant.Pipeline { return relabelOrDelete{p} }})
	live := e.heldRecord("a held file")
	orphan := e.landOrphan("bytes no row names")
	var units []tenant.Work
	queue := func(r Removal) {
		w, err := QueueRemoval(e.ctx, e.store, r, e.p.now())
		if err != nil {
			t.Fatal(err)
		}
		units = append(units, w)
	}
	queue(WithdrawnRemoval(corrRelabel, orphan))
	queue(WithdrawnRemoval(corrNone, orphan))
	queue(WithdrawnRemoval(corrDelete, live.Incoming))
	e.p.WakeWork()
	for _, w := range units {
		e.waitFor("a removal unit to end", time.Second, e.removalDone(w))
	}
	if _, _, err := e.p.area.Open(orphan); err != nil {
		t.Fatalf("bytes were removed without a delete correction: %v", err)
	}
	if got, err := e.readIncoming(live.Incoming); err != nil || string(got) != "a held file" {
		t.Fatalf("a live record's bytes after a withdrawn removal: %q %v", got, err)
	}
	queue(WithdrawnRemoval(corrDelete, orphan))
	e.p.WakeWork()
	e.waitFor("the removal", time.Second, e.removalDone(units[len(units)-1]))
	if _, _, err := e.p.area.Open(orphan); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the bytes of a purged delete after its removal: %v, want gone", err)
	}
}

// TestQueueRemovalRefusesWhatCannotBeDone guards that a removal is checked
// when it is queued: a path outside the incoming area, an id that is not
// one, a discard with a path and a removal of no kind are Invalid, and no
// unit is queued for them.
func TestQueueRemovalRefusesWhatCannotBeDone(t *testing.T) {
	e := newEnv(t, envOptions{})
	good := incoming.PathOf("dimse", incoming.NewID())
	for name, r := range map[string]Removal{
		"a path of another form":   RejectedRemoval("q1", "../../etc/passwd"),
		"no path":                  RejectedRemoval("q1", ""),
		"an id with a space":       RejectedRemoval("q 1", good),
		"an empty id":              DiscardedRemoval(""),
		"a discard with a path":    {kind: removeDiscarded, ref: "r1", path: good},
		"a removal of no kind":     {ref: "r1", path: good},
		"a withdrawn bad path":     WithdrawnRemoval(corrDelete, "dimse/zz"),
		"a withdrawn with bad id ": WithdrawnRemoval("not-a-ulid", good),
	} {
		if _, err := QueueRemoval(e.ctx, e.store, r, e.p.now()); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: %v, want Invalid", name, err)
		}
	}
	if _, err := QueueRemoval(e.ctx, e.store, RejectedRemoval("q1", good), e.p.now()); err != nil {
		t.Errorf("a valid removal: %v", err)
	}
}

// failsRecords is a database whose quarantine records cannot be read.
type failsRecords struct{ tenant.Pipeline }

func (failsRecords) QuarantinedFile(context.Context, string) (tenant.QuarantinedFile, error) {
	return tenant.QuarantinedFile{}, fault.New(fault.Unavailable, "the test database refuses the record")
}

// TestARemovalThatCannotBeDoneGivesUpCleanly guards the end of a removal
// unit whose attempts run out: it is held as failed with its reason, no
// reception fails with it (it names none), and the pipeline goes on taking
// work.
func TestARemovalThatCannotBeDoneGivesUpCleanly(t *testing.T) {
	e := newEnv(t, envOptions{db: func(p tenant.Pipeline) tenant.Pipeline { return failsRecords{p} }})
	w, err := QueueRemoval(e.ctx, e.store, RejectedRemoval("q1", incoming.PathOf("dimse", incoming.NewID())), e.p.now())
	if err != nil {
		t.Fatal(err)
	}
	e.p.WakeWork()
	e.waitFor("the unit to give up", time.Hour, func() bool {
		got, err := e.store.Work(e.ctx, w.ID)
		return err == nil && got.State == tenant.WorkFailed
	})
	got, _ := e.store.Work(e.ctx, w.ID)
	if got.LastError == "" || got.Attempt != tenant.DefaultWorkAttempts {
		t.Fatalf("the unit gave up with attempt %d and error %q", got.Attempt, got.LastError)
	}
	if rs := e.receptions(); len(rs) != 0 {
		t.Fatalf("%d receptions after a removal gave up", len(rs))
	}
	e.send(studyFiles...)
	e.waitFor("a reception to be archived after the removal gave up", time.Second, e.allEnded(1))
}

// TestRemovalsAreClaimedFirst guards the order a worker asks for work in:
// the removal of bytes a decision let go comes before the reception steps,
// so the incoming area empties first.
func TestRemovalsAreClaimedFirst(t *testing.T) {
	if len(claimOrder) == 0 || claimOrder[0] != kindRemoval {
		t.Fatalf("claim order %v, want %s first", claimOrder, kindRemoval)
	}
}
