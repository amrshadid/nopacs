package postgres

import (
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestDiscardedFilesLeaveOutPlacesInUse guards DiscardedFiles, the list a
// restart removes from the incoming area: it never lists a place that a
// received file of another reception still needs. A failed reception's file
// is held by a quarantine record, the reception's files are discarded, and
// the record is then released into a new reception, which receives the file
// at the same place. While that reception runs, and after it fails with its
// files not discarded (a retry would read them), the discarded reception's
// list leaves the place out; once that reception's files are discarded as
// well, nothing needs the place and the list has it again.
func TestDiscardedFilesLeaveOutPlacesInUse(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, 0), "rc-1", cT0)
	e.ok("open", err)
	kept, other := cFile(r, 0, 1), cFile(r, 0, 2)
	_, err = e.p().AddReceivedFiles(e.ctx, []tenant.ReceivedFile{kept, other}, cT0)
	e.ok("add", err)
	q := tenant.QuarantinedFile{ID: "q1", Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a", ReceptionID: r.ID,
		SOPInstanceUID: kept.SOPInstanceUID, StudyUID: kept.StudyUID, SeriesUID: kept.SeriesUID, SOPClassUID: kept.SOPClassUID,
		TransferSyntaxUID: kept.TransferSyntaxUID, SHA256: kept.SHA256, Size: kept.Size, Incoming: kept.Incoming,
		ContentKept: true, Quarantined: cT0}
	_, err = e.p().HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("hold", err)
	_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Fail,
		At: cT0.Add(time.Minute), Failure: "storage unreachable"}, nil)
	e.ok("fail", err)
	_, err = e.p().DiscardReception(e.ctx, r.ID, "cleanup", cT0.Add(2*time.Minute), e.audit(), nil)
	e.ok("discard", err)

	places := func(id string) []string {
		t.Helper()
		list, _, err := e.p().DiscardedFiles(e.ctx, id, master.Page{Limit: 10})
		e.ok("DiscardedFiles", err)
		var out []string
		for _, f := range list {
			out = append(out, f.Incoming)
		}
		return out
	}
	check := func(when string, want ...string) {
		t.Helper()
		if got := places(r.ID); !slices.Equal(got, want) {
			t.Fatalf("%s: DiscardedFiles of %s = %v; want %v", when, r.ID, got, want)
		}
	}
	check("the record keeps the file", other.Incoming)

	rel, err := e.p().ReleaseQuarantined(e.ctx, "q1", tenant.ReleaseAsIs, "", "rc-2", cT0.Add(3*time.Minute), e.audit(), nil)
	e.ok("release", err)
	live, _, err := e.p().ReceivedFiles(e.ctx, rel.ReleaseReception, master.Page{Limit: 10})
	e.ok("received of the new reception", err)
	if len(live) != 1 || live[0].Incoming != kept.Incoming {
		t.Fatalf("the new reception's received files = %+v; want the released file at %s", live, kept.Incoming)
	}
	check("the new reception receives the file", other.Incoming)

	_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: rel.ReleaseReception, From: reception.Receiving, Event: reception.Fail,
		At: cT0.Add(4 * time.Minute), Failure: "storage unreachable"}, nil)
	e.ok("fail the new reception", err)
	check("the new reception failed and may be retried", other.Incoming)

	_, err = e.p().DiscardReception(e.ctx, rel.ReleaseReception, "cleanup", cT0.Add(5*time.Minute), e.audit(), nil)
	e.ok("discard the new reception", err)
	want := []string{other.Incoming, kept.Incoming}
	slices.Sort(want)
	got := places(r.ID)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("after both were discarded: DiscardedFiles of %s = %v; want %v", r.ID, got, want)
	}
}
