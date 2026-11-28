package api_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// pythonSearchEnv names the Python, with pydicom, that runs the search
// comparison; it is listed in docs/develop/testing.md.
const pythonSearchEnv = "NOPACS_CHECK_SEARCH_PYTHON"

// searchCases are the conditions the comparison runs, at the level that
// leads each: equality (folded, exact and in a list, on a value with
// several values too), ranges of numbers, dates and date-times, contains,
// prefix and like, present and absent, and, or and not, and has and every
// across the levels, down and up. Each is served by an index.
var searchCases = []struct {
	level v1.SearchLevel
	query string
	facet string
}{
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, "", "Modality"},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality = "mr"`, "Manufacturer"},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "MR"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "mr"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality nocase in ("ct", "pt")`, "Modality"},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesDescription = "t1_mprage"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesDescription = "muller strasse"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesDescription == "Müller Straße"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Manufacturer = "SIEMENS"`, "Manufacturer"},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `PatientSex == "F"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ImageType == "AXIAL"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ImageType == "ORIGINAL" and ScanningSequence = "se"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ScanningSequence in ("SE", "EP")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `OperatorsName == "Roe^Ann"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SliceThickness < 1.5`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SliceThickness between 1 and 2.5`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SliceThickness == 3.3`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesNumber >= 3`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `MagneticFieldStrength == 3`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `EchoTime > 20`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `RepetitionTime <= 500`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `FlipAngle != 90 and Modality == "MR"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `AcquisitionDate between 2023-06-01 and 2024-12-31`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `AcquisitionDate >= 2025-01-01`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `AcquisitionDateTime >= 2024-01-01T12:00:00`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `AcquisitionDateTime < 2023-12-01T00:00`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesDescription contains "mpr"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ProtocolName contains "SAG"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `StudyDescription contains "brain"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `StudyDescription contains "cérébrale"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ProtocolName like "T1W*"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `SeriesDescription nocase prefix "t1"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `ProtocolName present`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "MR" and ProtocolName absent`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `OperatorsName present`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality = "ct" or Manufacturer = "ge medical systems"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "MR" and not SliceThickness < 1.5`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "MR" and (BodyPartExamined = "head" or BodyPartExamined = "brain")`, "BodyPartExamined"},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `Modality == "MR" and has session (session.date between 2024-01-01 and 2025-12-31)`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `has session (every scan (Modality == "MR")) and SliceThickness < 2`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SCAN, `every session (session.date >= 2023-01-01) and Modality == "US"`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, "", ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `session.date between 2023-06-01 and 2024-12-31`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `session.date > 2025-01-01`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (Modality == "CT")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `every scan (Modality == "MR")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `every scan (SeriesDescription present)`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (Modality == "MR" and SliceThickness < 1.5)`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (ImageType == "LOCALIZER") and session.date >= 2024-01-01`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (SeriesDescription contains "fmri") and has scan (Modality == "CT")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (Manufacturer = "siemens") or has scan (Manufacturer = "philips")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (Modality == "MR") and not every scan (Modality == "MR")`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, "", ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `has session (has scan (Modality == "PT"))`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `has scan (SliceThickness > 4)`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `every session (has scan (Modality == "MR"))`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `every session (every scan (Manufacturer = "siemens"))`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `has session (session.date < 2024-01-01)`, ""},
	{v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `every session (session.date >= 2023-01-01) and has scan (PatientSex == "F")`, ""},
}

// levelName is how the reference names a level.
var levelName = map[v1.SearchLevel]string{
	v1.SearchLevel_SEARCH_LEVEL_SCAN: "scan", v1.SearchLevel_SEARCH_LEVEL_SESSION: "session", v1.SearchLevel_SEARCH_LEVEL_SUBJECT: "subject",
}

