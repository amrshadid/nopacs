package access_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// TestCheckTable guards the whole decision over every credential state and
// every sort of requirement, on the real SQLite master: the kind and exact
// message the caller gets, and the audit entry written for a refusal (its
// outcome, its real reason, the key id it names) or held back for the caller
// when the request is allowed.
func TestCheckTable(t *testing.T) {
	e := newEnv(t)
	good := e.key(t0.Add(time.Hour), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	instReader := e.key(time.Time{}, on(master.Installation, master.RoleReader))
	instAdmin := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	revoked := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	if _, err := e.master.RevokeKey(e.ctx, revoked.id, t0, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	expired := e.key(t0.Add(-time.Second), on(master.Installation, master.RoleAdmin))
	other, err := secret.MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	unknown := other.Key()
	// The stored id with another key's secret.
	wrongSecret := "nopacs_" + good.id + "_" + other.Key()[len("nopacs_")+13:]

	const (
		unauth = access.MessageUnauthenticated
		denied = access.MessageDenied
		nf     = access.MessageNoTenant
	)
	tests := []struct {
		name    string
		cred    access.Credential
		req     access.Requirement
		kind    fault.Kind // Internal with ok true means allowed
		ok      bool
		message string
		outcome master.Outcome
		reason  string
		keyID   string
		tenant  string
	}{
		{"no key", access.Credential{}, access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "no key", "", ""},
		{"empty key", access.APIKey(""), access.OnInstallation(master.RoleReader), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "no key", "", ""},
		{"malformed", access.APIKey("nopacs_short"), access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "malformed key", "", ""},
		{"bearer word left in", access.APIKey("Bearer " + good.full), access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "malformed key", "", ""},
		{"unknown id", access.APIKey(unknown), access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "unknown key", other.ID, ""},
		{"wrong secret", access.APIKey(wrongSecret), access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "wrong secret", good.id, ""},
		{"revoked", access.APIKey(revoked.full), access.OnInstallation(master.RoleReader), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "key revoked", revoked.id, ""},
		{"expired", access.APIKey(expired.full), access.OnInstallation(master.RoleReader), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "key expired", expired.id, ""},
		{"unauthenticated before the tenant", access.APIKey(revoked.full), access.OnTenant("nosuch", master.RoleReader), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "key revoked", revoked.id, "nosuch"},
		{"local without user", access.LocalOperator(""), access.Authenticated(), fault.Unauthenticated, false, unauth, master.OutcomeRefused, "local caller without a usable user id", "", ""},

		{"good, authenticated", access.APIKey(good.full), access.Authenticated(), 0, true, "", "", "", good.id, ""},
		{"good, its tenant", access.APIKey(good.full), access.OnTenant("lab", master.RoleWriter), 0, true, "", "", "", good.id, "lab"},
		{"good, more than it holds", access.APIKey(good.full), access.OnTenant("lab", master.RoleOperator), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", good.id, "lab"},
		{"good, another tenant", access.APIKey(good.full), access.OnTenant("clinic", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", good.id, "clinic"},
		{"good, no tenant", access.APIKey(good.full), access.OnTenant("nosuch", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no such tenant", good.id, "nosuch"},
		{"good, a name no tenant can have", access.APIKey(good.full), access.OnTenant("Not/A Name", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no such tenant", good.id, `"Not/A Name"`},
		{"good, the installation", access.APIKey(good.full), access.OnInstallation(master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", good.id, ""},
		{"good, its tenant's registration", access.APIKey(good.full), access.TenantRegistration("lab"), 0, true, "", "", "", good.id, "lab"},
		{"good, another's registration", access.APIKey(good.full), access.TenantRegistration("clinic"), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", good.id, "clinic"},

		{"installation reader, installation", access.APIKey(instReader.full), access.OnInstallation(master.RoleReader), 0, true, "", "", "", instReader.id, ""},
		{"installation reader, any registration", access.APIKey(instReader.full), access.TenantRegistration("clinic"), 0, true, "", "", "", instReader.id, "clinic"},
		{"installation reader, inside a tenant", access.APIKey(instReader.full), access.OnTenant("clinic", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", instReader.id, "clinic"},
		{"installation reader, no tenant", access.APIKey(instReader.full), access.OnTenant("nosuch", master.RoleReader), fault.NotFound, false, nf, master.OutcomeFailed, "no such tenant", instReader.id, "nosuch"},
		{"installation reader, a name no tenant can have", access.APIKey(instReader.full), access.OnTenant("Not/A Name", master.RoleReader), fault.NotFound, false, nf, master.OutcomeFailed, "no such tenant", instReader.id, `"Not/A Name"`},
		{"installation reader, a Windows device name", access.APIKey(instReader.full), access.OnTenant("con", master.RoleReader), fault.NotFound, false, nf, master.OutcomeFailed, "no such tenant", instReader.id, `"con"`},
		{"good, a Windows device name", access.APIKey(good.full), access.OnTenant("lpt1", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no such tenant", good.id, `"lpt1"`},
		{"installation reader, no registration", access.APIKey(instReader.full), access.TenantRegistration("nosuch"), fault.NotFound, false, nf, master.OutcomeFailed, "no such tenant", instReader.id, "nosuch"},
		{"installation admin, inside a tenant", access.APIKey(instAdmin.full), access.OnTenant("lab", master.RoleReader), fault.Denied, false, denied, master.OutcomeRefused, "no sufficient grant", instAdmin.id, "lab"},
		{"installation admin, admin", access.APIKey(instAdmin.full), access.OnInstallation(master.RoleAdmin), 0, true, "", "", "", instAdmin.id, ""},

		{"local, admin on the installation", local, access.OnInstallation(master.RoleAdmin), 0, true, "", "", "", "", ""},
		{"local, admin on a tenant", local, access.OnTenant("clinic", master.RoleAdmin), 0, true, "", "", "", "", "clinic"},
		{"local, no tenant", local, access.OnTenant("nosuch", master.RoleAdmin), fault.NotFound, false, nf, master.OutcomeFailed, "no such tenant", "", "nosuch"},

		{"zero requirement", local, access.Requirement{}, fault.Internal, false, "internal error", master.OutcomeFailed, "server error: the check was called without a valid requirement or method", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.reset()
			d, err := e.access.Check(e.ctx, tc.cred, tc.req, facts("/nopacs.v1.Test/Method"))
			_, entries := e.recorded()
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("an allowed check wrote %d entries; the caller writes it", len(entries))
				}
				got := d.Entry(master.OutcomeDone, "", access.Facts{})
				if got.ID == "" || got.Method != "/nopacs.v1.Test/Method" || got.KeyID != tc.keyID || got.TenantName != tc.tenant || got.RemoteAddress != "192.0.2.7:51000" || got.RequestID != "req-1" || !got.Time.Equal(t0) {
					t.Fatalf("entry handed back: %+v", got)
				}
				if tc.cred == local && (got.LocalUser != "501" || !d.Principal().IsLocalOperator()) {
					t.Fatalf("local operator: entry %+v, principal %+v", got, d.Principal())
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("the entry handed back is not one the master takes: %v", err)
				}
				return
			}
			wantErr(t, "check", err, tc.kind, tc.message)
			if err.Error() != tc.message {
				t.Fatalf("Error() is %q: a refusal carries nothing beyond its fixed message", err.Error())
			}
			if !reflect.DeepEqual(d, access.Decision{}) {
				t.Fatalf("a refusal returned a decision: %+v", d)
			}
			if len(entries) != 1 {
				t.Fatalf("%d audit entries written, want 1", len(entries))
			}
			got := entries[0]
			if got.Outcome != tc.outcome || got.Reason != tc.reason || got.KeyID != tc.keyID || got.TenantName != tc.tenant {
				t.Fatalf("entry: outcome %q reason %q key %q tenant %q; want %q %q %q %q",
					got.Outcome, got.Reason, got.KeyID, got.TenantName, tc.outcome, tc.reason, tc.keyID, tc.tenant)
			}
		})
	}

	// The entries are in the SQLite master, not only handed to it.
	e.flush()
	stored, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, s := range stored {
		reasons = append(reasons, s.Reason)
	}
	for _, want := range []string{"wrong secret", "key revoked", "key expired", "unknown key", "no such tenant", "no sufficient grant"} {
		if !strings.Contains(strings.Join(reasons, "|"), want) {
			t.Errorf("no refusal with reason %q in the SQLite master", want)
		}
	}
}

// TestDeniedSamePath guards the two Denied cases: a good key naming a tenant
// it has no grant on, and a good key naming a tenant that does not exist,
// give byte-identical errors, read the same things from the store in the same
// order, and queue one entry each that differs only in id, tenant name and
// reason, so a key cannot learn which tenants exist. The key was marked used
// a moment before, so neither asks for a mark: the mark is decided by the key
// both read, not by the tenant. Timing equality follows from the same calls;
// it is argued, not measured.
func TestDeniedSamePath(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.TenantScope(e.clinic.ID), master.RoleAdmin))
	if _, err := e.master.MarkKeyUsed(e.ctx, k.id, t0.Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}
	run := func(tenant string) ([]string, master.AuditEntry, error) {
		e.reset()
		_, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnTenant(tenant, master.RoleReader), facts("/nopacs.v1.Test/Read"))
		calls, entries := e.recorded()
		if len(entries) != 1 {
			t.Fatalf("%s: %d entries", tenant, len(entries))
		}
		return calls, entries[0], err
	}
	callsNoGrant, entryNoGrant, errNoGrant := run("lab")
	callsNoTenant, entryNoTenant, errNoTenant := run("nosuch")

	for _, err := range []error{errNoGrant, errNoTenant} {
		wantErr(t, "denied", err, fault.Denied, access.MessageDenied)
	}
	if errNoGrant.Error() != errNoTenant.Error() || fault.Message(errNoGrant) != fault.Message(errNoTenant) {
		t.Fatalf("the two answers differ: %q and %q", errNoGrant.Error(), errNoTenant.Error())
	}
	if !reflect.DeepEqual(callsNoGrant, callsNoTenant) {
		t.Fatalf("the two paths differ:\nno grant:  %v\nno tenant: %v", callsNoGrant, callsNoTenant)
	}
	want := []string{"Key", "RolesFor"}
	if !reflect.DeepEqual(callsNoGrant, want) {
		t.Fatalf("calls %v, want %v", callsNoGrant, want)
	}
	t.Logf("both Denied paths: %v", callsNoGrant)
	// Both ask RolesFor by the name the request gave, never about the
	// installation only, so the store runs the same statement for both.
	e.store.mu.Lock()
	args := append([]string(nil), e.store.tenantArgs...)
	e.store.mu.Unlock()
	if want := []string{"lab", "nosuch"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("RolesFor tenant names %q, want %q", args, want)
	}
	a, b := entryNoGrant, entryNoTenant
	a.ID, b.ID, a.TenantName, b.TenantName, a.Reason, b.Reason = "", "", "", "", "", ""
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the two entries differ beyond id, tenant and reason:\n%+v\n%+v", a, b)
	}
	if err := e.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.store.marks.Load(); got != 0 {
		t.Fatalf("%d last-use marks for a key marked a second before, want none", got)
	}
}

// TestRevokedRefusedNext guards that there is no cache: a key that works is
// refused on its very next request once revoked, and a grant removed gives
// nothing on the next request.
func TestRevokedRefusedNext(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleReader))
	need := access.OnTenant("lab", master.RoleReader)
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), need, facts("m")); err != nil {
		t.Fatalf("before: %v", err)
	}
	if _, err := e.access.RemoveGrant(e.ctx, e.admin("RemoveGrant"), k.id, access.TenantNamed("lab")); err != nil {
		t.Fatal(err)
	}
	_, err := e.access.Check(e.ctx, access.APIKey(k.full), need, facts("m"))
	wantErr(t, "after the grant is removed", err, fault.Denied, access.MessageDenied)

	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.Authenticated(), facts("m")); err != nil {
		t.Fatalf("before revoking: %v", err)
	}
	if _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.id); err != nil {
		t.Fatal(err)
	}
	e.reset()
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.Authenticated(), facts("m"))
	wantErr(t, "after revoking", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != "key revoked" {
		t.Fatalf("entries %+v", entries)
	}
}

// TestExpiry guards the expiry: a key works until the instant before its
// expiry and is refused at it and after, with the clock moved by the test,
// not slept.
func TestExpiry(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleReader))
	for _, tc := range []struct {
		at time.Time
		ok bool
	}{
		{t0, true},
		{t0.Add(time.Hour - time.Microsecond), true},
		{t0.Add(time.Hour), false},
		{t0.Add(24 * time.Hour), false},
	} {
		e.clock.Set(tc.at)
		e.reset()
		_, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleReader), facts("m"))
		if tc.ok {
			if err != nil {
				t.Fatalf("at %v: %v", tc.at, err)
			}
			continue
		}
		wantErr(t, tc.at.String(), err, fault.Unauthenticated, access.MessageUnauthenticated)
		if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != "key expired" || !entries[0].Time.Equal(tc.at) {
			t.Fatalf("at %v: entries %+v", tc.at, entries)
		}
	}
}

