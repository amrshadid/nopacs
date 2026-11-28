package tenanttest

import (
	"errors"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of transfer jobs and of the provenance of what came in.

// transferSetup archives two sessions of sub-01 (ses-1 with two instances in
// two scans, ses-2 with one) and returns the instances of ses-1 as a
// listing makes them.
func (e *tenv) transferSetup() []tenant.TransferNewItem {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100, 101}, 100, at(1), "ses-1", nil)
	e.archiveScans("recv", 2, []int{200}, 200, at(2), "ses-2", nil)
	var items []tenant.TransferNewItem
	page, err := e.p.ListScans(e.ctx, tenant.ScanFilter{SubjectLabel: "sub-01", SessionLabel: "ses-1", Order: tenant.OrderLabel}, master.Page{Limit: 10})
	e.ok("ListScans", err)
	for _, sc := range page.Items {
		held, _, err := e.p.ListHeldInstances(e.ctx, label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}, master.Page{Limit: 10})
		e.ok("ListHeldInstances", err)
		for _, in := range held {
			items = append(items, tenant.TransferNewItem{Path: label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label},
				SOPInstanceUID: in.SOPInstanceUID, Size: in.Size, SHA256: in.ArchivedSHA256, AsReceived: in.Profile.IsZero()})
		}
	}
	if len(items) != 2 {
		e.Fatalf("the setup listed %d instances, want 2", len(items))
	}
	return items
}

// queueTransfer queues a transfer of the target to tenant "other" at second s.
func (e *tenv) queueTransfer(id string, target tenant.JobTarget, move bool, s int) (tenant.Job, error) {
	return e.p.QueueTransfer(e.ctx, tenant.TransferQueue{ID: id, Target: target, TargetTenant: "other", TargetTenantID: "t-other", Receiver: "xfer",
		SourceTenant: "lab", Move: move, Reason: "moved to other", KeyID: "k-admin", Now: at(s)}, e.audit(), nil)
}

