package registry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestNoDriverTextAnywhere guards that nothing a caller or the audit log
// sees, and nothing logged at any level, holds the address or the password
// in any form a driver's text may carry it (plain, quoted, libpq-escaped,
// URL-escaped, in a URL that does not parse), whatever the driver does:
// fails with the address in its cause, fails with a plain error, panics with
// it in the panic's value, or repeats it in a Location error.
func TestNoDriverTextAnywhere(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("down", "bad", "leaky", "panicky")
	e.flaky.set("down", down)
	e.flaky.set("bad", broken)
	e.flaky.set("leaky", leaky)
	e.flaky.set("panicky", panicky)
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, name := range []string{"down", "bad", "leaky", "panicky"} {
		st, _ := r.State(name)
		texts = append(texts, st.Reason, e.masterTenant(name).StateReason)
		_, err := r.Store(name)
		texts = append(texts, err.Error(), fault.Message(err))
	}
	for _, m := range []mode{down, broken, refused, leaky, panicky} {
		name := fmt.Sprintf("new%d", m)
		e.flaky.set(name, m)
		_, _, err := r.Create(ctx, NewTenant{ID: "id-" + name, Name: name, Driver: flakyDriver, Address: flakyAddress(name)}, e.audit("CreateTenant"), master.LocalOperator())
		if err == nil {
			t.Fatalf("Create of a %d tenant succeeded", m)
		}
		texts = append(texts, err.Error())
	}
	// A Location error that repeats the address is replaced by a fixed one.
	_, _, err := r.Create(ctx, NewTenant{ID: "id-other", Name: "other", Driver: flakyDriver, Address: "other://admin:" + password + "@" + flakyHost}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("an address the driver refuses = %v, want Invalid", err)
	}
	texts = append(texts, err.Error())
	all, _, err := e.master.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		texts = append(texts, fmt.Sprint(a))
	}
	texts = append(texts, e.logBuf.String())
	needles := append([]string{flakyHost, "admin:"}, passwordForms...)
	for _, text := range texts {
		mustNotContain(t, "a reason, error, audit entry or log line", text, needles...)
	}
	if !strings.Contains(e.logBuf.String(), "level=DEBUG") && !strings.Contains(e.logBuf.String(), "level=WARN") {
		t.Error("the log is empty, so the search proved nothing")
	}
	st, _ := r.State("down")
	if want := "the database does not answer: the database does not answer"; st.Reason != want {
		t.Errorf("the reason = %q, want exactly %q", st.Reason, want)
	}
}

