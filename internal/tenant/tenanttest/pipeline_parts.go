package tenanttest

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// parts returns the store as tenant.PipelineParts, the pipeline's narrower
// interfaces, which every driver implements as part of Pipeline.
func (e *penv) parts() tenant.PipelineParts { return e.p }

// testStorageSecret guards that an S3 storage's sealed secret comes back
// exactly as AddStorage or SetStorage was given it, kept through a change
// that does not name it, while Storage still never answers it; a storage
// not there, or not of type S3, is NotFound.
func testStorageSecret(e *penv) {
	pp := e.parts()
	_, err := e.p.AddStorage(e.ctx, s3Storage("bucket", "p/"), at(1), e.audit(), nil)
	e.ok("AddStorage", err)
	got, err := pp.StorageSecret(e.ctx, "bucket")
	e.ok("StorageSecret", err)
	if string(got) != "sealed-secret-bytes" {
		e.Errorf("StorageSecret = %q, want the sealed bytes given", got)
	}
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "bucket", S3: tenant.S3Storage{AccessKeyID: "NEWKEY", SealedSecret: []byte("new-sealed")}},
		[]tenant.StorageField{tenant.FieldS3AccessKeyID, tenant.FieldS3SecretKey}, at(2), e.audit(), nil)
	e.ok("SetStorage credentials", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "bucket", CapacityBytes: 10}, []tenant.StorageField{tenant.FieldCapacityBytes}, at(3), e.audit(), nil)
	e.ok("SetStorage capacity", err)
	if got, err = pp.StorageSecret(e.ctx, "bucket"); err != nil || string(got) != "new-sealed" {
		e.Errorf("StorageSecret after a change of credentials and one of capacity = %q, %v; want the new sealed bytes", got, err)
	}
	if s, _ := e.p.Storage(e.ctx, "bucket"); s.S3.SealedSecret != nil {
		e.Error("Storage answered the sealed secret")
	}
	e.addStorage("disk")
	_, err = pp.StorageSecret(e.ctx, "disk")
	e.wantKind("StorageSecret of a directory storage", err, fault.NotFound)
	_, err = pp.StorageSecret(e.ctx, "missing")
	e.wantKind("StorageSecret of a storage not there", err, fault.NotFound)
	_, err = pp.StorageSecret(e.ctx, "Bad Name")
	e.wantKind("StorageSecret of a malformed name", err, fault.Invalid)
}

// testTenantSecrets guards the tenant's own secrets: none before one is
// kept (NotFound); the sealed bytes come back exactly as kept; a second
// keep with other bytes answers the first and changes nothing, also when
// several keep at once; an unknown name, or sealed bytes empty or too long,
// is Invalid and keeps nothing.
func testTenantSecrets(e *penv) {
	pp := e.parts()
	_, err := pp.TenantSecret(e.ctx, tenant.SecretNaming)
	e.wantKind("TenantSecret before one is kept", err, fault.NotFound)
	_, err = pp.KeepTenantSecret(e.ctx, tenant.SecretNaming, nil, at(1))
	e.wantKind("KeepTenantSecret of no bytes", err, fault.Invalid)
	_, err = pp.KeepTenantSecret(e.ctx, tenant.SecretNaming, make([]byte, tenant.MaxSealedSecretLength+1), at(1))
	e.wantKind("KeepTenantSecret of too many bytes", err, fault.Invalid)
	_, err = pp.KeepTenantSecret(e.ctx, "date-shift-unknown", []byte("x"), at(1))
	e.wantKind("KeepTenantSecret of an unknown name", err, fault.Invalid)
	_, err = pp.TenantSecret(e.ctx, "date-shift-unknown")
	e.wantKind("TenantSecret of an unknown name", err, fault.Invalid)
	_, err = pp.TenantSecret(e.ctx, tenant.SecretNaming)
	e.wantKind("TenantSecret after refused keeps", err, fault.NotFound)

	// Several at once, each with its own bytes: one wins, and every caller
	// is answered the winner's.
	const n = 8
	got := make([][]byte, n)
	errs := make([]error, n)
	done := make(chan int, n)
	for i := range n {
		go func() {
			got[i], errs[i] = pp.KeepTenantSecret(e.ctx, tenant.SecretNaming, []byte{"abcdefgh"[i], 0, 0xff}, at(2))
			done <- i
		}()
	}
	for range n {
		<-done
	}
	for i := range n {
		e.ok("KeepTenantSecret at once", errs[i])
		if string(got[i]) != string(got[0]) {
			e.Fatalf("KeepTenantSecret at once answered %q and %q", got[0], got[i])
		}
	}
	if len(got[0]) != 3 || got[0][1] != 0 || got[0][2] != 0xff {
		e.Fatalf("the kept secret %q is not one of the bytes given", got[0])
	}
	again, err := pp.KeepTenantSecret(e.ctx, tenant.SecretNaming, []byte("other bytes"), at(3))
	e.ok("KeepTenantSecret again", err)
	if string(again) != string(got[0]) {
		e.Errorf("KeepTenantSecret again = %q, want the first %q", again, got[0])
	}
	stored, err := pp.TenantSecret(e.ctx, tenant.SecretNaming)
	e.ok("TenantSecret", err)
	if string(stored) != string(got[0]) {
		e.Errorf("TenantSecret = %q, want %q", stored, got[0])
	}
}

