package api_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// kind is what a list test knows of one kind of row: how to name it, how
// its order compares two rows, and every row of the catalog, to hold each
// row a list answers equal to the same row read another way.
type kind[T proto.Message] struct {
	key  func(T) string
	cmp  func(a, b T) int
	full []T
}

// check fails unless a list that was read to its end by pages of one row
// (paged) and by one page (whole) answered exactly the rows want names, in
// the order cmp gives, each equal to the catalog's row of that key.
func (k kind[T]) check(t *testing.T, name string, paged, whole, want []T) {
	t.Helper()
	if got, w := keys(paged, k.key), keys(want, k.key); !slices.Equal(got, w) {
		t.Errorf("%s: answered %v, want %v", name, got, w)
	}
	order := func(rows []T) []string {
		var out []string
		for _, r := range rows {
			out = append(out, k.key(r))
		}
		return out
	}
	if !slices.Equal(order(paged), order(whole)) {
		t.Errorf("%s: pages of one row answer %v, one page answers %v", name, order(paged), order(whole))
	}
	if !slices.IsSortedFunc(paged, k.cmp) {
		t.Errorf("%s: %v is not in the order asked for", name, order(paged))
	}
	byKey := map[string]T{}
	for _, r := range k.full {
		byKey[k.key(r)] = r
	}
	for _, r := range paged {
		if full, ok := byKey[k.key(r)]; !ok || !proto.Equal(full, r) {
			t.Errorf("%s: %s differs from the row of the whole catalog", name, k.key(r))
		}
	}
}

// at returns a time as a request's timestamp.
func at(ts *timestamppb.Timestamp) *timestamppb.Timestamp { return timestamppb.New(ts.AsTime()) }

// latest returns the latest of times.
func latest(times ...*timestamppb.Timestamp) *timestamppb.Timestamp {
	out := times[0]
	for _, t := range times {
		if t.AsTime().After(out.AsTime()) {
			out = t
		}
	}
	return out
}

