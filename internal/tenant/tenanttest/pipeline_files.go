package tenanttest

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReceivedFiles guards recording received files: each read back
// exactly, the reception's received counts, last file time, associations
// (each once) and calling AE titles (each once, at most 16, truncation
// said) kept in the same transaction, and the list paged by cursor.
func testReceivedFiles(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	f1, f2, f3 := file(r, 1, 1), file(r, 1, 2), file(r, 1, 3)
	f2.Association, f2.Caller.CallingAETitle = "assoc2", "SCANNER2"
	f3.Association, f3.Caller = "", tenant.Caller{}
	n, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f1, f2, f3}, at(9), nil)
	e.ok("AddReceivedFiles", err)
	if n != 3 {
		e.Errorf("AddReceivedFiles = %d new, want 3", n)
	}
	got, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if got.Counts.FilesReceived != 3 || got.Counts.BytesReceived != 3006 || !got.LastFile.Equal(at(9)) || got.Associations != 2 ||
		!slices.Equal(got.CallingAETitles, []string{"SCANNER1", "SCANNER2"}) || got.CallingAETitlesTruncated {
		e.Errorf("the reception after a batch: %+v", got)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 4)}, at(5), nil)
	e.ok("AddReceivedFiles, an earlier time", err)
	got, _, _ = e.p.Reception(e.ctx, r.ID)
	if !got.LastFile.Equal(at(9)) {
		e.Errorf("a batch recorded at an earlier time moved the last file time back to %v", got.LastFile)
	}
	listed := allPages(e, 2, func(p master.Page) ([]tenant.ReceivedFile, string, error) { return e.p.ReceivedFiles(e.ctx, r.ID, p) })
	if len(listed) != 4 {
		e.Fatalf("ReceivedFiles listed %d, want 4", len(listed))
	}
	want := f1
	want.Received = at(9)
	if !reflect.DeepEqual(listed[0], want) {
		e.Errorf("a received file read back\n%+v\nwant\n%+v", listed[0], want)
	}
	var batch []tenant.ReceivedFile
	for i := range 20 {
		f := file(r, 1, 100+i)
		f.Association, f.Caller.CallingAETitle = fmt.Sprintf("x%02d", i), fmt.Sprintf("AE%02d", i)
		batch = append(batch, f)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, batch, at(10), nil)
	e.ok("AddReceivedFiles", err)
	got, _, _ = e.p.Reception(e.ctx, r.ID)
	if len(got.CallingAETitles) != tenant.MaxCallingAETitles || !got.CallingAETitlesTruncated || got.Associations != 22 ||
		got.CallingAETitles[0] != "SCANNER1" {
		e.Errorf("after 22 associations: %d titles, truncated %v, %d associations", len(got.CallingAETitles), got.CallingAETitlesTruncated, got.Associations)
	}
}

// testReceivedBatchRepeated guards that a batch written again, as after a
// crash between its commit and its answer, changes nothing: no row, count,
// association or title twice; a batch that overlaps one written counts only
// its new files; and a batch is all or none.
func testReceivedBatchRepeated(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	batch := []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2), file(r, 1, 3)}
	_, err := e.p.AddReceivedFiles(e.ctx, batch, at(2), nil)
	e.ok("AddReceivedFiles", err)
	before, _, _ := e.p.Reception(e.ctx, r.ID)
	n, err := e.p.AddReceivedFiles(e.ctx, batch, at(3), nil)
	e.ok("AddReceivedFiles again", err)
	after, _, _ := e.p.Reception(e.ctx, r.ID)
	if n != 0 || after.Counts != before.Counts || after.Associations != before.Associations || !after.LastFile.Equal(before.LastFile) {
		e.Errorf("a batch written again: %d new, %+v -> %+v", n, before, after)
	}
	n, err = e.p.AddReceivedFiles(e.ctx, append(batch, file(r, 1, 4)), at(4), nil)
	e.ok("AddReceivedFiles overlapping", err)
	got, _, _ := e.p.Reception(e.ctx, r.ID)
	if n != 1 || got.Counts.FilesReceived != 4 {
		e.Errorf("an overlapping batch: %d new, %d received; want 1 and 4", n, got.Counts.FilesReceived)
	}
	changed := file(r, 1, 1)
	changed.SHA256 = sum(999)
	n, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{changed}, at(5), nil)
	e.ok("AddReceivedFiles of a UID again with another checksum", err)
	if n != 1 {
		e.Errorf("a UID received again with another checksum is %d new files, want 1: it is another file", n)
	}
	missing := file(r, 1, 50)
	missing.ReceptionID = "missing"
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 5), missing}, at(6), nil)
	e.wantKind("AddReceivedFiles naming a reception not there", err, fault.NotFound)
	if files, _, _ := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 100}); len(files) != 5 {
		e.Errorf("a batch that failed left %d files, want the 5 before it", len(files))
	}
}

