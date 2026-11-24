package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestTenantStatusFollowsItsEvents guards a tenant's status: it shows the
// counts, storages and receivers the status answers; an event the tenant
// commits makes it ask for the status again, so a change shows without a
// key; and esc leaves it at once, though its stream is open, rather than
// stopping the stream and staying.
func TestTenantStatusFollowsItsEvents(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			addTenants(f, 1)
			f.AddStorage("t000", &v1.Storage{Name: "main"})
			f.AddReceiver("t000", &v1.Receiver{Name: "scanner1", Storage: "main", Type: &v1.Receiver_Dimse{Dimse: &v1.DimseReceiver{AeTitle: "ARCHIVE", Port: 11112}}})
			f.SetStatusCounts("t000", &v1.TenantCounts{Subjects: 1, Sessions: 2, Scans: 3, Instances: 40})
		})
		r.key("enter", "enter")
		r.expect("t status")
		// An event of the tenant is waiting when the status opens: the
		// stream the screen follows hands it over, and the screen asks for
		// the status again, then follows on.
		r.fake.QueueEvent("t000", &v1.Event{Cursor: "c1", Type: v1.EventType_EVENT_TYPE_RECEPTION_STEP})
		status, watches := r.calls("GetStatus"), r.calls("Watch")
		r.key("t")
		r.expect("Status of t000", "Database:", "ready", "1 subjects, 2 sessions, 3 scans, 40 instances", "Storage main:", "Receiver scanner1:")
		if got := r.calls("GetStatus") - status; got != 2 {
			t.Errorf("the status was asked for %d times, want 2: on opening, and after the event", got)
		}
		if got := r.calls("Watch") - watches; got != 2 {
			t.Errorf("the events were followed %d times, want 2: until the event, and on after it", got)
		}
		r.refuse("waiting for the server")
		r.key("esc")
		r.expect("Tenant t000")
	})
}

// TestTenantListsAndAFileReleasedOrRejected guards the lists of a tenant's
// storages, receivers, receptions and quarantined files, and the release
// and the rejection of a file: each asks for a reason first, and the
// rejection, which removes bytes for good, for the file's id typed again.
func TestTenantListsAndAFileReleasedOrRejected(t *testing.T) {
	r := newRig(t, 120, 30, func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.AddStorage("t000", &v1.Storage{Name: "main"})
		f.AddReceiver("t000", &v1.Receiver{Name: "scanner1", Storage: "main", Type: &v1.Receiver_Dimse{Dimse: &v1.DimseReceiver{AeTitle: "ARCHIVE", Port: 11112}}})
		f.AddReception("t000", &v1.Reception{Id: "rc1", Receiver: "scanner1", Step: v1.ReceptionStep_RECEPTION_STEP_DONE, Counts: &v1.ReceptionCounts{FilesReceived: 7, FilesArchived: 7}})
		f.AddQuarantined("t000", &v1.QuarantinedFile{Id: "q1", Receiver: "scanner1", Reason: v1.QuarantineReason_QUARANTINE_REASON_NOT_DICOM,
			State: v1.QuarantineState_QUARANTINE_STATE_HELD, SizeBytes: 100, ContentKept: true}, "")
		f.AddQuarantined("t000", &v1.QuarantinedFile{Id: "q2", Receiver: "scanner1", Reason: v1.QuarantineReason_QUARANTINE_REASON_NOT_DICOM,
			State: v1.QuarantineState_QUARANTINE_STATE_HELD, SizeBytes: 200, ContentKept: true}, "")
	})
	r.key("enter", "enter", "o")
	r.expect("Storages of t000", "main")
	r.key("esc", "r")
	r.expect("Receivers of t000", "scanner1", "11112", "ARCHIVE")
	r.key("esc", "e")
	r.expect("Receptions of t000", "rc1", "done")
	r.key("esc", "h")
	r.expect("Quarantine of t000", "q1", "q2", "not dicom")

	// Release q1: no reason refused before anything is sent.
	r.key("enter")
	r.expect("Quarantined file q1", "r release", "d reject")
	r.key("r")
	r.expect("Release q1")
	releases := r.calls("ReleaseQuarantinedFile")
	r.key("enter")
	r.expect("say why the file is released")
	if r.calls("ReleaseQuarantinedFile") != releases {
		t.Fatal("a release with no reason was sent")
	}
	r.typeText("read again after the fix")
	r.key("enter")
	if r.calls("ReleaseQuarantinedFile") != releases+1 {
		t.Fatal("the release was not sent")
	}
	r.expect("released q1")

	// Reject q2: the id typed wrong is refused; then a reason is needed.
	r.key("down", "enter")
	r.expect("Quarantined file q2")
	r.key("d")
	r.expect("Reject q2", "removes the file's bytes for good")
	rejects := r.calls("RejectQuarantinedFile")
	r.typeText("q9")
	r.key("enter", "enter")
	if r.calls("RejectQuarantinedFile") != rejects {
		t.Fatal("a rejection with the wrong id was sent")
	}
}

// TestAFollowedCallIsNotWaitedFor guards the calls a screen follows rather
// than waits for, such as a status's stream: while one is open the bottom
// line does not say the program is waiting, esc does not stop it (so esc
// leaves the screen), and leaving the screen stops it.
func TestAFollowedCallIsNotWaitedFor(t *testing.T) {
	c := &ctx{base: context.Background(), pending: map[int]*pendingCall{}}
	stopped := make(chan struct{})
	cmd := c.follow(7, func(ctx context.Context) tea.Msg {
		<-ctx.Done()
		close(stopped)
		return nil
	})
	go cmd()
	if c.waiting() {
		t.Error("a followed call counts as waiting")
	}
	if c.cancelWaiting(7) {
		t.Error("esc's cancel stopped a followed call")
	}
	c.cancelOwner(7)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("leaving the screen did not stop the followed call")
	}
}
