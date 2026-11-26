package servecmd

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestNoPurgeBeforeTheHoldsFoundOnStorageAreAdopted guards the corrections'
// purge pass as serve wires it after a start: a subject deleted, its
// delete committed and past its grace, whose hold exists only as a record
// on the storage (a database lost and made again lacks its row). While
// the storages' records cannot be read, passes run and purge nothing; once
// they are read, the hold is adopted before any purge, and the delete
// waits as held, its data still on the storage.
func TestNoPurgeBeforeTheHoldsFoundOnStorageAreAdopted(t *testing.T) {
	r := newWorkRig(t)
	subject := archivedSubject(t, r)
	db := &activatedDB{tenantDB: r.db}
	r.w.tenants = oneTenantStore{st: db}
	storages := newTenantStorages(r.w.opener, oneTenant{}, "lab", r.db)
	t.Cleanup(func() { _ = storages.Close() })
	var later time.Duration
	now := func() time.Time { return time.Now().Add(later) }
	es, err := correct.NewEngines(correct.EnginesConfig{
		Tenants:  oneTenantStore{st: db},
		Interval: 20 * time.Millisecond,
		Configure: func(context.Context, string, tenant.Corrections) (correct.Config, func() error, error) {
			return correct.Config{TenantID: "T0000000000001", Keys: r.keys, OpenStorage: storages.Open, Now: now,
				Grace:      func(context.Context) (time.Duration, error) { return time.Hour, nil },
				ConfirmKey: r.w.confirmKey}, nil, nil
		},
		HoldsAdopted: (&purgeAdoptions{work: r.w}).check,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	e, release, err := es.Use(r.ctx, "lab", db)
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.CorrectionTarget{Subject: subject}
	pv, err := e.Preview(r.ctx, target)
	if err == nil {
		var c tenant.Correction
		c, err = e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: pv.Confirm, Audit: r.audit()})
		target.Subject = c.ID
	}
	release()
	if err != nil {
		t.Fatal(err)
	}
	id := target.Subject
	before := r.files("main", "data/")
	lost := putLostHoldRecord(t, r, "main", subject)
	db.fail.Store(true)
	later = 2 * time.Hour
	if err := es.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	state := func() tenant.Correction {
		c, err := r.db.Correction(r.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	time.Sleep(300 * time.Millisecond)
	if c := state(); c.State != record.StateCommitted {
		t.Fatalf("while the storages' records cannot be read the delete past its grace is %s (%q); want committed", c.State, c.StateReason)
	}
	if _, err := r.db.Hold(r.ctx, lost); err == nil {
		t.Fatal("the hold was adopted though the storages' records could not be read")
	}
	db.fail.Store(false)
	r.waitFor("the hold found on storage to be adopted", time.Minute, func() bool {
		_, err := r.db.Hold(r.ctx, lost)
		return err == nil
	})
	time.Sleep(300 * time.Millisecond)
	c := state()
	t.Logf("after the adoption the delete is %s (%q)", c.State, c.StateReason)
	if c.State == record.StatePurged || c.StateReason != tenant.StateReasonHeld {
		t.Fatalf("after the hold's adoption the delete is %s (%q); want it waiting, held", c.State, c.StateReason)
	}
	if after := r.files("main", "data/"); len(after) != len(before) || len(before) == 0 {
		t.Fatalf("the storage holds %d objects of the subject after the passes, want the %d it held", len(after), len(before))
	}
}
