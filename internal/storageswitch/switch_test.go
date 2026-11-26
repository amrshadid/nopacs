package storageswitch

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestSwitchSwapsAndKeepsTheCatalog guards a switch of a primary to a
// replica in step: it swaps the two places under their names, changing no
// catalog row; every instance then reads from the place the primary's name
// names, byte for byte as archived; the old place is the replica; the
// replica's place holds an owner record this installation wrote, at the
// generation the database now holds for the primary's name; and the
// switch ends done with its counts.
func TestSwitchSwapsAndKeepsTheCatalog(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 3)
	f.archive(2, "scan-2", 2)
	f.attach()
	f.replicate()
	f.inStep()
	before := f.catalog()
	s := f.switcher(f.config())
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if err != nil {
		t.Fatal(err)
	}
	if sw.State != tenant.SwitchAccepted {
		t.Fatalf("a switch begun reads %+v", sw)
	}
	done := f.wait(s, sw.ID)
	if done.State != tenant.SwitchDone || done.Refusal != "" || done.Swapped.IsZero() {
		t.Fatalf("the switch ended as %+v; want done", done)
	}
	after := f.catalog()
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the catalog changed with the swap:\nbefore %+v\nafter  %+v", before, after)
	}
	primary, err := f.p.Storage(f.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	replica, err := f.p.Storage(f.ctx, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if primary.Directory.Path != "copy" || replica.Directory.Path != "main" || replica.ReplicaOf != "main" || primary.SwitchGeneration != 1 {
		t.Fatalf("after the switch the primary is %+v and the replica %+v", primary, replica)
	}
	f.wantServed(after)
	owners, err := f.p.StorageOwners(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gen int64
	for _, o := range owners {
		if o.Storage == "main" {
			gen = o.Generation
		}
	}
	rc, err := f.place("copy").Read(f.ctx, layout.OwnerKey, 0, -1)
	if err != nil {
		t.Fatalf("the replica's former place holds no owner record: %v", err)
	}
	o, err := record.DecodeOwner(rc)
	_ = rc.Close()
	if err != nil || o.Installation != testInstallation || o.Tenant != testTenantID || o.Generation != gen || gen != 1 ||
		record.VerifyOwner(o, f.keys) != nil {
		t.Errorf("the owner record of the new primary's place is %+v (%v); the database holds generation %d", o, err, gen)
	}
	if again, err := s.Begin(f.ctx, f.request(sw.Token)); err != nil || again.ID != sw.ID {
		t.Errorf("the token sent again answered %+v, %v; want the switch it began", again, err)
	}
}

// TestWritesPauseDuringTheSwitch guards the pause: once a switch is
// accepted, a write to its primary is refused UNAVAILABLE with the reason
// STORAGE_SWITCHING (ErrStorageSwitching) and counted, a write to another
// storage is not; after the swap a write is admitted again.
func TestWritesPauseDuringTheSwitch(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.attach()
	// No replicator yet: the drain waits, and the pause holds meanwhile.
	s := f.switcher(f.config())
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := s.Gate().Enter(f.ctx, "main")
		if err != nil {
			if !errors.Is(err, ErrStorageSwitching) || fault.KindOf(err) != fault.Unavailable ||
				!bytes.HasPrefix([]byte(fault.Message(err)), []byte(ReasonStorageSwitching+": ")) {
				t.Fatalf("a write during the switch was refused with %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a write to the primary was admitted while the switch ran")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release, err := s.Gate().Enter(f.ctx, "copy")
	if err != nil {
		t.Fatalf("a write to another storage was refused during the switch: %v", err)
	}
	release()
	f.replicate()
	done := f.wait(s, sw.ID)
	if done.State != tenant.SwitchDone || done.AssociationsRefused < 1 {
		t.Fatalf("the switch ended as %+v; want done with the refused write counted", done)
	}
	release, err = s.Gate().Enter(f.ctx, "main")
	if err != nil {
		t.Fatalf("a write after the switch was refused: %v", err)
	}
	release()
}

// TestWriteInFlightFinishesBeforeTheSwap guards a write admitted before the
// switch began: the switch waits for it to release before it drains, so the
// scan it commits is copied to the replica and served from the new place
// after the swap, never left on the old place alone.
func TestWriteInFlightFinishesBeforeTheSwap(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.attach()
	f.replicate()
	f.inStep()
	s := f.switcher(f.config())
	token := f.preview(s, true).Confirm
	release, err := s.Gate().Enter(f.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	commit := f.stage(2, "scan-2", 3)
	sw, err := s.Begin(f.ctx, f.request(token))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchAccepted || got.Step != tenant.SwitchPausing {
		t.Fatalf("with a write in flight the switch reads %+v, %v; want it waiting at its pause", got, err)
	}
	commit()
	release()
	done := f.wait(s, sw.ID)
	if done.State != tenant.SwitchDone || done.ScansDrained != 1 {
		t.Fatalf("the switch ended as %+v; want done with the late scan drained", done)
	}
	f.wantServed(f.catalog())
}

// TestSwapWaitsForACopyInFlight guards a write admitted to the replica
// before the swap (a copy's put still streaming, its scan in step already
// by another copy): the switch, every scan in step, does not swap while
// that write holds the replica's gate, since the write's place would then
// be named by the primary; it swaps once the write released.
func TestSwapWaitsForACopyInFlight(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.attach()
	f.replicate()
	f.inStep()
	s := f.switcher(f.config())
	token := f.preview(s, true).Confirm
	release, err := s.Gate().Enter(f.ctx, "copy")
	if err != nil {
		t.Fatal(err)
	}
	sw, err := s.Begin(f.ctx, f.request(token))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchAccepted {
		t.Fatalf("with a copy's put in flight to the replica the switch reads %+v, %v; want it accepted, not swapped", got, err)
	}
	release()
	if done := f.wait(s, sw.ID); done.State != tenant.SwitchDone {
		t.Fatalf("the switch once the copy's put released ended as %+v; want done", done)
	}
	f.wantServed(f.catalog())
}

// TestWriteDuringTheDrainLandsOnTheNewPlace guards a write that comes while
// the switch drains: it waits at the gate and is admitted after the swap,
// on the place the primary's name names then, or is refused; never archived
// on the old place alone.
func TestWriteDuringTheDrainLandsOnTheNewPlace(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.attach()
	cfg := f.config()
	cfg.GateWait = 20 * time.Second
	s := f.switcher(cfg)
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan error, 1)
	go func() {
		release, err := s.Gate().Enter(f.ctx, "main")
		if err == nil {
			defer release()
		}
		entered <- err
	}()
	select {
	case err := <-entered:
		t.Fatalf("a write during the drain did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	f.replicate()
	if err := <-entered; err != nil {
		t.Fatalf("a write that waited through the switch was refused: %v", err)
	}
	if done := f.wait(s, sw.ID); done.State != tenant.SwitchDone {
		t.Fatalf("the switch ended as %+v", done)
	}
	f.archive(2, "scan-2", 2)
	key := f.catalog()[len(f.catalog())-1].Key
	if _, err := f.place("copy").Stat(f.ctx, key); err != nil {
		t.Fatalf("a scan archived after the swap is not on the primary's new place: %v", err)
	}
	f.inStep()
	if _, err := f.place("main").Stat(f.ctx, key); err != nil {
		t.Fatalf("a scan archived after the swap was not copied to the replica's place: %v", err)
	}
}

// TestDrainPastItsBoundRefuses guards the drain's bound: with the replica
// not in step and no replicator, the switch is refused with the drain's
// phrase once drain_timeout passes, nothing is swapped, and writes resume
// on the old place.
func TestDrainPastItsBoundRefuses(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.attach()
	s := f.switcher(f.config())
	r := f.request(f.preview(s, true).Confirm)
	r.DrainTimeout = time.Second
	sw, err := s.Begin(f.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(s, sw.ID)
	if done.State != tenant.SwitchRefused || done.Refusal != tenant.RefusalNotInStep {
		t.Fatalf("a drain past its bound ended as %+v; want refused %q", done, tenant.RefusalNotInStep)
	}
	if primary, _ := f.p.Storage(f.ctx, "main"); primary.Directory.Path != "main" || primary.SwitchGeneration != 0 {
		t.Fatalf("a refused switch changed the primary: %+v", primary)
	}
	release, err := s.Gate().Enter(f.ctx, "main")
	if err != nil {
		t.Fatalf("a write after the refusal was refused: %v", err)
	}
	release()
}

// TestForeignOwnerRecordRefuses guards the owner step: a replica's place
// whose owner record names another installation is not taken over; the
// switch is refused with the owner phrase and writes resume.
func TestForeignOwnerRecordRefuses(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	f.replicate()
	f.inStep()
	foreign, err := record.EncodeOwner(record.Owner{Format: record.Format, Installation: "someone-else", Tenant: testTenantID, Generation: 1,
		Written: time.Now().UTC()}, f.keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.place("copy").Put(f.ctx, layout.OwnerKey, bytes.NewReader(foreign)); err != nil {
		t.Fatal(err)
	}
	s := f.switcher(f.config())
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(s, sw.ID); done.State != tenant.SwitchRefused || done.Refusal != tenant.RefusalOwnerNotTaken {
		t.Fatalf("a switch onto a place another installation owns ended as %+v", done)
	}
	release, err := s.Gate().Enter(f.ctx, "main")
	if err != nil {
		t.Fatalf("a write after the refusal was refused: %v", err)
	}
	release()
}

// TestRestartRefusesAcceptedAndFinishesSwapped guards a start after a stop
// mid-switch: before the start's recovery the gate holds the primary's
// writes from the accepted row alone; a new Switcher over the same database
// refuses the accepted switch with "server restarted", after which writes
// resume; and finishes one whose swap committed.
func TestRestartRefusesAcceptedAndFinishesSwapped(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	f.replicate()
	f.inStep()
	sw, _, err := f.p.BeginSwitch(f.ctx, tenant.SwitchStart{ID: "sw-left", Primary: "main", Replica: "copy", Token: "tok-left",
		SkipVerification: true, SkipReason: "checked by hand", Now: time.Now().UTC()}, f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Gate().Enter(f.ctx, "main"); !errors.Is(err, ErrStorageSwitching) {
		t.Fatalf("before the start's recovery a write to a switch's primary answered %v; want it paused", err)
	}
	if err := s.Recover(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchRefused || got.Refusal != tenant.RefusalRestarted {
		t.Fatalf("an accepted switch after a restart reads %+v, %v", got, err)
	}
	release, err := s.Gate().Enter(f.ctx, "main")
	if err != nil {
		t.Fatalf("a write after the restart's refusal was refused: %v", err)
	}
	release()
	sw, _, err = f.p.BeginSwitch(f.ctx, tenant.SwitchStart{ID: "sw-swapped", Primary: "main", Replica: "copy", Token: "tok-swapped",
		SkipVerification: true, SkipReason: "checked by hand", Now: time.Now().UTC()}, f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.p.SwapStorages(f.ctx, tenant.SwitchSwap{ID: sw.ID, Now: time.Now().UTC()}, s.audit(sw, "swapped")); err != nil {
		t.Fatal(err)
	}
	if err := f.switcher(f.config()).Recover(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchDone {
		t.Fatalf("a swapped switch after a restart reads %+v, %v; want done", got, err)
	}
}

// TestStopRefusesARunBeforeItsSwap guards a stop mid-drain: the run refuses
// its switch with "server restarted", as the next start would, and leaves
// nothing accepted.
func TestStopRefusesARunBeforeItsSwap(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	s, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchRefused || got.Refusal != tenant.RefusalRestarted {
		t.Fatalf("a switch whose server stopped mid-drain reads %+v, %v", got, err)
	}
}

// TestStopBetweenAcceptAndLaunch guards a stop that comes after the
// database accepted a switch and before its run started: no run is started
// after the stop has waited for the runs (Begin answers UNAVAILABLE), the
// switch stays accepted, and the next start refuses it with "server
// restarted".
func TestStopBetweenAcceptAndLaunch(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	s, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	s.setBeforeLaunch(func() {
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	sw, err := s.Begin(f.ctx, f.request(f.preview(s, true).Confirm))
	if fault.KindOf(err) != fault.Unavailable || sw.ID == "" {
		t.Fatalf("a switch accepted as the server stopped answered %+v, %v; want it accepted and UNAVAILABLE", sw, err)
	}
	s.wg.Wait()
	s.mu.Lock()
	running := len(s.running)
	s.mu.Unlock()
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchAccepted || running != 0 {
		t.Fatalf("after a stop before its run the switch reads %+v, %v, with %d runs; want it accepted and no run", got, err, running)
	}
	f.switcher(f.config()) // The next start, which recovers.
	if got, err := f.p.Switch(f.ctx, sw.ID); err != nil || got.State != tenant.SwitchRefused || got.Refusal != tenant.RefusalRestarted {
		t.Fatalf("the next start left the switch as %+v, %v; want refused %q", got, err, tenant.RefusalRestarted)
	}
}
