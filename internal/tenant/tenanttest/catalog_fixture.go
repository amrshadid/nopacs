package tenanttest

import (
	"fmt"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// studySpec is one study the catalog tests archive: its receiver, labels,
// time (seconds after T0, the commit time of its first scan; each next
// scan is a second later), study date and scans.
type studySpec struct {
	receiver, subject, session string
	study, at                  int
	date                       string
	scans                      []scanSpec
	// profile is the profile version its instances record; the zero ref
	// archives them as received.
	profile tenant.ProfileRef
}

// scanSpec is one scan of a studySpec: its label, header values and
// instance count.
type scanSpec struct {
	label, modality, description string
	number                       int32
	hasNumber                    bool
	instances                    int
	// row is the search row the scan is committed with; the zero row for
	// none.
	row search.Row
}

// seriesOf returns the Series Instance UID of scan i of study.
func seriesOf(study, i int) string { return uidOf(2, study*10+i) }

// instanceOf returns the SOP Instance UID of instance k of scan i of study.
func instanceOf(study, i, k int) string { return uidOf(3, study*1000+i*100+k) }

// archiveStudy archives a study as the archive step would: a reception at
// the spec's receiver records the files of every scan, is identified with
// the spec's labels on storage "store", claims each scan's instances and
// commits each scan with its values. It returns the reception, left
// identified.
func (e *penv) archiveStudy(s studySpec) tenant.Reception {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, s.receiver, uidOf(1, s.study), e.id("rc"), at(s.at-5))
	e.ok("OpenReception", err)
	var files []tenant.ReceivedFile
	for i, sc := range s.scans {
		for k := range sc.instances {
			uid := instanceOf(s.study, i, k)
			files = append(files, tenant.ReceivedFile{
				ReceptionID: r.ID, SOPInstanceUID: uid, SeriesUID: seriesOf(s.study, i), StudyUID: r.StudyUID,
				SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
				Size: int64(500 + s.study*10 + i*3 + k), SHA256: sum(s.study*1000 + i*100 + k), Incoming: fmt.Sprintf("%s/%s.dcm", r.ID, uid),
				Caller: tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}, Association: "assoc1",
			})
		}
	}
	_, err = e.p.AddReceivedFiles(e.ctx, files, at(s.at-4))
	e.ok("AddReceivedFiles", err)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quiet, At: at(s.at - 3)}, nil)
	e.ok("StepReception quiet", err)
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Identify, At: at(s.at - 2),
		SubjectLabel: s.subject, SessionLabel: s.session, Storage: "store"}, nil)
	e.ok("StepReception identify", err)
	for i, sc := range s.scans {
		var part []tenant.ReceivedFile
		for _, f := range files {
			if f.SeriesUID == seriesOf(s.study, i) {
				part = append(part, f)
			}
		}
		e.row = sc.row
		e.commitValuesWith(s.profile, r, label.Path{Subject: s.subject, Session: s.session, Scan: sc.label}, s.at+i,
			tenant.ScanValuesOf(s.date, sc.modality, sc.description, numberText(sc)), part)
		e.row = search.Row{}
	}
	return r
}

// numberText returns a scan spec's series number as a header holds it.
func numberText(sc scanSpec) string {
	if !sc.hasNumber {
		return ""
	}
	return fmt.Sprint(sc.number)
}

// commitValues claims files of one series for r and commits those claimed
// into the scan at path, with the values, at T0 plus seconds.
func (e *penv) commitValues(r tenant.Reception, path label.Path, seconds int, v tenant.ScanValues, files []tenant.ReceivedFile) {
	e.commitValuesWith(tenant.ProfileRef{}, r, path, seconds, v, files)
}

// commitValuesWith is commitValues with the instances recording the profile
// version given, none for the zero ref (archived as received).
func (e *penv) commitValuesWith(profile tenant.ProfileRef, r tenant.Reception, path label.Path, seconds int, v tenant.ScanValues, files []tenant.ReceivedFile) {
	e.Helper()
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = claimOf(f)
	}
	answers, err := e.p.ClaimInstances(e.ctx, r.ID, claims, at(seconds), nil)
	e.ok("ClaimInstances", err)
	c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: files[0].SeriesUID, SubjectLabel: path.Subject,
		SessionLabel: path.Session, ScanLabel: path.Scan, Storage: "store", ManifestSHA256: sum(seconds + 7000), Values: v, Search: e.row, At: at(seconds)}
	for i, f := range files {
		if k := answers[i].Kind; k != tenant.ClaimedNow && k != tenant.ClaimedBefore {
			continue
		}
		key, err := layout.InstanceKey(path, f.SOPInstanceUID)
		e.ok("InstanceKey", err)
		c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size,
			Size: f.Size, ArchivedSHA256: f.SHA256, Profile: profile})
	}
	if len(c.Instances) == 0 {
		return
	}
	_, err = e.p.CommitScan(e.ctx, c)
	e.ok("CommitScan", err)
}

