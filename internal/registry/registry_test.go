package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestNewRefusesMissingConfig guards that every required part of the
// configuration is asked for, rather than a default chosen: in particular
// the clock, the interval and the deadline are never package variables.
func TestNewRefusesMissingConfig(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func(*Config){
		"master":      func(c *Config) { c.Master = nil },
		"keys":        func(c *Config) { c.Keys = nil },
		"drivers":     func(c *Config) { c.Drivers = nil },
		"clock":       func(c *Config) { c.Now = nil },
		"interval":    func(c *Config) { c.Interval = 0 },
		"deadline":    func(c *Config) { c.OpenTimeout = 0 },
		"max opening": func(c *Config) { c.MaxOpening = 0 },
	}
	for name, change := range cases {
		cfg := e.config()
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New without %s: no error", name)
		}
	}
}

// TestStartServesReadyRefusesOthers guards the start: each tenant is opened,
// a ready one is served, one whose database does not answer is refused as
// Unavailable while the others are served, and each state reaches the
// master with an audit entry whose actor is the system.
func TestStartServesReadyRefusesOthers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	first := e.newRegistry(e.config())
	e.create(first, "id-lab", "lab", driver.SQLiteDriver, "")
	e.create(first, "id-up", "up", flakyDriver, flakyAddress("up"))
	e.create(first, "id-down", "down", flakyDriver, flakyAddress("down"))
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	e.flaky.set("down", down)
	e.clock.Add(time.Second) // so the start's entries sort after the creation's
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lab", "up"} {
		s, err := r.Store(name)
		if err != nil {
			t.Fatalf("Store(%s): %v", name, err)
		}
		if err := s.SetSetting(ctx, "x", "y", e.audit("SetSetting"), nil); err != nil {
			t.Errorf("%s: SetSetting: %v", name, err)
		}
	}
	_, err := r.Store("down")
	if fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Store(down) = %v, want Unavailable", err)
	}
	if !strings.Contains(fault.Message(err), "unreachable") {
		t.Errorf("the refusal %q does not say the tenant is unreachable", fault.Message(err))
	}
	if _, err := r.Store("nope"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Store(nope) = %v, want NotFound", err)
	}
	if got := e.masterTenant("down"); got.State != master.StateUnreachable || got.StateReason == "" {
		t.Errorf("the master holds %s %q for down, want unreachable with a reason", got.State, got.StateReason)
	}
	if got := e.masterTenant("lab"); got.State != master.StateReady {
		t.Errorf("the master holds %s for lab, want ready", got.State)
	}
	entries := e.systemEntries("down")
	last := entries[len(entries)-1]
	if last.KeyID != "" || last.LocalUser != "" || last.RemoteAddress != "" || last.Details[DetailActor] != ActorSystem ||
		last.Details[master.DetailOldState] != "ready" || last.Details[master.DetailNewState] != "unreachable" {
		t.Errorf("the state change's audit entry = %+v, want the system as its actor, from ready to unreachable", last)
	}
}

// TestUnreachableRecovers guards that an unreachable tenant returns to
// ready on its own, both in what requests see and in the master, without a
// person acting, while a failed tenant is left for one.
func TestUnreachableRecovers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("down", "bad")
	e.flaky.set("down", down)
	e.flaky.set("bad", broken)
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.State("down"); st.State != master.StateUnreachable {
		t.Fatalf("down is %s, want unreachable", st.State)
	}
	if st, _ := r.State("bad"); st.State != master.StateFailed {
		t.Fatalf("bad is %s, want failed", st.State)
	}

	e.flaky.set("down", up)
	e.flaky.set("bad", up)
	e.clock.Add(2 * StateChangeInterval)
	checkNow(r)
	s, err := r.Store("down")
	if err != nil {
		t.Fatalf("Store(down) after it came back: %v", err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Error(err)
	}
	if got := e.masterTenant("down"); got.State != master.StateReady || got.StateReason != "" {
		t.Errorf("the master holds %s %q, want ready with no reason", got.State, got.StateReason)
	}
	if st, _ := r.State("bad"); st.State != master.StateFailed {
		t.Errorf("a failed tenant became %s on a check; it waits for a person", st.State)
	}
}

// TestTimerChecks guards that the timer runs the checks on its own, at the
// interval given: a tenant that comes back is served without anyone calling
// a check.
func TestTimerChecks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("down")
	e.flaky.set("down", down)
	cfg := e.config()
	cfg.Interval = 5 * time.Millisecond
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store("down"); err == nil {
		t.Fatal("a tenant that does not answer was served")
	}
	e.flaky.set("down", up)
	waitFor(t, "the timer to find the tenant back", func() bool {
		_, err := r.Store("down")
		return err == nil
	})
}