// testReleaseReason guards a release's record: the reason the person gave
// is kept on the record, a release again keeps the first reason, an empty
// reason records none, a reason of spaces or too long is Invalid; and the
// reception the file joined is in the audit entry, also when the release
// is answered as it stands.
func testReleaseReason(e *penv) {
	pp := e.parts()
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	joins, alone, plain := e.differentFile("a", r.ID, 7), e.heldFile("a", 40, 2), e.heldFile("a", 41, 3)
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{joins, alone, plain}, nil)
	e.ok("HoldFiles", err)
	for _, reason := range []string{"   ", strings.Repeat("x", tenant.MaxDecisionReasonLength+1)} {
		_, err := pp.ReleaseFile(e.ctx, tenant.Release{ID: joins.ID, Mode: tenant.ReleaseAsIs, ReceptionID: "rel-0", Reason: reason, Now: at(5)}, e.audit(), nil)
		e.wantKind("ReleaseFile with a reason of spaces or too long", err, fault.Invalid)
	}
	audit := e.audit()
	got, err := pp.ReleaseFile(e.ctx, tenant.Release{ID: joins.ID, Mode: tenant.ReleaseAsIs, ReceptionID: "rel-1",
		Reason: "the sender confirmed the file", Now: at(10)}, audit, nil)
	e.ok("ReleaseFile", err)
	if got.State != tenant.QuarantineReleased || got.ReleaseReason != "the sender confirmed the file" || got.ReleaseReception != r.ID {
		e.Errorf("a release with a reason reads %+v", got)
	}
	if read, _ := e.p.QuarantinedFile(e.ctx, joins.ID); read.ReleaseReason != got.ReleaseReason {
		e.Errorf("the record read back has the reason %q, want %q", read.ReleaseReason, got.ReleaseReason)
	}
	entryOf := func(id string) master.AuditEntry {
		e.Helper()
		for _, a := range e.allAudit(master.AuditFilter{}) {
			if a.ID == id {
				return a
			}
		}
		e.Fatalf("no audit entry %s", id)
		return master.AuditEntry{}
	}
	if d := entryOf(audit.ID).Details; d[tenant.DetailReleaseReception] != r.ID {
		e.Errorf("the release's audit entry holds %v, want the reception it joined, %s", d, r.ID)
	}
	again := e.audit()
	got, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: joins.ID, Mode: tenant.ReleaseAsIs, ReceptionID: "rel-2",
		Reason: "another reason", Now: at(20)}, again, nil)
	e.ok("ReleaseFile again", err)
	if got.ReleaseReason != "the sender confirmed the file" || !got.Decided.Equal(at(10)) {
		e.Errorf("a release again changed the record: %+v", got)
	}
	if d := entryOf(again.ID).Details; d[tenant.DetailReleaseReception] != r.ID {
		e.Errorf("a release answered as it stands has the entry %v, want the reception it joined", d)
	}
	lone := e.audit()
	got, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: alone.ID, Mode: tenant.ReleaseAsIs, ReceptionID: "rel-3",
		Reason: "looked at it", Now: at(30)}, lone, nil)
	e.ok("ReleaseFile of a file that joins no reception", err)
	if got.ReleaseReception != "" || got.ReleaseReason != "looked at it" {
		e.Errorf("a release that joined nothing reads %+v", got)
	}
	if _, ok := entryOf(lone.ID).Details[tenant.DetailReleaseReception]; ok {
		e.Error("a release that joined no reception names one in its audit entry")
	}
	got, err = e.p.ReleaseQuarantined(e.ctx, plain.ID, tenant.ReleaseAsIs, "", "rel-4", at(40), e.audit(), nil)
	e.ok("ReleaseQuarantined", err)
	if got.ReleaseReason != "" {
		e.Errorf("a release with no reason recorded %q", got.ReleaseReason)
	}
}