// testReceivedFilesClosed guards that files are recorded only into a
// reception that is receiving: one past it refuses them with
// ErrReceptionClosed, a precondition, so the receiver opens the next
// arrival; and a file of another study is Invalid.
func testReceivedFilesClosed(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	other := file(r, 1, 2)
	other.StudyUID = uidOf(1, 99)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{other}, at(2), nil)
	e.wantKind("AddReceivedFiles of another study's file", err, fault.Invalid)
	e.stepTo(r, reception.Complete, "store")
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1)}, at(3), nil)
	e.wantIs("AddReceivedFiles into a complete reception", err, tenant.ErrReceptionClosed, fault.Precondition)
}

// testReceivedBatchOrder guards which failure a batch naming several
// receptions reports: the first, in the order the batch first names its
// receptions, whatever the failures of the receptions after it. Both
// drivers must answer alike, since the receiver acts on the answer: a
// closed reception (ErrReceptionClosed) opens the next arrival, while a
// reception not there is the server's fault.
func testReceivedBatchOrder(e *penv) {
	e.addReceiver("a", "store", 11112)
	open := e.openReception("a", 1, at(1))
	closed := e.stepTo(e.openReception("a", 2, at(1)), reception.Complete, "store")
	missing := file(open, 1, 9)
	missing.ReceptionID = "missing"
	other := file(open, 1, 8)
	other.StudyUID = uidOf(1, 99)
	for _, c := range []struct {
		what  string
		batch []tenant.ReceivedFile
		kind  fault.Kind
	}{
		{"a closed reception, then one not there", []tenant.ReceivedFile{file(closed, 2, 1), missing}, fault.Precondition},
		{"one not there, then a closed reception", []tenant.ReceivedFile{missing, file(closed, 2, 1)}, fault.NotFound},
		{"an open reception, then a closed one, then the open one again", []tenant.ReceivedFile{file(open, 1, 1), file(closed, 2, 1), file(open, 1, 2)}, fault.Precondition},
		{"another study's file, then a closed reception", []tenant.ReceivedFile{other, file(closed, 2, 1)}, fault.Invalid},
		{"a closed reception, then another study's file", []tenant.ReceivedFile{file(closed, 2, 1), other}, fault.Precondition},
	} {
		_, err := e.p.AddReceivedFiles(e.ctx, c.batch, at(3), nil)
		e.wantKind("AddReceivedFiles naming "+c.what, err, c.kind)
		if c.kind == fault.Precondition {
			e.wantIs("AddReceivedFiles naming "+c.what, err, tenant.ErrReceptionClosed, fault.Precondition)
		}
	}
	if got, _, _ := e.p.Reception(e.ctx, open.ID); got.Counts.FilesReceived != 0 {
		e.Errorf("batches that failed recorded %d files", got.Counts.FilesReceived)
	}
}

