package tenanttest

import (
	"slices"
	"strings"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// dataSetOf returns a data set holding each attribute named by keyword with
// the value given, in the representation the data dictionary gives it.
func dataSetOf(e *penv, vals map[string]string) *dataset.Dataset {
	e.Helper()
	ds := dataset.NewDataset()
	for k, v := range vals {
		f, err := query.ParseField(k)
		e.ok("ParseField "+k, err)
		t := tag.Tag(f.Path[0].Tag)
		e.ok("Add "+k, ds.Add(dataelem.NewDataElement(t, dataelem.VR(t.GetVR()), []byte(v))))
	}
	return ds
}

// fixtureScan is one scan of the search tests' tenant: where it is, and the
// header its search row is read from.
type fixtureScan struct {
	subject, session, date, label string
	vals                          map[string]string
	ds                            *dataset.Dataset
}

// path is "subject/session/scan".
func (f fixtureScan) path() string { return f.subject + "/" + f.session + "/" + f.label }

// searchFixture is the search tests' tenant, in the shapes the grammar has
// to tell apart: case and accents, underscores and percent signs, brackets,
// Greek with a final sigma, Han, an attribute some scans lack, attributes
// with several values and a person's name with three component groups.
var searchFixture = []fixtureScan{
	{subject: "sub-a", session: "ses-1", date: "20240115", label: "t1", vals: map[string]string{"SeriesDescription": "T1_MPRAGE", "Modality": "MR",
		"SliceThickness": "1", "ProtocolName": "T1W sag", "Manufacturer": "SIEMENS", "ImageType": `ORIGINAL\PRIMARY\AXIAL`,
		"OperatorsName": "Doe^John=山田^太郎=やまだ^たろう"}},
	{subject: "sub-a", session: "ses-1", date: "20240115", label: "loc", vals: map[string]string{"SeriesDescription": "Localizer", "Modality": "MR",
		"SliceThickness": "5", "ProtocolName": "loc", "ImageType": `ORIGINAL\PRIMARY\LOCALIZER`}},
	{subject: "sub-a", session: "ses-2", date: "20230101", label: "ct", vals: map[string]string{"SeriesDescription": "CT Head 5mm", "Modality": "CT",
		"SliceThickness": "5", "ProtocolName": "head", "Manufacturer": "GE", "ImageType": `ORIGINAL\PRIMARY\AXIAL`, "OperatorsName": "Roe^Ann"}},
	{subject: "sub-a", session: "ses-2", date: "20230101", label: "t1r", vals: map[string]string{"SeriesDescription": "t1 mprage repeat", "Modality": "MR",
		"ProtocolName": "T1W axial", "Manufacturer": "GE", "ImageType": `DERIVED\SECONDARY`}},
	{subject: "sub-b", session: "ses-1", date: "20261101", label: "pet", vals: map[string]string{"SeriesDescription": "Ünï Täst 100%", "Modality": "PT",
		"SliceThickness": "2", "ProtocolName": "pet", "Manufacturer": "GE", "OperatorsName": "Doe^Jane"}},
	{subject: "sub-b", session: "ses-1", date: "20261101", label: "mpr", vals: map[string]string{"SeriesDescription": "T1XMPRAGE", "Modality": "MR",
		"SliceThickness": "0.9", "ProtocolName": "T1W sag", "Manufacturer": "SIEMENS"}},
	{subject: "sub-b", session: "ses-2", date: "20250601", label: "mue", vals: map[string]string{"SeriesDescription": "Müller Straße", "Modality": "MR"}},
	{subject: "sub-b", session: "ses-2", date: "20250601", label: "bra", vals: map[string]string{"SeriesDescription": "[MPRAGE] ΟΔΟΣ", "Modality": "MR",
		"ImageType": `DERIVED\PRIMARY`}},
	{subject: "sub-b", session: "ses-2", date: "20250601", label: "han", vals: map[string]string{"SeriesDescription": "山田太郎 οδος", "Modality": "CT"}},
	{subject: "sub-c", session: "ses-1", label: "plain", vals: map[string]string{"SeriesDescription": "plain scan", "Modality": "MR",
		"SliceThickness": "3", "ProtocolName": "plain", "Manufacturer": "GE"}},
}

// searchTenant archives the fixture: one study per session, one instance a
// scan, each scan committed with the search row read from its header.
// profile is the profile version its instances record; zero archives them
// as received. It returns the fixture with its data sets made.
func (e *penv) searchTenant(profile tenant.ProfileRef) []fixtureScan {
	e.Helper()
	e.addReceiver("recv-a", "store", 11112)
	fx := slices.Clone(searchFixture)
	for i := range fx {
		fx[i].ds = dataSetOf(e, fx[i].vals)
	}
	type key struct{ subject, session string }
	var order []key
	scans := map[key][]int{}
	for i, f := range fx {
		k := key{f.subject, f.session}
		if _, ok := scans[k]; !ok {
			order = append(order, k)
		}
		scans[k] = append(scans[k], i)
	}
	for n, k := range order {
		spec := studySpec{receiver: "recv-a", subject: k.subject, session: k.session, study: n + 1, at: 10 + 10*n, profile: profile,
			date: fx[scans[k][0]].date}
		for _, i := range scans[k] {
			spec.scans = append(spec.scans, scanSpec{label: fx[i].label, modality: fx[i].vals["Modality"], description: fx[i].vals["SeriesDescription"],
				instances: 1, row: search.Extract(fx[i].ds, instanceOf(n+1, len(spec.scans), 0))})
		}
		e.archiveStudy(spec)
	}
	return fx
}

// deidProfile returns the reference of a profile version the tenant has, which
// the scans archived through it record.
func (e *penv) deidProfile() tenant.ProfileRef {
	e.Helper()
	v, _ := e.create(newVersion("research", "one"), 1)
	return v.Ref()
}

// searchAll runs a search and reads every page of limit rows, returning the
// answer of the first page with the rows of all, as paths.
func (e *penv) searchAll(q tenant.SearchQuery, limit int) (tenant.SearchResult, []string) {
	e.Helper()
	q.Page = master.Page{Limit: limit}
	var first tenant.SearchResult
	var out []string
	for n := 0; ; n++ {
		r, err := e.p.Search(e.ctx, q)
		e.ok("Search", err)
		if n == 0 {
			first = r
		}
		out = append(out, scanPaths(r.Scans)...)
		out = append(out, sessionPaths(r.Sessions)...)
		out = append(out, subjectLabels(r.Subjects)...)
		if r.Next == "" {
			return first, out
		}
		if n > 1000 {
			e.Fatal("a search did not end")
		}
		q.Page.Cursor = r.Next
	}
}

// searchCondition parses a condition or fails the test.
func (e *penv) searchCondition(text string) query.Node {
	e.Helper()
	n, err := query.Parse(text)
	e.ok("Parse "+text, err)
	return n
}

// reference is the answer a search must give, read the way routing reads a
// file: each leaf evaluated over the scan's data set (query.Eval), a
// session's and a subject's own fields from the fixture, and "has" and
// "every" over the rows that belong with the row.
type reference struct {
	e  *penv
	fx []fixtureScan
}

// holds reports whether node n holds for the row of a level named by key:
// a scan's path, a session's "subject/session", a subject's label.
func (r reference) holds(n query.Node, level query.Level, key string) bool {
	r.e.Helper()
	switch n := n.(type) {
	case *query.And:
		for _, c := range n.Children {
			if !r.holds(c, level, key) {
				return false
			}
		}
		return true
	case *query.Or:
		for _, c := range n.Children {
			if r.holds(c, level, key) {
				return true
			}
		}
		return false
	case *query.Not:
		return !r.holds(n.Child, level, key)
	case *query.Has:
		some := false
		for _, k := range r.related(level, n.Level, key) {
			ok := r.holds(n.Child, n.Level, k)
			some = some || ok
			if n.Every && !ok {
				return false
			}
		}
		return some
	case *query.Leaf:
		s := query.Subject{Dataset: dataset.NewDataset(), Catalog: func(name string) (string, bool, error) {
			switch {
			case level == query.LevelSession && name == "session.date":
				for _, f := range r.fx {
					if f.subject+"/"+f.session == key {
						return f.date, f.date != "", nil
					}
				}
			case level == query.LevelSubject && name == "subject.label":
				return key, true, nil
			}
			return "", false, nil
		}}
		if level == query.LevelScan {
			for _, f := range r.fx {
				if f.path() == key {
					s.Dataset = f.ds
				}
			}
		}
		res, err := query.Eval(n, s)
		r.e.ok("Eval", err)
		return res.Truth == query.True
	}
	r.e.Fatal("a node is missing")
	return false
}

// keys returns the keys of the rows at a level.
func (r reference) keys(level query.Level) []string {
	var out []string
	for _, f := range r.fx {
		var k string
		switch level {
		case query.LevelScan:
			k = f.path()
		case query.LevelSession:
			k = f.subject + "/" + f.session
		default:
			k = f.subject
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// related returns the keys of the rows at level to that belong with the row
// key at level from.
func (r reference) related(from, to query.Level, key string) []string {
	var out []string
	for _, f := range r.fx {
		var of string
		switch from {
		case query.LevelScan:
			of = f.path()
		case query.LevelSession:
			of = f.subject + "/" + f.session
		default:
			of = f.subject
		}
		if of != key {
			continue
		}
		var k string
		switch to {
		case query.LevelScan:
			k = f.path()
		case query.LevelSession:
			k = f.subject + "/" + f.session
		default:
			k = f.subject
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// searchCases are the conditions the search tests ask: every operator,
// attributes some scans lack under "not", text outside ASCII, an underscore,
// a percent sign and a bracket in text, attributes with several values, a
// person's name, and "has" and "every" up and down the levels.
var searchCases = []struct {
	cond  string
	level query.Level
}{
	{`Modality == "MR"`, query.LevelScan},
	{`Modality = "mr"`, query.LevelScan},
	{`Modality in ("CT", "PT")`, query.LevelScan},
	{`SliceThickness >= 2`, query.LevelScan},
	{`SliceThickness between 0.9 and 1`, query.LevelScan},
	{`SliceThickness absent`, query.LevelScan},
	{`SliceThickness present and Modality == "MR"`, query.LevelScan},
	{`Modality == "MR" and not SliceThickness = 5`, query.LevelScan},
	{`Modality == "MR" and not (SliceThickness between 0.5 and 1 or Manufacturer in ("GE"))`, query.LevelScan},
	{`not Manufacturer == "GE" and Modality == "MR"`, query.LevelScan},
	{`SeriesDescription prefix "T1_"`, query.LevelScan},
	{`SeriesDescription prefix "[MPR"`, query.LevelScan},
	{`SeriesDescription nocase prefix "müller"`, query.LevelScan},
	{`SeriesDescription = "MÜLLER STRASSE"`, query.LevelScan},
	{`SeriesDescription like "T1?MPRAGE"`, query.LevelScan},
	{`SeriesDescription nocase like "*mprage*"`, query.LevelScan},
	{`SeriesDescription contains "mprage"`, query.LevelScan},
	{`SeriesDescription contains "100%"`, query.LevelScan},
	{`SeriesDescription contains "_mprage"`, query.LevelScan},
	{`SeriesDescription contains "ΟΔΟΣ"`, query.LevelScan},
	{`SeriesDescription contains "山田太"`, query.LevelScan},
	{`ProtocolName words "sag t1w"`, query.LevelScan},
	{`Modality == "MR" or SliceThickness = 5`, query.LevelScan},
	{`ImageType == "AXIAL"`, query.LevelScan},
	{`ImageType nocase in ("derived", "localizer")`, query.LevelScan},
	{`Modality == "MR" and not ImageType == "PRIMARY"`, query.LevelScan},
	{`OperatorsName == "山田^太郎"`, query.LevelScan},
	{`OperatorsName = "doe^jane"`, query.LevelScan},
	{`has session (session.date between 2024-01-01 and 2024-12-31) and Modality == "MR"`, query.LevelScan},
	{`has scan (Modality == "CT")`, query.LevelSession},
	{`every scan (Modality == "MR")`, query.LevelSession},
	{`has scan (Modality == "MR") and has scan (Modality == "CT")`, query.LevelSession},
	{`session.date >= 2024-01-01 and not has scan (Modality == "PT")`, query.LevelSession},
	{`every scan (SliceThickness <= 1 or SliceThickness absent)`, query.LevelSession},
	{`has subject (subject.label = "SUB-A")`, query.LevelSession},
	{`has session (has scan (Modality == "CT"))`, query.LevelSubject},
	{`every session (session.date >= 2024-01-01)`, query.LevelSubject},
}

// fc is a facet count.
func fc(v string, n int64) tenant.FacetCount { return tenant.FacetCount{Value: v, Count: n} }

// testSearchMatchesRouting guards the search against the reference: each
// condition returns exactly the rows that routing's reading of the same
// headers keeps, whatever the page size, and a condition that keeps no row
// or every row is not one that proves anything.
func testSearchMatchesRouting(e *penv) {
	fx := e.searchTenant(e.deidProfile())
	ref := reference{e: e, fx: fx}
	for _, k := range searchCases {
		n := e.searchCondition(k.cond)
		var want []string
		all := ref.keys(k.level)
		for _, key := range all {
			if ref.holds(n, k.level, key) {
				want = append(want, key)
			}
		}
		if len(want) == 0 || len(want) == len(all) {
			e.Errorf("%q keeps %d of %d rows: a condition that keeps none or every row proves nothing", k.cond, len(want), len(all))
		}
		slices.Sort(want)
		for _, limit := range []int{1, 3, 100} {
			_, got := e.searchAll(tenant.SearchQuery{Level: k.level, Condition: n, Now: T0}, limit)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				e.Errorf("%q with pages of %d answers %v, the reference %v", k.cond, limit, got, want)
			}
		}
	}
}

// searchValueOf is a value of each attribute the search table holds, and
// a condition that only a scan holding it keeps.
var searchValueOf = map[string][2]string{
	"Modality":              {"XA", `Modality == "XA"`},
	"SeriesDescription":     {"xx description", `SeriesDescription == "xx description"`},
	"SeriesNumber":          {"4242", `SeriesNumber = 4242`},
	"ProtocolName":          {"xx protocol", `ProtocolName == "xx protocol"`},
	"StudyDescription":      {"xx study", `StudyDescription == "xx study"`},
	"BodyPartExamined":      {"XXBODY", `BodyPartExamined == "XXBODY"`},
	"Manufacturer":          {"XXMAKER", `Manufacturer == "XXMAKER"`},
	"ManufacturerModelName": {"xx model", `ManufacturerModelName == "xx model"`},
	"StationName":           {"XXSTATION", `StationName == "XXSTATION"`},
	"SequenceName":          {"xxseq", `SequenceName == "xxseq"`},
	"PatientSex":            {"O", `PatientSex == "O"`},
	"PatientAge":            {"099Y", `PatientAge == "099Y"`},
	"MagneticFieldStrength": {"2.89", `MagneticFieldStrength = 2.89`},
	"SliceThickness":        {"7.77", `SliceThickness = 7.77`},
	"RepetitionTime":        {"2345", `RepetitionTime = 2345`},
	"EchoTime":              {"3.21", `EchoTime = 3.21`},
	"FlipAngle":             {"77", `FlipAngle = 77`},
	"AcquisitionDate":       {"20201231", `AcquisitionDate = 2020-12-31`},
	"AcquisitionDateTime":   {"20201231235959", `AcquisitionDateTime = 2020-12-31T23:59:59`},
	"ImageType":             {`XXONE\XXTWO`, `ImageType == "XXTWO"`},
	"ScanningSequence":      {`XXSE\XXGR`, `ScanningSequence == "XXGR"`},
	"OperatorsName":         {"Xx^Op", `OperatorsName == "Xx^Op"`},
}

// testSearchWritten guards that every attribute the search table holds is
// written when a scan is archived, read from the header the archive keeps:
// a scan whose header holds a value of the attribute is the only one a
// search for it finds, each attribute checked by itself.
func testSearchWritten(e *penv) {
	e.addReceiver("recv-a", "store", 11112)
	attrs := search.Attributes()
	study := 1
	for i, a := range attrs {
		v, ok := searchValueOf[a.Keyword]
		if !ok {
			e.Fatalf("the test has no value for the attribute %s", a.Keyword)
		}
		ds := dataSetOf(e, map[string]string{a.Keyword: v[0]})
		row := search.Extract(ds, instanceOf(study, 0, 0))
		e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-w", session: "ses-" + a.Keyword, study: study, at: 10 + 10*i,
			scans: []scanSpec{{label: "scan-w", instances: 1, row: row}}})
		study++
	}
	// A scan that holds none of them.
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-w", session: "ses-none", study: study, at: 10 + 10*len(attrs),
		scans: []scanSpec{{label: "scan-w", instances: 1, row: search.Extract(dataSetOf(e, nil), instanceOf(study, 0, 0))}}})
	for _, a := range attrs {
		_, got := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(searchValueOf[a.Keyword][1]), Now: T0}, 100)
		if want := "sub-w/ses-" + a.Keyword + "/scan-w"; !slices.Equal(got, []string{want}) {
			e.Errorf("a search for %s answers %v, want only %s", a.Keyword, got, want)
		}
		if a.Multi {
			// "absent" of an attribute with several values is served by no
			// index (a row has none where its table lacks its key).
			continue
		}
		_, absent := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(
			a.Keyword + ` absent`), Now: T0}, 100)
		if len(absent) != len(attrs) {
			e.Errorf("%s absent answers %d scans, want the %d that do not hold it", a.Keyword, len(absent), len(attrs))
		}
	}
}

