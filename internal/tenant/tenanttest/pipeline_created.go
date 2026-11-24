package tenanttest

import (
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCreatedIsEarliestHeld guards that a subject's, a session's and a
// scan's created time is the earliest held time of the instances beneath
// them, whatever the order their commits land in: each commit takes its
// held time from the caller's clock before its transaction, so a commit
// held later may land first. A scan held at 200 committed first, then a
// scan held at 100, then an instance of the first scan held at 50: the
// created times follow down each time, the session's modalities with the
// session (a list by modality bounded by a time range finds it), and never
// move up; a rebuild of the catalog from the storage derives the same
// times from the rows beneath, which the catalog in use must equal.
func testCreatedIsEarliestHeld(e *penv) {
	e.addReceiver("a", "store", 11112)
	late := uidOf(2, 5)
	r, f := e.identifiedWith("a", 1, func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{file(r, 1, 1), variant(file(r, 1, 2), 2, late), variant(file(r, 1, 3), 3, late)}
	})
	e.claim(r, claimOf(f[0]), claimOf(f[1]), claimOf(f[2]))
	commitAt := func(series string, held time.Time, files ...tenant.ReceivedFile) {
		e.Helper()
		p := labelPath("sub-01", "ses-"+r.StudyUID, "scan-"+series)
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: series, SubjectLabel: p.Subject,
			SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", At: held,
			Values: tenant.ScanValues{Modality: "MR"}}
		for _, x := range files {
			key, err := layout.InstanceKey(p, x.SOPInstanceUID)
			e.ok("InstanceKey", err)
			c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: x.SOPInstanceUID, Key: key,
				Length: x.Size, Size: x.Size, ArchivedSHA256: x.SHA256})
		}
		_, err := e.p.CommitScan(e.ctx, c)
		e.ok("CommitScan", err)
	}
	want := func(when string, subject, session, early, later time.Time) {
		e.Helper()
		sub, err := e.p.CatalogSubject(e.ctx, "sub-01")
		e.ok("CatalogSubject", err)
		ses, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-"+r.StudyUID)
		e.ok("CatalogSession", err)
		a, err := e.p.CatalogScan(e.ctx, labelPath("sub-01", "ses-"+r.StudyUID, "scan-"+uidOf(2, 1)))
		e.ok("CatalogScan", err)
		b, err := e.p.CatalogScan(e.ctx, labelPath("sub-01", "ses-"+r.StudyUID, "scan-"+late))
		e.ok("CatalogScan", err)
		for _, c := range []struct {
			what      string
			got, want time.Time
		}{{"subject", sub.Created, subject}, {"session", ses.Created, session}, {"scan held at 100", a.Created, early},
			{"scan held at 200", b.Created, later}} {
			if !c.got.Equal(c.want) {
				e.Errorf("%s: the %s's created time is %v, want %v, the earliest held time beneath it", when, c.what, c.got, c.want)
			}
		}
		page, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{Modality: "MR", Order: tenant.OrderNewest,
			Range: tenant.TimeRange{Start: session, End: session.Add(time.Second)}}, master.Page{Limit: 10})
		e.ok("ListSessions", err)
		if len(page.Items) != 1 || !page.Items[0].Created.Equal(session) {
			e.Errorf("%s: the sessions of modality MR created at %v are %+v, want the session", when, session, page.Items)
		}
	}
	commitAt(late, at(200), f[1])
	commitAt(uidOf(2, 1), at(100), f[0])
	want("a scan held earlier committed after one held later", at(100), at(100), at(100), at(200))
	commitAt(late, at(50), f[2])
	want("an instance held earliest committed last into the later scan", at(50), at(50), at(100), at(50))
}
