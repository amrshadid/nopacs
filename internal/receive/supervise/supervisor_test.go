package supervise_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestStartsWhatTheRowsAskFor guards a start: every enabled receiver's
// listener is made from its row and is up; a disabled one is down,
// disabled, with nothing made; one pipeline runs and every listener hands
// its files to it.
func TestStartsWhatTheRowsAskFor(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.add("mr", 11113, true)
	r.add("off", 11114, false)
	r.start()
	r.waitFor("both enabled receivers up", func() bool { return r.isUp("ct")() && r.isUp("mr")() })
	if h := r.health("off"); h.State != tenant.ReceiverDown || h.Reason != "disabled" {
		t.Fatalf("a disabled receiver's health %+v", h)
	}
	if len(r.made("off")) != 0 {
		t.Fatal("a listener was made for a disabled receiver")
	}
	l := r.running("ct")
	if len(l) != 1 || l[0].spec.Receiver.DIMSE.Port != 11112 || l[0].spec.Tenant != "lab" || l[0].spec.BindAddress != "" {
		t.Fatalf("listeners of ct %+v", l)
	}
	if out := l[0].spec.Sink.Accept(context.Background(), receiveArrival()); out.Status != receivedStatus {
		t.Fatalf("the sink did not reach the pipeline: %+v", out)
	}
	if n := len(r.pipelines); n != 1 {
		t.Fatalf("%d pipelines started", n)
	}
}

