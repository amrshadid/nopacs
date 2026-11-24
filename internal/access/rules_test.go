package access_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// sink collects the events an Access raises.
type sink struct {
	mu     sync.Mutex
	events []access.Event
}

// raise is the sink's access.Events.
func (s *sink) raise(e access.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// all returns the events raised so far.
func (s *sink) all() []access.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]access.Event(nil), s.events...)
}

// withEvents gives the env's Access the sink.
func withEvents(s *sink) func(*access.Config) {
	return func(c *access.Config) { c.Events = s.raise }
}

// keyWithRules makes a key with rules and grants directly in the master.
func (e *env) keyWithRules(rules master.KeyRules, grants ...master.Grant) testKey {
	e.t.Helper()
	k := e.key(t0.Add(24*time.Hour), grants...)
	if err := e.master.SetKeyRules(e.ctx, k.id, rules, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	return k
}

// networks parses a list of networks the test wrote.
func networks(t testing.TB, v string) settings.Networks {
	t.Helper()
	n, err := settings.ParseNetworks(v)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// from is facts naming method, from address.
func from(method, address string) access.Request {
	f := facts(method)
	f.RemoteAddress = address
	return f
}

// TestKeyNetworks guards a key's networks: a request from inside them is
// allowed, one from outside is refused exactly as a key that is no good,
// recorded with the rule's name and raised as an event, and a request on the
// local socket is never judged by them.
func TestKeyNetworks(t *testing.T) {
	s := &sink{}
	e := newEnv(t, withEvents(s))
	k := e.keyWithRules(master.KeyRules{Networks: networks(t, "10.0.0.0/8,2001:db8::/32")}, on(master.Installation, master.RoleReader))
	need := access.OnInstallation(master.RoleReader)
	for _, addr := range []string{"10.1.2.3:4000", "10.9.9.9", "[2001:db8::5]:443", "[::ffff:10.0.0.1]:80"} {
		if _, err := e.access.Check(e.ctx, access.APIKey(k.full), need, from("M", addr)); err != nil {
			t.Errorf("from %s: %v", addr, err)
		}
	}
	e.reset()
	for _, addr := range []string{"192.0.2.7:51000", "[2001:db9::1]:443", "", "not an address"} {
		_, err := e.access.Check(e.ctx, access.APIKey(k.full), need, from("M", addr))
		wantErr(t, "from "+addr, err, fault.Unauthenticated, access.MessageUnauthenticated)
		_, err = e.access.Authenticate(e.ctx, access.APIKey(k.full), from("M", addr))
		wantErr(t, "authenticated from "+addr, err, fault.Unauthenticated, access.MessageUnauthenticated)
	}
	_, entries := e.recorded()
	if len(entries) != 8 {
		t.Fatalf("%d entries, want 8", len(entries))
	}
	for _, en := range entries {
		if en.Outcome != master.OutcomeRefused || en.Reason != access.RuleReason(access.RuleKeyNetworks) || en.KeyID != k.id {
			t.Errorf("entry %+v", en)
		}
	}
	got := s.all()
	if len(got) != 8 || got[0].Kind != access.EventRefused || got[0].Rule != access.RuleKeyNetworks || got[0].KeyID != k.id || got[0].Address != "192.0.2.7:51000" {
		t.Errorf("events %+v", got)
	}
	onSocket := from("M", "")
	onSocket.Local = true
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), need, onSocket); err != nil {
		t.Errorf("a key on the local socket was judged by its networks: %v", err)
	}
}

