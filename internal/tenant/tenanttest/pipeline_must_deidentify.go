package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testMustDeidentifyRows guards that a received file keeps what its source
// required of it (ReceivedFile.MustDeidentify) beside how it was recorded
// (Deidentified), so the archive step can hold a file its receiver no longer
// de-identifies: every combination of the two flags is written and read
// back as it was given, a batch written again changes nothing, and the files
// of a retry of a failed reception keep both.
func testMustDeidentifyRows(e *penv) {
	e.addStorage("store")
	e.addReceiver("recv", "store", 11112)
	r := e.openReception("recv", 1, at(1))
	flags := []struct{ deid, must bool }{{false, false}, {true, false}, {false, true}, {true, true}}
	var files []tenant.ReceivedFile
	for i, c := range flags {
		f := file(r, 1, i+1)
		f.Deidentified, f.MustDeidentify = c.deid, c.must
		files = append(files, f)
	}
	for range 2 {
		_, err := e.p.AddReceivedFiles(e.ctx, files, at(2), nil)
		e.ok("AddReceivedFiles", err)
		got := allPages(e, 10, func(p master.Page) ([]tenant.ReceivedFile, string, error) {
			return e.p.ReceivedFiles(e.ctx, r.ID, p)
		})
		if len(got) != len(files) {
			e.Fatalf("%d files read back, want %d", len(got), len(files))
		}
		for _, g := range got {
			for i, f := range files {
				if g.SOPInstanceUID == f.SOPInstanceUID && (g.Deidentified != flags[i].deid || g.MustDeidentify != flags[i].must) {
					e.Errorf("file %d reads back de-identified %v, must %v; written %v, %v", i+1, g.Deidentified, g.MustDeidentify,
						flags[i].deid, flags[i].must)
				}
			}
		}
	}

	// A failed reception's files become its retry's, flags with them.
	r = e.stepTo(r, reception.Identified, "store")
	e.failReception(r)
	_, retry, err := e.p.RetryReception(e.ctx, r.ID, e.id("rr"), "", at(20), e.audit(), nil)
	e.ok("RetryReception", err)
	copied := allPages(e, 10, func(p master.Page) ([]tenant.ReceivedFile, string, error) {
		return e.p.ReceivedFiles(e.ctx, retry.ID, p)
	})
	if len(copied) != len(files) {
		e.Fatalf("the retry holds %d files, want %d", len(copied), len(files))
	}
	for _, g := range copied {
		for i, f := range files {
			if g.SOPInstanceUID == f.SOPInstanceUID && (g.Deidentified != flags[i].deid || g.MustDeidentify != flags[i].must) {
				e.Errorf("file %d of the retry is de-identified %v, must %v; want %v, %v", i+1, g.Deidentified, g.MustDeidentify,
					flags[i].deid, flags[i].must)
			}
		}
	}
}
