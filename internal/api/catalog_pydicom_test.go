package api_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// pythonCatalogEnv names the Python, with pydicom, that runs the catalog
// comparison; it is listed in docs/develop/testing.md. The corpus variable of the
// metadata comparison adds files to the archive.
const pythonCatalogEnv = "NOPACS_CHECK_CATALOG_PYTHON"

// TestCatalogEqualsPydicom guards that what the catalog answers for an
// archive made by reception (its subjects, sessions and scans, with study
// dates, modalities, series descriptions and numbers, instance counts and
// bytes, and the lists in the study date and series description orders and
// by modality) equals what pydicom reads of the files that were received.
// It runs scripts/check/catalog-pydicom.py and fails on any difference.
func TestCatalogEqualsPydicom(t *testing.T) {
	python, asked := os.LookupEnv(pythonCatalogEnv)
	if !asked {
		t.Skip(pythonCatalogEnv + " is not set; it runs the comparison with pydicom")
	}
	if python == "" {
		t.Fatal(pythonCatalogEnv + " is empty; name an interpreter that has pydicom")
	}
	files, _ := metaFiles(t)
	if dirName := os.Getenv(corpusEnv); dirName != "" {
		for _, c := range moreCorpus {
			b, err := os.ReadFile(filepath.Join(dirName, c.name)) // #nosec G304 G703 -- the operator's corpus.
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, reUID(t, b, c.last))
		}
	}
	d := newCatalogDataOf(t, files)
	work := t.TempDir()
	var paths []string
	for id, b := range d.a.sent {
		name := filepath.Join(work, id+".dcm")
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	out := map[string]any{"files": paths}
	var subjects, sessions, scans []map[string]any
	for _, s := range d.subjects {
		subjects = append(subjects, map[string]any{"label": s.GetLabel(), "session_count": s.GetSessionCount(), "instance_count": s.GetInstanceCount(), "size_bytes": s.GetSizeBytes()})
	}
	for _, s := range d.sessions {
		sessions = append(sessions, map[string]any{"subject_label": s.GetSubjectLabel(), "label": s.GetLabel(), "study_instance_uid": s.GetStudyInstanceUid(),
			"study_date": s.GetStudyDate(), "modalities": s.GetModalities(), "scan_count": s.GetScanCount(), "instance_count": s.GetInstanceCount(), "size_bytes": s.GetSizeBytes()})
	}
	for _, s := range d.scans {
		m := map[string]any{"subject_label": s.GetSubjectLabel(), "session_label": s.GetSessionLabel(), "label": s.GetLabel(), "series_instance_uid": s.GetSeriesInstanceUid(),
			"modality": s.GetModality(), "series_description": s.GetSeriesDescription(), "instance_count": s.GetInstanceCount(), "size_bytes": s.GetSizeBytes()}
		if s.SeriesNumber != nil {
			m["series_number"] = s.GetSeriesNumber()
		}
		scans = append(scans, m)
	}
	out["subjects"], out["sessions"], out["scans"] = subjects, sessions, scans
	// The orders and filters, read through the service by pages of one row.
	studyOrder, _ := pageAll(t, "study date order", &v1.ListSessionsRequest{Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE}, d.cat.ListSessions,
		func(r *v1.ListSessionsRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok },
		func(s *v1.ListSessionsResponse) ([]*v1.Session, string, *int64) {
			return s.GetSessions(), s.GetNextPageToken(), s.TotalCount
		})
	seriesOrder, _ := pageAll(t, "series description order", &v1.ListScansRequest{Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION}, d.cat.ListScans,
		func(r *v1.ListScansRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok },
		func(s *v1.ListScansResponse) ([]*v1.Scan, string, *int64) {
			return s.GetScans(), s.GetNextPageToken(), s.TotalCount
		})
	var byDate, bySeries []string
	for _, s := range studyOrder {
		byDate = append(byDate, s.GetStudyInstanceUid())
	}
	for _, s := range seriesOrder {
		bySeries = append(bySeries, s.GetSeriesInstanceUid())
	}
	out["sessions_by_study_date"], out["scans_by_series_description"] = byDate, bySeries
	sessionsBy, scansBy := map[string][]string{}, map[string][]string{}
	for _, m := range []string{"MR", "CT", "SEG", "US"} {
		ss, _ := pageAll(t, "sessions of "+m, &v1.ListSessionsRequest{Tenant: "lab1", Modality: m}, d.cat.ListSessions,
			func(r *v1.ListSessionsRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok },
			func(s *v1.ListSessionsResponse) ([]*v1.Session, string, *int64) {
				return s.GetSessions(), s.GetNextPageToken(), s.TotalCount
			})
		sessionsBy[m] = []string{}
		for _, s := range ss {
			sessionsBy[m] = append(sessionsBy[m], s.GetStudyInstanceUid())
		}
		sc, _ := pageAll(t, "scans of "+m, &v1.ListScansRequest{Tenant: "lab1", Modality: m}, d.cat.ListScans,
			func(r *v1.ListScansRequest, n int32, tok string) { r.PageSize, r.PageToken = n, tok },
			func(s *v1.ListScansResponse) ([]*v1.Scan, string, *int64) {
				return s.GetScans(), s.GetNextPageToken(), s.TotalCount
			})
		scansBy[m] = []string{}
		for _, s := range sc {
			scansBy[m] = append(scansBy[m], s.GetSeriesInstanceUid())
		}
	}
	out["sessions_by_modality"], out["scans_by_modality"] = sessionsBy, scansBy
	manifest := filepath.Join(work, "answers.json")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "scripts", "check", "catalog-pydicom.py")
	res, err := exec.Command(python, "-I", script, manifest).CombinedOutput() // #nosec G204 -- the operator's interpreter.
	t.Logf("%s", res)
	if err != nil {
		t.Fatalf("the comparison with pydicom failed: %v", err)
	}
	if !strings.Contains(string(res), "0 differ") {
		t.Fatal("the comparison did not report that nothing differs")
	}
}
