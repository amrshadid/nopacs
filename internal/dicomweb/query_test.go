package dicomweb

import (
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/tag"
)

// TestParseQueryReadsKeysAndPage guards the reading of a search's query
// (PS3.18 8.3.4): keys by keyword and by tag, each kind of matching, UID
// lists by commas, backslashes and repeats, includefield, a universal
// match kept as a return key, and the page with its defaults and cap.
func TestParseQueryReadsKeysAndPage(t *testing.T) {
	res := Resource{Kind: Search, Level: Series, Study: "1.2.3"}
	q, err := ParseQuery(res, "Modality=MR&0008103E=T1*&SeriesInstanceUID=1.5,1.4&SeriesInstanceUID=1.4%5C1.6"+
		"&PatientName=*&includefield=StudyDescription,00080021&includefield=all&limit=5000&offset=7&fuzzymatching=true")
	if err != nil {
		t.Fatal(err)
	}
	if q.Limit != MaxLimit || !q.Capped || q.Offset != 7 || !q.Fuzzy || !q.IncludeAll {
		t.Errorf("page and flags: %+v", q)
	}
	study, ok := q.Key(TagStudyInstanceUID)
	if !ok || study.Match != MatchSingle || study.Value != "1.2.3" {
		t.Errorf("the path's study is not a key: %+v", study)
	}
	series, _ := q.Key(TagSeriesInstanceUID)
	if series.Match != MatchList || !slices.Equal(series.UIDs, []string{"1.4", "1.5", "1.6"}) {
		t.Errorf("series list: %+v", series)
	}
	desc, _ := q.Key(TagSeriesDescription)
	if desc.Match != MatchWildcard || desc.Value != "T1*" || desc.Keyword != "SeriesDescription" {
		t.Errorf("description: %+v", desc)
	}
	if _, ok := q.Key(tag.New(0x0010, 0x0010)); ok {
		t.Error("a universal match is a key")
	}
	if !slices.Contains(q.Include, tag.New(0x0010, 0x0010)) || !slices.Contains(q.Include, tag.New(0x0008, 0x0021)) {
		t.Errorf("includes: %v", q.Include)
	}
	if got := q.Keywords(); got != "Modality,SeriesDescription,StudyInstanceUID,SeriesInstanceUID" {
		t.Errorf("keywords %q", got)
	}
	d, err := ParseQuery(Resource{Kind: Search, Level: Study}, "StudyDate=20200101-&StudyTime=-1200")
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := d.Key(TagStudyDate); k.Match != MatchRange || k.Low != "20200101" || k.High != "" {
		t.Errorf("date range: %+v", k)
	}
	if d.Limit != DefaultLimit || d.Offset != 0 {
		t.Errorf("default page: %d %d", d.Limit, d.Offset)
	}
}

// TestParseQueryRefusesWhatItCannotMean guards each refusal of a query,
// with its status: 400 for a request the search cannot mean (PS3.18 8.5),
// 406 for a charset other than UTF-8.
func TestParseQueryRefusesWhatItCannotMean(t *testing.T) {
	res := Resource{Kind: Search, Level: Study, Study: ""}
	for query, status := range map[string]int{
		"%zz":              http.StatusBadRequest,
		"NotAnAttribute=1": http.StatusBadRequest,
		"FFFF0001=1":       http.StatusBadRequest,
		"ReferencedSeriesSequence.SeriesInstanceUID=1.2": http.StatusBadRequest,
		"ReferencedSeriesSequence=1":                     http.StatusBadRequest,
		"Modality=MR&Modality=CT":                        http.StatusBadRequest,
		"limit=0":                                        http.StatusBadRequest,
		"limit=-1":                                       http.StatusBadRequest,
		"limit=ten":                                      http.StatusBadRequest,
		"offset=" + strconv.Itoa(MaxOffset+1):            http.StatusBadRequest,
		"fuzzymatching=maybe":                            http.StatusBadRequest,
		"StudyDate=2020-01-2021":                         http.StatusBadRequest,
		"StudyDate=2020*":                                http.StatusBadRequest,
		"includefield=Nope":                              http.StatusBadRequest,
		"charset=ISO-8859-1":                             http.StatusNotAcceptable,
	} {
		if _, err := ParseQuery(res, query); err == nil || err.Status != status {
			t.Errorf("%q: got %v, want %d", query, err, status)
		}
	}
	if _, err := ParseQuery(Resource{Kind: Search, Level: Series, Study: "1.2"}, "StudyInstanceUID=1.3"); err == nil {
		t.Error("a study the path names is given again")
	}
	many := "StudyInstanceUID=1"
	for i := 0; i < MaxUIDList; i++ {
		many += ",1." + strconv.Itoa(i)
	}
	if _, err := ParseQuery(res, many); err == nil {
		t.Error("a list past the bound is taken")
	}
}

