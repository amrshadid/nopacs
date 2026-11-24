package api_test

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// valuesUnkept is a tenant's store as one before the catalog kept the
// header values of a scan: each commit's values are dropped, and nothing
// fills them in, so the reception's own pass of frame tables leaves them
// for the pass the test runs.
type valuesUnkept struct{ tenant.Pipeline }

func (v valuesUnkept) CommitScan(ctx context.Context, c tenant.ScanCommit) (tenant.Scan, error) {
	c.Values = tenant.ScanValues{}
	return v.Pipeline.CommitScan(ctx, c)
}

func (valuesUnkept) FillScanValues(context.Context, string, tenant.ScanValues) error { return nil }

// TestQIDOFindsAnUpgradedArchiveOnceFilled guards an archive whose scans
// and sessions were made before the catalog kept their header values, as
// one upgraded is: QIDO-RS's StudyDate and ModalitiesInStudy filters do
// not find its study, which the pass over the scans then fills from the
// files' headers (frames.Run), after which both filters find it, a series
// search by Modality finds its series, and the scan holds the values
// pydicom reads from the file (Study Date 20010101, Modality CT, Series
// Description "Scout", Series Number 4).
func TestQIDOFindsAnUpgradedArchiveOnceFilled(t *testing.T) {
	f := newPlaceFixture(t)
	file := corpusFile(t, "ct2n-6293.dcm")
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	study, _ := h.UID(header.StudyUID)
	const date, modality = "20010101", "CT"
	a := receiveArchiveVia(t, f, [][]byte{file}, func(p tenant.Pipeline) tenant.Pipeline { return valuesUnkept{p} })
	s := serveWeb(t, f.env)
	studies := func(query string) []string {
		t.Helper()
		ans := s.get(t, http.MethodGet, webPath("studies?"+query), f.lab1Reader)
		if ans.status == http.StatusNoContent {
			return nil
		}
		var out []string
		for _, r := range results(t, ans) {
			out = append(out, value(r, "0020000D"))
		}
		return out
	}
	byDate, byModality := "StudyDate="+date, "ModalitiesInStudy="+modality
	if got := studies(byDate); slices.Contains(got, study) {
		t.Fatalf("before the fill, %s found the study: the archive was made with its values", byDate)
	}
	if got := studies(byModality); slices.Contains(got, study) {
		t.Fatalf("before the fill, %s found the study", byModality)
	}

	rep, err := frames.Run(context.Background(), frames.Config{
		Catalog:     a.p,
		OpenStorage: func(context.Context, string) (blob.Store, error) { return a.store, nil },
		Lock:        func(string, label.Path) func() { return func() {} },
		Clock:       &fakeClock{},
	})
	if err != nil || rep.ScansFilled != 1 {
		t.Fatalf("the pass: %+v, %v; want one scan filled", rep, err)
	}
	if got := studies(byDate); !slices.Equal(got, []string{study}) {
		t.Errorf("after the fill, %s found %q, want the study", byDate, got)
	}
	if got := studies(byModality); !slices.Equal(got, []string{study}) {
		t.Errorf("after the fill, %s found %q, want the study", byModality, got)
	}
	series := results(t, s.get(t, http.MethodGet, webPath("series?Modality="+modality), f.lab1Reader))
	if len(series) != 1 || value(series[0], "00080060") != modality {
		t.Errorf("after the fill, a series search by Modality answered %v", series)
	}
	scans := a.scans(t)
	if len(scans) != 1 {
		t.Fatalf("lab1 holds scans %v", scans)
	}
	sc, err := a.p.CatalogScan(context.Background(), scans[0])
	if err != nil || sc.Values != (tenant.ScanValues{Modality: "CT", SeriesDescription: "Scout", SeriesNumber: 4, HasSeriesNumber: true}) {
		t.Errorf("the scan's values after the fill: %+v, %v", sc.Values, err)
	}
}