// TestAChangeRestartsOnlyWhatItMust guards a change: a change of the
// listener's part (the AE title), seen through Health with the stored row,
// stops the listener and makes one from the new row; a change only the
// pipeline reads (the quiet period) does not restart it; disabling stops
// it; removing forgets it.
func TestAChangeRestartsOnlyWhatItMust(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	first := r.running("ct")[0]

	row := r.set(tenant.Receiver{Name: "ct", DIMSE: tenant.DIMSEReceiver{AETitle: "NEWTITLE"}}, tenant.FieldDIMSEAETitle)
	if h := r.sup.Health("lab", row); h.State != tenant.ReceiverStarting {
		t.Fatalf("health right after a change %+v, want starting", h)
	}
	r.waitFor("a listener from the new row", func() bool {
		l := r.running("ct")
		return first.stopped.Load() && len(l) == 1 && l[0].spec.Receiver.DIMSE.AETitle == "NEWTITLE" && r.isUp("ct")()
	})
	second := r.running("ct")[0]

	r.set(tenant.Receiver{Name: "ct", QuietPeriod: 30 * time.Second}, tenant.FieldQuietPeriod)
	r.waitFor("the change to be seen", func() bool { return r.health("ct").State == tenant.ReceiverUp })
	if second.stopped.Load() || len(r.made("ct")) != 2 {
		t.Fatalf("a change of the quiet period restarted the listener (%d made)", len(r.made("ct")))
	}

	r.set(tenant.Receiver{Name: "ct", Enabled: false}, tenant.FieldEnabled)
	r.waitFor("ct down, disabled", func() bool { return r.health("ct").Reason == "disabled" && second.stopped.Load() })

	if _, err := r.ts.pipe().RemoveReceiver(context.Background(), "ct", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.add("mr", 11113, true)
	r.waitFor("mr up after ct is removed", r.isUp("mr"))
	if len(r.running("ct")) != 0 {
		t.Fatal("a removed receiver still runs")
	}
}

// TestAReceiverThatCannotStartIsDownAndTheOthersRun guards a failure to
// start: a receiver whose port is taken is down with the listener's reason
// and phrase, tried again until the port is free and then up, while the
// other receiver is up throughout and never restarted.
func TestAReceiverThatCannotStartIsDownAndTheOthersRun(t *testing.T) {
	r := newRig(t)
	r.mu.Lock()
	r.taken[11113] = true
	r.mu.Unlock()
	r.add("ct", 11112, true)
	r.add("mr", 11113, true)
	r.start()
	r.waitFor("ct up and mr down", func() bool {
		h := r.health("mr")
		return r.isUp("ct")() && h.State == tenant.ReceiverDown && h.Reason == "address_in_use" &&
			h.Detail == "the port is taken by another program"
	})
	ct := r.running("ct")[0]
	r.mu.Lock()
	r.taken[11113] = false
	r.mu.Unlock()
	r.waitFor("mr up once its port is free", r.isUp("mr"))
	if ct.stopped.Load() || len(r.made("ct")) != 1 {
		t.Fatal("the failure of one receiver restarted another")
	}
	var downs, ups int
	for _, e := range r.healthEvents() {
		if e.Receiver == "mr" && e.State == tenant.ReceiverDown {
			downs++
		}
		if e.Receiver == "mr" && e.State == tenant.ReceiverUp {
			ups++
		}
	}
	if downs != 1 || ups != 1 {
		t.Fatalf("mr's health events: %d down, %d up; want one each whatever the retries", downs, ups)
	}
}

// TestNearlyFullRefusesWithOneEventEachWay guards the reserve: with the
// incoming disk below it, every listener's Refusal says nearly full, each
// receiver is down, nearly full, and the pipeline's gate refuses; when space
// returns it is up again; and each receiver has one event when it starts
// and one when it ends, however many checks ran.
func TestNearlyFullRefusesWithOneEventEachWay(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	l := r.running("ct")[0]
	if f := l.spec.Refusal(); f != "" {
		t.Fatalf("refusal %q with room", f)
	}
	// The default reserve is the larger of 10% and 10 GiB.
	r.free.Store(r.total / 20)
	r.waitFor("ct down, nearly full", func() bool { h := r.health("ct"); return h.Reason == "nearly_full" })
	if f := l.spec.Refusal(); f != tenant.FailureNearlyFull {
		t.Fatalf("refusal %q while the disk is nearly full", f)
	}
	if low, _ := r.pipelines[0].gates.LowDisk(context.Background()); !low {
		t.Fatal("the pipeline's gate does not refuse while the disk is nearly full")
	}
	for range 5 {
		r.sup.Poke("lab")
		time.Sleep(5 * time.Millisecond)
	}
	r.free.Store(r.total / 2)
	r.waitFor("ct up again", r.isUp("ct"))
	if f := l.spec.Refusal(); f != "" {
		t.Fatalf("refusal %q after space returned", f)
	}
	var kinds []string
	for _, e := range r.healthEvents() {
		kinds = append(kinds, string(e.State)+":"+string(e.DownReason))
	}
	want := []string{"up:", "down:nearly_full", "up:"}
	if len(kinds) != len(want) || kinds[0] != want[0] || kinds[1] != want[1] || kinds[2] != want[2] {
		t.Fatalf("health events %v, want %v", kinds, want)
	}
	if l.stopped.Load() {
		t.Fatal("the listener was stopped while nearly full; it holds its port")
	}
}

// TestAStorageBelowItsReserveRefusesAndSaysSo guards the storage's own
// reserve: its receivers refuse, a StorageSpaceChanged event says it is
// nearly full, and another when it recovers.
func TestAStorageBelowItsReserveRefusesAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.storage.Store(r.total / 2)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	r.storage.Store(r.total / 50)
	r.waitFor("ct down, its storage nearly full", func() bool {
		h := r.health("ct")
		return h.Reason == "nearly_full" && h.Detail == "the storage is nearly full"
	})
	if f := r.running("ct")[0].spec.Refusal(); f != tenant.FailureNearlyFull {
		t.Fatalf("refusal %q", f)
	}
	r.storage.Store(r.total / 2)
	r.waitFor("ct up again", r.isUp("ct"))
	ev := r.events(tenant.EventStorageSpace)
	if len(ev) != 2 || ev[0].Payload.(*tenant.StorageSpaceChanged).State != tenant.StorageNearlyFull ||
		ev[1].Payload.(*tenant.StorageSpaceChanged).State != tenant.StorageRecovered {
		t.Fatalf("storage events %+v", ev)
	}
}

// TestATenantNotReadyRefusesUntilItIsBack guards a database that stops
// answering: the listeners keep their ports and refuse as tenant not ready,
// and the receivers are up again when it answers.
func TestATenantNotReadyRefusesUntilItIsBack(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	l := r.running("ct")[0]
	row := r.row("ct")
	r.ts.mu.Lock()
	r.ts.down = true
	r.ts.mu.Unlock()
	r.waitFor("ct down, tenant not ready", func() bool {
		return r.sup.Health("lab", row).Reason == "tenant_not_ready" && l.spec.Refusal() == tenant.FailureTenantNotReady
	})
	r.ts.mu.Lock()
	r.ts.down = false
	r.ts.mu.Unlock()
	r.waitFor("ct up again", r.isUp("ct"))
	if l.stopped.Load() {
		t.Fatal("the listener was stopped while the tenant was not ready")
	}
}