// catalogStudies are the catalog tests' tenant: three subjects (one whose
// label has capitals), four sessions through two receivers, six scans of
// four modalities, one scan with no description or number and one session
// with no study date, and a description outside ASCII.
var catalogStudies = []studySpec{
	{receiver: "recv-a", subject: "sub-a", session: "ses-1", study: 1, at: 10, date: "20200105", scans: []scanSpec{
		{label: "scan-t1", modality: "MR", description: "T1 MPRAGE", number: 3, hasNumber: true, instances: 3},
		{label: "scan-loc", modality: "MR", description: "Localizer", number: 1, hasNumber: true, instances: 2}}},
	{receiver: "recv-b", subject: "sub-a", session: "ses-2", study: 2, at: 20, date: "20210301", scans: []scanSpec{
		{label: "scan-ct", modality: "CT", description: "Abdomen", number: 2, hasNumber: true, instances: 2}}},
	{receiver: "recv-a", subject: "Sub-B", session: "Ses-X", study: 3, at: 30, scans: []scanSpec{
		{label: "scan-sr", modality: "SR", instances: 1},
		{label: "scan-dwi", modality: "MR", description: "dwi b1000", number: 7, hasNumber: true, instances: 4}}},
	{receiver: "recv-b", subject: "sub-c", session: "ses-1", study: 4, at: 40, date: "20200105", scans: []scanSpec{
		{label: "scan-pt", modality: "PT", description: "PET Äbrain", number: -5, hasNumber: true, instances: 2}}},
}

// catalogTenant adds the receivers and storage and archives catalogStudies.
func (e *penv) catalogTenant() {
	e.Helper()
	e.addReceiver("recv-a", "store", 11112)
	e.addReceiver("recv-b", "store", 11113)
	for _, s := range catalogStudies {
		e.archiveStudy(s)
	}
}

// subjectLabels returns the labels of a page of subjects.
func subjectLabels(items []tenant.CatalogSubject) []string {
	out := []string{}
	for _, s := range items {
		out = append(out, s.Label)
	}
	return out
}

// sessionPaths returns "subject/session" of each session.
func sessionPaths(items []tenant.CatalogSession) []string {
	out := []string{}
	for _, s := range items {
		out = append(out, s.SubjectLabel+"/"+s.Label)
	}
	return out
}

// scanPaths returns "subject/session/scan" of each scan.
func scanPaths(items []tenant.CatalogScan) []string {
	out := []string{}
	for _, s := range items {
		out = append(out, s.SubjectLabel+"/"+s.SessionLabel+"/"+s.Label)
	}
	return out
}

// catalogPages reads a catalog list a page of limit at a time until its
// last page, failing if a page is longer than limit or a page's total
// differs from the first's, and returns every row and the first page's
// total.
func catalogPages[T any](e *penv, limit int, list func(master.Page) (tenant.CatalogPage[T], error)) ([]T, *int64) {
	e.Helper()
	var out []T
	var total *int64
	page := master.Page{Limit: limit}
	for n := 0; ; n++ {
		p, err := list(page)
		e.ok("list", err)
		if len(p.Items) > limit {
			e.Fatalf("a page of limit %d has %d rows", limit, len(p.Items))
		}
		if n == 0 {
			total = p.Total
		} else if (total == nil) != (p.Total == nil) || (total != nil && *total != *p.Total) {
			e.Errorf("page %d's total is %v, the first page's %v: a total is the same on every page", n, p.Total, total)
		}
		out = append(out, p.Items...)
		if p.Next == "" {
			return out, total
		}
		if n > 10000 {
			e.Fatalf("a list did not end; its last page held %d rows", len(p.Items))
		}
		page.Cursor = p.Next
	}
}

// wantList fails unless got is want.
func (e *penv) wantList(what string, got, want []string) {
	e.Helper()
	if !slices.Equal(got, want) {
		e.Errorf("%s: %q, want %q", what, got, want)
	}
}

// wantTotal fails unless total is want, or nil when want is negative.
func (e *penv) wantTotal(what string, total *int64, want int64) {
	e.Helper()
	switch {
	case want < 0 && total != nil:
		e.Errorf("%s: total %d, want none: only a running total answers a count", what, *total)
	case want >= 0 && (total == nil || *total != want):
		e.Errorf("%s: total %v, want %d", what, total, want)
	}
}
