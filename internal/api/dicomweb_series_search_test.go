package api_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// seriesOf returns the Series Instance UIDs of a search's answer, sorted.
func seriesOf(t *testing.T, a httpAnswer) []string {
	t.Helper()
	var out []string
	for _, r := range results(t, a) {
		out = append(out, value(r, "0020000E"))
	}
	slices.Sort(out)
	return out
}

// seriesTenant is a tenant of files with every attribute the search tables
// index (testkit.SearchFiles), received as received, and served through
// DICOMweb.
type seriesTenant struct {
	f *placeFixture
	s *dataServer
	// lowest is each series' file with the lowest SOP Instance UID, the one
	// its search row is read from, by series; study is each series' study.
	lowest, study map[string]string
	series        []string
}

// newSeriesTenant receives the files.
func newSeriesTenant(t *testing.T) *seriesTenant {
	t.Helper()
	made := testkit.SearchFiles(t, filepath.Join("..", "receive", "testdata"), 1)
	files := make([][]byte, len(made))
	st := &seriesTenant{lowest: map[string]string{}, study: map[string]string{}}
	for i, m := range made {
		files[i] = m.Bytes
		if cur, ok := st.lowest[m.Series]; !ok || m.SOP < cur {
			st.lowest[m.Series] = m.SOP
		}
		st.study[m.Series] = m.Study
	}
	for se := range st.lowest {
		st.series = append(st.series, se)
	}
	slices.Sort(st.series)
	st.f = newPlaceFixture(t)
	receiveArchive(t, st.f, files)
	st.s = serveWeb(t, st.f.env)
	return st
}

// get asks the tenant's DICOMweb root as c.
func (st *seriesTenant) get(t *testing.T, c caller, rest string) httpAnswer {
	t.Helper()
	return st.s.get(t, http.MethodGet, webPath(rest), c)
}

