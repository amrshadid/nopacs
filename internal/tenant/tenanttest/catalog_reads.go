package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCatalogGets guards the catalog's gets: a subject, a session and a
// scan found by their labels compared exactly (a label in another case is
// not found, never taken as the one held), each with its running totals,
// times, receiver, storage and header values as its commits left them, a
// session's modalities each once in byte order, and NotFound for what the
// tenant does not hold.
func testCatalogGets(e *penv) {
	e.catalogTenant()
	sub, err := e.p.CatalogSubject(e.ctx, "sub-a")
	e.ok("CatalogSubject", err)
	want := tenant.CatalogSubject{Label: "sub-a", Sessions: 2, Instances: 7, Bytes: 3*510 + 3 + 2*513 + 1 + 2*520 + 1,
		Created: at(10), Updated: at(20)}
	if sub != want {
		e.Errorf("CatalogSubject sub-a = %+v, want %+v", sub, want)
	}
	if _, err := e.p.CatalogSubject(e.ctx, "sub-b"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("CatalogSubject of a label held only in another case: %v, want NotFound", err)
	}
	if _, err := e.p.CatalogSubject(e.ctx, "sub-none"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("CatalogSubject of a label not held: %v, want NotFound", err)
	}
	_, err = e.p.CatalogSubject(e.ctx, "not a label")
	e.wantKind("CatalogSubject of text that is not a label", err, fault.Invalid)

	ses, err := e.p.CatalogSession(e.ctx, "Sub-B", "Ses-X")
	e.ok("CatalogSession", err)
	if ses.StudyUID != uidOf(1, 3) || ses.StudyDate != "" || !slices.Equal(ses.Modalities, []string{"MR", "SR"}) ||
		ses.Scans != 2 || ses.Instances != 5 || ses.Storage != "store" || ses.Receiver != "recv-a" ||
		!ses.Created.Equal(at(30)) || !ses.Updated.Equal(at(31)) {
		e.Errorf("CatalogSession Sub-B/Ses-X = %+v", ses)
	}
	ses, err = e.p.CatalogSession(e.ctx, "sub-a", "ses-2")
	e.ok("CatalogSession", err)
	if ses.StudyDate != "20210301" || ses.Receiver != "recv-b" || !slices.Equal(ses.Modalities, []string{"CT"}) {
		e.Errorf("CatalogSession sub-a/ses-2 = %+v", ses)
	}
	for _, p := range [][2]string{{"Sub-B", "ses-x"}, {"sub-b", "Ses-X"}, {"sub-a", "ses-3"}, {"sub-c", "ses-2"}} {
		if _, err := e.p.CatalogSession(e.ctx, p[0], p[1]); fault.KindOf(err) != fault.NotFound {
			e.Errorf("CatalogSession %s/%s: %v, want NotFound", p[0], p[1], err)
		}
	}

	scan, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-c", Session: "ses-1", Scan: "scan-pt"})
	e.ok("CatalogScan", err)
	wantValues := tenant.ScanValues{Modality: "PT", SeriesDescription: "PET Äbrain", SeriesNumber: -5, HasSeriesNumber: true}
	if scan.SeriesUID != seriesOf(4, 0) || scan.Values != wantValues || scan.Instances != 2 || scan.Bytes != 540+541 ||
		scan.Storage != "store" || scan.Receiver != "recv-b" || scan.ManifestSHA256 != sum(40+7000) || !scan.Created.Equal(at(40)) {
		e.Errorf("CatalogScan sub-c/ses-1/scan-pt = %+v", scan)
	}
	scan, err = e.p.CatalogScan(e.ctx, label.Path{Subject: "Sub-B", Session: "Ses-X", Scan: "scan-sr"})
	e.ok("CatalogScan", err)
	if scan.Values != (tenant.ScanValues{Modality: "SR"}) {
		e.Errorf("a scan whose files held no description or number: %+v", scan.Values)
	}
	for _, p := range []label.Path{{Subject: "sub-a", Session: "ses-1", Scan: "scan-T1"}, {Subject: "sub-a", Session: "ses-2", Scan: "scan-t1"},
		{Subject: "sub-c", Session: "ses-1", Scan: "scan-none"}} {
		if _, err := e.p.CatalogScan(e.ctx, p); fault.KindOf(err) != fault.NotFound {
			e.Errorf("CatalogScan %v: %v, want NotFound", p, err)
		}
	}
	_, err = e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-a", Session: "", Scan: "scan-t1"})
	e.wantKind("CatalogScan with an empty label", err, fault.Invalid)
}