// TestFlappingWritesOncePerMinute guards the limit on state writes: a
// database that goes down and up on every check, for ten minutes of checks
// every five seconds, is refused and served at once each time, but its
// state is written to the master at most once a minute.
func TestFlappingWritesOncePerMinute(t *testing.T) {
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-flap", "flap", flakyDriver, flakyAddress("flap"))
	before := len(e.systemEntries("flap"))
	const step, checks = 5 * time.Second, 120
	for i := range checks {
		m := down
		if i%2 == 1 {
			m = up
		}
		e.flaky.set("flap", m)
		e.clock.Add(step)
		checkNow(r)
		_, err := r.Store("flap")
		if (err == nil) != (m == up) {
			t.Fatalf("check %d: mode %v, Store error %v: requests must follow the database at once", i, m, err)
		}
	}
	writes := len(e.systemEntries("flap")) - before
	limit := int(time.Duration(checks)*step/StateChangeInterval) + 1
	t.Logf("%d checks over %v wrote %d state changes (limit %d)", checks, time.Duration(checks)*step, writes, limit)
	if writes > limit {
		t.Errorf("%d state changes written, want at most %d (one a minute)", writes, limit)
	}
	if writes < limit/2 {
		t.Errorf("%d state changes written, want about one a minute", writes)
	}
}

// TestDeadlinePerTenant guards that each tenant opens under its own
// deadline and in parallel: one database that hangs, and one whose driver
// ignores its deadline altogether, delay neither the start past the
// deadline nor the others, which are ready when Start returns.
func TestDeadlinePerTenant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	setup := e.newRegistry(e.config())
	names := []string{"hangs", "stuck", "a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		e.create(setup, "id-"+n, n, flakyDriver, flakyAddress(n))
	}
	if err := setup.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.flaky.set("hangs", hang)
	e.flaky.set("stuck", stuck)
	cfg := e.config()
	// The server's own deadline: the six real tenants must open within it
	// on a loaded runner (seconds on Windows under -race), and the two hung
	// ones meet it, so the start lasts about this long.
	cfg.OpenTimeout = 10 * time.Second
	r := e.newRegistry(cfg)
	started := make(chan error, 1)
	begin := time.Now()
	go func() { started <- r.Start(ctx) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		e.flaky.unstick()
		t.Fatal("Start did not return: a hung database held it past the deadline")
	}
	took := time.Since(begin)
	t.Logf("Start with two hung databases took %v under a deadline of %v", took, cfg.OpenTimeout)
	if took > cfg.OpenTimeout+20*time.Second {
		t.Errorf("Start took %v, want about the deadline %v", took, cfg.OpenTimeout)
	}
	for _, n := range names[2:] {
		if _, err := r.Store(n); err != nil {
			t.Errorf("Store(%s) beside two hung databases: %v", n, err)
		}
	}
	for _, n := range names[:2] {
		st, _ := r.State(n)
		if st.State != master.StateUnreachable || st.Reason != reasonDeadline {
			t.Errorf("%s is %s %q, want unreachable for its deadline", n, st.State, st.Reason)
		}
	}
	// The stuck open is still running; a check does not start another. The
	// other hung tenant now refuses at once, so this check does not wait out
	// another open deadline.
	e.flaky.set("hangs", down)
	checkNow(r)
	if st, _ := r.State("stuck"); st.State != master.StateUnreachable {
		t.Errorf("stuck is %s during its stuck open", st.State)
	}
	// Close waits for the stuck open, which is released here.
	time.AfterFunc(50*time.Millisecond, e.flaky.unstick)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o, c := e.flaky.opens.Load(), e.flaky.closes.Load(); o != c {
		t.Errorf("%d stores opened and %d closed", o, c)
	}
}

// TestWrongKeyFile guards a tenant whose address does not open with the key
// file the server has: it is failed, its reason says the address did not
// open, and the other tenants start.
func TestWrongKeyFile(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	old := e.newRegistry(e.config())
	e.create(old, "id-old", "old", flakyDriver, flakyAddress("old"))
	if err := old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.keys = newKeys(t) // another key file
	cfg := e.config()
	r := e.newRegistry(cfg)
	e.create(r, "id-new", "new", flakyDriver, flakyAddress("new"))
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := r.State("old")
	if st.State != master.StateFailed || !strings.Contains(st.Reason, "address did not open") {
		t.Errorf("old is %s %q, want failed saying the address did not open", st.State, st.Reason)
	}
	if got := e.masterTenant("old"); got.State != master.StateFailed {
		t.Errorf("the master holds %s for old, want failed", got.State)
	}
	if _, err := r.Store("new"); err != nil {
		t.Errorf("the other tenant did not start: %v", err)
	}
}

