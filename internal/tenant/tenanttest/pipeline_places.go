package tenanttest

import (
	"fmt"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReleaseIntoSameReception guards a file quarantined while its
// reception is receiving and released back into it: it is counted released
// there and no longer quarantined, received once, and once archived the
// tenant holds it like any file.
func testReleaseIntoSameReception(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	f := file(r, 1, 1)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2))
	e.ok("AddReceivedFiles", err)
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "attribute": "PatientID", "fault": tenant.FaultEmpty}, Receiver: "a", ReceptionID: r.ID,
		SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
		TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: at(3)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("HoldFiles", err)
	rel, err := e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", e.id("rc"), at(4), e.audit(), nil)
	e.ok("ReleaseQuarantined", err)
	if rel.ReleaseReception != r.ID {
		e.Fatalf("the release joined %s, want the reception still receiving, %s", rel.ReleaseReception, r.ID)
	}
	got, _, _ := e.p.Reception(e.ctx, r.ID)
	want := tenant.ReceptionCounts{FilesReceived: 1, BytesReceived: f.Size, FilesReleased: 1}
	if got.Counts != want {
		e.Errorf("after a release into the same reception: %+v, want %+v", got.Counts, want)
	}
	got = e.stepTo(got, reception.Identified, "store")
	e.commit(got, f)
	got = e.stepTo(got, reception.Done, "store")
	e.wantDup("a released file once archived", e.check(claimOf(f)), tenant.SameFile, f.SHA256, r.ID)
	if c := got.Counts; c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined {
		e.Errorf("at the end: %+v", c)
	}
}

// testDiscardKeepsQuarantined guards that a discard's list of files to
// remove, and the count and bytes in its audit entry, leave out every file
// a quarantine record keeps, and agree; the record stays held, with its
// bytes, until it is released or rejected.
func testDiscardKeepsQuarantined(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	kept, gone := file(r, 1, 1), file(r, 1, 2)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{kept, gone}, at(2))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, reception.Complete, "store")
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a", ReceptionID: r.ID,
		SOPInstanceUID: kept.SOPInstanceUID, StudyUID: kept.StudyUID, SeriesUID: kept.SeriesUID, SHA256: kept.SHA256,
		Size: kept.Size, Incoming: kept.Incoming, ContentKept: true, Quarantined: at(3)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("HoldFiles", err)
	r = e.failReception(r)
	_, _, err = e.p.DiscardedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	e.wantPrecondition("DiscardedFiles of a reception not discarded", err)
	audit := e.audit()
	_, err = e.p.DiscardReception(e.ctx, r.ID, "cleanup", at(900), audit, nil)
	e.ok("DiscardReception", err)
	list := allPages(e, 1, func(p master.Page) ([]tenant.ReceivedFile, string, error) { return e.p.DiscardedFiles(e.ctx, r.ID, p) })
	if len(list) != 1 || list[0].Incoming != gone.Incoming {
		e.Errorf("the files to remove = %+v, want only %s", list, gone.Incoming)
	}
	var entry master.AuditEntry
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.ID == audit.ID {
			entry = a
		}
	}
	if entry.ID != audit.ID || entry.Details[tenant.DetailFiles] != "1" || entry.Details[tenant.DetailBytes] != fmt.Sprint(gone.Size) {
		e.Errorf("the discard's audit entry %+v does not match its list", entry)
	}
	held, err := e.p.QuarantinedFile(e.ctx, q.ID)
	e.ok("QuarantinedFile", err)
	if held.State != tenant.QuarantineHeld || !held.ContentKept {
		e.Errorf("the record of a discarded reception reads %+v, want held with its bytes", held)
	}
}

// testDiscardLeavesLivePlaces guards that a discard's list of files to
// remove never names a place in the incoming area that a received file of
// a reception not yet ended occupies. A failed reception's file kept by a
// quarantine record, then released, joins a new reception at the same
// place, whether the failed reception's files were discarded before the
// release or after it; the failed reception's list must leave that place
// out, or the
// removal (and a restart that finds the list again) would delete bytes the
// new reception has yet to archive. The new reception counts as needing the
// place while it has not ended, and also once it failed until its own files
// are discarded, since a retry runs them again. The count in the audit
// entry agrees with the list.
func testDiscardLeavesLivePlaces(e *penv) {
	e.addReceiver("a", "store", 11112)
	failed := e.openReception("a", 1, at(1))
	shared, own := file(failed, 1, 1), file(failed, 1, 2)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{shared, own}, at(2))
	e.ok("AddReceivedFiles", err)
	failed = e.stepTo(failed, reception.Complete, "store")
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "a", ReceptionID: failed.ID,
		SOPInstanceUID: shared.SOPInstanceUID, StudyUID: shared.StudyUID, SeriesUID: shared.SeriesUID, SOPClassUID: shared.SOPClassUID,
		TransferSyntaxUID: shared.TransferSyntaxUID, SHA256: shared.SHA256, Size: shared.Size, Incoming: shared.Incoming,
		ContentKept: true, Quarantined: at(3)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("HoldFiles", err)
	failed = e.failReception(failed)
	audit := e.audit()
	_, err = e.p.DiscardReception(e.ctx, failed.ID, "cleanup", at(20), audit, nil)
	e.ok("DiscardReception", err)
	rel, err := e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", "live-1", at(25), e.audit(), nil)
	e.ok("ReleaseQuarantined", err)
	if rel.ReleaseReception != "live-1" {
		e.Fatalf("the release joined %q, want a new reception live-1", rel.ReleaseReception)
	}
	wantOnly := func(when string, receptionID string, want ...string) {
		e.Helper()
		var got []string
		for _, f := range allPages(e, 1, func(p master.Page) ([]tenant.ReceivedFile, string, error) {
			return e.p.DiscardedFiles(e.ctx, receptionID, p)
		}) {
			got = append(got, f.Incoming)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			e.Errorf("%s: the files to remove are %q, want %q", when, got, want)
		}
	}
	wantOnly("while the reception that took the place is receiving", failed.ID, own.Incoming)
	var entry master.AuditEntry
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.ID == audit.ID {
			entry = a
		}
	}
	if entry.ID != audit.ID || entry.Details[tenant.DetailFiles] != "1" || entry.Details[tenant.DetailBytes] != fmt.Sprint(own.Size) {
		e.Errorf("the discard's audit entry %+v does not match its list", entry)
	}
	live, _, err := e.p.Reception(e.ctx, "live-1")
	e.ok("Reception", err)
	live = e.failReception(e.stepTo(live, reception.Complete, "store"))
	wantOnly("while the reception that took the place failed and may be retried", failed.ID, own.Incoming)
	_, err = e.p.DiscardReception(e.ctx, live.ID, "cleanup", at(30), e.audit(), nil)
	e.ok("DiscardReception of the reception that took the place", err)
	wantOnly("once both are discarded, the first", failed.ID, shared.Incoming, own.Incoming)
	wantOnly("once both are discarded, the second", live.ID, shared.Incoming)
}