// TestOneCharacterPassword guards against scrubbing a password out of a
// driver's text, which fails on a short password: a password of one character
// mangled every reason. A reason is now made only of fixed phrases and the driver's
// safe message, so it reads whole whatever the password is.
func TestOneCharacterPassword(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-one", "one", flakyDriver, "flaky://a:e@h:1/one")
	e.flaky.set("one", down)
	checkNow(r)
	st, _ := r.State("one")
	if want := "the database does not answer: the database does not answer"; st.Reason != want {
		t.Errorf("the reason = %q, want exactly %q", st.Reason, want)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestDriverPanicIsContained guards that a panic in a driver's Open or Ping
// takes down neither the process nor the other tenants: the tenant is failed
// with a fixed reason, the stack goes to the log without the panic's value,
// and the others are served.
func TestDriverPanicIsContained(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("opens", "pings", "fine")
	e.flaky.set("opens", panicky)
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.flaky.set("pings", panicky)
	checkNow(r)
	for _, name := range []string{"opens", "pings"} {
		st, _ := r.State(name)
		if st.State != master.StateFailed || st.Reason != reasonPanic {
			t.Errorf("%s is %s %q, want failed with the fixed reason", name, st.State, st.Reason)
		}
	}
	if _, err := r.Store("fine"); err != nil {
		t.Errorf("the other tenant is not served: %v", err)
	}
	log := e.logBuf.String()
	if !strings.Contains(log, "a database driver panicked") || !strings.Contains(log, "stack=") {
		t.Error("the panic and its stack are not in the log")
	}
	mustNotContain(t, "the log", log, append(passwordForms, flakyHost)...)
}

// TestStateIsCurrent guards what the API answers: State changes the moment a
// check completes, with the time it was reached, even while the once-a-minute
// limit keeps the master's row behind; and a change of reason within one
// state is written to the master too, under the same limit.
func TestStateIsCurrent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("lab")
	cfg := e.config()
	// The new reason below comes from a ping the test driver holds until
	// its deadline: short enough to keep the test quick, and the pings that
	// should answer are of a real store at rest.
	cfg.PingTimeout = 2 * time.Second
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(10 * time.Second)
	e.flaky.set("lab", down)
	checkNow(r)
	st, _ := r.State("lab")
	if st.State != master.StateUnreachable || !st.Since.Equal(e.clock.Now()) {
		t.Errorf("State = %+v, want unreachable since %v", st, e.clock.Now())
	}
	// Back up within the minute: the view follows, the master does not yet.
	e.clock.Add(10 * time.Second)
	e.flaky.set("lab", up)
	checkNow(r)
	if st, _ := r.State("lab"); st.State != master.StateReady || !st.Since.Equal(e.clock.Now()) {
		t.Errorf("State = %+v, want ready since %v", st, e.clock.Now())
	}
	if got := e.masterTenant("lab"); got.State != master.StateUnreachable {
		t.Errorf("the master holds %s within the minute, want the earlier unreachable", got.State)
	}
	// A minute on, the same check writes it.
	e.clock.Add(StateChangeInterval)
	checkNow(r)
	if got := e.masterTenant("lab"); got.State != master.StateReady {
		t.Errorf("the master holds %s a minute on, want ready", got.State)
	}
	// A new reason in the same state is written, a minute after the last.
	e.flaky.set("lab", down)
	e.clock.Add(StateChangeInterval)
	checkNow(r)
	e.flaky.set("lab", hang)
	e.clock.Add(StateChangeInterval)
	checkNow(r)
	got := e.masterTenant("lab")
	if got.State != master.StateUnreachable || got.StateReason != reasonDeadline {
		t.Errorf("the master holds %s %q, want unreachable with the new reason %q", got.State, got.StateReason, reasonDeadline)
	}
}

// TestPingsDoNotWaitForOpens guards that a ready tenant's check does not
// wait behind tenants whose opens hang: with more hanging opens than
// opening slots, a ready tenant that goes down is marked within one interval
// and its own ping deadline, far sooner than the hanging opens' deadline.
func TestPingsDoNotWaitForOpens(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	names := []string{"h1", "h2", "h3", "h4", "ready"}
	e.seed(names...)
	for _, n := range names[:4] {
		e.flaky.set(n, down)
	}
	cfg := e.config()
	cfg.MaxOpening = 2
	cfg.Interval = 50 * time.Millisecond
	// The hanging opens hold their slots for the whole open deadline, so a
	// check that waited for a slot would take at least that long; the bound
	// below is a third of it, far above an interval and a ping on any
	// runner.
	cfg.OpenTimeout = 30 * time.Second
	cfg.PingTimeout = 2 * time.Second
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range names[:4] {
		e.flaky.set(n, hang)
	}
	waitFor(t, "two hanging opens to take both slots", func() bool { return e.flaky.opening.Load() >= 2 })
	begin := time.Now()
	e.flaky.set("ready", down)
	waitFor(t, "the ready tenant to be marked down", func() bool {
		st, _ := r.State("ready")
		return st.State == master.StateUnreachable
	})
	took := time.Since(begin)
	t.Logf("marked down in %v with both slots held by hanging opens (interval %v, ping deadline %v, open deadline %v)",
		took, cfg.Interval, cfg.PingTimeout, cfg.OpenTimeout)
	if took > 10*time.Second {
		t.Errorf("marked down after %v, want within an interval and a ping deadline", took)
	}
}

// TestCloseWithinItsDeadline guards that Close returns within the deadline
// it is given even when a driver ignores its context, says what it
// abandoned, and logs it.
func TestCloseWithinItsDeadline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("stuck")
	cfg := e.config()
	cfg.PingTimeout = 100 * time.Millisecond
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.flaky.set("stuck", stuck)
	defer e.flaky.unstick()
	checkNow(r)
	deadline, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := r.Close(deadline)
	took := time.Since(begin)
	if err == nil || !strings.Contains(fault.Message(err), "abandoned") {
		t.Errorf("Close = %v, want an error saying calls were abandoned", err)
	}
	// The deadline cuts the wait for the stuck call; what follows (closing
	// the stores) is bounded loosely, as a loaded runner closes files slowly.
	if took > 10*time.Second {
		t.Errorf("Close took %v under a deadline of 300ms", took)
	}
	if !strings.Contains(e.logBuf.String(), "abandoned=1") {
		t.Error("the abandoned call is not in the log")
	}
}

// TestFileLimitWarning guards the warning at start when the tenants could
// not all hold their files under the process's limit: one line, with both
// numbers; the start goes on.
func TestFileLimitWarning(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("a", "b", "c")
	r := e.newRegistry(e.config())
	r.fileLimit = func() int64 { return 100 }
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	log := e.logBuf.String()
	if !strings.Contains(log, "files_needed=289") || !strings.Contains(log, "limit=100") {
		t.Errorf("no warning with both numbers in the log:\n%s", log)
	}
	if _, err := r.Store("a"); err != nil {
		t.Errorf("the start did not go on: %v", err)
	}
}