// testFencedFiles guards the fence of the file writes: a worker whose lease
// was taken over records no received file and holds no file, and is told
// ErrLeaseLost, even with an empty batch; the worker that holds the lease
// records both; a nil fence writes under no lease, as a file's arrival does.
func testFencedFiles(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	e.enqueue("w1", at(0))
	first, err := e.p.ClaimWork(e.ctx, "reception", "worker-1", at(0), time.Minute)
	e.ok("ClaimWork", err)
	second, err := e.p.ClaimWork(e.ctx, "reception", "worker-2", at(120), time.Minute)
	e.ok("ClaimWork after the first lease ended", err)
	stale, held := first.Fence(), second.Fence()
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1)}, at(121), &stale)
	e.wantIs("AddReceivedFiles under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{e.heldFile("a", 10, 1)}, &stale)
	e.wantIs("HoldFiles under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	if got, _, _ := e.p.Reception(e.ctx, r.ID); got.Counts.FilesReceived != 0 {
		e.Errorf("a write under a lost lease recorded %d files", got.Counts.FilesReceived)
	}
	if files, _, _ := e.p.QuarantineTotals(e.ctx); files != 0 {
		e.Errorf("a hold under a lost lease recorded %d files", files)
	}
	n, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 1)}, at(122), &held)
	e.ok("AddReceivedFiles under the lease", err)
	m, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{e.heldFile("a", 10, 2)}, &held)
	e.ok("HoldFiles under the lease", err)
	k, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r, 1, 2)}, at(123), nil)
	e.ok("AddReceivedFiles with no fence", err)
	if n != 1 || m != 1 || k != 1 {
		e.Errorf("writes under the lease and with none recorded %d, %d, %d; want 1 each", n, m, k)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, nil, at(124), &stale)
	e.wantIs("an empty batch under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	_, err = e.p.HoldFiles(e.ctx, nil, &stale)
	e.wantIs("an empty hold under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	_, err = e.p.HoldFiles(e.ctx, nil, &tenant.WorkFence{ID: "w1"})
	e.wantKind("HoldFiles with a malformed fence", err, fault.Invalid)
}

// testIncomingNamed guards IncomingNamed: a path a received file's row or a
// held quarantine record keeps is named, a path nothing keeps is not, and a
// record whose bytes were let go by its rejection no longer names its place.
func testIncomingNamed(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	f := file(r, 1, 1)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2), nil)
	e.ok("AddReceivedFiles", err)
	q := e.heldFile("a", 10, 3)
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
	e.ok("HoldFiles", err)
	for path, want := range map[string]bool{f.Incoming: true, q.Incoming: true, "a/zz/none": false} {
		got, err := e.p.IncomingNamed(e.ctx, path)
		e.ok("IncomingNamed", err)
		if got != want {
			e.Errorf("IncomingNamed(%q) = %v, want %v", path, got, want)
		}
	}
	_, err = e.p.RejectQuarantined(e.ctx, q.ID, "a stray file", at(4), e.audit(), nil)
	e.ok("RejectQuarantined", err)
	if got, err := e.p.IncomingNamed(e.ctx, q.Incoming); err != nil || got {
		e.Errorf("a rejected record still names its place: %v %v", got, err)
	}
	// The record keeps where its bytes were, for the removal that follows.
	if got, err := e.p.QuarantinedFile(e.ctx, q.ID); err != nil || got.Incoming != q.Incoming || got.ContentKept {
		e.Errorf("a rejected record keeps place %q (kept %v), want %q, not kept: %v", got.Incoming, got.ContentKept, q.Incoming, err)
	}
}

