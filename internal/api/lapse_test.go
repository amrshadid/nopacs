package api_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// expiringKey makes a key that expires at expires, with the grants given,
// and returns it as a caller, with its id.
func (e *env) expiringKey(expires time.Time, grants ...master.Grant) (caller, string) {
	e.t.Helper()
	made, err := secret.MakeAPIKey()
	if err != nil {
		e.t.Fatal(err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "test", Created: t0, Expires: expires}
	if err := e.master.CreateKey(ctx, k, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	for _, g := range grants {
		g.KeyID = k.ID
		if _, err := e.master.SetGrant(ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
	return caller{key: made.Key()}, k.ID
}

// lapsing is one change the API makes, as the lapse test calls it: prepare
// sets up what it changes and returns the call and a probe that reports
// whether the call changed anything.
type lapsing struct {
	name    string
	tenant  bool // the change is in the tenant "lab"'s own database
	prepare func(e *env, n int) (do func(clients) outcome, changed func() bool)
}

// lapsingChanges is every change of the API's four services.
func lapsingChanges(t *testing.T) []lapsing {
	keyCount := func(e *env) int {
		keys, _, err := e.master.ListKeys(ctx, master.Page{Limit: master.MaxPageLimit})
		if err != nil {
			t.Fatal(err)
		}
		return len(keys)
	}
	role := func(e *env, id string) master.Role {
		r, err := e.master.RolesFor(ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		return r.Installation
	}
	labSetting := func(e *env) string {
		st, err := e.registry.Store("lab")
		if err != nil {
			t.Fatal(err)
		}
		v, err := st.Setting(ctx, testText)
		if fault.KindOf(err) == fault.NotFound {
			return "(none)"
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	masterSetting := func(e *env) string {
		v, err := e.master.Setting(ctx, testText)
		if fault.KindOf(err) == fault.NotFound {
			return "(none)"
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	value := func(n int) string { return "value-" + strings.Repeat("x", n) }
	return []lapsing{
		{"CreateKey", false, func(e *env, _ int) (func(clients) outcome, func() bool) {
			before := keyCount(e)
			return func(c clients) outcome {
				return call(c.access.CreateKey, &v1.CreateKeyRequest{Holder: "made after the check"})
			}, func() bool { return keyCount(e) != before }
		}},
		{"RevokeKey", false, func(e *env, _ int) (func(clients) outcome, func() bool) {
			_, id := e.key()
			return func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: id}) },
				func() bool {
					k, err := e.master.Key(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					return !k.Revoked.IsZero()
				}
		}},
		{"SetGrant", false, func(e *env, _ int) (func(clients) outcome, func() bool) {
			_, id := e.key()
			return func(c clients) outcome {
				return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: id, Scope: installation(), Role: v1.Role_ROLE_READER})
			}, func() bool { return role(e, id) != "" }
		}},
		{"RemoveGrant", false, func(e *env, _ int) (func(clients) outcome, func() bool) {
			_, id := e.key(onInstallation(master.RoleReader))
			return func(c clients) outcome {
				return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: id, Scope: installation()})
			}, func() bool { return role(e, id) == "" }
		}},
		{"CreateTenant", false, func(e *env, n int) (func(clients) outcome, func() bool) {
			name := "made" + strings.Repeat("x", n)
			return func(c clients) outcome {
					return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE})
				}, func() bool {
					_, err := e.master.TenantByName(ctx, name)
					return err == nil
				}
		}},
		{"RemoveTenant", false, func(e *env, n int) (func(clients) outcome, func() bool) {
			name := "gone" + strings.Repeat("x", n)
			e.tenant(name)
			return func(c clients) outcome {
					return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: name, ConfirmName: name})
				}, func() bool {
					_, err := e.master.TenantByName(ctx, name)
					return err != nil
				}
		}},
		{"SetSetting on the installation", false, func(e *env, n int) (func(clients) outcome, func() bool) {
			return setSetting(testText, installation(), value(n)), func() bool { return masterSetting(e) == value(n) }
		}},
		{"UnsetSetting on the installation", false, func(e *env, n int) (func(clients) outcome, func() bool) {
			if o := setSetting(testText, installation(), value(n))(e.as(local)); o.err != nil {
				t.Fatal(o.err)
			}
			return unsetSetting(testText, installation()), func() bool { return masterSetting(e) == "(none)" }
		}},
		{"SetSetting on a tenant", true, func(e *env, n int) (func(clients) outcome, func() bool) {
			return setSetting(testText, tenantScope("lab"), value(n)), func() bool { return labSetting(e) == value(n) }
		}},
		{"UnsetSetting on a tenant", true, func(e *env, n int) (func(clients) outcome, func() bool) {
			if o := setSetting(testText, tenantScope("lab"), value(n))(e.as(local)); o.err != nil {
				t.Fatal(o.err)
			}
			return unsetSetting(testText, tenantScope("lab")), func() bool { return labSetting(e) == "(none)" }
		}},
	}
}