// testCatalogLists guards each combination of filter and order the lists
// serve: the rows each keeps, in the order it names (newest first by time
// then id; parent first by folded label; study date latest first; series
// description by its folded text), whole when read a row a page at a time,
// with a count only where a running total gives it.
func testCatalogLists(e *penv) {
	e.catalogTenant()
	subjects := func(what string, f tenant.SubjectFilter, want []string, total int64) {
		e.Helper()
		for _, limit := range []int{1000, 1} {
			got, tot := catalogPages(e, limit, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
				return e.p.ListSubjects(e.ctx, f, p)
			})
			e.wantList(what, subjectLabels(got), want)
			e.wantTotal(what, tot, total)
		}
	}
	subjects("subjects newest first", tenant.SubjectFilter{}, []string{"sub-c", "Sub-B", "sub-a"}, 3)
	subjects("subjects first archived from 30 s", tenant.SubjectFilter{Range: tenant.TimeRange{Start: at(30)}}, []string{"sub-c", "Sub-B"}, -1)
	subjects("subjects first archived before 30 s", tenant.SubjectFilter{Range: tenant.TimeRange{End: at(30)}}, []string{"sub-a"}, -1)
	subjects("subjects by label", tenant.SubjectFilter{Order: tenant.OrderLabel}, []string{"sub-a", "Sub-B", "sub-c"}, 3)
	subjects("subjects by a label prefix in another case", tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "SUB-B"}, []string{"Sub-B"}, -1)
	subjects("subjects by a prefix of all", tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "sub-"}, []string{"sub-a", "Sub-B", "sub-c"}, -1)
	subjects("subjects by a prefix of none", tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "x"}, []string{}, -1)

	sessions := func(what string, f tenant.SessionFilter, want []string, total int64) {
		e.Helper()
		for _, limit := range []int{1000, 1} {
			got, tot := catalogPages(e, limit, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
				return e.p.ListSessions(e.ctx, f, p)
			})
			e.wantList(what, sessionPaths(got), want)
			e.wantTotal(what, tot, total)
		}
	}
	sessions("sessions newest first", tenant.SessionFilter{}, []string{"sub-c/ses-1", "Sub-B/Ses-X", "sub-a/ses-2", "sub-a/ses-1"}, 4)
	sessions("a subject's sessions newest first", tenant.SessionFilter{SubjectLabel: "sub-a"}, []string{"sub-a/ses-2", "sub-a/ses-1"}, 2)
	sessions("a receiver's sessions", tenant.SessionFilter{Receiver: "recv-a"}, []string{"Sub-B/Ses-X", "sub-a/ses-1"}, -1)
	sessions("the sessions of a removed or unknown receiver", tenant.SessionFilter{Receiver: "recv-gone"}, []string{}, -1)
	sessions("a modality's sessions", tenant.SessionFilter{Modality: "MR"}, []string{"Sub-B/Ses-X", "sub-a/ses-1"}, -1)
	sessions("another modality's sessions", tenant.SessionFilter{Modality: "CT"}, []string{"sub-a/ses-2"}, -1)
	sessions("a modality's sessions in a time range", tenant.SessionFilter{Modality: "MR", Range: tenant.TimeRange{End: at(30)}},
		[]string{"sub-a/ses-1"}, -1)
	sessions("sessions in a time range", tenant.SessionFilter{Range: tenant.TimeRange{Start: at(20), End: at(40)}},
		[]string{"Sub-B/Ses-X", "sub-a/ses-2"}, -1)
	sessions("sessions by label", tenant.SessionFilter{Order: tenant.OrderLabel},
		[]string{"sub-a/ses-1", "sub-a/ses-2", "Sub-B/Ses-X", "sub-c/ses-1"}, 4)
	sessions("a subject's sessions by label", tenant.SessionFilter{Order: tenant.OrderLabel, SubjectLabel: "Sub-B"}, []string{"Sub-B/Ses-X"}, 1)
	sessions("a subject's sessions by a label prefix", tenant.SessionFilter{Order: tenant.OrderLabel, SubjectLabel: "sub-a", LabelPrefix: "SES-2"},
		[]string{"sub-a/ses-2"}, -1)
	sessions("sessions by study date", tenant.SessionFilter{Order: tenant.OrderStudyDate}, []string{"sub-a/ses-2", "sub-c/ses-1", "sub-a/ses-1"}, -1)
	sessions("sessions by study date in a range", tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20200105", StudyDateEnd: "20201231"},
		[]string{"sub-c/ses-1", "sub-a/ses-1"}, -1)
	sessions("sessions by study date on one day", tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20210301", StudyDateEnd: "20210301"},
		[]string{"sub-a/ses-2"}, -1)
	sessions("a subject's sessions by study date", tenant.SessionFilter{Order: tenant.OrderStudyDate, SubjectLabel: "sub-a"},
		[]string{"sub-a/ses-2", "sub-a/ses-1"}, -1)
	page, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{Modality: "MR"}, master.Page{Limit: 10})
	e.ok("ListSessions", err)
	if len(page.Items) != 2 || !slices.Equal(page.Items[0].Modalities, []string{"MR", "SR"}) || page.Items[0].Scans != 2 {
		e.Errorf("a session listed by modality: %+v, want its whole row and every modality", page.Items)
	}
	_, err = e.p.ListSessions(e.ctx, tenant.SessionFilter{SubjectLabel: "sub-B"}, master.Page{Limit: 10})
	e.wantKind("the sessions of a subject not held (another case)", err, fault.NotFound)

	scans := func(what string, f tenant.ScanFilter, want []string, total int64) {
		e.Helper()
		for _, limit := range []int{1000, 1} {
			got, tot := catalogPages(e, limit, func(p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
				return e.p.ListScans(e.ctx, f, p)
			})
			e.wantList(what, scanPaths(got), want)
			e.wantTotal(what, tot, total)
		}
	}
	scans("scans newest first", tenant.ScanFilter{}, []string{"sub-c/ses-1/scan-pt", "Sub-B/Ses-X/scan-dwi", "Sub-B/Ses-X/scan-sr",
		"sub-a/ses-2/scan-ct", "sub-a/ses-1/scan-loc", "sub-a/ses-1/scan-t1"}, 6)
	scans("a session's scans newest first", tenant.ScanFilter{SubjectLabel: "sub-a", SessionLabel: "ses-1"},
		[]string{"sub-a/ses-1/scan-loc", "sub-a/ses-1/scan-t1"}, 2)
	scans("a modality's scans", tenant.ScanFilter{Modality: "MR"}, []string{"Sub-B/Ses-X/scan-dwi", "sub-a/ses-1/scan-loc", "sub-a/ses-1/scan-t1"}, -1)
	scans("a receiver's scans", tenant.ScanFilter{Receiver: "recv-b"}, []string{"sub-c/ses-1/scan-pt", "sub-a/ses-2/scan-ct"}, -1)
	scans("scans in a time range", tenant.ScanFilter{Range: tenant.TimeRange{Start: at(11), End: at(31)}},
		[]string{"Sub-B/Ses-X/scan-sr", "sub-a/ses-2/scan-ct", "sub-a/ses-1/scan-loc"}, -1)
	scans("scans by label", tenant.ScanFilter{Order: tenant.OrderLabel}, []string{"sub-a/ses-1/scan-loc", "sub-a/ses-1/scan-t1",
		"sub-a/ses-2/scan-ct", "Sub-B/Ses-X/scan-dwi", "Sub-B/Ses-X/scan-sr", "sub-c/ses-1/scan-pt"}, 6)
	scans("a subject's scans by label", tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: "sub-a"},
		[]string{"sub-a/ses-1/scan-loc", "sub-a/ses-1/scan-t1", "sub-a/ses-2/scan-ct"}, -1)
	scans("a session's scans by label", tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: "Sub-B", SessionLabel: "Ses-X"},
		[]string{"Sub-B/Ses-X/scan-dwi", "Sub-B/Ses-X/scan-sr"}, 2)
	scans("a session's scans by a label prefix", tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: "sub-a", SessionLabel: "ses-1",
		LabelPrefix: "SCAN-T"}, []string{"sub-a/ses-1/scan-t1"}, -1)
	scans("scans by series description", tenant.ScanFilter{Order: tenant.OrderSeriesDescription}, []string{"sub-a/ses-2/scan-ct",
		"Sub-B/Ses-X/scan-dwi", "sub-a/ses-1/scan-loc", "sub-c/ses-1/scan-pt", "sub-a/ses-1/scan-t1"}, -1)
	scans("scans by a description prefix in another case", tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "LOC"},
		[]string{"sub-a/ses-1/scan-loc"}, -1)
	scans("scans by a description prefix outside ASCII", tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "pet Ä"},
		[]string{"sub-c/ses-1/scan-pt"}, -1)
	scans("scans by a description prefix outside ASCII in another case", tenant.ScanFilter{Order: tenant.OrderSeriesDescription,
		DescriptionPrefix: "pet ä"}, []string{}, -1)
	// A prefix is folded, not trimmed: its spaces are part of it.
	scans("scans by a description prefix that ends in a space", tenant.ScanFilter{Order: tenant.OrderSeriesDescription,
		DescriptionPrefix: "T1 "}, []string{"sub-a/ses-1/scan-t1"}, -1)
	scans("scans by a description prefix whose space no description has", tenant.ScanFilter{Order: tenant.OrderSeriesDescription,
		DescriptionPrefix: "Abdomen "}, []string{}, -1)
	scans("scans by a description prefix of spaces alone", tenant.ScanFilter{Order: tenant.OrderSeriesDescription,
		DescriptionPrefix: "   "}, []string{}, -1)
	_, err = e.p.ListScans(e.ctx, tenant.ScanFilter{SubjectLabel: "sub-a", SessionLabel: "ses-9"}, master.Page{Limit: 10})
	e.wantKind("the scans of a session not held", err, fault.NotFound)
	_, err = e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: "sub-z"}, master.Page{Limit: 10})
	e.wantKind("the scans of a subject not held", err, fault.NotFound)
}