// TestKeyClientCert guards a key's client certificate: the request must
// present the one the key names, compared whole.
func TestKeyClientCert(t *testing.T) {
	e := newEnv(t)
	fp := strings.Repeat("0f", 32)
	k := e.keyWithRules(master.KeyRules{ClientCert: fp}, on(master.Installation, master.RoleReader))
	need := access.OnInstallation(master.RoleReader)
	f := facts("M")
	f.ClientCert = fp
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), need, f); err != nil {
		t.Errorf("with its certificate: %v", err)
	}
	for _, other := range []string{"", strings.Repeat("0e", 32), strings.ToUpper(fp)} {
		f.ClientCert = other
		_, err := e.access.Check(e.ctx, access.APIKey(k.full), need, f)
		wantErr(t, "with "+other, err, fault.Unauthenticated, access.MessageUnauthenticated)
	}
	e.reset()
	f.ClientCert = ""
	_, _ = e.access.Check(e.ctx, access.APIKey(k.full), need, f)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != access.RuleReason(access.RuleKeyClientCert) {
		t.Errorf("entries %+v", entries)
	}
}

// TestKeyRulesReread guards that a key whose rules were narrowed after the
// front step accepted it is refused by the check that reads it again.
func TestKeyRulesReread(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleReader))
	accepted, err := e.access.Authenticate(e.ctx, access.APIKey(k.full), facts("M"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.master.SetKeyRules(e.ctx, k.id, master.KeyRules{Networks: networks(t, "10.0.0.0/8")}, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	_, err = e.access.Check(e.ctx, accepted.Reread(), access.OnInstallation(master.RoleReader), facts("M"))
	wantErr(t, "reread", err, fault.Unauthenticated, access.MessageUnauthenticated)
}

// TestTenantNetworks guards a tenant's networks: judged only for a caller
// the role check allowed, so one without a grant is told Denied as before;
// a request from outside them is refused as unauthenticated with the rule
// recorded; one from inside, or on the local socket, is allowed; a rule
// that cannot be read fails the request rather than allow it, named as the
// tenant's database's failure, not the master's; and a tenant's
// registration read by a caller the installation's role allows is judged by
// no tenant rule, so the installation's admin sees a tenant whose database
// stopped, while one allowed only by its grant on the tenant is judged.
func TestTenantNetworks(t *testing.T) {
	s := &sink{}
	var readErr error
	rule := func(_ context.Context, tr access.TenantRef) (settings.Networks, error) {
		if readErr != nil {
			return nil, readErr
		}
		if tr.ID == "t-lab" && tr.Name == "lab" {
			return networks(t, "10.0.0.0/8"), nil
		}
		return nil, nil
	}
	e := newEnv(t, withEvents(s), func(c *access.Config) { c.TenantNetworks = rule })
	k := e.key(t0.Add(time.Hour), on(master.TenantScope(e.lab.ID), master.RoleReader), on(master.TenantScope(e.clinic.ID), master.RoleReader))
	stranger := e.key(t0.Add(time.Hour))

	_, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant("lab", master.RoleReader), from("M", "192.0.2.7:1"))
	wantErr(t, "outside the lab's networks", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if got := s.all(); len(got) != 1 || got[0].Rule != access.RuleTenantNetworks || got[0].Tenant != "lab" {
		t.Errorf("events %+v", got)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant("lab", master.RoleReader), from("M", "10.0.0.9:1")); err != nil {
		t.Errorf("inside the lab's networks: %v", err)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant("clinic", master.RoleReader), from("M", "192.0.2.7:1")); err != nil {
		t.Errorf("a tenant without networks: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(stranger.full), access.OnTenant("lab", master.RoleReader), from("M", "192.0.2.7:1"))
	wantErr(t, "no grant", err, fault.Denied, access.MessageDenied)
	sock := from("M", "")
	sock.Local = true
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant("lab", master.RoleReader), sock); err != nil {
		t.Errorf("on the local socket: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.TenantRegistration("lab"), from("M", "192.0.2.7:1"))
	wantErr(t, "the registration by a tenant grant, outside the lab's networks", err, fault.Unauthenticated, access.MessageUnauthenticated)

	readErr = fault.New(fault.Unavailable, "down")
	admin := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleAdmin))
	if _, err := e.access.Check(e.ctx, access.APIKey(admin.full), access.TenantRegistration("lab"), from("M", "192.0.2.7:1")); err != nil {
		t.Errorf("the registration by the installation's admin, the tenant's database down: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant("lab", master.RoleReader), from("M", "10.0.0.9:1"))
	wantErr(t, "a rule that cannot be read", err, fault.Unavailable, "the tenant's database does not answer")
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.TenantRegistration("lab"), from("M", "10.0.0.9:1"))
	wantErr(t, "the registration by a tenant grant, the rule unreadable", err, fault.Unavailable, "the tenant's database does not answer")
}

// TestRecordRefusal guards the record of a refusal the server judged: in the
// master's log as refused with the rule's name, the key when known, and its
// event, a block's carrying when it ends.
func TestRecordRefusal(t *testing.T) {
	s := &sink{}
	e := newEnv(t, withEvents(s))
	e.reset()
	until := t0.Add(15 * time.Minute)
	err := e.access.RecordRefusal(facts("M"), "", access.RuleDeniedNetworks, false, time.Time{})
	wantErr(t, "RecordRefusal", err, fault.Unauthenticated, access.MessageUnauthenticated)
	_ = e.access.RecordRefusal(facts("M"), "kid123", access.RuleKeyBlocked, true, until)
	_, entries := e.recorded()
	if len(entries) != 2 || entries[0].Reason != access.RuleReason(access.RuleDeniedNetworks) || entries[0].KeyID != "" ||
		entries[1].KeyID != "kid123" || entries[1].Outcome != master.OutcomeRefused {
		t.Fatalf("entries %+v", entries)
	}
	got := s.all()
	if len(got) != 2 || got[0].Kind != access.EventRefused || got[1].Kind != access.EventBlocked || !got[1].Until.Equal(until) {
		t.Errorf("events %+v", got)
	}
}

// TestLastAddress guards that a key's use records the address of the
// request beside its time.
func TestLastAddress(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleReader))
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleReader), from("M", "198.51.100.4:7000")); err != nil {
		t.Fatal(err)
	}
	if err := e.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := e.master.Key(e.ctx, k.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAddress != "198.51.100.4:7000" || !got.LastUsed.Equal(t0) {
		t.Errorf("last use %v from %q", got.LastUsed, got.LastAddress)
	}
}

// TestRefusedRequestLeavesLastUse guards that only an allowed request marks
// a key used: a good key refused for its grants, or by Authenticate's
// later check, leaves the key's last use and address as they were, so a
// refused caller cannot overwrite where the key was last really used from.
func TestRefusedRequestLeavesLastUse(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleReader))
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleAdmin), from("M", "203.0.113.66:1")); fault.KindOf(err) != fault.Denied {
		t.Fatalf("a reader asking for admin: %v, want Denied", err)
	}
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(k.full), from("M", "203.0.113.67:1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.access.Check(e.ctx, cred, access.OnInstallation(master.RoleAdmin), from("M", "203.0.113.67:1")); fault.KindOf(err) != fault.Denied {
		t.Fatalf("an authenticated reader asking for admin: %v, want Denied", err)
	}
	if err := e.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := e.master.Key(e.ctx, k.id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastUsed.IsZero() || got.LastAddress != "" {
		t.Errorf("refused requests marked the key used at %v from %q", got.LastUsed, got.LastAddress)
	}
}

