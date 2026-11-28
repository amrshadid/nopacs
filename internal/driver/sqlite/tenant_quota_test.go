package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestQuotaUseSurvivesARestartAndARebuildRecountsIt guards where a
// tenant's use against its quota lives: in its database, so a store opened
// again on the file (a restart) reads what the last one counted; and a
// rebuild that replaces the catalog counts the corrections' unpurged files
// again, so a total that went wrong is made right by one.
func TestQuotaUseSurvivesARestartAndARebuildRecountsIt(t *testing.T) {
	ctx := context.Background()
	ts, path := rebuildTenant(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE purge_totals SET instances = 3, bytes = 300`); err != nil {
		t.Fatal(err)
	}
	again, err := OpenTenant(ctx, path, "T0000000000001")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if u, err := again.QuotaUse(ctx); err != nil || u != (tenant.QuotaUse{PurgeInstances: 3, PurgeBytes: 300}) {
		t.Fatalf("the store opened again reads the use %+v, %v, want what the database holds", u, err)
	}
	now := time.Now()
	audit := master.AuditEntry{ID: "aq1", Time: now, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
		TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
	if _, err := ts.CreateJob(ctx, tenant.NewJob{ID: "rb-q", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildReplace},
		Created: now}, audit, nil); err != nil {
		t.Fatal(err)
	}
	w, err := ts.ClaimWork(ctx, tenant.RebuildWorkKind, "w", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.StartRebuildUnit(ctx, w.Fence(), now); err != nil {
		t.Fatal(err)
	}
	prog, err := ts.PrepareShadow(ctx, w.Fence())
	if err != nil {
		t.Fatal(err)
	}
	if prog, err = ts.FinishRebuildStorage(ctx, w.Fence(), prog.Cursor.Storage, "", nil); err != nil || !prog.Complete {
		t.Fatalf("the empty build ends %+v, %v", prog, err)
	}
	if _, err := ts.ActivateCatalog(ctx, w.Fence(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if u, err := ts.QuotaUse(ctx); err != nil || u != (tenant.QuotaUse{}) {
		t.Errorf("after the rebuild the use is %+v, %v, want it counted again from what the tenant holds: nothing", u, err)
	}
}