// testSearchRefusals guards what a search refuses: a condition no index
// serves (an "and" with none served child, an "or" with one unserved, a
// negation alone), a contains of fewer than three characters, a field the
// table does not hold, a value of another type, a level that is not one,
// a cursor from another search; and that none of them answers rows.
func testSearchRefusals(e *penv) {
	e.searchTenant(e.deidProfile())
	refused := func(cond string, level query.Level, want fault.Kind, in string) {
		e.Helper()
		n, err := query.Parse(cond)
		if err != nil {
			// The grammar's own refusals.
			if in != "" && !strings.Contains(err.Error(), in) {
				e.Errorf("%q: the parser refuses with %q, want it to name %q", cond, err, in)
			}
			return
		}
		_, err = e.p.Search(e.ctx, tenant.SearchQuery{Level: level, Condition: n, Page: master.Page{Limit: 10}, Now: T0})
		if fault.KindOf(err) != want || (in != "" && !strings.Contains(fault.Message(err), in)) {
			e.Errorf("%q: Search = %v, want %s naming %q", cond, err, want, in)
		}
	}
	refused(`not Modality == "MR"`, query.LevelScan, fault.Invalid, "negation")
	refused(`SeriesDescription contains "mp"`, query.LevelScan, fault.Invalid, "three characters")
	refused(`Modality == "MR" or SeriesDescription matches "x"`, query.LevelScan, fault.Invalid, "")
	refused(`PatientBirthDate == 2000-01-01`, query.LevelScan, fault.Invalid, "PatientBirthDate")
	refused(`SliceThickness == "thick"`, query.LevelScan, fault.Invalid, "SliceThickness")
	refused(`Modality == "MR" and SliceThickness absent`, query.LevelSession, fault.Invalid, "level")
	refused(`SeriesDescription like "*a?"`, query.LevelScan, fault.Invalid, "three characters")
	refused(`SeriesDescription contains "mprage" or Modality == "MR" or SeriesDescription like "??"`, query.LevelScan, fault.Invalid, "")
	if _, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelInstance, Page: master.Page{Limit: 10}, Now: T0}); fault.KindOf(err) != fault.Invalid {
		e.Errorf("a search of instances = %v, want Invalid", err)
	}
	// An "or" with one child no index serves is refused; with every child
	// served it is not; an "and" with one served child is not.
	for cond, ok := range map[string]bool{
		`Modality == "MR" or SeriesDescription like "*mp*"`:                          false,
		`Modality == "MR" or SeriesDescription contains "mprage"`:                    true,
		`Modality == "MR" and SeriesDescription like "*a?"`:                          true,
		`ProtocolName present and SeriesDescription like "*a?" and Modality == "CT"`: true,
	} {
		_, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(cond), Page: master.Page{Limit: 10}, Now: T0})
		if (err == nil) != ok {
			e.Errorf("%q: Search = %v, served %v", cond, err, ok)
		}
	}
	// A cursor of another search is refused, as is one the caller altered.
	a, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`Modality == "MR"`),
		Page: master.Page{Limit: 1}, Now: T0})
	e.ok("Search", err)
	if a.Next == "" {
		e.Fatal("a search of several rows answers one page")
	}
	for _, c := range []string{a.Next + "x", "garbage"} {
		_, err = e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`Modality == "MR"`),
			Page: master.Page{Limit: 1, Cursor: c}, Now: T0})
		e.wantKind("a page token not the search's", err, fault.Invalid)
	}
	_, err = e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`Modality == "CT"`),
		Page: master.Page{Limit: 1, Cursor: a.Next}, Now: T0})
	e.wantKind("a page token of another search", err, fault.Invalid)
}