// TestCatalogListsSubjects guards every list of subjects the contract
// serves, in each order with each filter: read by pages of one row to the
// end it answers exactly the rows the filter keeps, in the order asked for,
// with the total only where a running total gives it.
func TestCatalogListsSubjects(t *testing.T) {
	d := newCatalogData(t)
	k := kind[*v1.Subject]{key: subjectKey, full: d.subjects, cmp: func(a, b *v1.Subject) int { return 0 }}
	newest := func(a, b *v1.Subject) int { return b.GetArchiveTime().AsTime().Compare(a.GetArchiveTime().AsTime()) }
	byLabel := func(a, b *v1.Subject) int {
		return strings.Compare(strings.ToLower(a.GetLabel()), strings.ToLower(b.GetLabel()))
	}
	created := func(s *v1.Subject) *timestamppb.Timestamp { return s.GetArchiveTime() }
	last := latest(created(d.subjects[0]), created(d.subjects[1]), created(d.subjects[2]))
	first := d.subjects[0].GetLabel()
	for _, c := range []struct {
		name  string
		mod   func(*v1.ListSubjectsRequest)
		cmp   func(a, b *v1.Subject) int
		want  []*v1.Subject
		total int64
	}{
		{"unspecified order is newest", func(*v1.ListSubjectsRequest) {}, newest, d.subjects, 3},
		{"newest", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_NEWEST }, newest, d.subjects, 3},
		{"label", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_LABEL }, byLabel, d.subjects, 3},
		{"newest from the latest time", func(r *v1.ListSubjectsRequest) { r.StartTime = at(last) }, newest,
			filtered(d.subjects, func(s *v1.Subject) bool { return !created(s).AsTime().Before(last.AsTime()) }), -1},
		{"newest before the latest time", func(r *v1.ListSubjectsRequest) { r.EndTime = at(last) }, newest,
			filtered(d.subjects, func(s *v1.Subject) bool { return created(s).AsTime().Before(last.AsTime()) }), -1},
		{"newest in a range", func(r *v1.ListSubjectsRequest) {
			r.StartTime = timestamppb.New(slices.MinFunc(d.subjects, func(a, b *v1.Subject) int { return created(a).AsTime().Compare(created(b).AsTime()) }).GetArchiveTime().AsTime())
			r.EndTime = timestamppb.New(last.AsTime().Add(time.Millisecond))
		}, newest, d.subjects, -1},
		{"label prefix", func(r *v1.ListSubjectsRequest) {
			r.Order = v1.CatalogOrder_CATALOG_ORDER_LABEL
			r.LabelPrefix = "sub-6"
		}, byLabel,
			filtered(d.subjects, func(s *v1.Subject) bool { return lowerPrefix(s.GetLabel(), "sub-6") }), -1},
		{"label prefix in capitals", func(r *v1.ListSubjectsRequest) {
			r.Order = v1.CatalogOrder_CATALOG_ORDER_LABEL
			r.LabelPrefix = "SUB-6X"
		}, byLabel,
			filtered(d.subjects, func(s *v1.Subject) bool { return lowerPrefix(s.GetLabel(), "sub-6x") }), -1},
		{"label prefix of a whole label", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_LABEL; r.LabelPrefix = first }, byLabel,
			filtered(d.subjects, func(s *v1.Subject) bool { return s.GetLabel() == first }), -1},
		{"label prefix nobody has", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_LABEL; r.LabelPrefix = "zzz" }, byLabel, nil, -1},
	} {
		base := &v1.ListSubjectsRequest{Tenant: "lab1"}
		c.mod(base)
		rpc, set := d.cat.ListSubjects, func(r *v1.ListSubjectsRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok }
		read := func(s *v1.ListSubjectsResponse) ([]*v1.Subject, string, *int64) {
			return s.GetSubjects(), s.GetNextPageToken(), s.TotalCount
		}
		paged, total := pageAll(t, c.name, base, rpc, set, read)
		wholeReq := proto.Clone(base).(*v1.ListSubjectsRequest)
		wholeReq.PageSize = 1000
		whole := mustCall[*v1.ListSubjectsResponse](t, c.name, call(rpc, wholeReq))
		if whole.GetNextPageToken() != "" {
			t.Errorf("%s: a page of 1000 has a next page", c.name)
		}
		kk := k
		kk.cmp = c.cmp
		kk.check(t, c.name, paged, whole.GetSubjects(), c.want)
		checkTotal(t, c.name+" paged", total, c.total)
		checkTotal(t, c.name+" whole", whole.TotalCount, c.total)
	}
	// The counts of a subject equal those of what it holds.
	for _, s := range d.subjects {
		var sessions, instances, size int64
		for _, se := range d.sessions {
			if se.GetSubjectLabel() == s.GetLabel() {
				sessions++
				instances += se.GetInstanceCount()
				size += se.GetSizeBytes()
			}
		}
		if s.GetSessionCount() != sessions || s.GetInstanceCount() != instances || s.GetSizeBytes() != size {
			t.Errorf("%s: counts %d, %d, %d; its sessions hold %d, %d, %d", s.GetLabel(), s.GetSessionCount(), s.GetInstanceCount(), s.GetSizeBytes(), sessions, instances, size)
		}
	}
}