// TestRoleOrder guards the role order and where a role acts: every pair of
// held and required role, held on a tenant and on the installation, asked on
// a tenant and on the installation. A role acts only on its own scope;
// reading a tenant's registration is the one thing an installation role
// (reader or more) gives inside a tenant.
func TestRoleOrder(t *testing.T) {
	e := newEnv(t)
	roles := []master.Role{master.RoleReader, master.RoleWriter, master.RoleOperator, master.RoleAdmin}
	rank := map[master.Role]int{"": 0, master.RoleReader: 1, master.RoleWriter: 2, master.RoleOperator: 3, master.RoleAdmin: 4}
	held := append([]master.Role{""}, roles...)
	for _, scopeName := range []string{"installation", "tenant"} {
		for _, h := range held {
			var grants []master.Grant
			scope := master.Installation
			if scopeName == "tenant" {
				scope = master.TenantScope(e.lab.ID)
			}
			if h != "" {
				grants = append(grants, on(scope, h))
			}
			k := e.key(time.Time{}, grants...)
			cred := access.APIKey(k.full)
			for _, need := range roles {
				_, errInst := e.access.Check(e.ctx, cred, access.OnInstallation(need), facts("m"))
				_, errTen := e.access.Check(e.ctx, cred, access.OnTenant("lab", need), facts("m"))
				enough := rank[h] >= rank[need]
				wantInst := scopeName == "installation" && enough
				wantTen := scopeName == "tenant" && enough
				for _, c := range []struct {
					what string
					err  error
					want bool
				}{{"installation", errInst, wantInst}, {"tenant", errTen, wantTen}} {
					if c.want && c.err != nil {
						t.Errorf("held %q on %s, need %q on %s: refused (%v)", h, scopeName, need, c.what, c.err)
					}
					if !c.want {
						if c.err == nil {
							t.Errorf("held %q on %s, need %q on %s: allowed", h, scopeName, need, c.what)
						} else if fault.KindOf(c.err) != fault.Denied || fault.Message(c.err) != access.MessageDenied {
							t.Errorf("held %q on %s, need %q on %s: %v, want Denied", h, scopeName, need, c.what, c.err)
						}
					}
				}
			}
			// Reading the registration: reader or more on either scope.
			_, err := e.access.Check(e.ctx, cred, access.TenantRegistration("lab"), facts("m"))
			if want := h != ""; want != (err == nil) {
				t.Errorf("held %q on %s, read the registration: %v", h, scopeName, err)
			}
		}
	}
}

