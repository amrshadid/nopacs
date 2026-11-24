package api_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// endedImport makes the import fixture with the worker running, a text
// file in its session folder, and an import job of proj1 waited to its
// end; it returns the fixture, the job's id and the id of a scrub job, a
// job of another kind.
func endedImport(t *testing.T) (*importFixture, string, string) {
	t.Helper()
	f := newImportFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "proj1", "sess1", "notes.txt"), []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.runImports(t)
	op := f.as(f.lab1Operator)
	id := mustCall[*v1.CreateJobResponse](t, "the import", call(op.jobs.CreateJob, importJob("proj1", nil))).GetJob().GetId()
	deadline := time.Now().Add(30 * time.Second)
	for {
		j := mustCall[*v1.GetJobResponse](t, "the import", call(op.jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id})).GetJob()
		if j.GetEndTime() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import did not end: %v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
	scrub := mustCall[*v1.CreateJobResponse](t, "a scrub", call(op.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1",
		Kind: &v1.CreateJobRequest_Scrub{Scrub: &v1.ScrubJob{}}})).GetJob().GetId()
	return f, id, scrub
}

// testListImportFiles is ListImportFiles' table: operator on the tenant,
// since it answers paths; a reader is refused; a job not there, or not an
// import, is answered as the contract says; the files come with their
// paths and reasons, and its entry records the job's id and no path.
func testListImportFiles(t *testing.T) {
	f, id, scrub := endedImport(t)
	list := func(id string, kinds []v1.ImportFileKind, size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.jobs.ListImportFiles, &v1.ListImportFilesRequest{Tenant: "lab1", Id: id, Kinds: kinds, PageSize: size})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(id, nil, 0), code: connect.CodeUnauthenticated},
		{name: "a reader", as: f.lab1Reader, call: list(id, nil, 0), code: connect.CodePermissionDenied},
		{name: "a bad id", as: f.lab1Operator, call: list("Smith^John", nil, 0), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no such job", as: f.lab1Operator, call: list("nosuchjob", nil, 0), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "not an import job", as: f.lab1Operator, call: list(scrub, nil, 0), code: connect.CodeFailedPrecondition},
		{name: "an unspecified kind", as: f.lab1Operator, call: list(id, []v1.ImportFileKind{0}, 0), code: connect.CodeInvalidArgument, msg: "kinds"},
		{name: "a negative page size", as: f.lab1Operator, call: list(id, nil, -1), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "the files", as: f.lab1Operator, call: list(id, []v1.ImportFileKind{v1.ImportFileKind_IMPORT_FILE_KIND_SKIPPED}, 0),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: id},
			check: func(t *testing.T, o outcome) {
				fs := o.res.(*v1.ListImportFilesResponse).GetFiles()
				got := map[string]v1.ImportSkipCode{}
				for _, x := range fs {
					got[x.GetPath()] = x.GetSkipCode()
				}
				if len(fs) != 3 || got["proj1/afile"] != v1.ImportSkipCode_IMPORT_SKIP_CODE_OUTSIDE_LAYOUT || got["proj1/map.csv"] != v1.ImportSkipCode_IMPORT_SKIP_CODE_OUTSIDE_LAYOUT ||
					got["proj1/sess1/notes.txt"] != v1.ImportSkipCode_IMPORT_SKIP_CODE_NOT_PART10 {
					t.Errorf("the files: %v", fs)
				}
			}},
	})
}

// testListImportUnits is ListImportUnits' table, as ListImportFiles'.
func testListImportUnits(t *testing.T) {
	f, id, scrub := endedImport(t)
	list := func(id string, states []v1.ImportUnitState) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.jobs.ListImportUnits, &v1.ListImportUnitsRequest{Tenant: "lab1", Id: id, States: states})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(id, nil), code: connect.CodeUnauthenticated},
		{name: "a reader", as: f.lab1Reader, call: list(id, nil), code: connect.CodePermissionDenied},
		{name: "no such job", as: f.lab1Operator, call: list("nosuchjob", nil), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "not an import job", as: f.lab1Operator, call: list(scrub, nil), code: connect.CodeFailedPrecondition},
		{name: "an unspecified state", as: f.lab1Operator, call: list(id, []v1.ImportUnitState{0}), code: connect.CodeInvalidArgument, msg: "states"},
		{name: "the units", as: f.lab1Operator, call: list(id, nil), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: id},
			check: func(t *testing.T, o outcome) {
				us := o.res.(*v1.ListImportUnitsResponse).GetUnits()
				if len(us) == 0 || us[0].GetFolder() != "proj1" || us[0].GetCounts().GetUnits() != 1 {
					t.Errorf("the units: %v", us)
				}
				var found int64
				for _, u := range us {
					found += u.GetCounts().GetFound()
				}
				if found != 3 {
					t.Errorf("the units found %d entries, want 3", found)
				}
			}},
	})
}