// TestExpiringKeys guards the keys about to expire: those that work and
// expire within the warning, never a revoked, expired or never-expiring
// one; and the worker raises one event per key, not again within the day.
func TestExpiringKeys(t *testing.T) {
	s := &sink{}
	warn := 7 * 24 * time.Hour
	e := newEnv(t, withEvents(s), func(c *access.Config) {
		c.ExpiryWarning = func(context.Context) (time.Duration, error) { return warn, nil }
		c.ExpiryInterval = 10 * time.Millisecond
	})
	soon := e.key(t0.Add(48 * time.Hour))
	e.key(t0.Add(30 * 24 * time.Hour))
	e.key(time.Time{})
	e.key(t0.Add(-time.Hour))
	gone := e.key(t0.Add(time.Hour))
	if _, err := e.master.RevokeKey(e.ctx, gone.id, t0, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	keys, err := e.access.ExpiringKeys(e.ctx, warn)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != soon.id || keys[0].Hash != nil {
		t.Fatalf("expiring %+v, want only %s", keys, soon.id)
	}
	count := func() int {
		n := 0
		for _, ev := range s.all() {
			if ev.Kind == access.EventKeyExpiring && ev.KeyID == soon.id {
				n++
			}
		}
		return n
	}
	// The first round raises the event; three rounds more, within the day,
	// raise nothing. Rounds are waited for, not time, so a slow machine
	// cannot read the count before the first round or after too few.
	deadline := time.Now().Add(5 * time.Second)
	for count() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for start := access.ExpiryRounds(e.access); access.ExpiryRounds(e.access) < start+3 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d expiring events for one key within the day, want 1", n)
	}
	e.clock.Set(t0.Add(25 * time.Hour))
	deadline = time.Now().Add(5 * time.Second)
	for count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count(); n != 2 {
		t.Errorf("%d expiring events after a day, want 2", n)
	}
}