// heldFile returns a held record of reason not DICOM, kept, of size bytes.
func (e *penv) heldFile(receiver string, size int64, when int) tenant.QuarantinedFile {
	id := e.id("q")
	return tenant.QuarantinedFile{ID: id, Reason: tenant.QuarantineNotDICOM, Details: map[string]string{"offset": "128"},
		Receiver: receiver, Caller: tenant.Caller{CallingAETitle: "SCANNER1", Address: "[2001:db8::7]:51234"},
		Size: size, SHA256: sum(when), Incoming: "quarantine/" + id, ContentKept: true, Quarantined: at(when)}
}

// differentFile returns a held record of a different file under a UID the
// tenant holds, with what a release needs to join a reception.
func (e *penv) differentFile(receiver, receptionID string, n int) tenant.QuarantinedFile {
	id := e.id("q")
	return tenant.QuarantinedFile{ID: id, Reason: tenant.QuarantineDifferentFile, Receiver: receiver, ReceptionID: receptionID,
		Size: 500, SHA256: sum(n + 1000), KeptSHA256: sum(n), SOPClassUID: "1.2.840.10008.5.1.4.1.1.4",
		SOPInstanceUID: uidOf(3, n), StudyUID: uidOf(1, 1), SeriesUID: uidOf(2, 1), TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Incoming: "quarantine/" + id, ContentKept: true, Quarantined: at(n)}
}

// testQuarantineHold guards recording held files: each read back exactly;
// a batch written again changes nothing; the totals and the reception's
// quarantined counts move in the same transaction; a file too large counts
// as held with no bytes; a batch is all or none.
func testQuarantineHold(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	kept := e.heldFile("a", 300, 1)
	large := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineTooLarge, Details: map[string]string{"limit": "4294967296"},
		Receiver: "a", ReceptionID: r.ID, Size: 4096, StudyUID: r.StudyUID, Quarantined: at(2)}
	diff := e.differentFile("a", r.ID, 7)
	batch := []tenant.QuarantinedFile{kept, large, diff}
	n, err := e.p.HoldFiles(e.ctx, batch, nil)
	e.ok("HoldFiles", err)
	if n != 3 {
		e.Errorf("HoldFiles = %d new, want 3", n)
	}
	got, err := e.p.QuarantinedFile(e.ctx, kept.ID)
	e.ok("QuarantinedFile", err)
	want := kept
	want.State = tenant.QuarantineHeld
	if !reflect.DeepEqual(got, want) {
		e.Errorf("a record read back\n%+v\nwant\n%+v", got, want)
	}
	files, bytes, err := e.p.QuarantineTotals(e.ctx)
	e.ok("QuarantineTotals", err)
	if files != 3 || bytes != 800 {
		e.Errorf("totals = %d files, %d bytes; want 3 and 800 (the large file's bytes were never kept)", files, bytes)
	}
	rc, _, _ := e.p.Reception(e.ctx, r.ID)
	if rc.Counts.FilesQuarantined != 2 || rc.Counts.BytesQuarantined != 4596 {
		e.Errorf("the reception's quarantined counts = %d, %d; want 2, 4596", rc.Counts.FilesQuarantined, rc.Counts.BytesQuarantined)
	}
	n, err = e.p.HoldFiles(e.ctx, batch, nil)
	e.ok("HoldFiles again", err)
	files2, bytes2, _ := e.p.QuarantineTotals(e.ctx)
	rc2, _, _ := e.p.Reception(e.ctx, r.ID)
	if n != 0 || files2 != files || bytes2 != bytes || rc2.Counts != rc.Counts {
		e.Errorf("a batch written again: %d new, totals %d/%d, counts %+v", n, files2, bytes2, rc2.Counts)
	}
	orphan := e.heldFile("a", 1, 3)
	orphan.ReceptionID = "missing"
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{e.heldFile("a", 1, 4), orphan}, nil)
	e.wantPrecondition("HoldFiles naming a reception not there", err)
	if f, _, _ := e.p.QuarantineTotals(e.ctx); f != 3 {
		e.Errorf("a batch that failed moved the totals to %d", f)
	}
}