// testTransferQueue guards a transfer job's making: a subject or a session
// of the tenant becomes a queued job at the step listing that names its
// target tenant and receiver; a scan, a target not held, a transfer to the
// tenant itself, a taken id, and a second job over the subject while one has
// not ended are refused; the job is listed.
func testTransferQueue(e *tenv) {
	e.transferSetup()
	ses := tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}
	_, err := e.queueTransfer(e.id("j"), tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1", ScanLabel: "x"}, false, 10)
	e.wantKind("a transfer of a scan", err, fault.Invalid)
	_, err = e.queueTransfer(e.id("j"), tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-9"}, false, 10)
	e.wantKind("a transfer of a session not held", err, fault.NotFound)
	_, err = e.p.QueueTransfer(e.ctx, tenant.TransferQueue{ID: e.id("j"), Target: ses, TargetTenant: "lab", TargetTenantID: "t-lab", Receiver: "xfer",
		SourceTenant: "lab", Reason: "r", Now: at(10)}, e.audit(), nil)
	e.wantKind("a transfer to the tenant itself", err, fault.Invalid)
	id := e.id("j")
	j, err := e.queueTransfer(id, ses, true, 11)
	e.ok("QueueTransfer", err)
	if j.Kind != tenant.JobTransfer || j.State != tenant.JobQueued || j.Target != ses || j.CreatedByKeyID != "k-admin" || j.Receiver != "xfer" {
		e.Fatalf("the transfer job is %+v", j)
	}
	tj, err := e.p.TransferJob(e.ctx, id)
	e.ok("TransferJob", err)
	if tj.Step != tenant.TransferListing || !tj.Move || tj.TargetTenant != "other" || tj.TargetTenantID != "t-other" || tj.Receiver != "xfer" ||
		tj.Reason != "moved to other" || tj.DeleteCorrection != "" || tj.Items.Total() != 0 {
		e.Fatalf("the transfer job's own part is %+v", tj)
	}
	_, err = e.queueTransfer(id, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-2"}, false, 12)
	e.wantKind("a transfer with a taken id", err, fault.Conflict)
	_, err = e.queueTransfer(e.id("j"), tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-2"}, false, 12)
	e.wantKind("a second transfer over the subject while one has not ended", err, fault.Precondition)
	_, err = e.p.TransferJob(e.ctx, "nojob")
	e.wantKind("a transfer job that does not exist", err, fault.NotFound)
	list, next, err := e.p.ListTransfers(e.ctx, master.Page{Limit: 10})
	e.ok("ListTransfers", err)
	if next != "" || len(list) != 1 || list[0].Job.ID != id {
		e.Fatalf("the transfer jobs are %+v (next %q)", list, next)
	}
	if _, err := e.p.EndTransfer(e.ctx, id, tenant.JobFailed, "stopped", at(13)); err != nil {
		e.Fatalf("EndTransfer: %v", err)
	}
	if _, err := e.queueTransfer(e.id("j"), tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-2"}, false, 14); err != nil {
		e.Fatalf("a transfer after the first ended: %v", err)
	}
}

// testTransferItems guards the listing: items are added once whatever the
// number of times a listing runs, each with its revision; the job leaves
// listing with the instances and bytes its items hold, the steps go in
// order, and an item cannot be added after listing.
func testTransferItems(e *tenv) {
	items := e.transferSetup()
	id := e.id("j")
	_, err := e.queueTransfer(id, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, false, 10)
	e.ok("QueueTransfer", err)
	n, err := e.p.AddTransferItems(e.ctx, id, items[:1])
	e.ok("AddTransferItems", err)
	if n != 1 {
		e.Fatalf("the first call added %d, want 1", n)
	}
	n, err = e.p.AddTransferItems(e.ctx, id, items)
	e.ok("AddTransferItems again", err)
	if n != 1 {
		e.Fatalf("a listing run again added %d, want only the item it lacked", n)
	}
	_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferConfirming, at(11))
	e.wantKind("a step out of order", err, fault.Precondition)
	tj, err := e.p.StartTransferStep(e.ctx, id, tenant.TransferCopying, at(12))
	e.ok("StartTransferStep copying", err)
	var bytes int64
	for _, it := range items {
		bytes += it.Size
	}
	c := tj.Job.Counts
	if tj.Step != tenant.TransferCopying || tj.Job.State != tenant.JobRunning || !c.Listed || c.Instances != 2 || c.Bytes != bytes || tj.Items.Listed != 2 {
		e.Fatalf("the job leaving listing is %+v", tj)
	}
	again, err := e.p.StartTransferStep(e.ctx, id, tenant.TransferCopying, at(13))
	e.ok("StartTransferStep copying again", err)
	if again.Step != tenant.TransferCopying || !again.Job.StateTime.Equal(tj.Job.StateTime) {
		e.Fatalf("a step started again changed the job: %+v", again)
	}
	_, err = e.p.AddTransferItems(e.ctx, id, items)
	e.wantKind("an item added after listing", err, fault.Precondition)
	got, err := e.p.TransferItems(e.ctx, id, tenant.TransferItemListed, tenant.TransferItemKey{}, 10)
	e.ok("TransferItems", err)
	if len(got) != 2 || got[0].Path.Scan >= got[1].Path.Scan {
		e.Fatalf("the listed items are %+v, want two in the order of their scans", got)
	}
	for i, it := range got {
		w := items[i]
		if it.Path != w.Path || it.SOPInstanceUID != w.SOPInstanceUID || it.Size != w.Size || it.SHA256 != w.SHA256 || it.Revision != 1 || !it.AsReceived ||
			it.State != tenant.TransferItemListed || it.TargetUID != "" {
			e.Fatalf("item %d is %+v, want %+v", i+1, it, w)
		}
	}
	rest, err := e.p.TransferItems(e.ctx, id, tenant.TransferItemListed, got[0].Key(), 10)
	e.ok("TransferItems after the first", err)
	if len(rest) != 1 || rest[0].SOPInstanceUID != got[1].SOPInstanceUID {
		e.Fatalf("the items after the first are %+v", rest)
	}
}

// testTransferRecords guards an item's moves: listed to copied to
// confirmed counts the instance sent; failing one counts it refused and
// lists it among the job's failures with its code; a move recorded again
// changes nothing; a backward move is refused; a step cannot be left with
// items it must settle; and a job ended refuses a record, its counts final.
func testTransferRecords(e *tenv) {
	items := e.transferSetup()
	id := e.id("j")
	_, err := e.queueTransfer(id, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, false, 10)
	e.ok("QueueTransfer", err)
	_, err = e.p.AddTransferItems(e.ctx, id, items)
	e.ok("AddTransferItems", err)
	_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferCopying, at(11))
	e.ok("StartTransferStep copying", err)
	rec := func(i int, st tenant.TransferItemState, s int) tenant.TransferRecord {
		r := tenant.TransferRecord{JobID: id, Key: tenant.TransferItemKey{Path: items[i].Path, SOPInstanceUID: items[i].SOPInstanceUID}, State: st,
			Now: at(s)}
		if st != tenant.TransferItemFailed {
			r.TargetUID = "2.25.9." + items[i].SOPInstanceUID[7:]
		} else {
			r.Code, r.Detail = tenant.FailurePeerUnaccounted, tenant.TransferDetailNotFound
		}
		return r
	}
	_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferConfirming, at(12))
	e.wantKind("leaving copying with items not copied", err, fault.Precondition)
	e.ok("copy 1", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemCopied, 13)))
	e.ok("copy 1 again", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemCopied, 14)))
	e.wantKind("a confirmed item of a listed one", e.p.RecordTransferItem(e.ctx, rec(1, tenant.TransferItemConfirmed, 14)), fault.Precondition)
	e.ok("copy 2", e.p.RecordTransferItem(e.ctx, rec(1, tenant.TransferItemCopied, 15)))
	tj, err := e.p.StartTransferStep(e.ctx, id, tenant.TransferConfirming, at(16))
	e.ok("StartTransferStep confirming", err)
	if tj.Items.Copied != 2 || tj.Job.Counts.InstancesSent != 0 {
		e.Fatalf("two copied items count %+v and %+v", tj.Items, tj.Job.Counts)
	}
	_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferEnded, at(16))
	e.wantKind("a copy's last step is its end, not a step", err, fault.Precondition)
	copied, err := e.p.TransferItems(e.ctx, id, tenant.TransferItemCopied, tenant.TransferItemKey{}, 10)
	e.ok("TransferItems copied", err)
	if len(copied) != 2 || copied[0].TargetUID != rec(0, tenant.TransferItemCopied, 0).TargetUID || !copied[0].Copied.Equal(at(13)) {
		e.Fatalf("the copied items are %+v, the first copied at the first record", copied)
	}
	e.ok("confirm 1", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemConfirmed, 17)))
	e.ok("confirm 1 again", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemConfirmed, 18)))
	e.wantKind("a confirmed item sent back to copied", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemCopied, 18)), fault.Precondition)
	e.wantKind("a confirmed item failed", e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemFailed, 18)), fault.Precondition)
	e.ok("fail 2", e.p.RecordTransferItem(e.ctx, rec(1, tenant.TransferItemFailed, 19)))
	e.ok("fail 2 again", e.p.RecordTransferItem(e.ctx, rec(1, tenant.TransferItemFailed, 20)))
	bad := rec(1, tenant.TransferItemFailed, 21)
	bad.Code = "nonsense"
	e.wantKind("a failure with a code the contract does not have", e.p.RecordTransferItem(e.ctx, bad), fault.Invalid)
	tj, err = e.p.TransferJob(e.ctx, id)
	e.ok("TransferJob", err)
	c := tj.Job.Counts
	if tj.Items.Confirmed != 1 || tj.Items.Failed != 1 || c.InstancesSent != 1 || c.BytesSent != items[0].Size || c.InstancesRefused != 1 ||
		c.BytesRefused != items[1].Size || c.InstancesRemaining() != 0 {
		e.Fatalf("the job's items are %+v and its counts %+v", tj.Items, c)
	}
	fails, _, err := e.p.ListJobFailures(e.ctx, id, true, master.Page{Limit: 10})
	e.ok("ListJobFailures", err)
	if len(fails) != 1 || fails[0].SOPInstanceUID != items[1].SOPInstanceUID || fails[0].Code != tenant.FailurePeerUnaccounted ||
		fails[0].Detail != tenant.TransferDetailNotFound || fails[0].Path != items[1].Path {
		e.Fatalf("the job's failures are %+v", fails)
	}
	failed, err := e.p.TransferItems(e.ctx, id, tenant.TransferItemFailed, tenant.TransferItemKey{}, 10)
	e.ok("TransferItems failed", err)
	if len(failed) != 1 || failed[0].Code != tenant.FailurePeerUnaccounted || failed[0].Detail != tenant.TransferDetailNotFound {
		e.Fatalf("the failed items are %+v", failed)
	}
	_, err = e.p.EndTransfer(e.ctx, id, tenant.JobDone, "", at(22))
	e.wantKind("a job with a failed item ended done", err, fault.Precondition)
	ended, err := e.p.EndTransfer(e.ctx, id, tenant.JobPartial, "", at(23))
	e.ok("EndTransfer partial", err)
	if ended.State != tenant.JobPartial || !ended.Ended.Equal(at(23)) {
		e.Fatalf("the ended job is %+v", ended)
	}
	tj, err = e.p.TransferJob(e.ctx, id)
	e.ok("TransferJob", err)
	if tj.Step != tenant.TransferEnded {
		e.Fatalf("the ended job's step is %q", tj.Step)
	}
	err = e.p.RecordTransferItem(e.ctx, rec(0, tenant.TransferItemConfirmed, 24))
	if !errors.Is(err, tenant.ErrTransferEnded) || fault.KindOf(err) != fault.Precondition {
		e.Fatalf("a record into an ended job: %v, want ErrTransferEnded", err)
	}
	again, err := e.p.EndTransfer(e.ctx, id, tenant.JobFailed, "x", at(25))
	e.ok("EndTransfer again", err)
	if again.State != tenant.JobPartial {
		e.Fatalf("a job ended again is %s, want it left partial", again.State)
	}
}

