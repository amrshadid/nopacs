package tenanttest

import (
	"errors"
	"fmt"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// claimOf returns the claim of a received file, with another checksum or
// series when they are not zero.
func claimOf(f tenant.ReceivedFile) tenant.InstanceClaim {
	return tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
}

// claim claims files for r and fails the test on an error.
func (e *penv) claim(r tenant.Reception, claims ...tenant.InstanceClaim) []tenant.Duplicate {
	e.Helper()
	got, err := e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	return got
}

// check returns CheckDuplicates's answer for one claim.
func (e *penv) check(c tenant.InstanceClaim) tenant.Duplicate {
	e.Helper()
	got, err := e.p.CheckDuplicates(e.ctx, []tenant.DuplicateQuery{c.Query()})
	e.ok("CheckDuplicates", err)
	return got[0]
}

// wantDup fails unless d has the kind, and, when held is not zero, that
// held checksum, and when by is not empty, that reception.
func (e *penv) wantDup(what string, d tenant.Duplicate, kind tenant.DuplicateKind, held tenant.SHA256, by string) {
	e.Helper()
	if d.Kind != kind || (!held.IsZero() && d.HeldSHA256 != held) || (by != "" && d.HeldBy != by) {
		e.Errorf("%s: %+v, want %v held %s by %q", what, d, kind, held, by)
	}
}

// identified opens a reception of study at receiver with files 'ns' and
// moves it to identified, archiving to storage "store".
func (e *penv) identified(receiver string, study int, ns ...int) (tenant.Reception, []tenant.ReceivedFile) {
	e.Helper()
	return e.identifiedWith(receiver, study, func(r tenant.Reception) []tenant.ReceivedFile {
		var files []tenant.ReceivedFile
		for _, n := range ns {
			files = append(files, file(r, study, n))
		}
		return files
	})
}

// identifiedWith opens a reception of study at receiver, records the files
// make returns for it, and moves it to identified on storage "store".
func (e *penv) identifiedWith(receiver string, study int, make func(tenant.Reception) []tenant.ReceivedFile) (tenant.Reception, []tenant.ReceivedFile) {
	e.Helper()
	r := e.openReception(receiver, study, at(10))
	files := make(r)
	if len(files) > 0 {
		_, err := e.p.AddReceivedFiles(e.ctx, files, at(11))
		e.ok("AddReceivedFiles", err)
	}
	return e.stepTo(r, reception.Identified, "store"), files
}

// variant returns f with another checksum (and place), or another series.
func variant(f tenant.ReceivedFile, checksum int, series string) tenant.ReceivedFile {
	f.SHA256, f.Incoming = sum(checksum), fmt.Sprintf("%s.v%d", f.Incoming, checksum)
	if series != "" {
		f.SeriesUID = series
	}
	return f
}

// testClaims guards every answer of the claim, each against the rule that
// gives it: a UID no one has is claimed now; claimed again by the same
// reception it is claimed before; another reception's claim answers
// Claimed, whatever its study, until it is confirmed, then the held answers
// (the same file, a different one, the UID elsewhere); a UID this reception
// confirmed answers archived before when its step runs again; two checksums
// under one UID in one reception keep the lower checksum whatever the order
// given; a claim names only a file the reception received, as received. On
// SQLite's one writer two claims are never at the same moment; a driver
// with several writers is shown by testInstanceClaimsShared, which a
// Postgres run exercises with true concurrency.
func testClaims(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	e.addReceiver("c", "store", 11114)
	r1, f := e.identifiedWith("a", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		twin := file(r, 1, 30)
		return []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2), twin, variant(twin, 31, ""), file(r, 1, 600)}
	})
	f1, f2, twin1, twin2, late := f[0], f[1], f[2], f[3], f[4]
	r2, g := e.identifiedWith("b", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		same := file(r, 1, 1)
		return []tenant.ReceivedFile{same, variant(same, 77, ""), variant(same, 78, uidOf(2, 9))}
	})
	g1, gDifferent, gSeries := g[0], g[1], g[2]
	r3, h := e.identifiedWith("c", 9, func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{file(r, 9, 2)}
	})

	got := e.claim(r1, claimOf(f1), claimOf(f2))
	e.wantDup("a new UID", got[0], tenant.ClaimedNow, tenant.SHA256{}, "")
	e.wantDup("another new UID", got[1], tenant.ClaimedNow, tenant.SHA256{}, "")
	got = e.claim(r1, claimOf(f1), claimOf(f2))
	e.wantDup("a claim written again", got[0], tenant.ClaimedBefore, sum(1), r1.ID)
	e.wantDup("CheckDuplicates of a claimed UID", e.check(claimOf(f1)), tenant.Claimed, sum(1), r1.ID)
	e.wantDup("another reception's claim", e.claim(r2, claimOf(g1))[0], tenant.Claimed, sum(1), r1.ID)
	e.wantDup("a UID claimed by another under another study: decided against what is held, not a claim",
		e.claim(r3, claimOf(h[0]))[0], tenant.Claimed, sum(2), r1.ID)

	_, err := e.p.ClaimInstances(e.ctx, r2.ID, []tenant.InstanceClaim{claimOf(file(r2, 1, 500))}, at(20), nil)
	e.wantPrecondition("a claim of a file the reception did not receive", err)
	moved := claimOf(g1)
	moved.StudyUID = uidOf(1, 7)
	_, err = e.p.ClaimInstances(e.ctx, r2.ID, []tenant.InstanceClaim{moved}, at(20), nil)
	e.wantPrecondition("a claim of a received file under a study it was not received in", err)

	e.confirm(r1, []tenant.ReceivedFile{f1}, []tenant.Duplicate{{Kind: tenant.ClaimedBefore}})
	e.wantDup("a held UID, the same file", e.claim(r2, claimOf(g1))[0], tenant.SameFile, sum(1), r1.ID)
	e.wantDup("a held UID, another checksum", e.claim(r2, claimOf(gDifferent))[0], tenant.DifferentFile, sum(1), r1.ID)
	e.wantDup("CheckDuplicates of a different file", e.check(claimOf(gDifferent)), tenant.DifferentFile, sum(1), r1.ID)
	e.wantDup("a held UID under another series", e.claim(r2, claimOf(gSeries))[0], tenant.UIDElsewhere, sum(1), r1.ID)
	e.wantDup("CheckDuplicates of a UID no one has", e.check(claimOf(file(r2, 1, 500))), tenant.NotHeld, tenant.SHA256{}, "")

	got = e.claim(r1, claimOf(f1), claimOf(f2))
	e.wantDup("a claim written again after its scan was committed", got[0], tenant.ArchivedBefore, sum(1), r1.ID)
	e.wantDup("a claim written again, the file not committed yet", got[1], tenant.ClaimedBefore, sum(2), r1.ID)
	e.confirm(r1, []tenant.ReceivedFile{f2}, []tenant.Duplicate{{Kind: tenant.ClaimedBefore}})
	e.wantDup("a UID held under another study, once confirmed", e.claim(r3, claimOf(h[0]))[0], tenant.UIDElsewhere, sum(2), r1.ID)

	got = e.claim(r1, claimOf(twin2), claimOf(twin1))
	e.wantDup("one UID twice in one batch, the lower checksum", got[1], tenant.ClaimedNow, tenant.SHA256{}, "")
	e.wantDup("one UID twice in one batch, the other", got[0], tenant.DifferentFile, sum(30), r1.ID)
	got = e.claim(r1, claimOf(twin1), claimOf(twin2))
	e.wantDup("the twins again, in the other order, the kept one", got[0], tenant.ClaimedBefore, sum(30), r1.ID)
	e.wantDup("the twins again, in the other order, the other", got[1], tenant.DifferentFile, sum(30), r1.ID)

	_, err = e.p.ClaimInstances(e.ctx, "missing", []tenant.InstanceClaim{claimOf(f1)}, at(1), nil)
	e.wantKind("ClaimInstances for a reception not there", err, fault.NotFound)
	receiving := e.openReception("a", 3, at(1))
	_, err = e.p.ClaimInstances(e.ctx, receiving.ID, []tenant.InstanceClaim{claimOf(f1)}, at(1), nil)
	e.wantPrecondition("ClaimInstances for a reception not identified", err)
	ended, endedFiles := e.identified("a", 4, 700)
	ended = e.failReception(ended)
	_, err = e.p.ClaimInstances(e.ctx, ended.ID, []tenant.InstanceClaim{claimOf(endedFiles[0])}, at(1), nil)
	e.wantPrecondition("ClaimInstances for a reception that ended", err)
	_, err = e.p.ClaimInstances(e.ctx, r1.ID, []tenant.InstanceClaim{claimOf(f1), claimOf(f1)}, at(1), nil)
	e.wantKind("ClaimInstances naming one file twice", err, fault.Invalid)

	_, _, err = e.p.EnqueueWork(e.ctx, tenant.Work{ID: "claim-work", Kind: "reception", Due: at(0)}, T0)
	e.ok("EnqueueWork", err)
	w, err := e.p.ClaimWork(e.ctx, "reception", "worker-1", at(0), time.Minute)
	e.ok("ClaimWork", err)
	_, err = e.p.ClaimWork(e.ctx, "reception", "worker-2", at(61), time.Minute)
	e.ok("ClaimWork after the lease ended", err)
	fence := w.Fence()
	_, err = e.p.ClaimInstances(e.ctx, r1.ID, []tenant.InstanceClaim{claimOf(late)}, at(70), &fence)
	e.wantIs("ClaimInstances under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	e.wantDup("a claim under a lost lease", e.check(claimOf(late)), tenant.NotHeld, tenant.SHA256{}, "")
}