// TestCatalogListsSessions guards every list of sessions the contract
// serves: newest first with a subject, a modality, a receiver or a time
// range; by label with a subject and a prefix; by study date with a subject
// and a range of dates (both bounds inclusive).
func TestCatalogListsSessions(t *testing.T) {
	d := newCatalogData(t)
	k := kind[*v1.Session]{key: sessionKey, full: d.sessions}
	newest := func(a, b *v1.Session) int { return b.GetArchiveTime().AsTime().Compare(a.GetArchiveTime().AsTime()) }
	byLabel := func(a, b *v1.Session) int {
		return strings.Compare(strings.ToLower(sessionKey(a)), strings.ToLower(sessionKey(b)))
	}
	byDate := func(a, b *v1.Session) int { return strings.Compare(b.GetStudyDate(), a.GetStudyDate()) }
	mr, ct, seg := d.sessionWith(t, "MR"), d.sessionWith(t, "CT"), d.sessionWith(t, "SEG")
	last := latest(mr.GetArchiveTime(), ct.GetArchiveTime(), seg.GetArchiveTime())
	order := func(o v1.CatalogOrder, mod func(*v1.ListSessionsRequest)) func(*v1.ListSessionsRequest) {
		return func(r *v1.ListSessionsRequest) { r.Order = o; mod(r) }
	}
	const label, date = v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE
	is := func(rows ...*v1.Session) []*v1.Session { return rows }
	for _, c := range []struct {
		name  string
		mod   func(*v1.ListSessionsRequest)
		cmp   func(a, b *v1.Session) int
		want  []*v1.Session
		total int64
	}{
		{"newest", func(*v1.ListSessionsRequest) {}, newest, d.sessions, 3},
		{"newest of a subject", func(r *v1.ListSessionsRequest) { r.SubjectLabel = ct.GetSubjectLabel() }, newest, is(ct), 1},
		{"newest with a modality", func(r *v1.ListSessionsRequest) { r.Modality = "MR" }, newest, is(mr), -1},
		{"newest with another modality", func(r *v1.ListSessionsRequest) { r.Modality = "SEG" }, newest, is(seg), -1},
		{"newest with a modality none has", func(r *v1.ListSessionsRequest) { r.Modality = "XA" }, newest, nil, -1},
		{"newest with a receiver", func(r *v1.ListSessionsRequest) { r.Receiver = "scanner1" }, newest, d.sessions, -1},
		{"newest with a receiver that holds none", func(r *v1.ListSessionsRequest) { r.Receiver = "removed-one" }, newest, nil, -1},
		{"newest from the latest time", func(r *v1.ListSessionsRequest) { r.StartTime = at(last) }, newest,
			filtered(d.sessions, func(s *v1.Session) bool { return !s.GetArchiveTime().AsTime().Before(last.AsTime()) }), -1},
		{"newest before the latest time", func(r *v1.ListSessionsRequest) { r.EndTime = at(last) }, newest,
			filtered(d.sessions, func(s *v1.Session) bool { return s.GetArchiveTime().AsTime().Before(last.AsTime()) }), -1},
		{"newest of a modality in a range", func(r *v1.ListSessionsRequest) { r.Modality = "CT"; r.StartTime = at(ct.GetArchiveTime()) }, newest, is(ct), -1},
		{"label", order(label, func(*v1.ListSessionsRequest) {}), byLabel, d.sessions, 3},
		{"label of a subject", order(label, func(r *v1.ListSessionsRequest) { r.SubjectLabel = mr.GetSubjectLabel() }), byLabel, is(mr), 1},
		{"label of a subject with a prefix", order(label, func(r *v1.ListSessionsRequest) {
			r.SubjectLabel = mr.GetSubjectLabel()
			r.LabelPrefix = strings.ToUpper(mr.GetLabel()[:5])
		}), byLabel, is(mr), -1},
		{"label of a subject with a prefix none has", order(label, func(r *v1.ListSessionsRequest) { r.SubjectLabel = mr.GetSubjectLabel(); r.LabelPrefix = "zz" }), byLabel, nil, -1},
		{"study date", order(date, func(*v1.ListSessionsRequest) {}), byDate, d.sessions, -1},
		{"study date of a subject", order(date, func(r *v1.ListSessionsRequest) { r.SubjectLabel = seg.GetSubjectLabel() }), byDate, is(seg), -1},
		{"study date from a date", order(date, func(r *v1.ListSessionsRequest) { r.StudyDateStart = "20030417" }), byDate,
			filtered(d.sessions, func(s *v1.Session) bool { return s.GetStudyDate() >= "20030417" }), -1},
		{"study date up to a date, inclusive", order(date, func(r *v1.ListSessionsRequest) { r.StudyDateEnd = "20030417" }), byDate,
			filtered(d.sessions, func(s *v1.Session) bool { return s.GetStudyDate() <= "20030417" }), -1},
		{"study date on one day", order(date, func(r *v1.ListSessionsRequest) { r.StudyDateStart, r.StudyDateEnd = "20030417", "20030417" }), byDate, is(seg), -1},
		{"study date in a range of a subject", order(date, func(r *v1.ListSessionsRequest) {
			r.SubjectLabel = mr.GetSubjectLabel()
			r.StudyDateStart, r.StudyDateEnd = "20040101", "20041231"
		}), byDate, is(mr), -1},
		{"study date in a range nobody has", order(date, func(r *v1.ListSessionsRequest) { r.StudyDateStart, r.StudyDateEnd = "19000101", "19001231" }), byDate, nil, -1},
	} {
		base := &v1.ListSessionsRequest{Tenant: "lab1"}
		c.mod(base)
		rpc, set := d.cat.ListSessions, func(r *v1.ListSessionsRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok }
		read := func(s *v1.ListSessionsResponse) ([]*v1.Session, string, *int64) {
			return s.GetSessions(), s.GetNextPageToken(), s.TotalCount
		}
		paged, total := pageAll(t, c.name, base, rpc, set, read)
		wholeReq := proto.Clone(base).(*v1.ListSessionsRequest)
		wholeReq.PageSize = 1000
		whole := mustCall[*v1.ListSessionsResponse](t, c.name, call(rpc, wholeReq))
		kk := k
		kk.cmp = c.cmp
		kk.check(t, c.name, paged, whole.GetSessions(), c.want)
		checkTotal(t, c.name+" paged", total, c.total)
		checkTotal(t, c.name+" whole", whole.TotalCount, c.total)
	}
	// The counts of a session equal those of its scans.
	for _, s := range d.sessions {
		var scans, instances, size int64
		for _, sc := range d.scans {
			if sc.GetSubjectLabel() == s.GetSubjectLabel() && sc.GetSessionLabel() == s.GetLabel() {
				scans++
				instances += sc.GetInstanceCount()
				size += sc.GetSizeBytes()
			}
		}
		if s.GetScanCount() != scans || s.GetInstanceCount() != instances || s.GetSizeBytes() != size {
			t.Errorf("%s: counts %d, %d, %d; its scans hold %d, %d, %d", sessionKey(s), s.GetScanCount(), s.GetInstanceCount(), s.GetSizeBytes(), scans, instances, size)
		}
	}
}