// testCatalogRefusals guards that a list refuses, as Invalid, every
// combination of filter and order no index serves, and a malformed value,
// rather than reading a table to answer it.
func testCatalogRefusals(e *penv) {
	e.catalogTenant()
	page := master.Page{Limit: 10}
	for what, f := range map[string]tenant.SubjectFilter{
		"a label prefix with the newest order": {LabelPrefix: "sub"},
		"a time range with the label order":    {Order: tenant.OrderLabel, Range: tenant.TimeRange{Start: at(1)}},
		"the study date order":                 {Order: tenant.OrderStudyDate},
		"a prefix that is not a label's start": {Order: tenant.OrderLabel, LabelPrefix: "sub a"},
		"a time range ending at its start":     {Range: tenant.TimeRange{Start: at(5), End: at(5)}},
	} {
		_, err := e.p.ListSubjects(e.ctx, f, page)
		e.wantKind("ListSubjects with "+what, err, fault.Invalid)
	}
	for what, f := range map[string]tenant.SessionFilter{
		"a subject and a modality":                 {SubjectLabel: "sub-a", Modality: "MR"},
		"a modality and a receiver":                {Modality: "MR", Receiver: "recv-a"},
		"a label prefix with no subject":           {Order: tenant.OrderLabel, LabelPrefix: "ses"},
		"a label prefix with the newest order":     {SubjectLabel: "sub-a", LabelPrefix: "ses"},
		"a study date range with the newest order": {StudyDateStart: "20200101"},
		"a time range with the study date order":   {Order: tenant.OrderStudyDate, Range: tenant.TimeRange{Start: at(1)}},
		"a modality with the label order":          {Order: tenant.OrderLabel, Modality: "MR"},
		"a study date range ending before it":      {Order: tenant.OrderStudyDate, StudyDateStart: "20200102", StudyDateEnd: "20200101"},
		"a study date that does not exist":         {Order: tenant.OrderStudyDate, StudyDateStart: "20200230"},
		"a modality in lower case":                 {Modality: "mr"},
		"the series description order":             {Order: tenant.OrderSeriesDescription},
	} {
		_, err := e.p.ListSessions(e.ctx, f, page)
		e.wantKind("ListSessions with "+what, err, fault.Invalid)
	}
	for what, f := range map[string]tenant.ScanFilter{
		"a session with no subject":                    {SessionLabel: "ses-1"},
		"a subject alone with the newest order":        {SubjectLabel: "sub-a"},
		"a session and a modality":                     {SubjectLabel: "sub-a", SessionLabel: "ses-1", Modality: "MR"},
		"a label prefix with a subject alone":          {Order: tenant.OrderLabel, SubjectLabel: "sub-a", LabelPrefix: "scan"},
		"a description prefix with the newest order":   {DescriptionPrefix: "t1"},
		"a modality with the series description order": {Order: tenant.OrderSeriesDescription, Modality: "MR"},
		"a session with the series description order":  {Order: tenant.OrderSeriesDescription, SubjectLabel: "sub-a", SessionLabel: "ses-1"},
		"the study date order":                         {Order: tenant.OrderStudyDate},
		"a description prefix too long":                {Order: tenant.OrderSeriesDescription, DescriptionPrefix: string(make([]byte, 65))},
	} {
		_, err := e.p.ListScans(e.ctx, f, page)
		e.wantKind("ListScans with "+what, err, fault.Invalid)
	}
	_, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{}, master.Page{Limit: 0})
	e.wantKind("ListSubjects with a limit of 0", err, fault.Invalid)
}

