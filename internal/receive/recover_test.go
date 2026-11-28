package receive

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRecoverReconcilesEachState guards Recover against the incoming area's
// every state, set up by hand as a crash leaves them: a file being written
// is removed; a landed file the database never heard of is checked and
// recorded; a landed file whose row exists gets its recorded name; a
// recorded file no row keeps is removed; and a row whose file is gone is
// counted missing, not dropped. The pipeline then archives what it found,
// and holds the file whose copy is gone as incoming_lost.
func TestRecoverReconcilesEachState(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	land := func(name string) incoming.File {
		f, err := e.p.area.Land(e.ctx, "dimse", bytesReader(corpus(t, name)), 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// A landed file with no row: its sender was answered, its row never
	// written.
	orphan := land(studyFiles[0])
	// A landed file with its row: the rename after the batch was lost.
	renamed := land(studyFiles[1])
	d, err := e.p.check(e.ctx, landed{file: renamed, receiver: "dimse"})
	if err != nil {
		t.Fatal(err)
	}
	// A row whose file is gone.
	gone := land(studyFiles[2])
	d2, err := e.p.check(e.ctx, landed{file: gone, receiver: "dimse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{*d.received, *d2.received}, e.p.now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := e.p.area.Remove(gone.Path); err != nil {
		t.Fatal(err)
	}
	// A recorded file no row keeps: its removal was cut short.
	leftover := land(studyFiles[3])
	if err := e.p.area.Commit(leftover.Path); err != nil {
		t.Fatal(err)
	}
	// A file being written.
	id := incoming.NewID()
	tmp := filepath.Join(e.cfg.IncomingRoot, "lab", "dimse", id[:2])
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, id+".tmp"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.manual = false
	_ = e.p.area.Close()
	e.start()
	// Start ran Recover; what it found is in the stats and the area.
	if got := e.p.Stats().MissingFiles.Load(); got != 1 {
		t.Errorf("missing files %d, want 1", got)
	}
	if ok, _ := e.p.area.Exists(leftover.Path); ok {
		t.Error("a recorded file no row keeps was not removed")
	}
	if _, err := os.Stat(filepath.Join(tmp, id+".tmp")); !os.IsNotExist(err) {
		t.Error("a file being written was not removed")
	}
	// The reception holds the orphan, now recorded, the renamed file, and
	// the file whose bytes are gone. That one is held as lost, with no
	// bytes kept and what is known of it on its record, and the other two
	// are archived: one lost copy does not fail the study.
	e.waitFor("the reception to end", 10*time.Minute, e.allEnded(1))
	r := e.receptions()[0]
	if r.Counts.FilesReceived != 3 || r.State != reception.Done || r.Counts.FilesArchived != 2 || r.Counts.FilesQuarantined != 1 {
		t.Fatalf("reception %+v", r)
	}
	qs, _, err := e.store.ListQuarantined(e.ctx, tenant.QuarantineFilter{ReceptionID: r.ID}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineIncomingLost || qs[0].ContentKept || qs[0].SHA256 != gone.SHA256 ||
		qs[0].SOPInstanceUID != d2.received.SOPInstanceUID {
		t.Fatalf("quarantine %+v, want the gone file held as incoming_lost with nothing kept", qs)
	}
	for _, f := range []incoming.File{orphan, renamed} {
		if ok, _ := e.p.area.Exists(f.Path); ok {
			t.Errorf("file %s was archived and verified, and its incoming copy is still there", f.ID)
		}
	}
}

// TestRecoverReportCountsAtStart guards the report Start returns.
func TestRecoverReportCountsAtStart(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	f, err := e.p.area.Land(e.ctx, "dimse", bytesReader(corpus(t, studyFiles[0])), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	_ = f
	_ = e.p.area.Close()
	p, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := p.Start(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(e.ctx) })
	if rep.Rechecked != 1 || rep.Temp != 0 || rep.Missing != 0 {
		t.Fatalf("report %+v", rep)
	}
}

// TestAReleaseCutShortIsCheckedAgainAtStart guards a release stopped after
// the store recorded it and before the check of the released file was
// written, for a file that joins no reception: the file, which the release
// gave its landed name first, is checked again at start as a release of its
// record, held again under a record of its own (ReleasedFrom naming the
// released one), and kept through a second restart. Checked again as a
// file that just arrived, its record would take the released record's id,
// which the store skips as held already, and the next start would remove
// the file as kept by nothing.
func TestAReleaseCutShortIsCheckedAgainAtStart(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.sendBytes([]byte("not a DICOM file at all"), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 && e.p.batch.idle() })
	q := e.held()[0]
	if err := e.p.Stop(e.ctx); err != nil {
		t.Fatal(err)
	}
	// Release's first two steps, by hand, as a process killed before its
	// third leaves them: the landed name, then the store's release.
	area, err := incoming.Open(e.cfg.IncomingRoot, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := area.Unrecord(q.Incoming); err != nil {
		t.Fatal(err)
	}
	_ = area.Close()
	out, err := e.store.ReleaseFile(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, ReceptionID: newID(), Now: e.p.now()}, e.audit(), nil)
	if err != nil || out.ReleaseReception != "" || out.ContentKept {
		t.Fatalf("the release: %+v %v", out, err)
	}
	for restart := 1; restart <= 2; restart++ {
		e.start()
		e.waitFor("the check of the released file", 5*time.Second, func() bool { return e.p.batch.idle() })
		if ok, err := e.p.area.Exists(q.Incoming); err != nil || !ok {
			t.Fatalf("after restart %d the released file is gone (%v)", restart, err)
		}
		var again []tenant.QuarantinedFile
		for _, r := range e.held() {
			if r.ReleasedFrom == q.ID && r.State == tenant.QuarantineHeld && r.ContentKept && r.Incoming == q.Incoming {
				again = append(again, r)
			}
		}
		if len(again) != 1 {
			t.Fatalf("after restart %d: %d records hold the released file again, want 1: %+v", restart, len(again), e.held())
		}
		if err := e.p.Stop(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAHoldWhoseIDIsTakenTouchesNoBytes guards the batch against a record
// the store skips because another record has its id: the file is neither
// given its recorded name nor removed, but keeps its landed name for the
// next start to check again.
func TestAHoldWhoseIDIsTakenTouchesNoBytes(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.sendBytes([]byte("not a DICOM file at all"), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 && e.p.batch.idle() })
	q := e.held()[0]
	if err := e.p.area.Unrecord(q.Incoming); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ReleaseFile(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, ReceptionID: newID(), Now: e.p.now()}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	// Checked again with no word of the release: its hold has the released
	// record's id.
	f, err := e.p.landedFile(incoming.Entry{Receiver: q.Receiver, Path: q.Incoming, ID: pathID(q.Incoming)})
	if err != nil {
		t.Fatal(err)
	}
	e.p.intakeNow(e.ctx, landed{file: f, receiver: q.Receiver})
	e.waitFor("the batch", time.Second, func() bool { return e.p.batch.idle() })
	landedName := filepath.Join(e.cfg.IncomingRoot, "lab", filepath.FromSlash(q.Incoming)) + ".new"
	if _, err := os.Stat(landedName); err != nil {
		t.Fatalf("the file does not keep its landed name: %v", err)
	}
}

// TestARestartWaitsForAReleaseInFlight guards a release that a restart of
// the tenant's pipeline in the same process meets halfway: Stop waits for
// the release, so the next pipeline's Recover never sees the file's landed
// name with its record still held (it would give back the recorded name,
// and the release then recorded would leave a recorded file no row keeps,
// removed at the start after). The released file is checked again and
// kept through the next start.
func TestARestartWaitsForAReleaseInFlight(t *testing.T) {
	var e *env
	var armed, stopped atomic.Bool
	stopDone := make(chan struct{})
	e = newEnv(t, envOptions{hook: func(point string) {
		if point != "release.before-store" || !armed.CompareAndSwap(true, false) {
			return
		}
		old := e.p
		go func() {
			_ = old.Stop(context.Background())
			stopped.Store(true)
			close(stopDone)
		}()
		// Long enough for a Stop that does not wait to pass.
		time.Sleep(200 * time.Millisecond)
		if stopped.Load() {
			t.Error("Stop returned while a release was in flight")
		}
	}})
	e.sendBytes([]byte("not a DICOM file at all"), Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 && e.p.batch.idle() })
	q := e.held()[0]
	armed.Store(true)
	out, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
	if err != nil || out.State != tenant.QuarantineReleased {
		t.Fatalf("the release: %+v %v", out, err)
	}
	<-stopDone
	// A release refused while stopping records nothing.
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a release on a stopped pipeline: %v, want unavailable", err)
	}
	for restart := 1; restart <= 2; restart++ {
		e.start()
		e.waitFor("the check of the released file", 5*time.Second, func() bool { return e.p.batch.idle() })
		if ok, err := e.p.area.Exists(q.Incoming); err != nil || !ok {
			t.Fatalf("after restart %d the released file is gone (%v)", restart, err)
		}
		if err := e.p.Stop(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	held := 0
	for _, r := range e.held() {
		if r.ReleasedFrom == q.ID && r.State == tenant.QuarantineHeld && r.ContentKept {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("%d records hold the released file again, want 1", held)
	}
}

// failingReceiver is the tenant's database refusing every read of a
// receiver, as a database that answers lists but not this read does.
type failingReceiver struct{ tenant.Pipeline }

func (failingReceiver) Receiver(context.Context, string) (tenant.Receiver, error) {
	return tenant.Receiver{}, fault.New(fault.Unavailable, "the test database refuses to read a receiver")
}

// TestALandedFileWhoseReceiverCannotBeReadIsKeptAtStart guards Recover
// against a landed file with no row whose receiver cannot be read: whether
// it is an import's copy (let go, its unit hands it in again) or another
// receiver's (checked and recorded) cannot be told, so the start fails and
// the copy keeps its landed name for the next start. Taken for another
// receiver's, an import's copy would be recorded without its arrival.
func TestALandedFileWhoseReceiverCannotBeReadIsKeptAtStart(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	f, err := e.p.area.Land(e.ctx, "dimse", bytesReader(corpus(t, studyFiles[0])), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.p.area.Close()
	cfg := e.cfg
	cfg.Store = failingReceiver{e.store}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(e.ctx); fault.KindOf(err) != fault.Unavailable {
		_ = p.Stop(e.ctx)
		t.Fatalf("the start: %v, want the database's refusal", err)
	}
	_ = p.Stop(e.ctx)
	if _, err := os.Stat(filepath.Join(e.cfg.IncomingRoot, "lab", filepath.FromSlash(f.Path)) + ".new"); err != nil {
		t.Fatalf("the landed copy does not keep its landed name: %v", err)
	}
}

// failingList is the tenant's database refusing every list of receptions,
// as a database that is not ready does at a pipeline's start.
type failingList struct{ tenant.Pipeline }

func (failingList) ListReceptions(context.Context, tenant.ReceptionFilter, master.Page) ([]tenant.Reception, string, error) {
	return nil, "", fault.New(fault.Unavailable, "the test database refuses to list receptions")
}

// TestAFailedStartReleasesTheIncomingArea guards a start whose Recover
// fails: the pipeline releases the incoming area it opened (a supervisor
// retries a failed start with a new pipeline for as long as the database
// refuses, and each one left a directory open), takes no file and no
// decision, cannot be started again, and stops cleanly.
func TestAFailedStartReleasesTheIncomingArea(t *testing.T) {
	e := newEnv(t, envOptions{manual: true})
	cfg := e.cfg
	cfg.Store = failingList{e.store}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Start(e.ctx); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("the start: %v, want the database's refusal", err)
	}
	path := incoming.PathOf("dimse", incoming.NewID())
	if _, err := p.area.Exists(path); err == nil {
		t.Fatal("the incoming area is still open after a failed start")
	}
	if out := p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytesReader([]byte("x"))}); out.Status != Refused {
		t.Fatalf("a file offered after a failed start: %+v", out)
	}
	if _, err := p.Release(e.ctx, tenant.Release{ID: "q1", Mode: tenant.ReleaseAsIs}, e.audit(), nil); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a decision after a failed start: %v", err)
	}
	if _, err := p.Start(e.ctx); err == nil {
		t.Fatal("a pipeline whose start failed started again")
	}
	if err := p.Stop(e.ctx); err != nil {
		t.Fatalf("a stop after a failed start: %v", err)
	}
}