// TestCatalogListsScans guards every list of scans the contract serves:
// newest first with a session, a modality, a receiver or a time range; by
// label with a subject, a session and a prefix; by series description with
// a prefix, scans with no description left out.
func TestCatalogListsScans(t *testing.T) {
	d := newCatalogData(t)
	k := kind[*v1.Scan]{key: scanKey, full: d.scans}
	newest := func(a, b *v1.Scan) int { return b.GetArchiveTime().AsTime().Compare(a.GetArchiveTime().AsTime()) }
	byLabel := func(a, b *v1.Scan) int {
		return strings.Compare(strings.ToLower(scanKey(a)), strings.ToLower(scanKey(b)))
	}
	byDescription := func(a, b *v1.Scan) int {
		return strings.Compare(strings.ToLower(a.GetSeriesDescription()), strings.ToLower(b.GetSeriesDescription()))
	}
	ct := d.sessionWith(t, "CT")
	inCT := func(s *v1.Scan) bool {
		return s.GetSubjectLabel() == ct.GetSubjectLabel() && s.GetSessionLabel() == ct.GetLabel()
	}
	ctScans := filtered(d.scans, inCT)
	if len(ctScans) != 2 {
		t.Fatalf("the CT session has %d scans, want 2", len(ctScans))
	}
	last := d.scans[0].GetArchiveTime()
	for _, s := range d.scans {
		last = latest(last, s.GetArchiveTime())
	}
	described := filtered(d.scans, func(s *v1.Scan) bool { return s.GetSeriesDescription() != "" })
	if len(described) != 3 {
		t.Fatalf("%d scans have a series description, want 3", len(described))
	}
	byModality := func(m string) []*v1.Scan {
		return filtered(d.scans, func(s *v1.Scan) bool { return s.GetModality() == m })
	}
	described2 := func(p string) []*v1.Scan {
		return filtered(described, func(s *v1.Scan) bool { return lowerPrefix(s.GetSeriesDescription(), p) })
	}
	const label, series = v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION
	order := func(o v1.CatalogOrder, mod func(*v1.ListScansRequest)) func(*v1.ListScansRequest) {
		return func(r *v1.ListScansRequest) { r.Order = o; mod(r) }
	}
	session := func(r *v1.ListScansRequest) { r.SubjectLabel, r.SessionLabel = ct.GetSubjectLabel(), ct.GetLabel() }
	for _, c := range []struct {
		name  string
		mod   func(*v1.ListScansRequest)
		cmp   func(a, b *v1.Scan) int
		want  []*v1.Scan
		total int64
	}{
		{"newest", func(*v1.ListScansRequest) {}, newest, d.scans, 4},
		{"newest of a session", session, newest, ctScans, 2},
		{"newest with a modality", func(r *v1.ListScansRequest) { r.Modality = "CT" }, newest, byModality("CT"), -1},
		{"newest with another modality", func(r *v1.ListScansRequest) { r.Modality = "MR" }, newest, byModality("MR"), -1},
		{"newest with a modality none has", func(r *v1.ListScansRequest) { r.Modality = "XA" }, newest, nil, -1},
		{"newest with a receiver", func(r *v1.ListScansRequest) { r.Receiver = "scanner1" }, newest, d.scans, -1},
		{"newest with a receiver that holds none", func(r *v1.ListScansRequest) { r.Receiver = "removed-one" }, newest, nil, -1},
		{"newest from the latest time", func(r *v1.ListScansRequest) { r.StartTime = at(last) }, newest,
			filtered(d.scans, func(s *v1.Scan) bool { return !s.GetArchiveTime().AsTime().Before(last.AsTime()) }), -1},
		{"newest before the latest time", func(r *v1.ListScansRequest) { r.EndTime = at(last) }, newest,
			filtered(d.scans, func(s *v1.Scan) bool { return s.GetArchiveTime().AsTime().Before(last.AsTime()) }), -1},
		{"newest of a session in a range", func(r *v1.ListScansRequest) { session(r); r.StartTime = at(last) }, newest,
			filtered(ctScans, func(s *v1.Scan) bool { return !s.GetArchiveTime().AsTime().Before(last.AsTime()) }), -1},
		{"label", order(label, func(*v1.ListScansRequest) {}), byLabel, d.scans, 4},
		{"label of a session", order(label, session), byLabel, ctScans, 2},
		{"label of a subject", order(label, func(r *v1.ListScansRequest) { r.SubjectLabel = ct.GetSubjectLabel() }), byLabel, ctScans, -1},
		{"label of a session with a prefix", order(label, func(r *v1.ListScansRequest) {
			session(r)
			r.LabelPrefix = "SCAN-" + strings.ToUpper(ctScans[0].GetLabel()[5:6])
		}), byLabel,
			filtered(ctScans, func(s *v1.Scan) bool { return lowerPrefix(s.GetLabel(), "scan-"+ctScans[0].GetLabel()[5:6]) }), -1},
		{"label of a session with a prefix none has", order(label, func(r *v1.ListScansRequest) { session(r); r.LabelPrefix = "zz" }), byLabel, nil, -1},
		{"series description", order(series, func(*v1.ListScansRequest) {}), byDescription, described, -1},
		{"series description prefix", order(series, func(r *v1.ListScansRequest) { r.SeriesDescriptionPrefix = "S" }), byDescription, described2("s"), -1},
		{"series description prefix with a space", order(series, func(r *v1.ListScansRequest) { r.SeriesDescriptionPrefix = "SmartScore - G" }), byDescription, described2("smartscore - g"), -1},
		{"series description prefix of one", order(series, func(r *v1.ListScansRequest) { r.SeriesDescriptionPrefix = "liver" }), byDescription, described2("liver"), -1},
		{"series description prefix nobody has", order(series, func(r *v1.ListScansRequest) { r.SeriesDescriptionPrefix = "zzz" }), byDescription, nil, -1},
	} {
		base := &v1.ListScansRequest{Tenant: "lab1"}
		c.mod(base)
		rpc, set := d.cat.ListScans, func(r *v1.ListScansRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok }
		read := func(s *v1.ListScansResponse) ([]*v1.Scan, string, *int64) {
			return s.GetScans(), s.GetNextPageToken(), s.TotalCount
		}
		paged, total := pageAll(t, c.name, base, rpc, set, read)
		wholeReq := proto.Clone(base).(*v1.ListScansRequest)
		wholeReq.PageSize = 1000
		whole := mustCall[*v1.ListScansResponse](t, c.name, call(rpc, wholeReq))
		kk := k
		kk.cmp = c.cmp
		kk.check(t, c.name, paged, whole.GetScans(), c.want)
		checkTotal(t, c.name+" paged", total, c.total)
		checkTotal(t, c.name+" whole", whole.TotalCount, c.total)
	}
}