// testSearchCountsAndFacets guards the counts and facets of a search: the
// count is the rows that match, a count that reaches the bound says it
// stopped there, a facet counts each value once per row (a row with two
// values counts under each), a facet of more rows than the bound is
// refused, and a facet of a field of another level is.
func testSearchCountsAndFacets(e *penv) {
	e.searchTenant(e.deidProfile())
	q := tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`Modality == "MR"`), CountRows: 1000, Now: T0,
		Page: master.Page{Limit: 2}, Count: true}
	r, err := e.p.Search(e.ctx, q)
	e.ok("Search", err)
	if !r.Counted || r.Total != 7 || r.AtLeast || len(r.Scans) != 2 || r.Next == "" {
		e.Errorf("a count of the MR scans is %+v, want 7 exactly with a first page of 2", r)
	}
	q.CountRows = 1000
	q.Condition = nil
	r, err = e.p.Search(e.ctx, q)
	e.ok("Search of every scan", err)
	if !r.Counted || r.Total != 10 || r.AtLeast {
		e.Errorf("a count of every scan is %+v, want 10", r)
	}
	// The bound is at least one row; a count that reaches it says so.
	q.CountRows = 1
	if r, err = e.p.Search(e.ctx, q); err != nil || !r.Counted || r.Total != 1 || !r.AtLeast {
		e.Errorf("a count bounded at one row is %+v, %v, want 1 and at least", r, err)
	}
	facet := func(field, cond string, bound int64) ([]tenant.FacetCount, error) {
		f, err := query.ParseField(field)
		e.ok("ParseField", err)
		var n query.Node
		if cond != "" {
			n = e.searchCondition(cond)
		}
		r, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: n, Facet: &f, CountRows: bound, Page: master.Page{Limit: 1}, Now: T0})
		return r.Facet, err
	}
	got, err := facet("Modality", "", 1000)
	e.ok("a facet of Modality", err)
	if want := []tenant.FacetCount{fc("MR", 7), fc("CT", 2), fc("PT", 1)}; !slices.Equal(got, want) {
		e.Errorf("the facet of Modality is %v, want %v", got, want)
	}
	got, err = facet("ImageType", "", 1000)
	e.ok("a facet of ImageType", err)
	if want := []tenant.FacetCount{fc("PRIMARY", 4), fc("ORIGINAL", 3), fc("AXIAL", 2), fc("DERIVED", 2), fc("LOCALIZER", 1), fc("SECONDARY", 1)}; !slices.Equal(got, want) {
		e.Errorf("the facet of ImageType is %v, want %v", got, want)
	}
	got, err = facet("SliceThickness", `Modality == "MR"`, 1000)
	e.ok("a facet of SliceThickness", err)
	if len(got) != 4 {
		e.Errorf("the facet of SliceThickness over the MR scans is %v, want 4 values", got)
	}
	_, err = facet("Modality", "", 5)
	e.wantKind("a facet of more rows than the bound", err, fault.Precondition)
	if err != nil && !strings.Contains(fault.Message(err), "search.facet-rows") {
		e.Errorf("the refusal of a facet reads %q, want it to name search.facet-rows", fault.Message(err))
	}
	_, err = facet("session.date", "", 1000)
	e.wantKind("a facet of a field of another level", err, fault.Invalid)
	_, err = facet("PatientBirthDate", "", 1000)
	e.wantKind("a facet of a field the table does not hold", err, fault.Invalid)
}

