package api_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAnExportWhoseSummaryMayHaveGoneOutIsGivenBackPastIt guards an export
// whose summary's Send fails after its data was streamed: the summary, and
// its transfer key, may have reached the client, so before the tenant is
// ready again its owner record is taken one generation past the one the
// export holds. An import of that export then meets a record at a later
// generation, naming this installation, and is refused as one older than a
// reclaim (the import's own check: a record past the generation the export
// holds).
func TestAnExportWhoseSummaryMayHaveGoneOutIsGivenBackPastIt(t *testing.T) {
	f := newPlaceFixture(t)
	if o := call(f.as(local).storages.AddStorage, &v1.AddStorageRequest{Tenant: "lab1", Storage: dirStorage("main", "fast", "main")}); o.err != nil {
		t.Fatal(o.err)
	}
	f.handoffs.mu.Lock()
	f.handoffs.owners = &backup.OwnerCheck{Installation: testInstallation, Keys: f.keys,
		Opener: place.Opener{Values: settingvalue.Reader{Master: f.master}, Keys: f.keys, StaleUploadAge: -1}}
	f.handoffs.mu.Unlock()
	api.SetSummarySend(f.handlers, fault.New(fault.Unavailable, "the connection broke"))

	x := export(f.as(f.instAdmin), "lab1")
	if x.code() != connect.CodeUnavailable || x.summary != nil || len(x.data) == 0 {
		t.Fatalf("an export whose summary could not be sent: %v, summary %v, %d bytes of data", x.err, x.summary != nil, len(x.data))
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_READY {
		t.Fatalf("after the export the tenant is %v", s)
	}
	f.handoffs.mu.Lock()
	gens := slices.Clone(f.handoffs.generations)
	f.handoffs.mu.Unlock()
	want := [][]backup.Owner{{{Storage: "main", Generation: 1}}, {{Storage: "main", Generation: 2}}}
	if len(gens) != 2 || !slices.Equal(gens[0], want[0]) || !slices.Equal(gens[1], want[1]) {
		t.Fatalf("the owner records were taken to %v, want the export's 1 and then 2", gens)
	}
	held := 1 // the generation the export holds, written before its snapshot

	rc, err := os.Open(filepath.Join(f.root, "lab1", "main", filepath.FromSlash(layout.OwnerKey))) // #nosec G304 -- the test's own storage.
	if err != nil {
		t.Fatal(err)
	}
	o, err := record.DecodeOwner(rc)
	_ = rc.Close()
	if err != nil || o.Installation != testInstallation || record.VerifyOwner(o, f.keys) != nil {
		t.Fatalf("the owner record after the give-back: %+v %v", o, err)
	}
	if o.Generation <= int64(held) {
		t.Fatalf("the owner record is at generation %d, not past the %d the export holds: its import would be taken", o.Generation, held)
	}
	st, err := f.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	db, err := st.(tenant.Restoring).StorageOwners(ctx)
	if err != nil || len(db) != 1 || db[0].Generation != o.Generation {
		t.Fatalf("the database holds %+v (%v), the record %d", db, err, o.Generation)
	}
}