// TestCatalogPagesOfTwo guards the cursor with a page size that leaves a
// short last page: two pages of two hold every scan once, in label order,
// and the last carries no token.
func TestCatalogPagesOfTwo(t *testing.T) {
	d := newCatalogData(t)
	first := mustCall[*v1.ListScansResponse](t, "first", call(d.cat.ListScans,
		&v1.ListScansRequest{Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_LABEL, PageSize: 2}))
	if len(first.GetScans()) != 2 || first.GetNextPageToken() == "" {
		t.Fatalf("the first page holds %d scans and token %q", len(first.GetScans()), first.GetNextPageToken())
	}
	rest := mustCall[*v1.ListScansResponse](t, "rest", call(d.cat.ListScans,
		&v1.ListScansRequest{Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_LABEL, PageSize: 2, PageToken: first.GetNextPageToken()}))
	got := append(slices.Clone(first.GetScans()), rest.GetScans()...)
	if !slices.Equal(keys(got, scanKey), keys(d.scans, scanKey)) || rest.GetNextPageToken() != "" {
		t.Errorf("two pages of two answer %v and token %q, want every scan once", keys(got, scanKey), rest.GetNextPageToken())
	}
	for i := range got {
		if scanKey(got[i]) != scanKey(d.scans[i]) {
			t.Errorf("row %d is %s, the label order has %s", i, scanKey(got[i]), scanKey(d.scans[i]))
		}
	}
}