// TestReturnedFollowsTheLevel guards the attributes a result holds by
// default (PS3.18 10.6.3.3): a level's own and those of each level above
// its path does not name, never a level below it.
func TestReturnedFollowsTheLevel(t *testing.T) {
	study, _ := ParseQuery(Resource{Kind: Search, Level: Study}, "")
	if w := study.Returned(); !w[TagPatientID] || w[TagModality] || w[TagSOPClassUID] {
		t.Errorf("study level: %v", w)
	}
	inSeries, _ := ParseQuery(Resource{Kind: Search, Level: Instance, Study: "1.2", Series: "1.3"}, "")
	if w := inSeries.Returned(); !w[TagSOPClassUID] || w[TagModality] || w[TagPatientID] || !w[TagSeriesInstanceUID] {
		t.Errorf("instances of a series: %v", w)
	}
	all, _ := ParseQuery(Resource{Kind: Search, Level: Instance}, "includefield=all")
	if w := all.Returned(); !w[TagPatientID] || !w[TagModality] || !all.AllAttributes() {
		t.Errorf("instances of the tenant: %v", w)
	}
}

// TestParseQueryStopsALongUIDListEarly guards the cost of a UID list past
// MaxUIDList: it is refused when the list passes the limit, before the
// rest of the query is split and compared, so a query of tens of
// thousands of UIDs allocates what MaxUIDList of them take, and repeats
// are still counted once.
func TestParseQueryStopsALongUIDListEarly(t *testing.T) {
	var b strings.Builder
	b.WriteString("SOPInstanceUID=")
	for i := 0; b.Len() < 60000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("1." + strconv.Itoa(i))
	}
	raw := b.String()
	res := Resource{Kind: Search, Level: Instance}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const runs = 4
	for range runs {
		if _, err := ParseQuery(res, raw); err == nil || err.Status != http.StatusBadRequest || !strings.Contains(err.Message, "at most 100 UIDs") {
			t.Fatalf("a list of thousands of UIDs: %v, want 400 naming the limit", err)
		}
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 64<<10 {
		t.Errorf("refusing a long UID list allocated %d bytes a query, want under 64 KiB", per)
	}
	// MaxUIDList distinct UIDs, each given twice, is a list of MaxUIDList.
	var ok strings.Builder
	ok.WriteString("SOPInstanceUID=")
	for i := range 2 * MaxUIDList {
		if i > 0 {
			ok.WriteString(",")
		}
		ok.WriteString("1." + strconv.Itoa(i%MaxUIDList))
	}
	q, err := ParseQuery(res, ok.String())
	if err != nil {
		t.Fatalf("%d UIDs given twice each: %v", MaxUIDList, err)
	}
	if k, _ := q.Key(TagSOPInstanceUID); len(k.UIDs) != MaxUIDList {
		t.Errorf("the list holds %d UIDs, want %d", len(k.UIDs), MaxUIDList)
	}
}