// TestNoSecondOpenWhileOneRuns guards the opening flag: while an open that
// outlived its deadline is still running, no check starts another.
func TestNoSecondOpenWhileOneRuns(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("slow")
	e.flaky.set("slow", down)
	cfg := e.config()
	cfg.OpenTimeout = 100 * time.Millisecond
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.flaky.set("slow", stuck)
	defer e.flaky.unstick()
	checkNow(r)
	n := e.flaky.opensOf("slow")
	for range 3 {
		checkNow(r)
	}
	if got := e.flaky.opensOf("slow"); got != n {
		t.Errorf("%d opens began while one was still running, want none", got-n)
	}
}

// TestLateStoreIsClosed guards that a store a driver returns after its
// deadline, which nobody holds, is closed.
func TestLateStoreIsClosed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("late")
	e.flaky.set("late", down)
	cfg := e.config()
	cfg.OpenTimeout = 100 * time.Millisecond
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	opened := e.flaky.opens.Load()
	e.flaky.set("late", late)
	checkNow(r)
	e.flaky.unstick()
	waitFor(t, "the late store to be opened and closed", func() bool {
		return e.flaky.opens.Load() > opened && e.flaky.opens.Load() == e.flaky.closes.Load()
	})
}

// TestCreateAndRemoveExcludeEachOther guards that a removal of a name waits
// for a creation of the same name in progress, and then removes what it
// made, never a half-created tenant.
func TestCreateAndRemoveExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.flaky.set("x", late)
	created := make(chan error, 1)
	go func() {
		_, _, err := r.Create(ctx, NewTenant{ID: "id-x", Name: "x", Driver: flakyDriver, Address: flakyAddress("x")}, e.audit("CreateTenant"), master.LocalOperator())
		created <- err
	}()
	waitFor(t, "the creation's open to begin", func() bool { return e.flaky.opensOf("x") == 1 })
	removed := make(chan error, 1)
	go func() {
		_, err := r.Remove(ctx, "x", e.audit("RemoveTenant"), master.LocalOperator())
		removed <- err
	}()
	select {
	case err := <-removed:
		t.Fatalf("Remove returned (%v) while the creation was in progress", err)
	case <-time.After(100 * time.Millisecond):
	}
	e.flaky.unstick()
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := <-removed; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, known := r.State("x"); known {
		t.Error("the removed tenant is registered")
	}
}

// TestRemoveDuringStart guards that a removal while Start lists the tenants
// waits for the listing, so the removed tenant is not registered from a
// listing read before the removal.
func TestRemoveDuringStart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("victim")
	tricky := &trickyMaster{Store: e.master, listGate: make(chan struct{}), listEntered: make(chan struct{})}
	cfg := e.config()
	cfg.Master = tricky
	r := e.newRegistry(cfg)
	started := make(chan error, 1)
	go func() { started <- r.Start(ctx) }()
	<-tricky.listEntered
	removed := make(chan error, 1)
	go func() {
		_, err := r.Remove(ctx, "victim", e.audit("RemoveTenant"), master.LocalOperator())
		removed <- err
	}()
	select {
	case err := <-removed:
		t.Fatalf("Remove returned (%v) during Start's listing", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(tricky.listGate)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if _, known := r.State("victim"); known {
		t.Error("a removed tenant is registered")
	}
}

// TestRoundSkipsALockedTenant guards that a timer round does not wait on a
// tenant whose lock is held: the other tenants are checked at once.
func TestRoundSkipsALockedTenant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("held", "other")
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	held := (*r.entries.Load())["held"]
	held.op.Lock()
	defer held.op.Unlock()
	e.flaky.set("other", down)
	done := make(chan struct{})
	go func() {
		checkNow(r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the round waited on a tenant whose lock is held")
	}
	if st, _ := r.State("other"); st.State != master.StateUnreachable {
		t.Errorf("the other tenant is %s, want unreachable", st.State)
	}
}

// TestRemoveCancelsAnOpen guards that a removal ends the tenant's open in
// progress, through the entry's context, instead of waiting out its long
// deadline.
func TestRemoveCancelsAnOpen(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("h")
	e.flaky.set("h", down)
	cfg := e.config()
	cfg.OpenTimeout = 2 * time.Minute
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	n := e.flaky.opensOf("h")
	e.flaky.set("h", hang)
	r.checkAll()
	waitFor(t, "the hanging open to begin", func() bool { return e.flaky.opensOf("h") > n })
	begin := time.Now()
	if _, err := r.Remove(ctx, "h", e.audit("RemoveTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > 30*time.Second {
		t.Errorf("Remove took %v behind an open with a two-minute deadline", took)
	}
}