// testClaimNextWork guards that a claim says what it gave up on the way:
// each unit held as failed because its last attempt's lease ended, with
// the reception it carried and whether that reception failed then; nothing
// due is not an error and still lists them; a unit leased is answered with
// them.
func testClaimNextWork(e *penv) {
	pp := e.parts()
	e.addReceiver("a", "store", 11112)
	r, _ := e.identified("a", 1, 1)
	ended := e.stepTo(e.openReception("a", 2, at(1)), reception.Done, "store")
	for _, w := range []tenant.Work{
		{ID: "g1", Kind: "archive", ReceptionID: r.ID, Due: at(0), MaxAttempts: 1},
		{ID: "g2", Kind: "archive", Due: at(1), MaxAttempts: 1},
		{ID: "g3", Kind: "archive", ReceptionID: ended.ID, Due: at(2), MaxAttempts: 1},
	} {
		_, _, err := e.p.EnqueueWork(e.ctx, w, T0)
		e.ok("EnqueueWork", err)
	}
	for range 3 {
		_, err := e.p.ClaimWork(e.ctx, "archive", "worker-1", at(10), time.Minute)
		e.ok("ClaimWork", err)
	}
	c, err := pp.ClaimNextWork(e.ctx, "archive", "worker-2", at(3600), time.Minute)
	e.ok("ClaimNextWork with only units whose last lease ended", err)
	want := []tenant.GaveUpWork{{WorkID: "g1", ReceptionID: r.ID, ReceptionFailed: true}, {WorkID: "g2"}, {WorkID: "g3", ReceptionID: ended.ID}}
	if c.Found || !slices.Equal(c.GaveUp, want) {
		e.Errorf("ClaimNextWork = found %v, gave up %+v; want nothing found and %+v", c.Found, c.GaveUp, want)
	}
	if got, _, _ := e.p.Reception(e.ctx, r.ID); got.State != reception.Failed || got.Failure != tenant.FailureWorkGaveUp {
		e.Errorf("the reception of a unit given up is %s (%q)", got.State, got.Failure)
	}
	if again, err := pp.ClaimNextWork(e.ctx, "archive", "worker-2", at(3601), time.Minute); err != nil || again.Found || len(again.GaveUp) != 0 {
		e.Errorf("a claim after the units were given up = %+v, %v; want nothing", again, err)
	}
	_, _, err = e.p.EnqueueWork(e.ctx, tenant.Work{ID: "g4", Kind: "archive", Due: at(0), MaxAttempts: 1}, T0)
	e.ok("EnqueueWork", err)
	_, _, err = e.p.EnqueueWork(e.ctx, tenant.Work{ID: "g5", Kind: "archive", Due: at(5000)}, T0)
	e.ok("EnqueueWork", err)
	_, err = e.p.ClaimWork(e.ctx, "archive", "worker-1", at(4000), time.Minute)
	e.ok("ClaimWork", err)
	c, err = pp.ClaimNextWork(e.ctx, "archive", "worker-2", at(6000), time.Minute)
	e.ok("ClaimNextWork", err)
	if !c.Found || c.Work.ID != "g5" || c.Work.Attempt != 1 || len(c.GaveUp) != 1 || c.GaveUp[0].WorkID != "g4" {
		e.Errorf("ClaimNextWork past a unit given up = %+v", c)
	}
	_, err = pp.ClaimNextWork(e.ctx, "archive", "worker-2", at(6000), 0)
	e.wantKind("ClaimNextWork with no lease", err, fault.Invalid)
}

// testScanInstances guards the catalog's list of a scan: every instance
// held in it, whichever reception confirmed it, in order of SOP Instance
// UID, read back as committed, paged by a cursor bound to its series; a
// claim not confirmed is not listed; a series with no scan lists nothing.
func testScanInstances(e *penv) {
	pp := e.parts()
	e.addReceiver("a", "store", 11112)
	first, files := e.identified("a", 1, 3, 1, 2)
	e.commit(first, files...)
	second, more := e.identified("a", 1, 4)
	e.commit(second, more...)
	third, pending := e.identified("a", 1, 5)
	e.claim(third, claimOf(pending[0]))
	series := uidOf(2, 1)
	list := allPages(e, 2, func(p master.Page) ([]tenant.ScanInstance, string, error) { return pp.ScanInstances(e.ctx, series, p) })
	var uids []string
	for _, in := range list {
		uids = append(uids, in.SOPInstanceUID)
	}
	if want := []string{uidOf(3, 1), uidOf(3, 2), uidOf(3, 3), uidOf(3, 4)}; !slices.Equal(uids, want) {
		e.Fatalf("ScanInstances = %q, want %q", uids, want)
	}
	byUID := map[string]tenant.ReceivedFile{}
	for _, f := range append(files, more...) {
		byUID[f.SOPInstanceUID] = f
	}
	labels := e.labelsOf(first.StudyUID, series)
	for _, in := range list {
		f := byUID[in.SOPInstanceUID]
		key, err := layout.InstanceKey(labels, in.SOPInstanceUID)
		e.ok("InstanceKey", err)
		by := first.ID
		if in.SOPInstanceUID == uidOf(3, 4) {
			by = second.ID
		}
		if in.ReceptionID != by || in.Storage != "store" || in.Size != f.Size || in.Length != f.Size || in.Offset != 0 ||
			in.SHA256 != f.SHA256 || in.ArchivedSHA256 != f.SHA256 || in.Key != key || in.Held.IsZero() {
			e.Errorf("an instance of the scan reads %+v", in)
		}
	}
	none, next, err := pp.ScanInstances(e.ctx, uidOf(2, 99), master.Page{Limit: 10})
	e.ok("ScanInstances of a series with no scan", err)
	if len(none) != 0 || next != "" {
		e.Errorf("a series with no scan lists %d instances", len(none))
	}
	_, cursor, err := pp.ScanInstances(e.ctx, series, master.Page{Limit: 1})
	e.ok("ScanInstances", err)
	_, _, err = pp.ScanInstances(e.ctx, uidOf(2, 99), master.Page{Limit: 1, Cursor: cursor})
	e.wantKind("ScanInstances with another series' cursor", err, fault.Invalid)
	_, _, err = pp.ScanInstances(e.ctx, "1.2.x", master.Page{Limit: 1})
	e.wantKind("ScanInstances of a malformed UID", err, fault.Invalid)
}

