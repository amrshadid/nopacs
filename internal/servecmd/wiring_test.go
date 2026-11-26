package servecmd

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readingOpener is the rig's storages opened for reading, as the scrubs and
// the rebuilds open them.
func readingOpener(r *workRig) place.Opener {
	return place.Opener{Values: roots{{Name: "fast", Path: r.root}}, Keys: r.keys, StaleUploadAge: -1}
}

// rigTenantID answers the rig's tenant's id.
func rigTenantID(context.Context, string) (string, error) { return "T0000000000001", nil }

// waitJob waits until the job of id has ended, failing the test unless it
// ended done.
func (r *workRig) waitJob(what, id string, poke func()) tenant.Job {
	r.t.Helper()
	var j tenant.Job
	r.waitFor(what, 2*time.Minute, func() bool {
		poke()
		var err error
		if j, err = r.db.Job(r.ctx, id); err != nil {
			r.t.Fatal(err)
		}
		return j.State.Ended()
	})
	if j.State != tenant.JobDone {
		r.t.Fatalf("%s ended %s (%s)", what, j.State, j.Reason)
	}
	return j
}

// TestAReplicaScrubbedAndComparedEmptyPassesTheSwitchBlockers guards the
// switch's checks of a replica as serve wires them: a scrub of the replica
// run through serve's scrub configuration records when each of its scans
// was read whole (rows a storage's coverage is read from), and a rebuild
// compare built from the replica alone that found no difference lifts the
// empty-compare blocker, so once both are done the switch's preview has no
// blocker without skip_verification.
func TestAReplicaScrubbedAndComparedEmptyPassesTheSwitchBlockers(t *testing.T) {
	r := newWorkRig(t)
	r.start()
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	r.waitFor("every scan to be in step on the replica", time.Minute, r.inStep)
	sw, err := r.w.switcherFor(r.ctx, "lab", r.db)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := sw.Validate(r.ctx, "main", "copy", false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pv.Blockers, storageswitch.BlockerNoEmptyCompare) || !slices.Contains(pv.Blockers, storageswitch.BlockerNoCleanScrub) {
		t.Fatalf("before any check of the replica the switch's blockers are %v", pv.Blockers)
	}

	opener := readingOpener(r)
	cfg := scrubConfig(Deps{Keys: r.keys, Master: oneTenant{}}, settingvalue.Reader{Master: noSettings{}}, opener, oneTenantStore{st: r.db},
		nil, &checkedread.Slots{})
	scrubs, err := scrub.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := scrubs.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scrubs.Stop(context.Background()) })
	if _, err := r.db.CreateJob(r.ctx, tenant.NewJob{ID: "scrub-copy-1", Kind: tenant.JobScrub, Storage: "copy", Created: time.Now()},
		r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.waitJob("the replica's scrub", "scrub-copy-1", func() { scrubs.Poke("lab") })
	verified, _, err := r.db.ScanVerifications(r.ctx, "copy", "", tenant.MaxVerificationPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) == 0 {
		t.Fatal("the scrub of the replica recorded no scan read whole")
	}
	for _, v := range verified {
		if v.Verified.IsZero() || v.Objects == 0 {
			t.Fatalf("a scan of the replica is recorded %+v after its scrub, want read whole", v)
		}
	}

	rb, err := rebuild.New(rebuild.Config{Tenants: oneTenantStore{st: r.db},
		OpenStorages: func(name string, p tenant.Pipeline) rebuild.Storages {
			return checkedread.NewStores(opener, rigTenantID, p, name)
		},
		Keys: r.keys, TenantID: rigTenantID, Decide: rebuild.ByRow})
	if err != nil {
		t.Fatal(err)
	}
	if err := rb.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rb.Stop(context.Background()) })
	if _, err := r.db.CreateJob(r.ctx, tenant.NewJob{ID: "compare-copy-1", Kind: tenant.JobRebuild, Created: time.Now(),
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, FromStorage: "copy"}}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.waitJob("the compare from the replica", "compare-copy-1", func() { rb.Poke("lab") })
	if pv, err = sw.Validate(r.ctx, "main", "copy", false); err != nil {
		t.Fatal(err)
	}
	if !pv.Ready || len(pv.Blockers) != 0 {
		t.Fatalf("after a clean scrub and an empty compare of the replica the switch's blockers are %v", pv.Blockers)
	}
}