// testQuarantineDetails guards that a record's details carry only the keys
// its reason lists, each in the form the reason states, so a value from the
// file other than a UID can never be kept in one; and the other rules of a
// record.
func testQuarantineDetails(e *penv) {
	e.addReceiver("a", "store", 11112)
	for _, c := range []struct {
		what   string
		change func(*tenant.QuarantinedFile)
	}{
		{"a key the reason does not carry", func(q *tenant.QuarantinedFile) { q.Details = map[string]string{"patient_name": "x"} }},
		{"an offset that is not a number", func(q *tenant.QuarantinedFile) { q.Details = map[string]string{"offset": "DOE^JOHN"} }},
		{"a UID detail holding a name", func(q *tenant.QuarantinedFile) {
			q.Reason, q.Details = tenant.QuarantineUIDElsewhere, map[string]string{"study_instance_uid": "DOE^JOHN"}
		}},
		{"a fault holding free text", func(q *tenant.QuarantinedFile) {
			q.Reason, q.Details = tenant.QuarantineBadUID, map[string]string{"attribute": "StudyInstanceUID", "fault": "Doe John"}
		}},
		{"an unknown reason", func(q *tenant.QuarantinedFile) { q.Reason, q.Details = "lost", nil }},
		{"content kept with no place", func(q *tenant.QuarantinedFile) { q.Incoming = "" }},
		{"a place that leaves the incoming area", func(q *tenant.QuarantinedFile) { q.Incoming = "../etc" }},
		{"a different file without its kept checksum", func(q *tenant.QuarantinedFile) { q.Reason, q.Details = tenant.QuarantineDifferentFile, nil }},
		{"a kept checksum for another reason", func(q *tenant.QuarantinedFile) { q.KeptSHA256 = sum(5) }},
		{"a bad caller address", func(q *tenant.QuarantinedFile) { q.Caller.Address = "scanner.example.org" }},
		{"a bad UID", func(q *tenant.QuarantinedFile) { q.SOPInstanceUID = "1.2.x" }},
		{"another state", func(q *tenant.QuarantinedFile) { q.State = tenant.QuarantineReleased }},
	} {
		q := e.heldFile("a", 10, 1)
		c.change(&q)
		_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.wantKind("HoldFiles with "+c.what, err, fault.Invalid)
	}
	ok := e.heldFile("a", 10, 1)
	ok.Reason, ok.Details = tenant.QuarantineBadLabel, map[string]string{"label": "session", "attribute": "(0010,4000)", "fault": "no_match"}
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{ok}, nil)
	e.ok("HoldFiles with a bad label's details", err)
}