// testClaimsOutliveTheirReception guards what stays and what goes when a
// reception ends: the instances it confirmed stay held whatever its state
// (failed after archiving, retried, discarded), so its retry finds them the
// same file and archives them no second time; the claims it did not confirm
// are released in the step's transaction, so another reception can claim
// them.
func testClaimsOutliveTheirReception(e *penv) {
	e.addReceiver("a", "store", 11112)
	r, f := e.identified("a", 1, 1, 2)
	e.claim(r, claimOf(f[0]), claimOf(f[1]))
	e.confirm(r, f[:1], []tenant.Duplicate{{Kind: tenant.ClaimedBefore}})
	r = e.failReception(r)
	e.wantDup("a confirmed instance of a reception that failed", e.check(claimOf(f[0])), tenant.SameFile, sum(1), r.ID)
	e.wantDup("a claim of a reception that failed", e.check(claimOf(f[1])), tenant.NotHeld, tenant.SHA256{}, "")
	_, retry, err := e.p.RetryReception(e.ctx, r.ID, "retry-1", "", at(500), e.audit(), nil)
	e.ok("RetryReception", err)
	retry = e.stepTo(retry, reception.Identified, "store")
	rf := []tenant.ReceivedFile{f[0], f[1]}
	rf[0].ReceptionID, rf[1].ReceptionID = retry.ID, retry.ID
	got := e.claim(retry, claimOf(rf[0]), claimOf(rf[1]))
	e.wantDup("the retry's claim of what the failed reception archived", got[0], tenant.SameFile, sum(1), r.ID)
	e.wantDup("the retry's claim of what it did not", got[1], tenant.ClaimedNow, tenant.SHA256{}, "")

	d, df := e.identified("a", 2, 10)
	pending := claimOf(df[0])
	e.claim(d, pending)
	d = e.failReception(d)
	_, err = e.p.DiscardReception(e.ctx, d.ID, "not wanted", at(600), e.audit(), nil)
	e.ok("DiscardReception", err)
	e.wantDup("a claim of a discarded reception", e.check(pending), tenant.NotHeld, tenant.SHA256{}, "")
}