// testSearchHidesOriginals guards what a caller that may not learn original
// values is shown: a scan archived as received, and a session holding one,
// match no condition and are in no count or facet, however the condition
// reaches them (a "has" or an "every" included), while the scans archived
// de-identified are found as for any caller.
func testSearchHidesOriginals(e *penv) {
	e.addReceiver("recv-a", "store", 11112)
	deid := e.deidProfile()
	ds := func(m map[string]string) search.Row { return search.Extract(dataSetOf(e, m), "2.25.9") }
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-a", session: "deid", study: 1, at: 10, date: "20240101", profile: deid,
		scans: []scanSpec{{label: "s1", instances: 1, row: ds(map[string]string{"Modality": "MR", "Manufacturer": "ACME"})}}})
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-a", session: "plain", study: 2, at: 20, date: "20240202",
		scans: []scanSpec{{label: "s2", instances: 1, row: ds(map[string]string{"Modality": "MR", "Manufacturer": "ACME"})}}})
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-b", session: "deid", study: 3, at: 30, date: "20240303", profile: deid,
		scans: []scanSpec{{label: "s3", instances: 1, row: ds(map[string]string{"Modality": "CT", "Manufacturer": "ACME"})},
			{label: "s4", instances: 1, row: ds(map[string]string{"Modality": "MR", "Manufacturer": "OTHER"})}}})
	for _, c := range []struct {
		cond  string
		level query.Level
		all   []string // what an unrestricted caller finds
		seen  []string // what a caller that may not learn originals finds
	}{
		{`Manufacturer == "ACME"`, query.LevelScan, []string{"sub-a/deid/s1", "sub-a/plain/s2", "sub-b/deid/s3"}, []string{"sub-a/deid/s1", "sub-b/deid/s3"}},
		{`has scan (Manufacturer == "ACME")`, query.LevelSession, []string{"sub-a/deid", "sub-a/plain", "sub-b/deid"}, []string{"sub-a/deid", "sub-b/deid"}},
		{`session.date >= 2024-01-01 and has scan (Modality == "MR")`, query.LevelSession, []string{"sub-a/deid", "sub-a/plain", "sub-b/deid"}, []string{"sub-a/deid", "sub-b/deid"}},
		{`every scan (Manufacturer == "ACME")`, query.LevelSession, []string{"sub-a/deid", "sub-a/plain"}, []string{"sub-a/deid"}},
		{`has session (has scan (Modality == "MR" and Manufacturer == "ACME")) and subject.label = "sub-a"`, query.LevelSubject, []string{"sub-a"}, []string{"sub-a"}},
		{`has session (session.date = 2024-02-02)`, query.LevelSubject, []string{"sub-a"}, nil},
	} {
		n := e.searchCondition(c.cond)
		for _, hide := range []bool{false, true} {
			r, got := e.searchAll(tenant.SearchQuery{Level: c.level, Condition: n, Hide: hide, Count: true, CountRows: 100, Now: T0}, 100)
			slices.Sort(got)
			want := c.all
			if hide {
				want = c.seen
			}
			if !slices.Equal(got, want) || r.Total != int64(len(want)) {
				e.Errorf("%q hiding originals %v answers %v counting %d, want %v", c.cond, hide, got, r.Total, want)
			}
		}
	}
	f, err := query.ParseField("Manufacturer")
	e.ok("ParseField", err)
	r, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Facet: &f, Hide: true, CountRows: 100, Page: master.Page{Limit: 1}, Now: T0})
	e.ok("a facet hiding originals", err)
	if want := []tenant.FacetCount{fc("ACME", 2), fc("OTHER", 1)}; !slices.Equal(r.Facet, want) {
		e.Errorf("the facet of Manufacturer hiding originals is %v, want %v: the scan archived as received is no part of it", r.Facet, want)
	}
}