// TestChangeByKeyThatLapsedAfterCheck guards every change of the API against
// a key that could make it when its request was checked and no longer can
// when the change is written: revoked, expired, its grant removed, or its
// grant lowered, between the check and the handler. The change is not made;
// the answer is the check's own for such a key, UNAUTHENTICATED or
// PERMISSION_DENIED with the same fixed message, which tells nothing more;
// and the request has one entry, refused, in the installation's log, with
// the real cause, and none in the tenant's. A change in the master is
// refused by its own transaction; a change in a tenant's database, which
// holds no keys, by the master's check made from inside the tenant's
// transaction.
func TestChangeByKeyThatLapsedAfterCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.tenant("lab")
	lapses := []struct {
		name    string
		code    connect.Code
		message string
		reason  string
		// make returns a key that may make every change, and what to do
		// after the check to take that right away.
		make func() (caller, func())
	}{
		{"revoked", connect.CodeUnauthenticated, access.MessageUnauthenticated, "key revoked since the check", func() (caller, func()) {
			c, id := e.key(onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
			return c, func() {
				if _, err := e.master.RevokeKey(ctx, id, e.now(), e.setupEntry(), master.LocalOperator()); err != nil {
					t.Error(err)
				}
			}
		}},
		{"expired", connect.CodeUnauthenticated, access.MessageUnauthenticated, "key expired since the check", func() (caller, func()) {
			c, _ := e.expiringKey(e.now().Add(time.Hour), onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
			return c, func() { e.advance(2 * time.Hour) }
		}},
		{"whose grant was removed", connect.CodePermissionDenied, access.MessageDenied, "no sufficient grant since the check", func() (caller, func()) {
			c, id := e.key(onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
			return c, func() {
				for _, g := range []master.Grant{onInstallation(""), e.onTenant("lab", "")} {
					if _, err := e.master.RemoveGrant(ctx, id, g.Scope, e.setupEntry(), master.LocalOperator()); err != nil {
						t.Error(err)
					}
				}
			}
		}},
		{"whose grant was lowered", connect.CodePermissionDenied, access.MessageDenied, "no sufficient grant since the check", func() (caller, func()) {
			c, id := e.key(onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
			return c, func() {
				for _, g := range []master.Grant{onInstallation(master.RoleOperator), e.onTenant("lab", master.RoleOperator)} {
					g.KeyID = id
					if _, err := e.master.SetGrant(ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
						t.Error(err)
					}
				}
			}
		}},
	}
	n := 0
	for _, ch := range lapsingChanges(t) {
		for _, l := range lapses {
			n++
			what := ch.name + " by a key " + l.name + " after the check"
			c, lapse := l.make()
			do, changed := ch.prepare(e, n)
			e.afterCheck.Store(&lapse)
			o := do(e.as(c))
			if o.code() != l.code || o.message() != l.message {
				t.Errorf("%s: %v %q, want %v %q", what, o.code(), o.message(), l.code, l.message)
			}
			if changed() {
				t.Errorf("%s: the change was made", what)
			}
			got := entriesOf(e.masterLog(), o.requestID)
			if len(got) != 1 || got[0].Outcome != master.OutcomeRefused || got[0].Reason != l.reason {
				t.Errorf("%s: the installation's log has %+v, want one entry, refused, %q", what, got, l.reason)
			}
			if ch.tenant {
				if got := entriesOf(e.tenantLog("lab"), o.requestID); len(got) != 0 {
					t.Errorf("%s: the tenant's log has %+v, want nothing", what, got)
				}
			}
		}
	}

	// The same changes by a key that keeps its right are made.
	for _, ch := range lapsingChanges(t) {
		n++
		c, _ := e.key(onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
		do, changed := ch.prepare(e, n)
		if o := do(e.as(c)); o.err != nil || !changed() {
			t.Errorf("%s by a key that keeps its right: %v, changed %v", ch.name, o.err, changed())
		}
	}
}

// TestKeyLapsedAfterCheckIsRefusedFirst guards the contract's order of checks
// for a key revoked between the check and the handler: the credential comes
// first, so every managing method answers such a key UNAUTHENTICATED with the
// fixed message, whether what it names exists or not and whether its request
// is valid or not; it is never told "no such key", "already exists" or which
// argument is wrong. Each request is recorded once, as refused, in the
// installation's log.
func TestKeyLapsedAfterCheckIsRefusedFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.tenant("lab")
	e.tenant("taken")
	_, existingKey := e.key()
	cases := []struct {
		name string
		do   func(clients) outcome
	}{
		{"RevokeKey of a key that exists", func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: existingKey}) }},
		{"RevokeKey of no such key", func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: "nosuchkey123"}) }},
		{"RevokeKey of a malformed id", func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: "a.b"}) }},
		{"CreateKey with no holder", func(c clients) outcome { return call(c.access.CreateKey, &v1.CreateKeyRequest{}) }},
		{"CreateKey", func(c clients) outcome { return call(c.access.CreateKey, &v1.CreateKeyRequest{Holder: "h"}) }},
		{"SetGrant on a tenant that exists", func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: existingKey, Scope: tenantScope("lab"), Role: v1.Role_ROLE_READER})
		}},
		{"SetGrant on no such tenant", func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: existingKey, Scope: tenantScope("nosuch"), Role: v1.Role_ROLE_READER})
		}},
		{"SetGrant for no such key", func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: "nosuchkey123", Scope: installation(), Role: v1.Role_ROLE_READER})
		}},
		{"SetGrant of writer on the installation", func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: existingKey, Scope: installation(), Role: v1.Role_ROLE_WRITER})
		}},
		{"SetGrant with no role", func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: existingKey, Scope: installation()})
		}},
		{"RemoveGrant on no such tenant", func(c clients) outcome {
			return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: existingKey, Scope: tenantScope("nosuch")})
		}},
		{"RemoveGrant with no scope", func(c clients) outcome {
			return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: existingKey})
		}},
		{"RemoveTenant of a tenant that exists", func(c clients) outcome {
			return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "lab", ConfirmName: "lab"})
		}},
		{"RemoveTenant of no such tenant", func(c clients) outcome {
			return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "nosuch", ConfirmName: "nosuch"})
		}},
		{"RemoveTenant with another confirmation", func(c clients) outcome {
			return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: "lab", ConfirmName: "lab2"})
		}},
		{"CreateTenant of a fresh name", func(c clients) outcome {
			return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: "fresh", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE})
		}},
		{"CreateTenant of a name taken", func(c clients) outcome {
			return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: "taken", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE})
		}},
		{"CreateTenant with no type", func(c clients) outcome {
			return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: "fresh2"})
		}},
		{"SetSetting of no such setting", setSetting("no.such", installation(), "v")},
		{"SetSetting with a bad value", setSetting(testRetention, installation(), "soon")},
		{"SetSetting on a tenant, a bad value", setSetting(testRetention, tenantScope("lab"), "soon")},
		{"UnsetSetting of no such setting", unsetSetting("no.such", installation())},
		{"UnsetSetting on a tenant", unsetSetting(testRetention, tenantScope("lab"))},
	}
	for _, cs := range cases {
		c, id := e.key(onInstallation(master.RoleAdmin), e.onTenant("lab", master.RoleAdmin))
		lapse := func() {
			if _, err := e.master.RevokeKey(ctx, id, e.now(), e.setupEntry(), master.LocalOperator()); err != nil {
				t.Error(err)
			}
		}
		e.afterCheck.Store(&lapse)
		o := cs.do(e.as(c))
		if o.code() != connect.CodeUnauthenticated || o.message() != access.MessageUnauthenticated {
			t.Errorf("%s by a key revoked after the check: %v %q, want unauthenticated %q", cs.name, o.code(), o.message(), access.MessageUnauthenticated)
		}
		got := entriesOf(e.masterLog(), o.requestID)
		if len(got) != 1 || got[0].Outcome != master.OutcomeRefused || got[0].Reason != "key revoked since the check" {
			t.Errorf("%s: the installation's log has %+v, want one entry, refused", cs.name, got)
		}
	}
}
