package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantTotalsRecounted fails unless every running total of the catalog
// equals a count of what it totals, read through the port: each scan's
// instances and bytes the instances it lists, each session's scans,
// instances and bytes its scans, each subject's sessions, instances and
// bytes its sessions, the tenant's totals the subjects, sessions, scans and
// instances listed, and the count a list answers the rows it lists. Every
// row's update time is at or after its creation.
func (e *penv) wantTotalsRecounted(when string) {
	e.Helper()
	totals, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	subjects, total := catalogPages(e, 1000, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
		return e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, p)
	})
	e.wantTotal(when+": the subjects' count", total, int64(len(subjects)))
	var sessions, scans, instances int64
	for _, sub := range subjects {
		list, total := catalogPages(e, 1000, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
			return e.p.ListSessions(e.ctx, tenant.SessionFilter{SubjectLabel: sub.Label}, p)
		})
		e.wantTotal(when+": the count of "+sub.Label+"'s sessions", total, int64(len(list)))
		var subInstances, subBytes int64
		for _, ses := range list {
			scanList, total := catalogPages(e, 1000, func(p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
				return e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: sub.Label, SessionLabel: ses.Label}, p)
			})
			e.wantTotal(when+": the count of "+ses.Label+"'s scans", total, int64(len(scanList)))
			var sesInstances, sesBytes int64
			for _, sc := range scanList {
				held := allPages(e, 1000, func(p master.Page) ([]tenant.ScanInstance, string, error) {
					return e.p.ListHeldInstances(e.ctx, label.Path{Subject: sub.Label, Session: ses.Label, Scan: sc.Label}, p)
				})
				var bytes int64
				for _, in := range held {
					bytes += in.Size
				}
				if sc.Instances != int64(len(held)) || sc.Bytes != bytes {
					e.Errorf("%s: scan %s/%s/%s totals %d instances, %d bytes; it holds %d, %d", when, sub.Label, ses.Label, sc.Label,
						sc.Instances, sc.Bytes, len(held), bytes)
				}
				if sc.Updated.Before(sc.Created) {
					e.Errorf("%s: scan %s was updated before it was made", when, sc.Label)
				}
				sesInstances, sesBytes = sesInstances+sc.Instances, sesBytes+sc.Bytes
			}
			if ses.Scans != int64(len(scanList)) || ses.Instances != sesInstances || ses.Bytes != sesBytes {
				e.Errorf("%s: session %s/%s totals %d scans, %d instances, %d bytes; its scans count %d, %d, %d", when, sub.Label, ses.Label,
					ses.Scans, ses.Instances, ses.Bytes, len(scanList), sesInstances, sesBytes)
			}
			if ses.Updated.Before(ses.Created) {
				e.Errorf("%s: session %s was updated before it was made", when, ses.Label)
			}
			scans += int64(len(scanList))
			subInstances, subBytes = subInstances+sesInstances, subBytes+sesBytes
		}
		if sub.Sessions != int64(len(list)) || sub.Instances != subInstances || sub.Bytes != subBytes {
			e.Errorf("%s: subject %s totals %d sessions, %d instances, %d bytes; its sessions count %d, %d, %d", when, sub.Label,
				sub.Sessions, sub.Instances, sub.Bytes, len(list), subInstances, subBytes)
		}
		if sub.Updated.Before(sub.Created) {
			e.Errorf("%s: subject %s was updated before it was made", when, sub.Label)
		}
		sessions += int64(len(list))
		instances += subInstances
	}
	want := tenant.CatalogCounts{Subjects: int64(len(subjects)), Sessions: sessions, Scans: scans, Instances: instances}
	if totals != want {
		e.Errorf("%s: the catalog's totals %+v; a count gives %+v", when, totals, want)
	}
}