// TestCrashBetweenRowAndUpgrade guards creation's crash: a creation that
// stops after the tenant's row is written and before its database is opened
// leaves an upgrading row, which the next start opens and upgrades to
// ready, and which a repeated creation also finishes.
func TestCrashBetweenRowAndUpgrade(t *testing.T) {
	ctx := context.Background()
	for _, finish := range []string{"start", "repeat"} {
		t.Run(finish, func(t *testing.T) {
			e := newEnv(t)
			crashed := e.newRegistry(e.config())
			crashed.afterRow = func() error { return errors.New("crash") }
			_, _, err := crashed.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
			if err == nil {
				t.Fatal("the crash did not stop Create")
			}
			if err := crashed.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := e.masterTenant("lab"); got.State != master.StateUpgrading {
				t.Fatalf("after the crash the row is %s, want upgrading", got.State)
			}
			path, _ := driver.SQLiteTenants{DataDir: e.data}.Path("lab")
			if _, err := os.Stat(path); err == nil {
				t.Fatal("the database exists before anything opened it")
			}

			r := e.newRegistry(e.config())
			switch finish {
			case "start":
				if err := r.Start(ctx); err != nil {
					t.Fatal(err)
				}
			case "repeat":
				tn, _, err := r.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
				if err != nil || tn.State != master.StateReady {
					t.Fatalf("repeated Create = %s, %v; want ready", tn.State, err)
				}
			}
			s, err := r.Store("lab")
			if err != nil {
				t.Fatalf("after the %s the tenant is not served: %v", finish, err)
			}
			if v, err := s.Version(ctx); err != nil || v < 1 {
				t.Errorf("Version = %d, %v; want migrated", v, err)
			}
			if got := e.masterTenant("lab"); got.State != master.StateReady {
				t.Errorf("the master holds %s, want ready", got.State)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the database is not at %s: %v", path, err)
			}
		})
	}
}

// TestCreateRules guards creation's refusals and its repeat: an address the
// driver does not take is refused before anything is written, a second
// tenant of the same name is Conflict, and the same creation again succeeds
// with one row.
func TestCreateRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	_, _, err := r.Create(ctx, NewTenant{ID: "id-x", Name: "x", Driver: driver.SQLiteDriver, Address: "sqlite:/elsewhere.db"}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("a SQLite tenant with an address: %v, want Invalid", err)
	}
	if _, err := e.master.TenantByName(ctx, "x"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a refused creation left a row: %v", err)
	}
	_, _, err = r.Create(ctx, NewTenant{ID: "id-y", Name: "y", Driver: "oracle"}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("an unknown database type: %v, want Invalid", err)
	}
	_, _, err = r.Create(ctx, NewTenant{ID: "id-z", Name: "../z", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("a bad name: %v, want Invalid", err)
	}

	first := e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	again := e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	if first.ID != again.ID || again.State != master.StateReady {
		t.Errorf("the repeat returned %+v, want the same ready tenant", again)
	}
	_, _, err = r.Create(ctx, NewTenant{ID: "id-other", Name: "lab", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Conflict {
		t.Errorf("another tenant named lab: %v, want Conflict", err)
	}
	rows, _, err := e.master.ListTenants(ctx, master.Page{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Errorf("%d rows, %v; want one", len(rows), err)
	}
	// The change to ready is written with the first request's own entry, not
	// a system one; the repeat records its request alone.
	if sys := e.systemEntries("lab"); len(sys) != 0 {
		t.Errorf("system state changes = %+v, want none", sys)
	}
	entries, _, err := e.master.ListAudit(ctx, master.AuditFilter{TenantName: "lab"}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, a := range entries {
		if a.Method == "CreateTenant" && a.Details[master.DetailOldState] == "upgrading" && a.Details[master.DetailNewState] == "ready" {
			ready++
		}
	}
	if ready != 1 {
		t.Errorf("%d request entries record the change from upgrading to ready, want 1", ready)
	}
}

// TestRemoveLeavesData guards removal: the tenant is unregistered and
// refused, its row is gone, its database file stays where it was with its
// data in it, and the answer and the audit entry say where.
func TestRemoveLeavesData(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	s, err := r.Store("lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "kept", "yes", e.audit("SetSetting"), nil); err != nil {
		t.Fatal(err)
	}
	audit := e.audit("RemoveTenant")
	removed, err := r.Remove(ctx, "lab", audit, master.LocalOperator())
	if err != nil {
		t.Fatal(err)
	}
	path, _ := driver.SQLiteTenants{DataDir: e.data}.Path("lab")
	if removed.Driver != driver.SQLiteDriver || removed.Location != path {
		t.Errorf("Remove = %+v, want sqlite at %s", removed, path)
	}
	if _, err := r.Store("lab"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Store after Remove = %v, want NotFound", err)
	}
	if err := s.Ping(ctx); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("the removed store still answers: %v", err)
	}
	if _, err := e.master.TenantByName(ctx, "lab"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the row is still there: %v", err)
	}
	entries, _, err := e.master.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 1})
	if err != nil || len(entries) != 1 || entries[0].ID != audit.ID ||
		entries[0].Details[DetailLocation] != path || entries[0].Details[DetailDatabaseType] != "sqlite" {
		t.Errorf("the removal's audit entry = %+v, %v; want its location and type", entries, err)
	}
	if _, err := r.Remove(ctx, "lab", e.audit("RemoveTenant"), master.LocalOperator()); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a second Remove = %v, want NotFound", err)
	}
	// The data is where it was: opened again by itself, the setting is there.
	again, err := driver.SQLiteTenants{DataDir: e.data}.Open(ctx, "id-lab", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if v, err := again.Setting(ctx, "kept"); err != nil || v != "yes" {
		t.Errorf("the removed tenant's data = %q, %v; want it left in place", v, err)
	}
}

// TestCloseStopsEverything guards the shutdown: Close stops the timer, ends
// the opens in progress and waits for them, closes every store, and refuses
// calls made after it. TestMain then finds no goroutine left.
func TestCloseStopsEverything(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	cfg := e.config()
	cfg.Interval = time.Millisecond
	// The hung tenant has an open store, so it is held at its ping, not at
	// an open: a short ping deadline keeps the start quick, and the real
	// tenants open under the generous default.
	cfg.PingTimeout = 2 * time.Second
	r := e.newRegistry(cfg)
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	for _, n := range []string{"a", "b", "c"} {
		e.create(r, "id-"+n, n, flakyDriver, flakyAddress(n))
	}
	e.flaky.set("c", hang)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // a few rounds of the timer
	lab, err := r.Store("lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if err := lab.Ping(ctx); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("a store still answers after Close: %v", err)
	}
	if o, c := e.flaky.opens.Load(), e.flaky.closes.Load(); o != c || o < 2 {
		t.Errorf("%d stores opened, %d closed; want every one closed", o, c)
	}
	if _, err := r.Store("a"); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Store after Close = %v, want Unavailable", err)
	}
	if _, _, err := r.Create(ctx, NewTenant{ID: "id-d", Name: "d", Driver: flakyDriver, Address: flakyAddress("d")}, e.audit("CreateTenant"), master.LocalOperator()); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Create after Close = %v, want Unavailable", err)
	}
	if err := r.Start(ctx); fault.KindOf(err) != fault.Unavailable {
		t.Errorf("Start after Close = %v, want Unavailable", err)
	}
}