// testQuarantineRelease guards releasing a held file: it joins the
// reception of its study receiving at the receiver, or a new one, as a
// received file counted released; the record is released with its mode,
// receiver, reception and time, its bytes no longer counted held; a repeat
// answers it as it stands; a different file is released only while the
// tenant holds no instance under its UID, so in this version a different
// file whose instance is held can only be rejected; there is no mode but
// as is; the contract's other refusals.
func testQuarantineRelease(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	r := e.openReception("a", 1, at(1))
	diff, notDICOM, large := e.differentFile("a", r.ID, 7), e.heldFile("a", 50, 2), tenant.QuarantinedFile{ID: e.id("q"),
		Reason: tenant.QuarantineTooLarge, Receiver: "a", Size: 9000, Quarantined: at(3)}
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{diff, notDICOM, large}, nil)
	e.ok("HoldFiles", err)
	e.stepTo(r, reception.Complete, "store")

	got, err := e.p.ReleaseQuarantined(e.ctx, diff.ID, tenant.ReleaseAsIs, "", "released-1", at(100), e.audit(), nil)
	e.ok("ReleaseQuarantined of a different file whose UID no instance holds", err)
	if got.State != tenant.QuarantineReleased || got.ReleaseMode != tenant.ReleaseAsIs || got.ReleaseReceiver != "a" ||
		got.ReleaseReception != "released-1" || got.ContentKept || !got.Decided.Equal(at(100)) {
		e.Errorf("a released record reads %+v", got)
	}
	joined, _, err := e.p.Reception(e.ctx, "released-1")
	e.ok("Reception", err)
	if joined.State != reception.Receiving || joined.Arrival != 2 || joined.Counts.FilesReceived != 1 || joined.Counts.FilesReleased != 1 ||
		joined.Counts.BytesReceived != 500 {
		e.Errorf("the reception a release opened reads %+v", joined)
	}
	again, err := e.p.ReleaseQuarantined(e.ctx, diff.ID, tenant.ReleaseAsIs, "", "released-2", at(200), e.audit(), nil)
	e.ok("ReleaseQuarantined again", err)
	if again.ReleaseReception != "released-1" || !again.Decided.Equal(at(100)) {
		e.Errorf("a repeated release changed the record: %+v", again)
	}
	if j, _, _ := e.p.Reception(e.ctx, "released-1"); j.Counts.FilesReceived != 1 {
		e.Errorf("a repeated release received the file again: %d", j.Counts.FilesReceived)
	}
	holder, hf := e.identified("a", 1, 8)
	e.commit(holder, hf...)
	held := e.differentFile("a", "", 8)
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{held}, nil)
	e.ok("HoldFiles", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, held.ID, tenant.ReleaseAsIs, "", "released-3", at(200), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined of a different file whose UID an instance holds", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, notDICOM.ID, "overwrite", "", "released-4", at(200), e.audit(), nil)
	e.wantKind("ReleaseQuarantined with a mode other than as is", err, fault.Invalid)
	_, err = e.p.ReleaseQuarantined(e.ctx, notDICOM.ID, tenant.ReleaseAsIs, "nobody", "released-4", at(200), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined through a receiver not there", err)
	got, err = e.p.ReleaseQuarantined(e.ctx, notDICOM.ID, tenant.ReleaseAsIs, "b", "released-4", at(200), e.audit(), nil)
	e.ok("ReleaseQuarantined of a file with no study", err)
	if got.ReleaseReception != "" || got.ReleaseReceiver != "b" {
		e.Errorf("a file with no study joined %q through %q, want none through b", got.ReleaseReception, got.ReleaseReceiver)
	}
	_, err = e.p.ReleaseQuarantined(e.ctx, large.ID, tenant.ReleaseAsIs, "", "released-5", at(200), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined of a file never kept", err)
	gone := e.heldFile("b", 5, 4)
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{gone}, nil)
	e.ok("HoldFiles", err)
	_, err = e.p.RemoveReceiver(e.ctx, "b", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, gone.ID, tenant.ReleaseAsIs, "", "released-6", at(200), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined of a removed receiver's file, naming none", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, "missing", tenant.ReleaseAsIs, "", "released-6", at(200), e.audit(), nil)
	e.wantKind("ReleaseQuarantined of a record not there", err, fault.NotFound)
	_, err = e.p.ReleaseQuarantined(e.ctx, gone.ID, "", "", "released-6", at(200), e.audit(), nil)
	e.wantKind("ReleaseQuarantined with no mode", err, fault.Invalid)
}

// testQuarantineReject guards rejecting a held file: rejected with its
// reason and time, its bytes no longer kept; a repeat answers the first
// reason; a released file is a precondition and a rejected one cannot be
// released; the reason, size and checksum go in the audit entry.
func testQuarantineReject(e *penv) {
	e.addReceiver("a", "store", 11112)
	q, released := e.heldFile("a", 70, 1), e.heldFile("a", 80, 2)
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q, released}, nil)
	e.ok("HoldFiles", err)
	audit := e.audit()
	got, err := e.p.RejectQuarantined(e.ctx, q.ID, "a test pattern, not a study", at(50), audit, nil)
	e.ok("RejectQuarantined", err)
	if got.State != tenant.QuarantineRejected || got.ContentKept || got.RejectReason != "a test pattern, not a study" || !got.Decided.Equal(at(50)) {
		e.Errorf("a rejected record reads %+v", got)
	}
	entries := e.allAudit(master.AuditFilter{})
	if d := entries[0].Details; entries[0].ID != audit.ID || d[tenant.DetailReason] != "not_dicom" || d[tenant.DetailSize] != "70" ||
		d[tenant.DetailSHA256] != sum(1).String() {
		e.Errorf("the rejection's audit entry is %+v", entries[0])
	}
	got, err = e.p.RejectQuarantined(e.ctx, q.ID, "another reason", at(60), e.audit(), nil)
	e.ok("RejectQuarantined again", err)
	if got.RejectReason != "a test pattern, not a study" || !got.Decided.Equal(at(50)) {
		e.Errorf("a repeated rejection changed the record: %+v", got)
	}
	_, err = e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", "rel-1", at(60), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined of a rejected file", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, released.ID, tenant.ReleaseAsIs, "", "rel-2", at(60), e.audit(), nil)
	e.ok("ReleaseQuarantined", err)
	_, err = e.p.RejectQuarantined(e.ctx, released.ID, "x", at(60), e.audit(), nil)
	e.wantPrecondition("RejectQuarantined of a released file", err)
	_, err = e.p.RejectQuarantined(e.ctx, q.ID, " ", at(60), e.audit(), nil)
	e.wantKind("RejectQuarantined with a reason of spaces", err, fault.Invalid)
}