// testNewQuarantineReasons guards the rules of the reasons a header past
// the reader's bound and a lost incoming copy are held with: a header too
// large is kept, with its bound, so a person can release or reject it; a
// lost copy has no bytes to keep; each read back as held.
func testNewQuarantineReasons(e *penv) {
	e.addReceiver("a", "store", 11112)
	header := e.heldFile("a", 70000, 1)
	header.Reason, header.Details = tenant.QuarantineHeaderTooLarge, map[string]string{"limit": "65536"}
	lost := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineIncomingLost, Receiver: "a", Size: 900, SHA256: sum(5),
		SOPInstanceUID: uidOf(3, 5), StudyUID: uidOf(1, 1), SeriesUID: uidOf(2, 1), Quarantined: at(2)}
	n, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{header, lost}, nil)
	e.ok("HoldFiles", err)
	if n != 2 {
		e.Errorf("HoldFiles = %d, want 2", n)
	}
	for _, q := range []tenant.QuarantinedFile{header, lost} {
		got, err := e.p.QuarantinedFile(e.ctx, q.ID)
		e.ok("QuarantinedFile", err)
		if got.Reason != q.Reason || got.ContentKept != q.ContentKept || got.State != tenant.QuarantineHeld {
			e.Errorf("a record of %s reads %+v", q.Reason, got)
		}
	}
	if files, bytes, _ := e.p.QuarantineTotals(e.ctx); files != 2 || bytes != 70000 {
		e.Errorf("totals %d files, %d bytes; want 2 and 70000, the lost file's bytes not held", files, bytes)
	}
	unkept := e.heldFile("a", 1, 3)
	unkept.Reason, unkept.Details, unkept.Incoming, unkept.ContentKept = tenant.QuarantineHeaderTooLarge, nil, "", false
	kept := e.heldFile("a", 1, 4)
	kept.Reason, kept.Details = tenant.QuarantineIncomingLost, nil
	for _, q := range []tenant.QuarantinedFile{unkept, kept} {
		_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.wantKind("HoldFiles of "+string(q.Reason)+" with its bytes kept the wrong way", err, fault.Invalid)
	}
	_, err = e.p.RejectQuarantined(e.ctx, header.ID, "not readable", at(10), e.audit(), nil)
	e.ok("RejectQuarantined of a header too large", err)
	_, err = e.p.ReleaseQuarantined(e.ctx, lost.ID, tenant.ReleaseAsIs, "", "rel-1", at(10), e.audit(), nil)
	e.wantPrecondition("ReleaseQuarantined of a file whose incoming copy is lost", err)
}