// TestAReopenedDatabaseRestartsThePipelineNotTheListeners guards a tenant
// whose database the registry opened again: the pipeline over the old one
// is stopped and one over the new one started, and the listeners, which
// hand files to whichever runs, are not restarted.
func TestAReopenedDatabaseRestartsThePipelineNotTheListeners(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	r.ts.reopen(t)
	r.waitFor("a second pipeline", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.pipelines) == 2 && r.pipelines[0].stopped.Load()
	})
	if len(r.made("ct")) != 1 || r.running("ct")[0].stopped.Load() {
		t.Fatal("a reopened database restarted the listener")
	}
	if r.pipelines[1].store != r.ts.pipe() {
		t.Fatal("the new pipeline does not run over the reopened database")
	}
}

// TestADecisionNeverReachesAStoppingPipeline guards the pipeline handed
// to a person's decision across a restart of the tenant's pipeline: while
// the old one stops, none is handed out (a release is then refused, never
// taken by a pipeline whose successor's start may already have read the
// incoming area), and the new one is handed out once it has started.
func TestADecisionNeverReachesAStoppingPipeline(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	r.waitFor("a pipeline handed out", func() bool { _, ok := r.sup.Decisions("lab"); return ok })
	r.mu.Lock()
	first := r.pipelines[0]
	r.mu.Unlock()
	var handedWhileStopping atomic.Bool
	first.duringStop = func() {
		if d, ok := r.sup.Decisions("lab"); ok && d == supervise.Decisions(first) {
			handedWhileStopping.Store(true)
		}
	}
	r.ts.reopen(t)
	r.waitFor("the new pipeline handed out", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.pipelines) != 2 {
			return false
		}
		d, ok := r.sup.Decisions("lab")
		return ok && d == supervise.Decisions(r.pipelines[1])
	})
	if handedWhileStopping.Load() {
		t.Fatal("a pipeline that was stopping was handed to a decision")
	}
}