// testQuarantineTotals guards the running totals: after every change, held,
// released, rejected or a batch written again, they equal the count of the
// records held and the sum of the bytes kept for them, read from the
// records themselves.
func testQuarantineTotals(e *penv) {
	e.addReceiver("a", "store", 11112)
	check := func(when string) {
		e.Helper()
		var files, bytes int64
		for _, q := range allPages(e, 7, func(p master.Page) ([]tenant.QuarantinedFile, string, error) {
			return e.p.ListQuarantined(e.ctx, tenant.QuarantineFilter{State: tenant.QuarantineHeld}, p)
		}) {
			files++
			bytes += q.HeldBytes()
		}
		f, b, err := e.p.QuarantineTotals(e.ctx)
		e.ok("QuarantineTotals", err)
		if f != files || b != bytes {
			e.Errorf("%s: totals %d files, %d bytes; the records hold %d, %d", when, f, b, files, bytes)
		}
	}
	var ids []string
	for i := range 30 {
		var batch []tenant.QuarantinedFile
		for j := range i%3 + 1 {
			q := e.heldFile("a", int64(100*i+j), i)
			if (i+j)%4 == 0 {
				q.Reason, q.Details, q.Incoming, q.ContentKept = tenant.QuarantineTooLarge, nil, "", false
			}
			batch = append(batch, q)
			ids = append(ids, q.ID)
		}
		_, err := e.p.HoldFiles(e.ctx, batch, nil)
		e.ok("HoldFiles", err)
		check("after a batch")
		_, err = e.p.HoldFiles(e.ctx, batch, nil)
		e.ok("HoldFiles again", err)
		check("after a batch written again")
	}
	for i, id := range ids {
		switch i % 3 {
		case 0:
			_, err := e.p.RejectQuarantined(e.ctx, id, "not wanted", at(1000), e.audit(), nil)
			e.ok("RejectQuarantined", err)
		case 1:
			_, err := e.p.ReleaseQuarantined(e.ctx, id, tenant.ReleaseAsIs, "", e.id("rel"), at(1000), e.audit(), nil)
			if fault.KindOf(err) != fault.Precondition {
				e.ok("ReleaseQuarantined", err)
			}
		}
		check("after a decision")
	}
}