// testCommitScan guards the catalog a commit writes: the subject, session
// and scan found or made, with the labels they were first given and the
// session's storage; a commit written again counting nothing twice; the
// running totals equal to what was made; every label conflict, each with
// its level and fault; labels and storage other than an existing session's
// or scan's own refused; a series of another session refused; a commit
// with no instance, under another study than its reception's, of an
// instance claimed in another series, for a reception that ended, or of an
// instance no one claims or another reception claims, refused.
func testCommitScan(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addStorage("other-store")
	r, f := e.identifiedWith("a", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{file(r, 1, 1), file(r, 1, 2), file(r, 1, 3), file(r, 1, 50), variant(file(r, 1, 40), 40, uidOf(2, 2)), file(r, 1, 51)}
	})
	answers := e.commit(r, f[:3]...)
	for i, a := range answers {
		if a.Kind != tenant.ClaimedNow {
			e.Errorf("file %d claimed as %v", i, a.Kind)
		}
	}
	totals, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	if totals != (tenant.CatalogCounts{Subjects: 1, Sessions: 1, Scans: 1, Instances: 3}) {
		e.Errorf("totals after one scan of three = %+v", totals)
	}
	labels, err := e.p.CatalogLabels(e.ctx, r.StudyUID, []string{uidOf(2, 1), uidOf(2, 9)})
	e.ok("CatalogLabels", err)
	if labels.Session == nil || labels.Session.SubjectLabel != "sub-01" || labels.Session.SessionLabel != "ses-"+r.StudyUID ||
		labels.Session.Storage != "store" || labels.Scans[uidOf(2, 1)] != "scan-"+uidOf(2, 1) || len(labels.Scans) != 1 {
		e.Errorf("CatalogLabels = %+v %v", labels.Session, labels.Scans)
	}
	scans, truncated, err := e.p.ReceptionScans(e.ctx, r.ID)
	e.ok("ReceptionScans", err)
	if truncated || len(scans) != 1 || scans[0].Instances != 3 || scans[0].Bytes != 1001+1002+1003 || scans[0].SeriesUID != uidOf(2, 1) {
		e.Errorf("ReceptionScans = %+v, %v", scans, truncated)
	}
	rc, _, _ := e.p.Reception(e.ctx, r.ID)
	if rc.Counts.FilesArchived != 3 || rc.Counts.BytesArchived != 3006 {
		e.Errorf("the reception's archived counts = %d, %d; want 3, 3006", rc.Counts.FilesArchived, rc.Counts.BytesArchived)
	}

	e.confirm(r, f[:3], []tenant.Duplicate{{Kind: tenant.ClaimedBefore}, {Kind: tenant.ClaimedBefore}, {Kind: tenant.ClaimedBefore}})
	again, _ := e.p.CatalogTotals(e.ctx)
	rc2, _, _ := e.p.Reception(e.ctx, r.ID)
	if again != totals || rc2.Counts != rc.Counts {
		e.Errorf("a commit written again changed the totals %+v -> %+v or the counts %+v -> %+v", totals, again, rc.Counts, rc2.Counts)
	}

	// withInstance gives a commit one instance whose key is its scan's, so
	// the rules of labels and studies are reached before the instance's
	// own; an instance no one claims is refused after them.
	withInstance := func(c tenant.ScanCommit, uid string) tenant.ScanCommit {
		key, err := layout.InstanceKey(labelPath(c.SubjectLabel, c.SessionLabel, c.ScanLabel), uid)
		e.ok("InstanceKey", err)
		c.Instances = []tenant.ConfirmedInstance{{SOPInstanceUID: uid, Key: key, Size: 1, ArchivedSHA256: sum(1)}}
		return c
	}
	base := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: uidOf(2, 1), SubjectLabel: "sub-01",
		SessionLabel: "ses-" + r.StudyUID, ScanLabel: "scan-" + uidOf(2, 1), Storage: "store", At: at(100)}
	commit := func(change func(*tenant.ScanCommit)) error {
		c := base
		change(&c)
		_, err := e.p.CommitScan(e.ctx, withInstance(c, "2.25.99.1"))
		return err
	}
	wantConflict := func(what string, err error, level, faultWord string) {
		e.Helper()
		var lc *tenant.LabelConflict
		if !errors.As(err, &lc) || lc.Label != level || lc.Fault != faultWord || fault.KindOf(err) != fault.Conflict {
			e.Errorf("%s: %v, want a %s label conflict %s", what, err, level, faultWord)
		}
	}
	_, err = e.p.CommitScan(e.ctx, base)
	e.wantKind("a commit with no instance", err, fault.Invalid)
	// claimed is an instance this reception claims in the scan's series, so
	// a commit of it is refused only by the rule a case breaks.
	e.claim(r, claimOf(f[5]))
	claimed := func(change func(*tenant.ScanCommit)) error {
		c := base
		change(&c)
		_, err := e.p.CommitScan(e.ctx, withInstance(c, f[5].SOPInstanceUID))
		return err
	}
	wantConflict("an existing session under another subject", commit(func(c *tenant.ScanCommit) { c.SubjectLabel = "sub-02" }),
		"subject", tenant.FaultTwoSubjectsForOneStudy)
	e.wantPrecondition("an existing session under another label", claimed(func(c *tenant.ScanCommit) { c.SessionLabel = "ses-renamed" }))
	e.wantPrecondition("an existing session on another storage", claimed(func(c *tenant.ScanCommit) { c.Storage = "other-store" }))
	e.wantPrecondition("an existing scan under another label", claimed(func(c *tenant.ScanCommit) { c.ScanLabel = "scan-renamed" }))
	e.wantPrecondition("a storage the tenant does not have", commit(func(c *tenant.ScanCommit) { c.Storage = "nosuch" }))
	e.wantPrecondition("a study other than its reception's", commit(func(c *tenant.ScanCommit) { c.StudyUID = uidOf(1, 99) }))
	wantConflict("a new scan with a label its session holds", commit(func(c *tenant.ScanCommit) { c.SeriesUID = uidOf(2, 2) }),
		"scan", tenant.FaultHeldByAnotherScan)
	wantConflict("a new scan with a label its session holds in another case", commit(func(c *tenant.ScanCommit) {
		c.SeriesUID, c.ScanLabel = uidOf(2, 2), "SCAN-"+uidOf(2, 1)
	}), "scan", tenant.FaultCaseCollision)
	e.wantPrecondition("an instance no one claims", commit(func(*tenant.ScanCommit) {}))

	// An instance claimed in series 1, confirmed into the scan of series 99.
	e.claim(r, claimOf(f[3]))
	c := base
	c.SeriesUID, c.ScanLabel = uidOf(2, 99), "scan-99"
	_, err = e.p.CommitScan(e.ctx, withInstance(c, f[3].SOPInstanceUID))
	e.wantPrecondition("an instance claimed in another series", err)

	r2, f2 := e.identified("a", 2, 20)
	other := tenant.ScanCommit{ReceptionID: r2.ID, StudyUID: r2.StudyUID, SeriesUID: uidOf(2, 2), SubjectLabel: "sub-01",
		SessionLabel: "ses-" + r.StudyUID, ScanLabel: "scan-x", Storage: "store", At: at(100)}
	try := func(change func(*tenant.ScanCommit), uid string) error {
		c := other
		change(&c)
		_, err := e.p.CommitScan(e.ctx, withInstance(c, uid))
		return err
	}
	wantConflict("a new session with a label its subject holds", try(func(*tenant.ScanCommit) {}, f2[0].SOPInstanceUID),
		"session", tenant.FaultHeldByAnotherSession)
	wantConflict("a new session with a label its subject holds in another case", try(func(c *tenant.ScanCommit) {
		c.SessionLabel = "SES-" + r.StudyUID
	}, f2[0].SOPInstanceUID), "session", tenant.FaultCaseCollision)
	wantConflict("a new subject with a held subject label in another case", try(func(c *tenant.ScanCommit) {
		c.SubjectLabel, c.SessionLabel = "SUB-01", "ses-two"
	}, f2[0].SOPInstanceUID), "subject", tenant.FaultCaseCollision)
	err = try(func(c *tenant.ScanCommit) { c.SessionLabel, c.SeriesUID = "ses-two", uidOf(2, 1) }, f2[0].SOPInstanceUID)
	if !errors.Is(err, tenant.ErrSeriesElsewhere) || fault.KindOf(err) != fault.Conflict {
		e.Errorf("a series of another session: %v, want ErrSeriesElsewhere", err)
	}
	// An instance another reception claims: r claims it, r2 may not confirm
	// it.
	e.claim(r, claimOf(f[4]))
	e.wantPrecondition("an instance another reception claims", try(func(c *tenant.ScanCommit) { c.SessionLabel = "ses-two" }, f[4].SOPInstanceUID))
	e.wantDup("an instance another reception tried to confirm", e.check(claimOf(f[4])), tenant.Claimed, f[4].SHA256, r.ID)
	if final, _ := e.p.CatalogTotals(e.ctx); final != totals {
		e.Errorf("refused commits changed the totals %+v -> %+v", totals, final)
	}
	e.claim(r2, claimOf(f2[0]))
	e.wantPrecondition("a new session on a storage the tenant does not have", try(func(c *tenant.ScanCommit) {
		c.SessionLabel, c.Storage = "ses-two", "nosuch"
	}, f2[0].SOPInstanceUID))
	// Another reception of the same study may not confirm what r claims.
	e.addReceiver("b", "store", 11113)
	same, _ := e.identified("b", 1)
	sameStudy := base
	sameStudy.ReceptionID, sameStudy.SeriesUID, sameStudy.ScanLabel = same.ID, uidOf(2, 2), "scan-x2"
	_, err = e.p.CommitScan(e.ctx, withInstance(sameStudy, f[4].SOPInstanceUID))
	e.wantPrecondition("an instance another reception of the same study claims", err)
	e.ok("a second session of the subject", try(func(c *tenant.ScanCommit) { c.SessionLabel = "ses-two" }, f2[0].SOPInstanceUID))
	if final, _ := e.p.CatalogTotals(e.ctx); final != (tenant.CatalogCounts{Subjects: 1, Sessions: 2, Scans: 2, Instances: 4}) {
		e.Errorf("totals after a second session = %+v", final)
	}

	ended := e.failReception(r)
	c = base
	c.ReceptionID = ended.ID
	_, err = e.p.CommitScan(e.ctx, withInstance(c, f[3].SOPInstanceUID))
	e.wantPrecondition("a commit of a reception that ended", err)
}