// testSearchPagesHoldWhileArchiving guards a search's cursor against
// writes between its pages: scans archived after the first page neither
// shift the rows of the later pages nor repeat one, and a scan archived
// is found by the next search.
func testSearchPagesHoldWhileArchiving(e *penv) {
	e.searchTenant(e.deidProfile())
	q := tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`Modality == "MR"`), Page: master.Page{Limit: 3}, Now: T0}
	first, err := e.p.Search(e.ctx, q)
	e.ok("Search", err)
	if len(first.Scans) != 3 || first.Next == "" {
		e.Fatalf("the first page is %d scans with next %q, want 3 with more", len(first.Scans), first.Next)
	}
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-late", session: "ses-late", study: 90, at: 900, date: "20300101",
		scans: []scanSpec{{label: "late", instances: 1, row: search.Extract(dataSetOf(e, map[string]string{"Modality": "MR"}), instanceOf(90, 0, 0))}}})
	seen := scanPaths(first.Scans)
	for next := first.Next; next != ""; {
		q.Page.Cursor = next
		r, err := e.p.Search(e.ctx, q)
		e.ok("Search", err)
		seen = append(seen, scanPaths(r.Scans)...)
		next = r.Next
	}
	if len(seen) != 8 || !slices.Contains(seen, "sub-late/ses-late/late") {
		e.Errorf("the pages hold %v, want the seven MR scans of the tenant and the one archived between the pages", seen)
	}
	dup := map[string]bool{}
	for _, p := range seen {
		if dup[p] {
			e.Errorf("the pages repeat %s", p)
		}
		dup[p] = true
	}
}
