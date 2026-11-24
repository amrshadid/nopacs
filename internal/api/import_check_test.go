package api_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// gateRoots is an import root whose file system answers only when the test
// lets it: Root tells entered it was called, then waits, deaf to its
// context as a call into a hung mount is, until release is closed, and
// answers dir.
type gateRoots struct {
	dir     string
	entered chan struct{}
	release chan struct{}
}

// Root implements dirimport.Roots.
func (g gateRoots) Root(context.Context, string, tenant.Store, string) (string, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.dir, nil
}

// TestImportJobCheckBounded guards the check of an import job against a
// file system that does not answer: the check holds no lock that a change
// of the installation's places takes, so a change of import.roots made
// while it is stalled completes; a check past its bound is answered
// UNAVAILABLE; and a receiver changed while its job was being checked
// refuses the job, UNAVAILABLE, rather than making it under rules checked
// of the receiver before the change.
func TestImportJobCheckBounded(t *testing.T) {
	f := newImportFixture(t)
	op := f.as(f.lab1Operator)
	dir, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}

	// Past its bound.
	stalled := gateRoots{dir: dir, entered: make(chan struct{}, 1), release: make(chan struct{})}
	api.SetImportCheck(f.handlers, stalled, 100*time.Millisecond)
	start := time.Now()
	wantCode(t, "a check past its bound", call(op.jobs.CreateJob, importJob("proj1", nil)), connect.CodeUnavailable, "did not answer in time")
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("a check past its bound answered after %v", took)
	}
	close(stalled.release)

	// Stalled while a setting and the receiver change.
	gate := gateRoots{dir: dir, entered: make(chan struct{}), release: make(chan struct{})}
	api.SetImportCheck(f.handlers, gate, time.Minute)
	made := make(chan outcome, 1)
	go func() { made <- call(op.jobs.CreateJob, importJob("proj1", nil)) }()
	select {
	case <-gate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the check did not reach the file system")
	}
	changed := make(chan outcome, 1)
	go func() {
		changed <- call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: "import.roots", Scope: installation(),
			Value: "src=" + f.root + "\nother=" + filepath.Join(f.dir, "other-root")}})
	}()
	select {
	case o := <-changed:
		if o.err != nil {
			t.Errorf("a change of import.roots during a stalled check: %v", o.err)
		}
	case <-time.After(10 * time.Second):
		close(gate.release)
		t.Fatal("a change of import.roots waited for a stalled check")
	}
	mustCall[*v1.SetReceiverResponse](t, "change the receiver during the check", call(f.as(local).receivers.SetReceiver, &v1.SetReceiverRequest{
		Tenant: "lab1", Receiver: &v1.Receiver{Name: "from-tree", Type: &v1.Receiver_Importer{Importer: &v1.ImportReceiver{
			PathPattern: `^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$`}}}, UpdateMask: mask("importer.path_pattern")}))
	close(gate.release)
	wantCode(t, "a receiver changed during the check", <-made, connect.CodeUnavailable, "changed while the job was checked")

	// Left alone, the same job is made.
	api.SetImportCheck(f.handlers, nil, 0)
	mustCall[*v1.CreateJobResponse](t, "the job asked again", call(op.jobs.CreateJob, importJob("proj1", nil)))
}

// TestImportJobMapFileBound guards the bound of a map file through the
// API: a map file past the 3 MiB a job keeps is refused as a
// precondition naming the bound, before the server reads it whole.
func TestImportJobMapFileBound(t *testing.T) {
	f := newImportFixture(t)
	big := filepath.Join(f.root, "proj1", "big.csv")
	if err := os.WriteFile(big, []byte(importMap), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, 64<<20); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "a 64 MiB map file", call(f.as(f.lab1Operator).jobs.CreateJob, importJob("proj1", &v1.ImportJob{MapFile: "proj1/big.csv"})),
		connect.CodeFailedPrecondition, "3 MiB")
}

// TestImportJobEndedHoldsName guards what an ended import job holds: its
// retry is answered UNIMPLEMENTED, a part of the contract not served yet,
// not refused as a precondition; and once its receiver is removed, the
// receiver's name is not given to a new receiver while the job names it
// (ALREADY_EXISTS, as for a name a reception holds).
func TestImportJobEndedHoldsName(t *testing.T) {
	f, id, _ := endedImport(t)
	wantCode(t, "the retry of an import job", call(f.as(f.lab1Operator).jobs.RetryJob, &v1.RetryJobRequest{Tenant: "lab1", Id: id}),
		connect.CodeUnimplemented, "not served yet")
	mustCall[*v1.RemoveReceiverResponse](t, "remove from-tree", call(f.as(local).receivers.RemoveReceiver,
		&v1.RemoveReceiverRequest{Tenant: "lab1", Name: "from-tree"}))
	wantCode(t, "the name of a receiver an import job names", addReceiver("lab1", importReceiverMsg("from-tree"))(f.as(local)),
		connect.CodeAlreadyExists, "import job")
	mustCall[*v1.AddReceiverResponse](t, "another name", addReceiver("lab1", importReceiverMsg("from-tree-2"))(f.as(local)))
}