// testCatalogCursors guards the lists' cursors: opaque and bound to their
// list, order and filter and the tenant (one used elsewhere is Invalid),
// and stable under archives made between pages, a row archived after the
// first page appearing on a later one only when it sorts after the rows
// already answered.
func testCatalogCursors(e *penv) {
	e.catalogTenant()
	first, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{}, master.Page{Limit: 1})
	e.ok("ListSubjects", err)
	byLabel, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, master.Page{Limit: 2})
	e.ok("ListSubjects", err)
	e.wantList("the first page by label", subjectLabels(byLabel.Items), []string{"sub-a", "Sub-B"})
	sessions, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{SubjectLabel: "sub-a"}, master.Page{Limit: 1})
	e.ok("ListSessions", err)
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderSeriesDescription}, master.Page{Limit: 1})
	e.ok("ListScans", err)

	// Archived between pages: a subject newer than all, one that sorts
	// before the label cursor and one after it.
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-d", session: "ses-1", study: 5, at: 50,
		scans: []scanSpec{{label: "scan-1", modality: "MR", description: "T2", instances: 1}}})
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-aa", session: "ses-1", study: 6, at: 60,
		scans: []scanSpec{{label: "scan-1", modality: "MR", instances: 1}}})
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-z", session: "ses-1", study: 7, at: 70,
		scans: []scanSpec{{label: "scan-1", modality: "MR", instances: 1}}})

	rest, _ := catalogPages(e, 1000, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
		if p.Cursor == "" {
			p.Cursor = first.Next
		}
		return e.p.ListSubjects(e.ctx, tenant.SubjectFilter{}, p)
	})
	e.wantList("subjects newest first after its first page, with newer ones archived", subjectLabels(rest), []string{"Sub-B", "sub-a"})
	rest, _ = catalogPages(e, 1000, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
		if p.Cursor == "" {
			p.Cursor = byLabel.Next
		}
		return e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, p)
	})
	e.wantList("subjects by label after its first page, with others archived", subjectLabels(rest), []string{"sub-c", "sub-d", "sub-z"})
	next, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderSeriesDescription}, master.Page{Limit: 10, Cursor: scans.Next})
	e.ok("ListScans", err)
	e.wantList("the first page by description", scanPaths(scans.Items), []string{"sub-a/ses-2/scan-ct"})
	e.wantList("scans by description after the first page, with one archived after it", scanPaths(next.Items),
		[]string{"Sub-B/Ses-X/scan-dwi", "sub-a/ses-1/scan-loc", "sub-c/ses-1/scan-pt", "sub-a/ses-1/scan-t1", "sub-d/ses-1/scan-1"})

	for what, call := range map[string]func() error{
		"a newest cursor with the label order": func() error {
			_, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1, Cursor: first.Next})
			return err
		},
		"a cursor with another filter": func() error {
			_, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "sub"},
				master.Page{Limit: 1, Cursor: byLabel.Next})
			return err
		},
		"a subject list's cursor in the session list": func() error {
			_, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{}, master.Page{Limit: 1, Cursor: first.Next})
			return err
		},
		"a cursor with another subject": func() error {
			_, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{SubjectLabel: "sub-c"}, master.Page{Limit: 1, Cursor: sessions.Next})
			return err
		},
		"a cursor cut short": func() error {
			_, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{}, master.Page{Limit: 1, Cursor: first.Next[:len(first.Next)-2]})
			return err
		},
		"a cursor of the instance list in the scan list": func() error {
			_, c, err := e.p.ListHeldInstances(e.ctx, label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-t1"}, master.Page{Limit: 1})
			e.ok("ListHeldInstances", err)
			_, err = e.p.ListScans(e.ctx, tenant.ScanFilter{}, master.Page{Limit: 1, Cursor: c})
			return err
		},
	} {
		e.wantKind(what, call(), fault.Invalid)
	}
	other := e.open(e.T, "other-tenant")
	_, err = other.ListSubjects(e.ctx, tenant.SubjectFilter{}, master.Page{Limit: 1, Cursor: first.Next})
	e.wantKind("a cursor of another tenant", err, fault.Invalid)
}

