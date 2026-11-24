package frames

import (
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPassFillsTheValuesOfScansListedWithout guards the pass's fill of the
// catalog's header values: a scan the catalog lists without a modality,
// whether its sidecar is of version 1 or already of this version, is given
// the values the header of its sidecar's first line's file holds (as
// pydicom reads them: SC_rgb_rle_2frame.dcm Study Date 20170101, Modality
// OT, Series Number 1; MR_small.dcm 20040826, MR, 1, no description); a
// scan listed with its values is not asked for and its file not read; a
// scan whose file is missing is left as it is without failing the pass;
// and a pass run again asks nothing of the scans filled.
func TestPassFillsTheValuesOfScansListedWithout(t *testing.T) {
	a := newArchive(t)
	scanD := label.Path{Subject: "S3", Session: "E1", Scan: "D"}
	a.scan(scanA, true, filesA...)
	a.unvalued(scanA)
	a.scan(scanB, false, file{name: "MR_small.dcm", uid: "1.2.4.1"})
	a.unvalued(scanB)
	a.scan(scanC, false, file{name: "MR_small.dcm", uid: "1.2.6.1"})
	a.scan(scanD, false, file{name: "MR_small.dcm", uid: "1.2.7.1", missing: true})
	a.unvalued(scanD)

	rep, err := Run(a.ctx, a.config())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ScansFilled != 2 || rep.ScansDone != 1 {
		t.Errorf("report %+v, want two scans filled and the one of version 1 given its tables", rep)
	}
	want := []fillAsked{
		{series: seriesUIDOf(0), values: tenant.ScanValues{StudyDate: "20170101", Modality: "OT", SeriesNumber: 1, HasSeriesNumber: true}},
		{series: seriesUIDOf(1), values: tenant.ScanValues{StudyDate: "20040826", Modality: "MR", SeriesNumber: 1, HasSeriesNumber: true}},
	}
	if !slices.Equal(a.cat.fills, want) {
		t.Errorf("fills asked %+v, want %+v", a.cat.fills, want)
	}
	key, _ := layout.InstanceKey(scanC, "1.2.6.1")
	if n := len(a.store.reads[key]); n != 0 {
		t.Errorf("the file of a scan listed with its values was read %d times", n)
	}

	a.cat.fills = nil
	rep, err = Run(a.ctx, a.config())
	if err != nil || rep.ScansFilled != 0 || len(a.cat.fills) != 0 {
		t.Errorf("a pass run again: %+v, %v, fills %+v", rep, err, a.cat.fills)
	}
}