// testCatalogTotals guards the running totals of subjects and sessions
// (and the scans' and the tenant's) against a recount after each path that
// adds held instances or could seem to: a reception's commits, a commit
// written again after a crash (which moves neither the subject's nor the
// scan's update time), a file sent again (a duplicate, which adds
// nothing), claims released when a reception fails, a quarantined file
// released into a new reception and archived, and a failed reception
// retried after part of it was archived (what it held counted once).
func testCatalogTotals(e *penv) {
	e.catalogTenant()
	e.wantTotalsRecounted("after the receptions")

	// A commit written again: its instances are counted once.
	r1, _, err := e.p.Reception(e.ctx, e.receptionOf(uidOf(1, 1)))
	e.ok("Reception", err)
	path := label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-t1"}
	held := allPages(e, 1000, func(p master.Page) ([]tenant.ScanInstance, string, error) {
		return e.p.ListHeldInstances(e.ctx, path, p)
	})
	again := tenant.ScanCommit{ReceptionID: r1.ID, StudyUID: r1.StudyUID, SeriesUID: seriesOf(1, 0), SubjectLabel: path.Subject,
		SessionLabel: path.Session, ScanLabel: path.Scan, Storage: "store", At: at(150), Values: tenant.ScanValuesOf("", "MR", "", "")}
	for _, in := range held {
		again.Instances = append(again.Instances, tenant.ConfirmedInstance{SOPInstanceUID: in.SOPInstanceUID, Key: in.Key,
			Length: in.Length, Size: in.Size, ArchivedSHA256: in.ArchivedSHA256})
	}
	scanBefore, err := e.p.CatalogScan(e.ctx, path)
	e.ok("CatalogScan", err)
	_, err = e.p.CommitScan(e.ctx, again)
	e.ok("CommitScan written again", err)
	e.wantTotalsRecounted("after a commit written again")
	sub, err := e.p.CatalogSubject(e.ctx, "sub-a")
	e.ok("CatalogSubject", err)
	if !sub.Updated.Equal(at(20)) {
		e.Errorf("a commit that added nothing moved the subject's update time to %v", sub.Updated)
	}
	scanAfter, err := e.p.CatalogScan(e.ctx, path)
	e.ok("CatalogScan", err)
	if !scanAfter.Updated.Equal(scanBefore.Updated) {
		e.Errorf("a commit that added nothing moved the scan's update time from %v to %v", scanBefore.Updated, scanAfter.Updated)
	}

	// The same files sent again: duplicates, and a claim of a new file that
	// its reception's failure releases.
	r, _, err := e.p.OpenReception(e.ctx, "recv-a", uidOf(1, 1), e.id("rc"), at(160))
	e.ok("OpenReception", err)
	var files []tenant.ReceivedFile
	for _, in := range held {
		files = append(files, tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: in.SOPInstanceUID, SeriesUID: seriesOf(1, 0),
			StudyUID: r.StudyUID, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: in.Size,
			SHA256: in.SHA256, Incoming: r.ID + "/" + in.SOPInstanceUID, Association: "assoc9"})
	}
	extra := files[0]
	extra.SOPInstanceUID, extra.SHA256, extra.Incoming = instanceOf(1, 0, 50), sum(4242), r.ID+"/extra"
	_, err = e.p.AddReceivedFiles(e.ctx, append(append([]tenant.ReceivedFile{}, files...), extra), at(161))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, reception.Identified, "store")
	claims := []tenant.InstanceClaim{claimOf(extra)}
	for _, f := range files {
		claims = append(claims, claimOf(f))
	}
	answers := e.claim(r, claims...)
	for i, a := range answers[1:] {
		e.wantDup("a file sent again", a, tenant.SameFile, files[i].SHA256, "")
	}
	e.wantTotalsRecounted("with an instance claimed and not confirmed")
	e.failReception(r)
	e.wantTotalsRecounted("after the claims of a failed reception are released")

	// A quarantined file released into a new reception and archived.
	q := e.releasedFile()
	rel, err := e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", e.id("rc"), at(220), e.audit(), nil)
	e.ok("ReleaseQuarantined", err)
	joined, _, err := e.p.Reception(e.ctx, rel.ReleaseReception)
	e.ok("Reception", err)
	joined = e.stepTo(joined, reception.Identified, "store")
	got := allPages(e, 1000, func(p master.Page) ([]tenant.ReceivedFile, string, error) {
		return e.p.ReceivedFiles(e.ctx, joined.ID, p)
	})
	e.commitValues(joined, label.Path{Subject: "sub-r", Session: "ses-r", Scan: "scan-r"}, 230, tenant.ScanValuesOf("20240101", "US", "", ""), got)
	e.wantTotalsRecounted("after a released file is archived")
	if s, err := e.p.CatalogSubject(e.ctx, "sub-r"); err != nil || s.Instances != 1 {
		e.Errorf("the subject of a released file: %+v, %v", s, err)
	}

	// A failed reception that archived part of its files, retried.
	r = e.openReception("recv-b", 99, at(300))
	var parts []tenant.ReceivedFile
	for k := range 3 {
		f := file(r, 99, 10000+k)
		parts = append(parts, f)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, parts, at(301))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, reception.Identified, "store")
	retryPath := label.Path{Subject: "sub-01", Session: "ses-" + r.ID, Scan: "scan-retry"}
	e.commitValues(r, retryPath, 310, tenant.ScanValuesOf("", "MR", "", ""), parts[:1])
	e.wantTotalsRecounted("after part of a reception is archived")
	e.failReception(r)
	_, retry, err := e.p.RetryReception(e.ctx, r.ID, e.id("rr"), "", at(320), e.audit(), nil)
	e.ok("RetryReception", err)
	retry = e.stepTo(retry, reception.Identified, "store")
	again2 := allPages(e, 1000, func(p master.Page) ([]tenant.ReceivedFile, string, error) {
		return e.p.ReceivedFiles(e.ctx, retry.ID, p)
	})
	e.commitValues(retry, retryPath, 330, tenant.ScanValuesOf("", "MR", "", ""), again2)
	e.wantTotalsRecounted("after a retry")
	ses, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-"+r.ID)
	e.ok("CatalogSession", err)
	if ses.Instances != 3 || ses.Scans != 1 || !ses.Updated.Equal(at(330)) || ses.Receiver != "recv-b" {
		e.Errorf("the session of a retried reception: %+v, want its 3 instances counted once", ses)
	}
}

// receptionOf returns the id of the first arrival of the study.
func (e *penv) receptionOf(studyUID string) string {
	e.Helper()
	list, _, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{StudyUID: studyUID}, master.Page{Limit: 1000})
	e.ok("ListReceptions", err)
	if len(list) == 0 {
		e.Fatalf("no reception of study %s", studyUID)
	}
	return list[len(list)-1].ID
}

// releasedFile receives one file of a new study at recv-a, holds it in
// quarantine as a label the rule could not make, and ends its reception,
// so a release of the record opens a new reception; it returns the record.
func (e *penv) releasedFile() tenant.QuarantinedFile {
	e.Helper()
	r := e.openReception("recv-a", 9, at(200))
	f := file(r, 9, 9001)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(201))
	e.ok("AddReceivedFiles", err)
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
		Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: "recv-a", ReceptionID: r.ID,
		SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
		TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: at(202)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("HoldFiles", err)
	e.stepTo(r, reception.Done, "store")
	return q
}