// TestNotFoundOnlyForListers guards the one exception to Denied for a missing
// tenant: every installation role from reader up and the local operator, who
// may list every tenant anyway, are told NotFound; a key with no installation
// grant, even an admin of another tenant, is told Denied, so it cannot learn
// which tenants exist.
func TestNotFoundOnlyForListers(t *testing.T) {
	e := newEnv(t)
	need := access.OnTenant("nosuch", master.RoleReader)
	for _, r := range []master.Role{master.RoleReader, master.RoleOperator, master.RoleAdmin} {
		k := e.key(time.Time{}, on(master.Installation, r))
		_, err := e.access.Check(e.ctx, access.APIKey(k.full), need, facts("m"))
		wantErr(t, "installation "+string(r), err, fault.NotFound, access.MessageNoTenant)
	}
	_, err := e.access.Check(e.ctx, local, need, facts("m"))
	wantErr(t, "local", err, fault.NotFound, access.MessageNoTenant)
	k := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), need, facts("m"))
	wantErr(t, "tenant admin", err, fault.Denied, access.MessageDenied)
}

// TestPrincipalUnforgeable guards the one way in: the zero Principal and the
// zero Decision, all another package can make, hold no role anywhere, and a
// zero Decision's entry is one the master refuses, so no change can be
// recorded without a check.
func TestPrincipalUnforgeable(t *testing.T) {
	var p access.Principal
	if p.Role(master.Installation) != "" || p.Role(master.TenantScope("x")) != "" || p.CanListTenants() || p.IsLocalOperator() {
		t.Fatal("the zero Principal holds something")
	}
	var d access.Decision
	if err := d.Entry(master.OutcomeDone, "", access.Facts{}).Validate(); fault.KindOf(err) != fault.Invalid {
		t.Fatalf("the zero Decision's entry validates: %v", err)
	}
}