// labelPath returns the three labels as a label.Path.
func labelPath(subject, session, scan string) label.Path {
	return label.Path{Subject: subject, Session: session, Scan: scan}
}

// testCommitSharesSubject guards that two studies of one subject, committed
// one after the other, share one subject row, and that a third study whose
// subject label differs only in case is refused rather than made a second
// subject. On SQLite the commits are ordered by its one writer; with several
// writers the find-or-make of each row (an insert that does nothing when the
// row is there, then a read) gives the same answer for commits made at once,
// which a Postgres run of testInstanceClaimsShared's kind would show.
func testCommitSharesSubject(e *penv) {
	e.addReceiver("a", "store", 11112)
	r1, f1 := e.identified("a", 1, 1)
	r2, f2 := e.identified("a", 2, 2)
	e.commit(r1, f1...)
	e.commit(r2, f2...)
	totals, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	if totals.Subjects != 1 || totals.Sessions != 2 {
		e.Errorf("two studies of one subject made %d subjects and %d sessions, want 1 and 2", totals.Subjects, totals.Sessions)
	}
}

// testInstanceUIDOncePerTenant guards the port's rule that a SOP Instance
// UID names one instance in a tenant, not one per scan: an instance held in
// a scan of one study is the only one under its UID, and a file under the
// same UID and the same bytes in another study, or in another series of
// the same study, is the UID elsewhere, by CheckDuplicates and by the
// claim, names the scan that holds it, cannot be confirmed into its own
// scan, and leaves the catalog as it was; so an instance is found by its
// UID alone.
func testInstanceUIDOncePerTenant(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	r1, f1 := e.identified("a", 1, 7)
	e.commit(r1, f1...)
	held := f1[0]
	before, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)

	// The same UID and checksum: in another study (series 2 of study 2),
	// then in another series of the same study.
	r2, f2 := e.identifiedWith("b", 2, func(r tenant.Reception) []tenant.ReceivedFile { return []tenant.ReceivedFile{file(r, 2, 7)} })
	r3, f3 := e.identifiedWith("a", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{variant(file(r, 1, 7), 7, uidOf(2, 3))}
	})
	for _, c := range []struct {
		what string
		r    tenant.Reception
		f    tenant.ReceivedFile
	}{{"another study", r2, f2[0]}, {"another series of the same study", r3, f3[0]}} {
		if c.f.SOPInstanceUID != held.SOPInstanceUID || c.f.SHA256 != held.SHA256 || c.f.SeriesUID == held.SeriesUID {
			e.Fatalf("%s: the suite's file is not the held UID and bytes under another series", c.what)
		}
		d := e.check(claimOf(c.f))
		e.wantDup("CheckDuplicates, "+c.what, d, tenant.UIDElsewhere, held.SHA256, r1.ID)
		if d.HeldStudyUID != held.StudyUID || d.HeldSeriesUID != held.SeriesUID {
			e.Errorf("CheckDuplicates, %s: names study %s series %s, want the held instance's %s %s", c.what,
				d.HeldStudyUID, d.HeldSeriesUID, held.StudyUID, held.SeriesUID)
		}
		e.wantDup("the claim, "+c.what, e.claim(c.r, claimOf(c.f))[0], tenant.UIDElsewhere, held.SHA256, r1.ID)
		p := e.labelsOf(c.r.StudyUID, c.f.SeriesUID)
		key, err := layout.InstanceKey(p, c.f.SOPInstanceUID)
		e.ok("InstanceKey", err)
		_, err = e.p.CommitScan(e.ctx, tenant.ScanCommit{ReceptionID: c.r.ID, StudyUID: c.r.StudyUID, SeriesUID: c.f.SeriesUID,
			SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", At: at(100),
			Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: c.f.SOPInstanceUID, Key: key, Length: c.f.Size, Size: c.f.Size, ArchivedSHA256: c.f.SHA256}}})
		e.wantPrecondition("a commit of a held UID into a scan of "+c.what, err)
		if got := allPages(e, 10, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
			return e.p.ScanInstances(e.ctx, c.f.SeriesUID, pg)
		}); len(got) != 0 {
			e.Errorf("the scan of %s lists %d instances, want none", c.what, len(got))
		}
	}
	got := allPages(e, 10, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
		return e.p.ScanInstances(e.ctx, held.SeriesUID, pg)
	})
	if len(got) != 1 || got[0].SOPInstanceUID != held.SOPInstanceUID || got[0].ReceptionID != r1.ID {
		e.Errorf("the held scan lists %+v, want the one instance its reception confirmed", got)
	}
	if after, _ := e.p.CatalogTotals(e.ctx); after != before {
		e.Errorf("the catalog changed %+v -> %+v for a UID it holds", before, after)
	}
}