// testTransferMoveGate guards that a move deletes its source only after
// every item is confirmed: the step deleting is refused while an item is
// listed, copied or failed, and for a copy always; a job is done only when
// every instance it lists is confirmed; the delete a move made is recorded
// once and a second, different one is refused.
func testTransferMoveGate(e *tenv) {
	items := e.transferSetup()
	run := func(move bool, s int) string {
		id := e.id("j")
		_, err := e.queueTransfer(id, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, move, s)
		e.ok("QueueTransfer", err)
		_, err = e.p.AddTransferItems(e.ctx, id, items)
		e.ok("AddTransferItems", err)
		_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferCopying, at(s+1))
		e.ok("StartTransferStep copying", err)
		for i, it := range items {
			e.ok("copy", e.p.RecordTransferItem(e.ctx, tenant.TransferRecord{JobID: id, Key: tenant.TransferItemKey{Path: it.Path,
				SOPInstanceUID: it.SOPInstanceUID}, State: tenant.TransferItemCopied, TargetUID: uidOf(3, 900+i), Now: at(s + 2)}))
		}
		_, err = e.p.StartTransferStep(e.ctx, id, tenant.TransferConfirming, at(s+3))
		e.ok("StartTransferStep confirming", err)
		return id
	}
	confirm := func(id string, i, s int) {
		e.ok("confirm", e.p.RecordTransferItem(e.ctx, tenant.TransferRecord{JobID: id, Key: tenant.TransferItemKey{Path: items[i].Path,
			SOPInstanceUID: items[i].SOPInstanceUID}, State: tenant.TransferItemConfirmed, TargetUID: uidOf(3, 900+i), Now: at(s)}))
	}
	cp := run(false, 100)
	confirm(cp, 0, 110)
	confirm(cp, 1, 111)
	_, err := e.p.StartTransferStep(e.ctx, cp, tenant.TransferDeleting, at(112))
	e.wantKind("a copy's step deleting", err, fault.Precondition)
	done, err := e.p.EndTransfer(e.ctx, cp, tenant.JobDone, "", at(113))
	e.ok("EndTransfer done", err)
	if done.State != tenant.JobDone || done.Counts.InstancesSent != 2 {
		e.Fatalf("the copy ended %+v", done)
	}

	mv := run(true, 200)
	confirm(mv, 0, 210)
	_, err = e.p.StartTransferStep(e.ctx, mv, tenant.TransferDeleting, at(211))
	e.wantKind("a move's step deleting with an instance copied and not confirmed", err, fault.Precondition)
	_, err = e.p.EndTransfer(e.ctx, mv, tenant.JobDone, "", at(212))
	e.wantKind("a move done with an instance not confirmed", err, fault.Precondition)
	e.wantKind("a delete recorded before the step deleting", e.p.RecordTransferDelete(e.ctx, mv, e.newCorrectionID()), fault.Precondition)
	confirm(mv, 1, 213)
	tj, err := e.p.StartTransferStep(e.ctx, mv, tenant.TransferDeleting, at(214))
	e.ok("StartTransferStep deleting", err)
	if tj.Step != tenant.TransferDeleting {
		e.Fatalf("the move's step is %q", tj.Step)
	}
	c1, c2 := e.newCorrectionID(), e.newCorrectionID()
	e.ok("RecordTransferDelete", e.p.RecordTransferDelete(e.ctx, mv, c1))
	e.ok("RecordTransferDelete again", e.p.RecordTransferDelete(e.ctx, mv, c1))
	e.wantKind("a second, different delete", e.p.RecordTransferDelete(e.ctx, mv, c2), fault.Precondition)
	tj, err = e.p.TransferJob(e.ctx, mv)
	e.ok("TransferJob", err)
	if tj.DeleteCorrection != c1 {
		e.Fatalf("the recorded delete is %q, want %q", tj.DeleteCorrection, c1)
	}
	if _, err := e.p.EndTransfer(e.ctx, mv, tenant.JobDone, "", at(215)); err != nil {
		e.Fatalf("EndTransfer done: %v", err)
	}
}