// testQuarantineList guards the quarantine list: newest first by time then
// id, each filter, and a cursor bound to its filter.
func testQuarantineList(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	r := e.openReception("a", 1, at(0))
	var all []tenant.QuarantinedFile
	for i := range 15 {
		var q tenant.QuarantinedFile
		if i%3 == 0 {
			q = e.differentFile("a", r.ID, i)
		} else {
			q = e.heldFile([]string{"a", "b"}[i%2], int64(i), i)
		}
		q.Quarantined = at(i / 2)
		_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.ok("HoldFiles", err)
		if i%5 == 0 {
			var err error
			q, err = e.p.RejectQuarantined(e.ctx, q.ID, "no", at(100), e.audit(), nil)
			e.ok("RejectQuarantined", err)
		}
		q.State = map[bool]tenant.QuarantineState{true: tenant.QuarantineRejected, false: tenant.QuarantineHeld}[i%5 == 0]
		all = append(all, q)
	}
	slices.SortFunc(all, func(a, b tenant.QuarantinedFile) int {
		if c := b.Quarantined.Compare(a.Quarantined); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	for _, c := range []struct {
		what string
		f    tenant.QuarantineFilter
		keep func(tenant.QuarantinedFile) bool
	}{
		{"no filter", tenant.QuarantineFilter{}, func(tenant.QuarantinedFile) bool { return true }},
		{"held", tenant.QuarantineFilter{State: tenant.QuarantineHeld}, func(q tenant.QuarantinedFile) bool { return q.State == tenant.QuarantineHeld }},
		{"a reason", tenant.QuarantineFilter{Reason: tenant.QuarantineDifferentFile}, func(q tenant.QuarantinedFile) bool { return q.Reason == tenant.QuarantineDifferentFile }},
		{"a receiver", tenant.QuarantineFilter{Receiver: "b"}, func(q tenant.QuarantinedFile) bool { return q.Receiver == "b" }},
		{"a reception", tenant.QuarantineFilter{ReceptionID: r.ID}, func(q tenant.QuarantinedFile) bool { return q.ReceptionID == r.ID }},
		{"a study", tenant.QuarantineFilter{StudyUID: uidOf(1, 1)}, func(q tenant.QuarantinedFile) bool { return q.StudyUID == uidOf(1, 1) }},
		{"an instance", tenant.QuarantineFilter{SOPInstanceUID: uidOf(3, 3)}, func(q tenant.QuarantinedFile) bool { return q.SOPInstanceUID == uidOf(3, 3) }},
		{"a time range", tenant.QuarantineFilter{Since: at(2), Until: at(5)}, func(q tenant.QuarantinedFile) bool {
			return !q.Quarantined.Before(at(2)) && q.Quarantined.Before(at(5))
		}},
	} {
		var got, want []string
		for _, q := range allPages(e, 2, func(p master.Page) ([]tenant.QuarantinedFile, string, error) {
			return e.p.ListQuarantined(e.ctx, c.f, p)
		}) {
			got = append(got, q.ID)
		}
		for _, q := range all {
			if c.keep(q) {
				want = append(want, q.ID)
			}
		}
		if !slices.Equal(got, want) {
			e.Errorf("ListQuarantined with %s = %q, want %q", c.what, got, want)
		}
	}
	_, cursor, err := e.p.ListQuarantined(e.ctx, tenant.QuarantineFilter{Receiver: "a"}, master.Page{Limit: 1})
	e.ok("ListQuarantined", err)
	_, _, err = e.p.ListQuarantined(e.ctx, tenant.QuarantineFilter{}, master.Page{Limit: 1, Cursor: cursor})
	e.wantKind("ListQuarantined with another filter's cursor", err, fault.Invalid)
	_, _, err = e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 1, Cursor: cursor})
	e.wantKind("ListReceptions with a quarantine cursor", err, fault.Invalid)
}