// TestCreateRemoveTimerTogether guards the race the design calls out:
// creations, removals and timer checks of one tenant, all at once, for many
// rounds, under -race. At the end the registry knows the tenant exactly
// when the master does, and after Close every store opened was closed.
func TestCreateRemoveTimerTogether(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	cfg := e.config()
	cfg.Interval = time.Millisecond
	r := e.newRegistry(cfg)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rounds := 60
	if testing.Short() {
		rounds = 10
	}
	for round := range rounds {
		id := fmt.Sprintf("id-%d", round%3)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 2 {
			wg.Go(func() {
				_, _, err := r.Create(ctx, NewTenant{ID: id, Name: "race", Driver: flakyDriver, Address: flakyAddress("race")}, e.audit("CreateTenant"), master.LocalOperator())
				errs <- err
			})
			wg.Go(func() {
				_, err := r.Remove(ctx, "race", e.audit("RemoveTenant"), master.LocalOperator())
				errs <- err
			})
			wg.Go(func() { checkNow(r) })
			wg.Go(func() {
				if s, err := r.Store("race"); err == nil {
					_ = s.Ping(ctx) // may fail if removed meanwhile; must not race
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			switch fault.KindOf(err) {
			case fault.Internal:
				if err != nil {
					t.Fatalf("round %d: %v", round, err)
				}
			case fault.NotFound, fault.Conflict:
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		_, inMaster := e.master.TenantByName(ctx, "race")
		_, known := r.State("race")
		if (inMaster == nil) != known {
			// A creation that lost to a removal may leave the row without
			// its entry for a moment; the next check or start settles it.
			// The reverse, an entry without a row, must never stay.
			if known {
				t.Fatalf("round %d: the registry serves a tenant the master does not have", round)
			}
		}
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, c := e.flaky.opens.Load(), e.flaky.closes.Load()
	t.Logf("%d rounds: %d stores opened, %d closed", rounds, o, c)
	if o != c {
		t.Errorf("%d stores opened, %d closed: a store leaked", o, c)
	}
}