// testCatalogInstances guards the reads of a scan's instances by its
// labels: every instance held, whichever reception confirmed it, in order
// of SOP Instance UID and paged, never a claim not confirmed; one instance
// by the scan's labels and its UID, NotFound when the scan does not hold
// it (held in another scan, only claimed, or not there) and when the scan
// is not there.
func testCatalogInstances(e *penv) {
	e.catalogTenant()
	t1 := label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-t1"}
	// A later arrival of the study adds one instance to scan-t1 and claims
	// another without confirming it.
	r, _, err := e.p.OpenReception(e.ctx, "recv-a", uidOf(1, 1), e.id("rc"), at(90))
	e.ok("OpenReception", err)
	more := func(k int) tenant.ReceivedFile {
		uid := instanceOf(1, 0, k)
		return tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: uid, SeriesUID: seriesOf(1, 0), StudyUID: r.StudyUID,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 900 + int64(k), SHA256: sum(9000 + k),
			Incoming: r.ID + "/" + uid + ".dcm", Association: "assoc2"}
	}
	held, claimed := more(7), more(8)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{held, claimed}, at(91))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, "identified", "store")
	e.claim(r, claimOf(claimed))
	e.commitValues(r, t1, 95, tenant.ScanValuesOf("", "MR", "", ""), []tenant.ReceivedFile{held})

	got := allPages(e, 2, func(p master.Page) ([]tenant.ScanInstance, string, error) { return e.p.ListHeldInstances(e.ctx, t1, p) })
	var uids []string
	for _, in := range got {
		uids = append(uids, in.SOPInstanceUID)
	}
	want := []string{instanceOf(1, 0, 0), instanceOf(1, 0, 1), instanceOf(1, 0, 2), held.SOPInstanceUID}
	slices.Sort(want)
	e.wantList("a scan's held instances", uids, want)
	if len(got) == 4 {
		for _, in := range got {
			if in.SOPInstanceUID == held.SOPInstanceUID && (in.ReceptionID != r.ID || in.Size != held.Size || in.Storage != "store" ||
				in.ArchivedSHA256 != held.SHA256 || !in.Held.Equal(at(95))) {
				e.Errorf("the instance a later arrival confirmed: %+v", in)
			}
		}
	}
	in, err := e.p.HeldInstance(e.ctx, t1, held.SOPInstanceUID)
	e.ok("HeldInstance", err)
	if in.SOPInstanceUID != held.SOPInstanceUID || in.Length != held.Size || in.Key == "" {
		e.Errorf("HeldInstance = %+v", in)
	}
	for what, c := range map[string]struct {
		path label.Path
		uid  string
	}{
		"an instance only claimed":      {t1, claimed.SOPInstanceUID},
		"an instance of another scan":   {t1, instanceOf(1, 1, 0)},
		"an instance not there":         {t1, uidOf(3, 99999)},
		"a scan in another case":        {label.Path{Subject: "sub-a", Session: "ses-1", Scan: "Scan-t1"}, instanceOf(1, 0, 0)},
		"the right UID in another scan": {label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-loc"}, instanceOf(1, 0, 0)},
	} {
		_, err := e.p.HeldInstance(e.ctx, c.path, c.uid)
		e.wantKind("HeldInstance of "+what, err, fault.NotFound)
	}
	_, err = e.p.HeldInstance(e.ctx, t1, "not-a-uid")
	e.wantKind("HeldInstance of a UID that is not one", err, fault.Invalid)
	_, _, err = e.p.ListHeldInstances(e.ctx, label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-none"}, master.Page{Limit: 10})
	e.wantKind("ListHeldInstances of a scan not there", err, fault.NotFound)
	_, next, err := e.p.ListHeldInstances(e.ctx, t1, master.Page{Limit: 1})
	e.ok("ListHeldInstances", err)
	_, _, err = e.p.ListHeldInstances(e.ctx, label.Path{Subject: "sub-a", Session: "ses-1", Scan: "scan-loc"}, master.Page{Limit: 1, Cursor: next})
	e.wantKind("an instance cursor with another scan", err, fault.Invalid)
}

// testCatalogValuesKept guards that a scan and a session keep the first
// header values they were given: a later commit with other values changes
// none of them (a listing's order does not move under a reader), and fills
// each one still empty, which the lists then find it by.
func testCatalogValuesKept(e *penv) {
	e.catalogTenant()
	r, _, err := e.p.OpenReception(e.ctx, "recv-b", uidOf(1, 3), e.id("rc"), at(100))
	e.ok("OpenReception", err)
	f := func(i, k int) tenant.ReceivedFile {
		uid := instanceOf(3, i, k)
		return tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: uid, SeriesUID: seriesOf(3, i), StudyUID: r.StudyUID,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 700, SHA256: sum(8000 + i*10 + k),
			Incoming: r.ID + "/" + uid + ".dcm", Association: "assoc3"}
	}
	sr, dwi := f(0, 50), f(1, 50)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{sr, dwi}, at(101))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, "identified", "store")
	e.commitValues(r, label.Path{Subject: "Sub-B", Session: "Ses-X", Scan: "scan-sr"}, 110,
		tenant.ScanValuesOf("20220202", "OT", "Report", "9"), []tenant.ReceivedFile{sr})
	e.commitValues(r, label.Path{Subject: "Sub-B", Session: "Ses-X", Scan: "scan-dwi"}, 111,
		tenant.ScanValuesOf("20230303", "CT", "other", "1"), []tenant.ReceivedFile{dwi})

	scan, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "Sub-B", Session: "Ses-X", Scan: "scan-sr"})
	e.ok("CatalogScan", err)
	if scan.Values != (tenant.ScanValues{Modality: "SR", SeriesDescription: "Report", SeriesNumber: 9, HasSeriesNumber: true}) {
		e.Errorf("a scan's empty values after a later commit: %+v, want the modality kept and the rest filled", scan.Values)
	}
	scan, err = e.p.CatalogScan(e.ctx, label.Path{Subject: "Sub-B", Session: "Ses-X", Scan: "scan-dwi"})
	e.ok("CatalogScan", err)
	if scan.Values != (tenant.ScanValues{Modality: "MR", SeriesDescription: "dwi b1000", SeriesNumber: 7, HasSeriesNumber: true}) ||
		!scan.Updated.Equal(at(111)) || scan.Instances != 5 || scan.Receiver != "recv-a" {
		e.Errorf("a scan's held values after a later commit with others: %+v", scan)
	}
	ses, err := e.p.CatalogSession(e.ctx, "Sub-B", "Ses-X")
	e.ok("CatalogSession", err)
	if ses.StudyDate != "20220202" || !slices.Equal(ses.Modalities, []string{"MR", "SR"}) || ses.Receiver != "recv-a" ||
		!ses.Updated.Equal(at(111)) || !ses.Created.Equal(at(30)) {
		e.Errorf("a session after a later arrival: %+v, want its empty study date filled and the rest kept", ses)
	}
	got, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
		return e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "rep"}, p)
	})
	e.wantList("a description filled later", scanPaths(got), []string{"Sub-B/Ses-X/scan-sr"})
	dated, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
		return e.p.ListSessions(e.ctx, tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20220101"}, p)
	})
	e.wantList("a study date filled later", sessionPaths(dated), []string{"Sub-B/Ses-X"})
	_, err = e.p.CommitScan(e.ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: seriesOf(3, 0),
		SubjectLabel: "Sub-B", SessionLabel: "Ses-X", ScanLabel: "scan-sr", Storage: "store", At: at(120),
		Values: tenant.ScanValues{Modality: "mr"}, Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: sr.SOPInstanceUID,
			Key: "data/Sub-B/Ses-X/scan-sr/" + sr.SOPInstanceUID + ".dcm", Size: 1, ArchivedSHA256: sum(1)}}})
	e.wantKind("a commit with a value not of its kind", err, fault.Invalid)
}