// testReclaimWorkOfKinds guards ReclaimWorkOfKinds: every unit of a kind
// named that is leased is queued again at once, with no owner; a unit
// leased of a kind not named stays leased by its owner (another part of the
// same server may be working it); a unit queued is left as it is; the next
// claim takes the unit with its next attempt, and the fence of the lease
// that was given back can no longer renew it; the kinds are bounded.
func testReclaimWorkOfKinds(e *penv) {
	pp := e.parts()
	held := e.enqueue("w1", at(0))
	e.enqueue("w2", at(0))
	waiting := e.enqueue("w3", at(100))
	_, _, err := e.p.EnqueueWork(e.ctx, tenant.Work{ID: "live", Kind: "send-peer", Due: at(0)}, T0)
	e.ok("EnqueueWork of another kind", err)
	leased, err := e.p.ClaimWork(e.ctx, "reception", "dead-1", at(1), time.Hour)
	e.ok("ClaimWork", err)
	second, err := e.p.ClaimWork(e.ctx, "reception", "dead-2", at(1), time.Hour)
	e.ok("ClaimWork", err)
	live, err := e.p.ClaimWork(e.ctx, "send-peer", "alive-sender", at(1), time.Hour)
	e.ok("ClaimWork of another kind", err)
	n, err := pp.ReclaimWorkOfKinds(e.ctx, []string{"reception", "scrub", "reception"}, at(2))
	e.ok("ReclaimWorkOfKinds", err)
	if n != 2 {
		e.Errorf("ReclaimWorkOfKinds gave back %d units, want the 2 leased of the kind named", n)
	}
	for _, id := range []string{leased.ID, second.ID} {
		w, err := e.p.Work(e.ctx, id)
		e.ok("Work", err)
		if w.State != tenant.WorkQueued || w.Owner != "" || !w.Due.Equal(at(2)) || w.Attempt != leased.Attempt {
			e.Errorf("a unit given back is %s, owner %q, due %v, attempt %d; want queued, no owner, due at the reclaim, attempt %d kept",
				w.State, w.Owner, w.Due, w.Attempt, leased.Attempt)
		}
	}
	if w, _ := e.p.Work(e.ctx, live.ID); w.State != tenant.WorkLeased || w.Owner != "alive-sender" || w.Attempt != live.Attempt {
		e.Errorf("a unit of a kind not named is %s, owner %q, attempt %d; want still leased by alive-sender, attempt %d",
			w.State, w.Owner, w.Attempt, live.Attempt)
	}
	_, err = e.p.RenewWork(e.ctx, tenant.WorkFence{ID: live.ID, Attempt: live.Attempt}, at(2), time.Hour)
	e.ok("RenewWork of a unit of a kind not named", err)
	if w, _ := e.p.Work(e.ctx, waiting.ID); w.State != tenant.WorkQueued || !w.Due.Equal(at(100)) {
		e.Errorf("a unit queued was changed: %s due %v", w.State, w.Due)
	}
	again, err := e.p.ClaimWork(e.ctx, "reception", "alive", at(3), time.Minute)
	e.ok("ClaimWork after the reclaim", err)
	if again.ID != held.ID || again.Attempt != leased.Attempt+1 {
		e.Errorf("the claim after the reclaim took %s attempt %d, want %s attempt %d", again.ID, again.Attempt, held.ID, leased.Attempt+1)
	}
	_, err = e.p.RenewWork(e.ctx, tenant.WorkFence{ID: leased.ID, Attempt: leased.Attempt}, at(4), time.Minute)
	e.wantIs("RenewWork with the fence of a lease given back", err, tenant.ErrLeaseLost, fault.Conflict)

	most := make([]string, tenant.MaxReclaimKinds)
	for i := range most {
		most[i] = "kind-" + strconv.Itoa(i)
	}
	most[0] = "send-peer"
	n, err = pp.ReclaimWorkOfKinds(e.ctx, most, at(5))
	e.ok("ReclaimWorkOfKinds of the most kinds", err)
	if w, _ := e.p.Work(e.ctx, live.ID); n != 1 || w.State != tenant.WorkQueued {
		e.Errorf("ReclaimWorkOfKinds of the most kinds gave back %d units, the live one %s; want 1, queued", n, w.State)
	}
	for _, c := range []struct {
		name  string
		kinds []string
	}{
		{"no kind", nil},
		{"too many kinds", append(most, "one-more")},
		{"a kind that is not a name", []string{"reception", "Not A Name"}},
		{"an empty kind", []string{""}},
	} {
		_, err := pp.ReclaimWorkOfKinds(e.ctx, c.kinds, at(6))
		e.wantKind("ReclaimWorkOfKinds of "+c.name, err, fault.Invalid)
	}
	_, err = pp.ReclaimWorkOfKinds(e.ctx, []string{"reception"}, time.Time{})
	e.wantKind("ReclaimWorkOfKinds at the zero time", err, fault.Invalid)
}