// echoscu returns dcmtk's echoscu, or skips the test.
func echoscu(t *testing.T) string {
	t.Helper()
	var candidates []string
	if d := os.Getenv("NOPACS_DCMTK"); d != "" {
		candidates = append(candidates, filepath.Join(d, "echoscu"))
	}
	if p, err := exec.LookPath("echoscu"); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, _ := exec.CommandContext(ctx, p, "--version").CombinedOutput() //nolint:gosec // G204: a test's own tool.
		cancel()
		if bytes.Contains(out, []byte("dcmtk")) {
			return p
		}
	}
	t.Skip("dcmtk's echoscu is not installed; put it on the PATH or set NOPACS_DCMTK")
	return ""
}

// loopbackPort returns a TCP port on loopback no one listens on now.
func loopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// rigPipeline starts the reception pipeline of the rig as serve's
// supervisor starts it, its writes asking the tenant's switch gate.
func rigPipeline(ctx context.Context, r *workRig) (supervise.Pipeline, error) {
	pause, committed, err := r.w.receptionWrites(ctx, "lab", r.db, func(string) {})
	if err != nil {
		return nil, err
	}
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		return nil, err
	}
	storages := newTenantStorages(r.w.opener, oneTenant{}, "lab", r.db)
	p, err := receive.New(receive.Config{Tenant: "lab", Store: r.db, Hasher: hasher, Clock: quickClock{start: time.Now()},
		OpenStorage: storages.Open, IncomingRoot: filepath.Join(r.data, "incoming"), Workers: 2, Lease: 30 * time.Second,
		PollInterval: time.Second, BatchInterval: time.Second, RetryInterval: 5 * time.Second, Pause: pause, Wake: committed})
	if err != nil {
		return nil, err
	}
	if _, err := p.Start(ctx); err != nil {
		_ = storages.Close()
		return nil, err
	}
	return pipelineRun{Pipeline: p, storages: storages, name: "lab", pipes: &runningPipelines{}}, nil
}