// values returns the values of the attribute the search of series is tried
// with: the first value of the lowest file of each series, up to three distinct,
// and the forms of each a matching rule tells apart (another case, a
// prefix pattern, a range of a date), and one no series holds.
func (st *seriesTenant) values(t *testing.T, a search.Attribute) []string {
	t.Helper()
	hex := fmt.Sprintf("%08X", a.Tag())
	var seen []string
	for _, se := range st.series {
		r := results(t, st.get(t, st.f.lab1Reader, "studies/"+st.study[se]+"/series/"+se+"/instances/"+st.lowest[se]+"/metadata"))
		if len(r) != 1 {
			t.Fatalf("series %s: %d metadata answers", se, len(r))
		}
		if v := value(r[0], hex); v != "" && !slices.Contains(seen, v) {
			seen = append(seen, v)
		}
	}
	slices.Sort(seen)
	if len(seen) > 3 {
		seen = []string{seen[0], seen[len(seen)/2], seen[len(seen)-1]}
	}
	out := slices.Clone(seen)
	for _, v := range seen {
		switch a.Type {
		case query.TypeText:
			// A person's name is matched without regard to case by the search
			// tables (query.FromQIDO) and exactly at the instance level;
			// TestSeriesSearchMatchesNamesWithoutRegardToCase holds that.
			if a.Keyword != "OperatorsName" {
				out = append(out, strings.ToLower(v), strings.ToUpper(v))
			}
			if len([]rune(v)) >= 3 {
				out = append(out, string([]rune(v)[:2])+"*")
			}
		case query.TypeDate:
			out = append(out, v+"-", "-"+v, v+"-"+v)
		case query.TypeDateTime:
			// A bound written to less than the second ("202304") is the start
			// of the period in the search tables, as routing reads it, and the
			// whole period at the instance level (PS3.4 C.2.2.2.5): the
			// ranges are tried with full date-times.
			if whole, _, _ := strings.Cut(v, "."); len(whole) == 14 {
				out = append(out, v+"-", "-"+v, v+"-"+v)
			}
		}
	}
	out = append(out, "ZZZ-none")
	if a.Type == query.TypeNumber {
		out = append(out[:len(out)-1], "9999")
	}
	if a.Type == query.TypeDate {
		out[len(out)-1] = "19000101"
	}
	if a.Type == query.TypeDateTime {
		out[len(out)-1] = "19000101120000"
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestSeriesSearchThroughSearchTables guards, over one tenant of files with
// every attribute the search tables index, a QIDO-RS
// search of series by each attribute the search tables index: its answer,
// for a study and for the whole tenant, equals what the metadata files
// give, which is each series' file with the lowest SOP Instance UID (the one
// its search row is read from) matched at the instance level as the
// metadata files always were; and for the keys the catalog also holds
// (modality, description, number) it equals what the catalog's lists
// answered before the tables; a masked key finds no series archived as
// received, by either path.
func TestSeriesSearchThroughSearchTables(t *testing.T) {
	st := newSeriesTenant(t)
	t.Run("EqualsMetadataFiles", func(t *testing.T) { seriesSearchEqualsMetadataFiles(t, st) })
	t.Run("NamesWithoutRegardToCase", func(t *testing.T) { seriesSearchNamesWithoutRegardToCase(t, st) })
	t.Run("WhileAnAttributeIsBeingFilled", func(t *testing.T) { seriesSearchWhileFilling(t, st) })
}

// seriesSearchEqualsMetadataFiles is the comparison the test above names.
func seriesSearchEqualsMetadataFiles(t *testing.T, st *seriesTenant) {
	masked, _ := st.f.maskedKey(st.f.onTenant("lab1", master.RoleReader))
	reader := st.f.lab1Reader
	studies := map[string]bool{}
	for _, se := range st.series {
		studies[st.study[se]] = true
	}
	compared, nonEmpty, refused, notServed := 0, 0, 0, 0
	for _, a := range search.Attributes() {
		catalogKey := a.Keyword == "Modality" || a.Keyword == "SeriesDescription" || a.Keyword == "SeriesNumber"
		for vi, v := range st.values(t, a) {
			param := a.Keyword + "=" + url.QueryEscape(v)
			all := st.get(t, reader, "series?limit=1000&"+param)
			// The metadata files: every instance of the tenant whose own
			// header matches the key, of which each series' file with the
			// lowest SOP Instance UID is the one its search row is read from.
			var want []string
			bad := false
			ia := st.get(t, reader, "instances?limit=1000&"+param)
			switch ia.status {
			case http.StatusOK, http.StatusNoContent:
				for _, r := range results(t, ia) {
					if se := value(r, "0020000E"); st.lowest[se] == value(r, "00080018") {
						want = append(want, se)
					}
				}
			default:
				bad = true
			}
			if bad {
				// A value the request's grammar refuses (a date range of a
				// partial date-time): refused at the series level too.
				if all.status != http.StatusBadRequest {
					t.Errorf("%s: the instance level refuses it, the series level answers %d", param, all.status)
				}
				refused++
				continue
			}
			if all.status == http.StatusBadRequest && strings.Contains(string(all.body), "no index serves") {
				// A pattern no index serves (a value that begins with "*"):
				// refused, naming the index it needs, where the instance level
				// reads every file.
				if !catalogKey {
					notServed++
					continue
				}
			}
			slices.Sort(want)
			got := seriesOf(t, all)
			if !slices.Equal(got, want) {
				t.Errorf("%s: series through the search tables %v, through the metadata files %v", param, got, want)
			}
			compared++
			if len(want) > 0 {
				nonEmpty++
			}
			if vi < 2 {
				for study := range studies {
					inStudy := seriesOf(t, st.get(t, reader, "studies/"+study+"/series?limit=1000&"+param))
					var wantStudy []string
					for _, se := range want {
						if st.study[se] == study {
							wantStudy = append(wantStudy, se)
						}
					}
					if !slices.Equal(inStudy, wantStudy) {
						t.Errorf("%s in study %s: %v, want %v", param, study, inStudy, wantStudy)
					}
				}
			}
			// A masked key finds nothing: every series was archived as received.
			if m := seriesOf(t, st.get(t, masked, "series?limit=1000&"+param)); len(m) != 0 {
				t.Errorf("%s: a masked key finds the series %v archived as received", param, m)
			}
			if catalogKey {
				api.SetSeriesFromCatalog(st.s.h, true)
				old := seriesOf(t, st.get(t, reader, "series?limit=1000&"+param))
				oldMasked := seriesOf(t, st.get(t, masked, "series?limit=1000&"+param))
				api.SetSeriesFromCatalog(st.s.h, false)
				if !slices.Equal(old, got) {
					t.Errorf("%s: series through the catalog's lists %v, through the search tables %v", param, old, got)
				}
				if len(oldMasked) != 0 {
					t.Errorf("%s: a masked key finds the series %v through the catalog's lists", param, oldMasked)
				}
			}
		}
	}
	if compared < 80 || nonEmpty < 60 {
		t.Errorf("%d searches compared and %d matched a series (%d refused, %d not served): the comparison proves little", compared, nonEmpty, refused, notServed)
	}
}

// seriesSearchNamesWithoutRegardToCase guards that the search
// tables match a person's name at the series level without regard to case,
// as PS3.4 C.2.2.2.1 permits and query.FromQIDO maps it, so a name in
// capitals finds the series a name as written does, while the instance level
// still matches the name as written.
func seriesSearchNamesWithoutRegardToCase(t *testing.T, st *seriesTenant) {
	reader := st.f.lab1Reader
	asWritten := seriesOf(t, st.get(t, reader, "series?limit=1000&OperatorsName=Doe%5EJohn"))
	capitals := seriesOf(t, st.get(t, reader, "series?limit=1000&OperatorsName=DOE%5EJOHN"))
	if len(asWritten) == 0 || !slices.Equal(asWritten, capitals) {
		t.Errorf("a name as written finds %v, in capitals %v: want the same, not none", asWritten, capitals)
	}
	se := asWritten[0]
	a := st.get(t, reader, "studies/"+st.study[se]+"/series/"+se+"/instances?OperatorsName=DOE%5EJOHN")
	if a.status != http.StatusNoContent {
		t.Errorf("the instance level answers %d to a name in capitals, want 204: it matches the name as written", a.status)
	}
}

// seriesSearchWhileFilling guards a search of series
// while the search tables' column of an attribute is still being filled: a
// key the catalog also holds (modality) is answered from the catalog's
// lists, as it was before the tables, so it never answers from a part of the
// scans; a key only the tables hold is refused with the precondition that
// names it, and answers once the column is ready.
func seriesSearchWhileFilling(t *testing.T, st *seriesTenant) {
	reader := st.f.lab1Reader
	before := seriesOf(t, st.get(t, reader, "series?limit=1000&Modality=MR"))
	if len(before) == 0 {
		t.Fatal("no series has the modality MR")
	}
	db, err := sql.Open("sqlite", filepath.Join(st.f.data, "tenants", "lab1.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	set := func(state string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `UPDATE search_attributes SET state = ? WHERE field IN ('Modality', 'ProtocolName')`, state); err != nil {
			t.Fatal(err)
		}
	}
	ready := seriesOf(t, st.get(t, reader, "series?limit=1000&ProtocolName=T1W%20sag"))
	set("building")
	if got := seriesOf(t, st.get(t, reader, "series?limit=1000&Modality=MR")); !slices.Equal(got, before) {
		t.Errorf("modality while it is being filled finds %v, want %v from the catalog", got, before)
	}
	a := st.get(t, reader, "series?limit=1000&ProtocolName=T1W%20sag")
	if a.status != http.StatusServiceUnavailable || !strings.Contains(string(a.body), "ProtocolName is still being filled") {
		t.Errorf("a key only the tables hold while it is being filled: %d %q, want a refusal that names it", a.status, a.body)
	}
	set("ready")
	if got := seriesOf(t, st.get(t, reader, "series?limit=1000&ProtocolName=T1W%20sag")); !slices.Equal(got, ready) || len(ready) == 0 {
		t.Errorf("after the fill the key finds %v, want %v (not none)", got, ready)
	}
}