// TestStoreFailureIsNotRefusal guards that a master that does not answer is
// a failure (Unavailable), never a refusal, as the contract says.
func TestStoreFailureIsNotRefusal(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	if err := e.master.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := e.access.Check(e.ctx, access.APIKey(k.full), access.Authenticated(), facts("m"))
	if err == nil || fault.KindOf(err) == fault.Unauthenticated || fault.KindOf(err) == fault.Denied {
		t.Fatalf("a closed master gave %v (%s)", err, fault.KindOf(err))
	}
	t.Logf("closed master: kind %s, message %q", fault.KindOf(err), fault.Message(err))
}

// TestHostileFactsAreClipped guards the audit entry against hostile request
// facts: a method, address, object, tenant name and details that break the
// master's bounds are cut so the refusal is still recorded.
func TestHostileFactsAreClipped(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("é", 400) + "\x00\xff"
	f := access.Request{Method: long, RemoteAddress: long, RequestID: long, Object: long, Details: map[string]string{long: long}}
	_, err := e.access.Check(e.ctx, access.Credential{}, access.OnTenant(long, master.RoleReader), f)
	wantErr(t, "check", err, fault.Unauthenticated, access.MessageUnauthenticated)
	e.flush()
	stored, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{Outcome: master.OutcomeRefused}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d refusals stored, want 1: the entry was refused by the master", len(stored))
	}
	if !strings.HasPrefix(stored[0].TenantName, `"`) {
		t.Fatalf("an invalid tenant name is not quoted: %q", stored[0].TenantName)
	}
}