// TestAReceiverRefusesAssociationsWhileItsStorageSwitches guards the
// receivers' supervisor as serve wires it (supervise.Config.Switches is the
// tenant's switch gate): a DIMSE receiver into a primary answers dcmtk's
// echoscu; while a switch of that primary runs (held at its owner step by
// the tenant's owner lock) the receiver rejects a new association; once
// the switch has swapped it takes associations again.
func TestAReceiverRefusesAssociationsWhileItsStorageSwitches(t *testing.T) {
	scu := echoscu(t)
	r := newWorkRig(t)
	r.start()
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	r.waitFor("every scan to be in step on the replica", time.Minute, r.inStep)
	if err := p.Stop(r.ctx); err != nil {
		t.Fatal(err)
	}
	port := loopbackPort(t)
	scanner := tenant.Receiver{Name: "scanner", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", QuietPeriod: 5 * time.Second,
		MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30, DIMSE: tenant.DIMSEReceiver{Port: port, AETitle: "SWITCHED", AnyCaller: true}}
	if _, err := r.db.AddReceiver(r.ctx, scanner, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.RemoveReceiver(r.ctx, "dimse", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	sup, err := supervise.New(supervise.Config{Tenants: oneTenantStore{st: r.db}, Installation: noSettings{},
		StartPipeline: func(ctx context.Context, _ string, _ tenant.Pipeline, _ supervise.Gates) (supervise.Pipeline, error) {
			return rigPipeline(ctx, r)
		},
		NewListener: func(spec supervise.ListenerSpec) (supervise.Listener, error) {
			return dimse.New(dimse.Config{Receiver: spec.Receiver, BindAddress: "127.0.0.1", Sink: spec.Sink, TLSDir: r.data,
				Refusal: dimse.Refusal(spec.Refusal), Report: dimse.Report(spec.Report)})
		},
		Switches:      r.w.switchGate,
		SpaceInterval: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })
	echo := func() (bool, string) {
		ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, scu, "-aet", "MODALITY", "-aec", "SWITCHED", "127.0.0.1", strconv.Itoa(port)).CombinedOutput() //nolint:gosec // G204: a test's own tool.
		return err == nil, string(out)
	}
	r.waitFor("the receiver to answer an echo", 30*time.Second, func() bool { ok, _ := echo(); return ok })

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
		SkipReason: "the test checks the receiver only", DrainTimeout: 2 * time.Minute, Audit: r.audit()})
	if err != nil {
		t.Fatalf("the switch was not accepted: %v (blockers %v)", err, pv.Blockers)
	}
	r.waitFor("the switch to reach its owner step", 30*time.Second, func() bool {
		got, err := r.db.Switch(r.ctx, s.ID)
		return err == nil && got.Step == tenant.SwitchOwning
	})
	var refused string
	r.waitFor("the receiver to reject an association", 30*time.Second, func() bool {
		ok, out := echo()
		refused = out
		return !ok
	})
	if !bytes.Contains([]byte(refused), []byte("Association Rejected")) {
		t.Fatalf("while the storage switches the echo failed otherwise than by a rejected association:\n%s", refused)
	}
	release()
	done, err := sw.Wait(r.ctx, s.ID)
	if err != nil || done.State != tenant.SwitchDone {
		t.Fatalf("the switch ended %+v (%v)", done, err)
	}
	r.waitFor("the receiver to answer an echo after the switch", 30*time.Second, func() bool { ok, _ := echo(); return ok })
}

// TestTheFenceSplitStillRefusesAMovedTenantsWrites guards the two checks
// of a tenant's background work over a real registry: a write to a storage
// asks the registry's state of the tenant, which reads no owner record
// (the fence's check is not run for it), while a write to the database
// asks the fence, which does; once the tenant is moved by an export, both
// refuse, so a moved tenant's work writes nothing to either.
func TestTheFenceSplitStillRefusesAMovedTenantsWrites(t *testing.T) {
	r := newRegistryRig(t)
	state, fence := r.w.stateOf("lab"), r.w.fenceOf("lab")
	if state == nil || fence == nil {
		t.Fatal("the background work has no check of the registry")
	}
	before := r.fences.Load()
	for range 10 {
		if err := state(); err != nil {
			t.Fatalf("the state of a ready tenant: %v", err)
		}
	}
	if n := r.fences.Load(); n != before {
		t.Fatalf("ten checks of the state before writes to a storage read the owner records %d times, want none", n-before)
	}
	if err := fence(); err != nil {
		t.Fatalf("the fence of a ready tenant: %v", err)
	}
	if n := r.fences.Load(); n != before+1 {
		t.Fatalf("a check of the fence before a database write read the owner records %d times, want once", n-before)
	}
	if err := r.reg.SetHandoff(r.ctx, "lab", master.StateMoved, "handed off", r.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	counted := &countingStore{}
	open := fencedOpen(func(context.Context, string) (blob.Store, error) { return counted, nil }, state)
	st, err := open(r.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(r.ctx, "data/x", bytes.NewReader([]byte("x"))); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a put of a moved tenant's work: %v, want the registry's refusal", err)
	}
	if err := fence(); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a database write of a moved tenant's work: %v, want the registry's refusal", err)
	}
	if n := counted.writes.Load(); n != 0 {
		t.Fatalf("%d writes of a moved tenant reached the storage", n)
	}
}