// TestSearchEqualsPydicom guards that what Search answers over a tenant
// archived by reception equals a reference filter that shares nothing with
// the archive: the files are made with go-dicom (values drawn from pools of
// case, accents, ranges, dates, lists and absences, the second file of a
// scan with the lower SOP Instance UID and other values), archived through
// the reception pipeline, and searched through the service by pages of
// seven rows with a count and, for some queries, a facet; then pydicom reads
// the same files and scripts/check/search-pydicom.py evaluates the same
// conditions, written from the grammar, and compares the rows, the counts
// and the facets. It fails on any difference.
func TestSearchEqualsPydicom(t *testing.T) {
	python, asked := os.LookupEnv(pythonSearchEnv)
	if !asked {
		t.Skip(pythonSearchEnv + " is not set; `make check-search` runs this comparison with pydicom")
	}
	if python == "" {
		t.Fatal(pythonSearchEnv + " is empty; name an interpreter that has pydicom")
	}
	made := testkit.SearchFiles(t, filepath.Join("..", "receive", "testdata"), 1)
	files := make([][]byte, len(made))
	for i, f := range made {
		files[i] = f.Bytes
	}
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, files)
	reader := f.as(f.lab1Reader)

	work := t.TempDir()
	var paths []string
	for id, b := range a.sent {
		name := filepath.Join(work, id+".dcm")
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)

	// A subject's key is its sessions' study UIDs, sorted and joined.
	sessions := mustCall[*v1.ListSessionsResponse](t, "ListSessions", call(reader.catalog.ListSessions,
		&v1.ListSessionsRequest{Tenant: "lab1", PageSize: 1000})).GetSessions()
	studiesOf := map[string][]string{}
	for _, s := range sessions {
		studiesOf[s.GetSubjectLabel()] = append(studiesOf[s.GetSubjectLabel()], s.GetStudyInstanceUid())
	}
	for _, s := range studiesOf {
		slices.Sort(s)
	}

	type facetCount = map[string]int64
	var queries []map[string]any
	for _, c := range searchCases {
		var keys []string
		var total *int64
		counts := facetCount{}
		token := ""
		for page := 0; ; page++ {
			if page > 200 {
				t.Fatalf("%s search %q: more than 200 pages of 7 rows", levelName[c.level], c.query)
			}
			r := mustCall[*v1.SearchResponse](t, "Search "+c.query, call(reader.search.Search, &v1.SearchRequest{
				Tenant: "lab1", Level: c.level, Query: c.query, PageSize: 7, PageToken: token, Count: page == 0, Facet: ifFirst(page == 0, c.facet)}))
			if page == 0 {
				total = r.TotalCount
				for _, fv := range r.GetFacet() {
					counts[fv.GetValue()] = fv.GetCount()
				}
			}
			for _, s := range r.GetScans() {
				keys = append(keys, s.GetSeriesInstanceUid())
			}
			for _, s := range r.GetSessions() {
				keys = append(keys, s.GetStudyInstanceUid())
			}
			for _, s := range r.GetSubjects() {
				keys = append(keys, strings.Join(studiesOf[s.GetLabel()], ","))
			}
			if token = r.GetNextPageToken(); token == "" {
				break
			}
		}
		if total == nil {
			t.Fatalf("%s search %q: no count", levelName[c.level], c.query)
		}
		q := map[string]any{"level": levelName[c.level], "query": c.query, "keys": append([]string{}, keys...), "count": *total}
		if c.facet != "" {
			q["facet"], q["facet_counts"] = c.facet, counts
		}
		queries = append(queries, q)
	}

	manifest := filepath.Join(work, "answers.json")
	b, err := json.Marshal(map[string]any{"files": paths, "queries": queries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "scripts", "check", "search-pydicom.py")
	res, err := exec.Command(python, "-I", script, manifest).CombinedOutput() // #nosec G204 -- the operator's interpreter.
	t.Logf("%s", res)
	if err != nil {
		t.Fatalf("the comparison with pydicom failed: %v", err)
	}
	if !strings.Contains(string(res), "0 differ") {
		t.Fatal("the comparison did not report that nothing differs")
	}
}

// ifFirst returns s on the first page and "" on the others, since a facet
// is asked once.
func ifFirst(first bool, s string) string {
	if first {
		return s
	}
	return ""
}