// TestRemovalsAreQueuedAsWorkOfTheTenant guards the removal of bytes after a
// rejection, a discard or a purge: each is a unit of work in the tenant's
// database, kept while no pipeline runs and not limited in number (more than
// 4,096 are all there), for the pipeline to run once it does; one for a
// tenant the supervisor does not run queues nothing.
func TestRemovalsAreQueuedAsWorkOfTheTenant(t *testing.T) {
	r := newRig(t)
	// No pipeline ever starts: every start waits until it is cut.
	r.startWait = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	r.add("ct", 11112, true)
	r.start()
	ctx := context.Background()
	const rejected = 4100
	for i := range rejected {
		if err := r.sup.RemoveRejected(ctx, "lab", fmt.Sprintf("Q%031d", i), incoming.PathOf("ct", fmt.Sprintf("%032x", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.sup.RemoveDiscarded(ctx, "lab", "reception-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.RemoveWithdrawn(ctx, "lab", "01arz3ndektsv4rrffq69g5fa1", incoming.PathOf("ct", strings.Repeat("b", 32))); err != nil {
		t.Fatal(err)
	}
	// The same removal again is the same unit.
	if err := r.sup.RemoveDiscarded(ctx, "lab", "reception-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.sup.RemoveRejected(ctx, "nobody", "Q1", incoming.PathOf("ct", strings.Repeat("c", 32))); err != nil {
		t.Fatalf("a removal for a tenant not run: %v", err)
	}
	var subjects []string
	for {
		c, err := r.ts.pipe().ClaimNextWork(ctx, "incoming-removal", "test", r.clock.Now(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Found {
			break
		}
		subjects = append(subjects, c.Work.Subject)
	}
	if len(subjects) != rejected+2 {
		t.Fatalf("%d removals owed in the database, want %d", len(subjects), rejected+2)
	}
	for _, want := range []string{"discarded reception-1", "withdrawn 01arz3ndektsv4rrffq69g5fa1 " + incoming.PathOf("ct", strings.Repeat("b", 32))} {
		if !slices.Contains(subjects, want) {
			t.Errorf("no unit %q among the removals owed", want)
		}
	}
}

// TestAPipelineStartMayOutlastAStop guards a pipeline's start, which reads
// the whole incoming area after a crash: it is bounded by its own timeout,
// not by the stop's, so a start longer than a stop still ends with the
// pipeline running instead of being cut short and tried again from the
// beginning for ever.
func TestAPipelineStartMayOutlastAStop(t *testing.T) {
	r := newRig(t)
	const stop = 20 * time.Millisecond
	r.edit = func(c *supervise.Config) { c.StopTimeout = stop; c.StartTimeout = time.Minute }
	r.startWait = func(ctx context.Context) error {
		select {
		case <-time.After(10 * stop):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("a pipeline started", func() bool { _, ok := r.sup.Decisions("lab"); return ok })
}

// TestAPipelineStartIsBoundedByTheTenantsLimit guards that the bound of a
// pipeline's start is the limit the supervisor is given for the tenant
// (receiver.start-limit), not a fixed time: with a limit of 50 ms a start
// that waits for ever is cut, its pipeline is not published, and the next
// round tries it again, the limit read each time. A limit that cannot be
// read leaves the configured timeout.
func TestAPipelineStartIsBoundedByTheTenantsLimit(t *testing.T) {
	r := newRig(t)
	var asked, cut atomic.Int64
	r.edit = func(c *supervise.Config) {
		c.StartTimeout = time.Hour
		c.StartLimit = func(_ context.Context, name string, _ tenant.Pipeline) (time.Duration, error) {
			if name != "lab" {
				t.Errorf("the limit was asked for %q, want lab", name)
			}
			asked.Add(1)
			return 50 * time.Millisecond, nil
		}
	}
	r.startWait = func(ctx context.Context) error {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cut.Add(1)
		}
		return ctx.Err()
	}
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("a start cut at its limit", func() bool { return cut.Load() >= 1 })
	if asked.Load() < 1 {
		t.Fatal("the limit was never read")
	}
	if _, ok := r.sup.Decisions("lab"); ok {
		t.Fatal("a pipeline that was cut was published")
	}
}

// TestAStopEndsALongPipelineStart guards the supervisor's stop while a
// tenant's pipeline is starting: the start is ended at once, not waited
// for until its bound.
func TestAStopEndsALongPipelineStart(t *testing.T) {
	r := newRig(t)
	r.edit = func(c *supervise.Config) { c.StartTimeout = time.Hour }
	starting := make(chan struct{})
	var once sync.Once
	r.startWait = func(ctx context.Context) error {
		once.Do(func() { close(starting) })
		<-ctx.Done()
		return ctx.Err()
	}
	r.add("ct", 11112, true)
	r.start()
	<-starting
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	begun := time.Now()
	if err := r.sup.Stop(ctx); err != nil {
		t.Fatalf("the stop: %v", err)
	}
	if d := time.Since(begun); d > 10*time.Second {
		t.Fatalf("the stop waited %v for a start", d)
	}
}

// TestDrainedWaitsForAPipelineStarting guards what a hand-off's drain
// reads while a tenant's pipeline is starting: no pipeline is published
// yet and none is stopping, but the tenant is not drained until the start
// is over, since the pipeline it makes would write after the snapshot.
func TestDrainedWaitsForAPipelineStarting(t *testing.T) {
	r := newRig(t)
	r.edit = func(c *supervise.Config) { c.StartTimeout = time.Minute }
	starting, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.startWait = func(ctx context.Context) error {
		once.Do(func() { close(starting) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.add("ct", 11112, true)
	r.start()
	<-starting
	if r.sup.Drained("lab") {
		close(release)
		t.Fatal("the tenant was drained while its pipeline was starting")
	}
	close(release)
	r.waitFor("a pipeline started", func() bool { _, ok := r.sup.Decisions("lab"); return ok })
	if r.sup.Drained("lab") {
		t.Fatal("the tenant was drained with its pipeline running")
	}
}

// TestAPipelineStartAsksTheTenantAgain guards a tenant made read only (an
// export) after the worker read its store and before its pipeline starts:
// the start asks the tenant again, is refused, and starts nothing, so no
// pipeline writes after the drain said the tenant was drained.
func TestAPipelineStartAsksTheTenantAgain(t *testing.T) {
	r := newRig(t)
	r.ts.movedFrom = 2
	r.start()
	r.waitFor("the tenant asked again", func() bool {
		r.ts.mu.Lock()
		defer r.ts.mu.Unlock()
		return r.ts.calls >= 2
	})
	r.waitFor("drained", func() bool { return r.sup.Drained("lab") })
	r.mu.Lock()
	n := len(r.pipelines)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pipelines started for a tenant refused before the start", n)
	}
}

// TestARestartStartsWhatIsDue guards that state lives in the database: a
// second supervisor over the same rows, as after a restart, starts every
// enabled receiver with nothing told to it.
func TestARestartStartsWhatIsDue(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	if err := r.sup.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.running("ct")) != 0 {
		t.Fatal("Stop left a listener running")
	}
	r.start()
	r.waitFor("ct up after a restart", r.isUp("ct"))
	if len(r.made("ct")) != 2 || len(r.running("ct")) != 1 {
		t.Fatalf("%d listeners made, %d running", len(r.made("ct")), len(r.running("ct")))
	}
}

// TestStopStopsEverything guards Stop: every listener and the pipeline are
// stopped, and each receiver says the server is stopping.
func TestStopStopsEverything(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.add("mr", 11113, true)
	r.start()
	r.waitFor("both up", func() bool { return r.isUp("ct")() && r.isUp("mr")() })
	if err := r.sup.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.running("ct")) != 0 || len(r.running("mr")) != 0 || !r.pipelines[0].stopped.Load() {
		t.Fatal("Stop left something running")
	}
	if h := r.health("ct"); h.State != tenant.ReceiverDown || h.Reason != "stopping" {
		t.Fatalf("health after Stop %+v", h)
	}
}

// TestBindAddressDefaultsToTheInstallations guards the bind address: a
// receiver that names none listens on the first address the installation
// lists.
func TestBindAddressDefaultsToTheInstallations(t *testing.T) {
	r := newRig(t)
	r.inst.v["receiver.bind-addresses"] = "192.0.2.1,192.0.2.2"
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	if b := r.running("ct")[0].spec.BindAddress; b != "192.0.2.1" {
		t.Fatalf("bind address %q", b)
	}
}

// TestNewChecksItsPorts guards New: a missing port is refused.
func TestNewChecksItsPorts(t *testing.T) {
	if _, err := supervise.New(supervise.Config{}); err == nil {
		t.Fatal("New took a config with no ports")
	}
}

// TestAnExpiredCertificateIsDownWithItsPhrase guards the periodic check of
// a running listener (Checker): once the clock passes its certificate's
// end, the receiver is down, tls, certificate expired, and its listener is
// stopped; a listener made after the certificate is renewed is up again.
func TestAnExpiredCertificateIsDownWithItsPhrase(t *testing.T) {
	r := newRig(t)
	end := t0.Add(time.Hour)
	r.expires.Store(&end)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	first := r.running("ct")[0]
	r.clock.advance(2 * time.Hour)
	r.waitFor("ct down, its certificate expired", func() bool {
		h := r.health("ct")
		return h.State == tenant.ReceiverDown && h.Reason == tenant.DownTLS && h.Detail == tenant.PhraseCertificateExpired
	})
	if !first.stopped.Load() {
		t.Fatal("the listener whose certificate expired still runs")
	}
	renewed := t0.Add(24 * time.Hour)
	r.expires.Store(&renewed)
	r.waitFor("ct up with the renewed certificate", func() bool {
		ls := r.running("ct")
		return r.isUp("ct")() && len(ls) == 1 && ls[0].expires == &renewed
	})
}

// TestViewHoldsWhatAStatusNeedsInMemory guards View: the receivers as last
// read, ordered by name, each with its health; the space of the storage the
// enabled receivers write to, as last measured, nearly full or not; and no
// tenant the supervisor does not run.
func TestViewHoldsWhatAStatusNeedsInMemory(t *testing.T) {
	r := newRig(t)
	r.add("mr", 11113, true)
	r.add("ct", 11112, true)
	r.storage.Store(r.total / 2)
	r.start()
	r.waitFor("both up", func() bool { return r.isUp("ct")() && r.isUp("mr")() })
	r.waitFor("the storage measured", func() bool { v, _ := r.sup.View("lab"); _, ok := v.Storages["main"]; return ok })
	v, ok := r.sup.View("lab")
	if !ok || len(v.Receivers) != 2 || v.Receivers[0].Receiver.Name != "ct" || v.Receivers[1].Receiver.Name != "mr" {
		t.Fatalf("the view: %v %+v, want ct then mr", ok, v.Receivers)
	}
	if v.Receivers[0].Health.State != tenant.ReceiverUp {
		t.Errorf("ct's health in the view: %+v", v.Receivers[0].Health)
	}
	if s := v.Storages["main"]; s.NearlyFull || s.FreeBytes != r.total/2 {
		t.Errorf("the storage in the view: %+v, want %d free and not nearly full", s, r.total/2)
	}
	r.storage.Store(r.total / 50)
	r.waitFor("the storage nearly full in the view", func() bool { v, _ := r.sup.View("lab"); return v.Storages["main"].NearlyFull })
	if _, ok := r.sup.View("other"); ok {
		t.Error("a tenant the supervisor does not run has a view")
	}
}