// TestExpiryWorkerFailure guards that a warning that cannot be read raises
// nothing and is logged.
func TestExpiryWorkerFailure(t *testing.T) {
	s := &sink{}
	e := newEnv(t, withEvents(s), func(c *access.Config) {
		c.ExpiryWarning = func(context.Context) (time.Duration, error) { return 0, errors.New("down") }
		c.ExpiryInterval = time.Hour
	})
	e.key(t0.Add(time.Hour))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.logText(), "about to expire were not read") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(e.logText(), "about to expire were not read") {
		t.Error("the failure was not logged")
	}
	if len(s.all()) != 0 {
		t.Errorf("events %+v", s.all())
	}
}

// logText returns what the env's log holds.
func (e *env) logText() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.log.String()
}

// TestCachedTenantNetworks guards the cache of tenants' networks: an answer
// is kept for its time per tenant, read again after it, and a failure is
// not kept.
func TestCachedTenantNetworks(t *testing.T) {
	reads := 0
	var fail error
	now := t0
	c := access.CachedTenantNetworks(func(_ context.Context, tr access.TenantRef) (settings.Networks, error) {
		reads++
		if fail != nil {
			return nil, fail
		}
		return networks(t, "10.0.0.0/8"), nil
	}, 5*time.Second, func() time.Time { return now })
	lab := access.TenantRef{ID: "t1", Name: "lab"}
	for range 3 {
		if n, err := c(context.Background(), lab); err != nil || n.String() != "10.0.0.0/8" {
			t.Fatalf("%v, %v", n, err)
		}
	}
	if reads != 1 {
		t.Errorf("%d reads within the time, want 1", reads)
	}
	_, _ = c(context.Background(), access.TenantRef{ID: "t2", Name: "clinic"})
	if reads != 2 {
		t.Errorf("another tenant was not read: %d reads", reads)
	}
	now = now.Add(5 * time.Second)
	fail = errors.New("down")
	if _, err := c(context.Background(), lab); err == nil {
		t.Error("a failed read after the time was answered from the cache")
	}
	fail = nil
	if _, err := c(context.Background(), lab); err != nil || reads != 4 {
		t.Errorf("%v after %d reads, want a fresh read", err, reads)
	}
}

// TestLogEvents guards the log sink: one warning line per event with its
// fields, and none empty.
func TestLogEvents(t *testing.T) {
	var b strings.Builder
	log := slog.New(slog.NewTextHandler(&b, nil))
	access.LogEvents(log)(access.Event{Kind: access.EventRefused, Rule: access.RuleKeyNetworks, KeyID: "k1", Address: "192.0.2.1:9"})
	got := b.String()
	for _, want := range []string{"level=WARN", `msg="access event"`, "event=access.refused", "rule=key-networks", "key_id=k1", "remote=192.0.2.1:9"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "tenant=") || strings.Contains(got, "until=") {
		t.Errorf("%q holds an empty field", got)
	}
}