// TestMarkOnlyWhenStale guards the skip of the last-use mark: a key marked
// within the minute asks for no mark at all, so a busy key does not take the
// master's write connection on every request; a key never marked, or marked
// a minute or more before, asks for one. The marks are counted after Close,
// when the worker has made every call it will make.
func TestMarkOnlyWhenStale(t *testing.T) {
	e := newEnv(t)
	want := map[string]bool{}
	for _, tc := range []struct {
		name     string
		lastUsed time.Time
		mark     bool
	}{
		{"never used", time.Time{}, true},
		{"used a second before", t0.Add(-time.Second), false},
		{"used 59 seconds before", t0.Add(-59 * time.Second), false},
		{"used a minute before", t0.Add(-time.Minute), true},
		{"used an hour before", t0.Add(-time.Hour), true},
	} {
		k := e.key(time.Time{})
		if !tc.lastUsed.IsZero() {
			if _, err := e.master.MarkKeyUsed(e.ctx, k.id, tc.lastUsed, ""); err != nil {
				t.Fatal(err)
			}
		}
		if tc.mark {
			want[k.id] = true
		}
		if _, err := e.access.Check(e.ctx, access.APIKey(k.full), access.Authenticated(), facts("m")); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	if err := e.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.store.mu.Lock()
	got := map[string]bool{}
	for _, id := range e.store.markedKeys {
		got[id] = true
	}
	n := len(e.store.markedKeys)
	e.store.mu.Unlock()
	if !reflect.DeepEqual(got, want) || n != len(want) {
		t.Fatalf("marked %v (%d calls), want %v", got, n, want)
	}
}