// newCorrectionID returns a well-formed correction id, which differs from
// every one this test made before.
func (e *tenv) newCorrectionID() string {
	e.Helper()
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	n := e.seqNumber()
	b := []byte("01" + strings.Repeat("0", 24))
	for i := len(b) - 1; n > 0 && i >= 2; i-- {
		b[i] = alphabet[n%32]
		n /= 32
	}
	return string(b)
}

// seqNumber returns the next number of the test's sequence.
func (e *tenv) seqNumber() int {
	var n int
	for _, c := range e.id("") {
		n = n*10 + int(c-'0')
	}
	return n
}

// testTransferProvenance guards the target's side: an instance the tenant
// holds is found by its UID with the checksum it was received with and its
// labels, a UID it does not hold is absent; provenance is recorded keyed by
// the target's UID, kept as it is when recorded again, whoever the source, read by
// UID and listed by the source's job in pages; and a row that names an
// invalid UID or no revision is refused.
func testTransferProvenance(e *tenv) {
	items := e.transferSetup()
	got, err := e.p.FindTransferred(e.ctx, []string{items[0].SOPInstanceUID, items[1].SOPInstanceUID, uidOf(3, 777)})
	e.ok("FindTransferred", err)
	if len(got) != 2 {
		e.Fatalf("FindTransferred answered %+v, want the two instances held", got)
	}
	for _, it := range items {
		f, ok := got[it.SOPInstanceUID]
		if !ok || f.SHA256 != it.SHA256 || f.Path != it.Path {
			e.Fatalf("the instance %s is found as %+v, want checksum %x at %+v", it.SOPInstanceUID, f, it.SHA256, it.Path)
		}
	}
	empty, err := e.p.FindTransferred(e.ctx, nil)
	e.ok("FindTransferred of none", err)
	if len(empty) != 0 {
		e.Fatalf("FindTransferred of none answered %+v", empty)
	}
	_, err = e.p.FindTransferred(e.ctx, []string{"not a uid"})
	e.wantKind("a UID that is not one", err, fault.Invalid)
	rows := []tenant.TransferProvenance{
		{SOPInstanceUID: uidOf(3, 500), SourceTenantID: "t-src", Path: label.Path{Subject: "s1", Session: "e1", Scan: "c1"}, Revision: 2, JobID: "job-1", Recorded: at(5)},
		{SOPInstanceUID: uidOf(3, 501), SourceTenantID: "t-src", Revision: 1, JobID: "job-1", Recorded: at(5)},
		{SOPInstanceUID: uidOf(3, 502), SourceTenantID: "t-src", Revision: 1, JobID: "job-2", Recorded: at(5)},
	}
	e.ok("RecordTransferProvenance", e.p.RecordTransferProvenance(e.ctx, rows))
	e.ok("RecordTransferProvenance again", e.p.RecordTransferProvenance(e.ctx, rows))
	p, ok, err := e.p.TransferProvenanceOf(e.ctx, uidOf(3, 500))
	e.ok("TransferProvenanceOf", err)
	if !ok || p != rows[0] {
		e.Fatalf("the provenance is %+v (found %v), want %+v", p, ok, rows[0])
	}
	_, ok, err = e.p.TransferProvenanceOf(e.ctx, uidOf(3, 999))
	e.ok("TransferProvenanceOf of none", err)
	if ok {
		e.Fatalf("an instance with no provenance was found")
	}
	pg, next, err := e.p.ListTransferProvenance(e.ctx, "job-1", master.Page{Limit: 1})
	e.ok("ListTransferProvenance", err)
	if len(pg) != 1 || next == "" || pg[0].SOPInstanceUID != uidOf(3, 500) {
		e.Fatalf("the first page is %+v (next %q)", pg, next)
	}
	pg, next, err = e.p.ListTransferProvenance(e.ctx, "job-1", master.Page{Limit: 1, Cursor: next})
	e.ok("ListTransferProvenance second page", err)
	if len(pg) != 1 || next != "" || pg[0].SOPInstanceUID != uidOf(3, 501) {
		e.Fatalf("the second page is %+v (next %q)", pg, next)
	}
	// A later transfer that found the instance held, from another source,
	// does not take its provenance: the first stays.
	again := rows[0]
	again.SourceTenantID, again.Path, again.Revision, again.JobID, again.Recorded = "t-other", label.Path{}, 3, "job-3", at(9).Add(time.Second)
	e.ok("RecordTransferProvenance of another source", e.p.RecordTransferProvenance(e.ctx, []tenant.TransferProvenance{again}))
	p, _, err = e.p.TransferProvenanceOf(e.ctx, uidOf(3, 500))
	e.ok("TransferProvenanceOf", err)
	if p != rows[0] {
		e.Fatalf("the provenance after another source's is %+v, want the first %+v", p, rows[0])
	}
	for _, bad := range []tenant.TransferProvenance{
		{SOPInstanceUID: "x y", SourceTenantID: "t-src", Revision: 1, JobID: "j", Recorded: at(1)},
		{SOPInstanceUID: uidOf(3, 503), SourceTenantID: "t-src", Revision: 0, JobID: "j", Recorded: at(1)},
	} {
		e.wantKind("a provenance row that is not valid", e.p.RecordTransferProvenance(e.ctx, []tenant.TransferProvenance{bad}), fault.Invalid)
	}
}
