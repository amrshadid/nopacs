package servecmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// workStudy is the public CT study of the pipeline's test files: one study,
// two series, seven instances; workLate are three more of it, sent later.
var (
	workStudy = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm"}
	workLate  = []string{"ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}
)

// TestAReceivedStudyIsCopiedToTheReplica guards the replicator as the
// server wires it per tenant: a study received by the real pipeline,
// through the switch gate, is copied to the primary's replica by the
// tenant's background work: the replica holds every object of the primary under data/ with the same
// bytes, and every scan's manifest, read straight from the disk.
func TestAReceivedStudyIsCopiedToTheReplica(t *testing.T) {
	r := newWorkRig(t)
	r.start()
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	r.waitFor("every scan to be in step on the replica", time.Minute, r.inStep)
	for _, prefix := range []string{"data/", "meta/sub-"} {
		main, cp := r.files("main", prefix), r.files("copy", prefix)
		if len(main) == 0 || len(main) != len(cp) {
			t.Fatalf("under %s the primary holds %v and the replica %v", prefix, sortedKeys(main), sortedKeys(cp))
		}
		for k, sum := range main {
			if cp[k] != sum {
				t.Fatalf("the replica's %s is %q, the primary's %q", k, cp[k], sum)
			}
		}
	}
	if n := len(r.catalogKeys()); n != len(workStudy) {
		t.Fatalf("the catalog holds %d instances, want %d", n, len(workStudy))
	}
}

// TestASwitchAcrossARealReception guards the switch as the server wires it,
// across a reception of the real pipeline: the switch is held at its owner
// step by the tenant's owner lock (as an export's advance of the owner
// records holds it), its gate paused; a reception that comes then writes
// nothing to the old place while the switch runs, every correction step is
// refused as switching, and a purge's removal from the primary is held. Once the lock is let go the switch swaps, and the
// reception is archived on the new primary: every instance the catalog holds
// reads whole from the place the primary's name names now, and the old
// place holds none that is not also there.
func TestASwitchAcrossARealReception(t *testing.T) {
	r := newWorkRig(t)
	r.start()
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.waitFor("the first reception to be archived", time.Minute, func() bool { return r.archived(1) })
	r.waitFor("every scan to be in step on the replica", time.Minute, r.inStep)

	sw, err := r.w.switcherFor(r.ctx, "lab", r.db)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := sw.Validate(r.ctx, "main", "copy", true)
	if err != nil {
		t.Fatal(err)
	}
	release, err := r.w.owners.lock(r.ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s, err := sw.Begin(r.ctx, storageswitch.Request{Primary: "main", Replica: "copy", Confirm: pv.Confirm, SkipVerification: true,
		SkipReason: "the test checks the copy itself", DrainTimeout: 2 * time.Minute, Audit: r.audit()})
	if err != nil {
		t.Fatalf("the switch was not accepted: %v (blockers %v)", err, pv.Blockers)
	}
	r.waitFor("the switch to reach its owner step", 30*time.Second, func() bool {
		got, err := r.db.Switch(r.ctx, s.ID)
		return err == nil && got.Step == tenant.SwitchOwning
	})
	fence, pause, err := r.w.correctionWrites(r.ctx, "lab", r.db, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := fence(r.ctx); !errors.Is(err, storageswitch.ErrStorageSwitching) || fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a correction step while the switch runs: %v, want the switching refusal", err)
	}
	pctx, pcancel := context.WithTimeout(r.ctx, 200*time.Millisecond)
	if rel, err := pause.Enter(pctx, "main"); err == nil {
		rel()
		t.Fatal("a purge's removal from the primary was admitted while the switch ran")
	}
	pcancel()
	before := r.files("main", "data/")
	r.accept(p, workLate...)
	// Long enough at the pipeline's quick clock for the reception's quiet
	// period to pass and its archive step to come to the gate, short of the
	// gate's own wait (30 seconds).
	time.Sleep(3 * time.Second)
	if during := r.files("main", "data/"); len(during) != len(before) {
		t.Fatalf("while the switch ran, the old place got %d objects more", len(during)-len(before))
	}
	release()
	done, err := sw.Wait(r.ctx, s.ID)
	if err != nil || done.State != tenant.SwitchDone {
		t.Fatalf("the switch ended %+v (%v)", done, err)
	}
	r.waitFor("the second reception to be archived", time.Minute, func() bool { return r.archived(2) })
	row, err := r.db.Storage(r.ctx, "main")
	if err != nil || row.Directory.Path != "copy" {
		t.Fatalf("after the switch the primary names %+v (%v), want the place copy", row.Directory, err)
	}
	keys := r.catalogKeys()
	if len(keys) != len(workStudy)+len(workLate) {
		t.Fatalf("the catalog holds %d instances, want %d", len(keys), len(workStudy)+len(workLate))
	}
	onNew := r.files("copy", "data/")
	for k, in := range keys {
		sum := r.readWhole("main", k)
		if sum != in.ArchivedSHA256.String() && sum != in.SHA256.String() {
			t.Fatalf("%s reads on the new primary with another checksum than the catalog's", k)
		}
	}
	for k := range r.files("main", "data/") {
		if _, ok := onNew[k]; !ok {
			t.Fatalf("%s is on the old place only", k)
		}
	}
}

// TestRepairLeaseRefusedWhenTooShort guards checkRepairLease and the repair
// worker's use of it: a lease whose put window does not carry the largest
// file a receiver of the tenant accepts at 100 MiB a second is refused, and
// the worker makes no runner (so claims nothing) while replica.repair-lease
// holds it; the default minute is taken for a receiver of 4 GiB.
func TestRepairLeaseRefusedWhenTooShort(t *testing.T) {
	r := newWorkRig(t)
	if _, err := r.db.AddReceiver(r.ctx, tenant.Receiver{Name: "big", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 32,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "BIG", AnyCaller: true}}, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	err := checkRepairLease(r.ctx, r.db, 30*time.Second)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "replica.repair-lease to at least 56s") {
		t.Fatalf("a lease of 30s for files of 4 GiB: %v", err)
	}
	if err := checkRepairLease(r.ctx, r.db, time.Minute); err != nil {
		t.Fatalf("the default lease for files of 4 GiB: %v", err)
	}
	if err := r.db.SetSetting(r.ctx, "replica.repair-lease", "30s", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	rep := &tenantRepairs{w: r.w, name: "lab", st: r.db, log: r.w.log, wake: make(chan struct{}, 1),
		open: func(context.Context, string) (blob.Store, error) { return nil, errors.New("not opened") }}
	if run, err := rep.runner(r.ctx); err == nil || run != nil {
		t.Fatalf("the worker made a runner with a lease too short: %v", err)
	}
	if err := r.db.SetSetting(r.ctx, "replica.repair-lease", "2m", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rep.runner(r.ctx); err != nil {
		t.Fatalf("a lease long enough: %v", err)
	}
}

// TestReplacingASwitcherDoesNotWaitOnItsOwnRecordsOwed guards switcherFor
// when the tenant's database is opened anew while a switch drains: the
// switch's run is inside WriteRecordsOwed, making the tenant's corrections
// engine, whose configuration asks switcherFor (correctionWrites). The old
// switcher is stopped with the create lock let go, so the run gets its
// answer, sees the stop and ends, and the new switcher is made at once
// rather than after the stop's deadline.
func TestReplacingASwitcherDoesNotWaitOnItsOwnRecordsOwed(t *testing.T) {
	r := newWorkRig(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	storages := newTenantStorages(r.w.opener, oneTenant{}, "lab", r.db)
	t.Cleanup(func() { _ = storages.Close() })
	es, err := correct.NewEngines(correct.EnginesConfig{
		Tenants: oneTenantStore{st: r.db},
		Configure: func(ctx context.Context, name string, st tenant.Corrections) (correct.Config, func() error, error) {
			once.Do(func() {
				close(entered)
				<-proceed
			})
			db, err := dbOf(st)
			if err != nil {
				return correct.Config{}, nil, err
			}
			fence, pause, err := r.w.correctionWrites(ctx, name, db, func() error { return nil })
			if err != nil {
				return correct.Config{}, nil, err
			}
			return correct.Config{TenantID: "T0000000000001", Keys: r.keys, OpenStorage: storages.Open, Now: r.w.now, Fence: fence, Pause: pause,
				Grace: func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil }, ConfirmKey: r.w.confirmKey}, nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	r.w.setEngines(es)
	sw, err := r.w.switcherFor(r.ctx, "lab", r.db)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := sw.Validate(r.ctx, "main", "copy", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Begin(r.ctx, storageswitch.Request{Primary: "main", Replica: "copy", Confirm: pv.Confirm, SkipVerification: true,
		SkipReason: "the test checks the stop only", DrainTimeout: 2 * time.Minute, Audit: r.audit()}); err != nil {
		t.Fatalf("the switch was not accepted: %v (blockers %v)", err, pv.Blockers)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the switch's drain did not ask for the records owed")
	}
	again, err := (driver.SQLiteTenants{DataDir: r.data}).Open(r.ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	db2, err := dbOf(again)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := r.w.switcherFor(r.ctx, "lab", db2)
		done <- err
	}()
	// The replacement reaches the stop of the old switcher, whose run waits
	// in the engine's configuration; then the configuration goes on.
	time.Sleep(200 * time.Millisecond)
	close(proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the switcher over the database opened anew: %v", err)
		}
	case <-time.After(tenantWorkStop + 10*time.Second):
		t.Fatal("the switcher over the database opened anew was never made")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the switcher over the database opened anew took %v: the stop waited on its own run", took)
	}
}

// TestTheOwnerStepHoldsTheOwnerLock guards ownerSerialDB and
// ownerSerialPlace: a switch's read of the owner generations takes the
// tenant's owner lock, which an advance of the owner records then waits
// for, until the replica's place is closed; an open of the place that fails
// lets the lock go at once.
func TestTheOwnerStepHoldsTheOwnerLock(t *testing.T) {
	r := newWorkRig(t)
	held := &heldOwnerLock{locks: r.w.owners, name: "lab"}
	db := ownerSerialDB{DB: r.db, held: held}
	fails := errors.New("the place does not answer")
	failing := ownerSerialPlace(func(context.Context, tenant.Storage) (blob.Store, error) { return nil, fails }, held)
	opening := ownerSerialPlace(func(ctx context.Context, row tenant.Storage) (blob.Store, error) {
		return r.w.opener.OpenBeside(ctx, place0(r), row)
	}, held)
	free := func() bool {
		ctx, cancel := context.WithTimeout(r.ctx, 100*time.Millisecond)
		defer cancel()
		release, err := r.w.owners.lock(ctx, "lab")
		if err != nil {
			return false
		}
		release()
		return true
	}
	if _, err := db.StorageOwners(r.ctx); err != nil {
		t.Fatal(err)
	}
	if free() {
		t.Fatal("an advance took the owner lock while a switch's owner step held it")
	}
	if _, err := failing(r.ctx, tenant.Storage{}); !errors.Is(err, fails) || !free() {
		t.Fatalf("a failed open of the place kept the lock (%v)", err)
	}
	if _, err := db.StorageOwners(r.ctx); err != nil {
		t.Fatal(err)
	}
	row, err := r.db.Storage(r.ctx, "copy")
	if err != nil {
		t.Fatal(err)
	}
	bs, err := opening(r.ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if free() {
		t.Fatal("the lock was let go before the place was closed")
	}
	if err := bs.Close(); err != nil || !free() {
		t.Fatalf("closing the place kept the lock (%v)", err)
	}
}
