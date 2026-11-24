package access_test

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// TestCreateKeyExpiry guards the contract's expiry rules: 90 days unless
// given, "never" only when asked for, a positive period of at most 10 years,
// and a key that works with the secret returned once.
func TestCreateKeyExpiry(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name string
		exp  access.Expiry
		want time.Time
		ok   bool
	}{
		{"default", access.Expiry{}, t0.Add(90 * 24 * time.Hour), true},
		{"never", access.NeverExpire(), time.Time{}, true},
		{"a day", access.ExpireAfter(24 * time.Hour), t0.Add(24 * time.Hour), true},
		{"ten years", access.ExpireAfter(access.MaxKeyLifetime), t0.Add(3650 * 24 * time.Hour), true},
		{"past ten years", access.ExpireAfter(access.MaxKeyLifetime + time.Microsecond), time.Time{}, false},
		{"zero", access.ExpireAfter(0), time.Time{}, false},
		{"negative", access.ExpireAfter(-time.Hour), time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.reset()
			made, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "lab robot", tc.exp, master.KeyRules{})
			_, entries := e.recorded()
			if len(entries) != 1 {
				t.Fatalf("%d audit entries, want 1", len(entries))
			}
			if !tc.ok {
				wantErr(t, "create", err, fault.Invalid, "")
				if entries[0].Outcome != master.OutcomeFailed {
					t.Fatalf("entry %+v", entries[0])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !made.Key.Expires.Equal(tc.want) || made.Key.Holder != "lab robot" || made.Key.Hash != nil {
				t.Fatalf("key %+v", made.Key)
			}
			if entries[0].Object != made.Key.ID || entries[0].Details["holder"] != "lab robot" || entries[0].Outcome != master.OutcomeDone {
				t.Fatalf("entry %+v", entries[0])
			}
			if _, err := e.access.Check(e.ctx, access.APIKey(made.Secret.Key()), access.Authenticated(), facts("m")); err != nil {
				t.Fatalf("the new key does not work: %v", err)
			}
		})
	}
}

// TestCreateKeyHolder guards the holder's rules: required, at most 256
// bytes of UTF-8.
func TestCreateKeyHolder(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		holder string
		ok     bool
	}{
		{"", false},
		{strings.Repeat("a", 256), true},
		{strings.Repeat("a", 257), false},
		{"Zoë 山田", true},
		{"bad\xff", false},
	} {
		_, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), tc.holder, access.Expiry{}, master.KeyRules{})
		if tc.ok && err != nil {
			t.Errorf("holder of %d bytes: %v", len(tc.holder), err)
		}
		if !tc.ok && fault.KindOf(err) != fault.Invalid {
			t.Errorf("holder of %d bytes: %v, want Invalid", len(tc.holder), err)
		}
	}
}

// managementCalls returns each management function called with d, so a
// test can give all of them the same Decision.
func managementCalls(e *env, keyID string) map[string]func(access.Decision) error {
	return map[string]func(access.Decision) error{
		"CreateKey": func(d access.Decision) error {
			_, err := e.access.CreateKey(e.ctx, d, "h", access.Expiry{}, master.KeyRules{})
			return err
		},
		"GetKey": func(d access.Decision) error {
			_, err := e.access.GetKey(e.ctx, d, keyID)
			return err
		},
		"ListKeys": func(d access.Decision) error {
			_, _, err := e.access.ListKeys(e.ctx, d, master.Page{})
			return err
		},
		"RevokeKey": func(d access.Decision) error {
			_, err := e.access.RevokeKey(e.ctx, d, keyID)
			return err
		},
		"SetGrant": func(d access.Decision) error {
			_, _, err := e.access.SetGrant(e.ctx, d, keyID, access.InstallationScope, master.RoleAdmin)
			return err
		},
		"RemoveGrant": func(d access.Decision) error {
			_, err := e.access.RemoveGrant(e.ctx, d, keyID, access.InstallationScope)
			return err
		},
		"ListGrants": func(d access.Decision) error {
			_, _, err := e.access.ListGrants(e.ctx, d, access.GrantsOfKey(keyID), master.Page{})
			return err
		},
		"WhoAmI": func(d access.Decision) error {
			_, _, err := e.access.WhoAmI(e.ctx, d)
			return err
		},
	}
}

// TestManagementNeedsDecision guards the management functions' own check of
// the Decision they are given, which keeps a handler from skipping Check
// and a wrong requirement in the server's table from opening them: the zero
// Decision is Denied by every one, with no store call and no entry; a
// Decision whose caller is not the installation's admin (an installation
// operator, checked for operator; a tenant admin, checked only to be
// authenticated) is Denied by every one but WhoAmI, with a refusal entry
// and no change; and each function given the admin's Decision writes its
// one audit entry.
func TestManagementNeedsDecision(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	operator := e.key(time.Time{}, on(master.Installation, master.RoleOperator))
	tenantAdmin := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	calls := managementCalls(e, k.id)

	for name, call := range calls {
		e.reset()
		wantErr(t, name+" with the zero Decision", call(access.Decision{}), fault.Denied, access.MessageDenied)
		if c, entries := e.recorded(); len(c) != 0 || len(entries) != 0 {
			t.Fatalf("%s with the zero Decision: calls %v, entries %d", name, c, len(entries))
		}
	}
	// Not even an attempt at an entry: the zero Decision has none to give,
	// and one made up would be refused as invalid and counted as failed.
	if s := e.access.AuditStats(); s.Failed != 0 {
		t.Fatalf("the zero Decision led to %d failed audit entries", s.Failed)
	}

	lesser := map[string]access.Decision{
		"installation operator": e.decide(access.APIKey(operator.full), access.OnInstallation(master.RoleOperator), "m"),
		"tenant admin":          e.decide(access.APIKey(tenantAdmin.full), access.Authenticated(), "m"),
	}
	for who, d := range lesser {
		for name, call := range calls {
			e.reset()
			err := call(d)
			_, entries := e.recorded()
			if name == "WhoAmI" {
				if err != nil || len(entries) != 1 || entries[0].Outcome != master.OutcomeDone {
					t.Fatalf("WhoAmI for %s: %v, entries %+v", who, err, entries)
				}
				continue
			}
			wantErr(t, name+" for "+who, err, fault.Denied, access.MessageDenied)
			// Refused from the Decision itself, before the key is read again.
			if len(entries) != 1 || entries[0].Outcome != master.OutcomeRefused ||
				entries[0].Reason != "the decision does not carry admin on the installation" {
				t.Fatalf("%s for %s: entries %+v, want one refusal for the Decision", name, who, entries)
			}
		}
	}
	if got, err := e.master.Key(e.ctx, k.id); err != nil || !got.Revoked.IsZero() {
		t.Fatalf("a refused RevokeKey changed the key: %+v %v", got, err)
	}

	for _, name := range []string{"GetKey", "ListKeys", "ListGrants", "SetGrant", "RemoveGrant", "WhoAmI", "CreateKey", "RevokeKey"} {
		e.reset()
		if err := calls[name](e.admin(name)); err != nil {
			t.Fatalf("%s by the admin: %v", name, err)
		}
		if _, entries := e.recorded(); len(entries) != 1 || entries[0].Outcome != master.OutcomeDone || entries[0].Method != name {
			t.Fatalf("%s by the admin: entries %+v, want its one entry", name, entries)
		}
	}
}

// TestManagementValidatesAfterRole guards the order of checks on the
// operations this package implements: the role is decided by Check and by
// the function's own check of the Decision before the request is
// validated, so a caller who may not call a method learns nothing about
// what a valid request to it looks like.
func TestManagementValidatesAfterRole(t *testing.T) {
	e := newEnv(t)
	operator := e.key(time.Time{}, on(master.Installation, master.RoleOperator))
	d := e.decide(access.APIKey(operator.full), access.Authenticated(), "CreateKey")
	_, err := e.access.CreateKey(e.ctx, d, "", access.ExpireAfter(-1), master.KeyRules{})
	wantErr(t, "operator, bad request", err, fault.Denied, access.MessageDenied)
	_, err = e.access.CreateKey(e.ctx, e.admin("CreateKey"), "", access.ExpireAfter(-1), master.KeyRules{})
	wantErr(t, "admin, bad request", err, fault.Invalid, "")
}

// TestRevokeTwice guards that revoking a revoked key succeeds and keeps its
// first revoke time, and that an unknown key is NotFound.
func TestRevokeTwice(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	first, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.id)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Set(t0.Add(time.Hour))
	second, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.id)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Revoked.Equal(t0) || !second.Revoked.Equal(t0) || second.Hash != nil {
		t.Fatalf("first %v, second %v", first.Revoked, second.Revoked)
	}
	_, err = e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), "NoSuchKey123")
	wantErr(t, "unknown", err, fault.NotFound, "")
}

// TestGrants guards setting, listing and removing grants: the previous role
// returned, writer refused on the installation, unknown key and tenant
// NotFound, a revoked key Conflict, removing what is not held a success, and
// scopes named by tenant name.
func TestGrants(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	set := func(scope access.Scope, role master.Role) (master.Role, error) {
		_, prev, err := e.access.SetGrant(e.ctx, e.admin("SetGrant"), k.id, scope, role)
		return prev, err
	}
	if prev, err := set(access.TenantNamed("lab"), master.RoleReader); err != nil || prev != "" {
		t.Fatalf("first set: %q %v", prev, err)
	}
	if prev, err := set(access.TenantNamed("lab"), master.RoleAdmin); err != nil || prev != master.RoleReader {
		t.Fatalf("second set: %q %v", prev, err)
	}
	if prev, err := set(access.InstallationScope, master.RoleReader); err != nil || prev != "" {
		t.Fatalf("installation: %q %v", prev, err)
	}
	_, err := set(access.InstallationScope, master.RoleWriter)
	wantErr(t, "writer on the installation", err, fault.Invalid, "")
	_, err = set(access.TenantNamed("nosuch"), master.RoleReader)
	wantErr(t, "unknown tenant", err, fault.NotFound, access.MessageNoTenant)
	_, err = set(access.TenantNamed("lab"), "boss")
	wantErr(t, "unknown role", err, fault.Invalid, "")
	_, _, err = e.access.SetGrant(e.ctx, e.admin("SetGrant"), "NoSuchKey123", access.InstallationScope, master.RoleReader)
	wantErr(t, "unknown key", err, fault.NotFound, "")

	got, next, err := e.access.ListGrants(e.ctx, e.admin("ListGrants"), access.GrantsOfKey(k.id), master.Page{})
	if err != nil || next != "" {
		t.Fatal(err)
	}
	want := []access.Grant{
		{KeyID: k.id, Scope: access.InstallationScope, Role: master.RoleReader},
		{KeyID: k.id, Scope: access.TenantNamed("lab"), Role: master.RoleAdmin},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("grants %+v, want %+v", got, want)
	}
	onLab, _, err := e.access.ListGrants(e.ctx, e.admin("ListGrants"), access.GrantsOnScope(access.TenantNamed("lab")), master.Page{})
	if err != nil || len(onLab) != 1 || onLab[0] != want[1] {
		t.Fatalf("on lab: %+v %v", onLab, err)
	}
	_, _, err = e.access.ListGrants(e.ctx, e.admin("ListGrants"), access.GrantFilter{}, master.Page{})
	wantErr(t, "no filter", err, fault.Invalid, "")
	_, _, err = e.access.ListGrants(e.ctx, e.admin("ListGrants"), access.GrantsOfKey(k.id), master.Page{Limit: 1001})
	wantErr(t, "page too large", err, fault.Invalid, "")

	prev, err := e.access.RemoveGrant(e.ctx, e.admin("RemoveGrant"), k.id, access.TenantNamed("lab"))
	if err != nil || prev != master.RoleAdmin {
		t.Fatalf("remove: %q %v", prev, err)
	}
	prev, err = e.access.RemoveGrant(e.ctx, e.admin("RemoveGrant"), k.id, access.TenantNamed("lab"))
	if err != nil || prev != "" {
		t.Fatalf("remove again: %q %v", prev, err)
	}

	if _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.id); err != nil {
		t.Fatal(err)
	}
	e.reset()
	_, err = set(access.TenantNamed("lab"), master.RoleReader)
	wantErr(t, "revoked key", err, fault.Conflict, "")
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Outcome != master.OutcomeFailed || entries[0].Details["scope"] != "tenant/lab" {
		t.Fatalf("entries %+v", entries)
	}
}

// TestKeysListedWithoutHash guards that GetKey, ListKeys and WhoAmI return
// keys without their hashes, and that WhoAmI names its scopes by tenant.
func TestKeysListedWithoutHash(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.TenantScope(e.clinic.ID), master.RoleReader))
	keys, _, err := e.access.ListKeys(e.ctx, e.admin("ListKeys"), master.Page{})
	if err != nil || len(keys) != 1 {
		t.Fatalf("%d keys, %v", len(keys), err)
	}
	got, err := e.access.GetKey(e.ctx, e.admin("GetKey"), k.id)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].Hash != nil || got.Hash != nil {
		t.Fatal("a key came back with its hash")
	}
	_, err = e.access.GetKey(e.ctx, e.admin("GetKey"), "NoSuchKey123")
	wantErr(t, "unknown", err, fault.NotFound, "")

	p, grants, err := e.access.WhoAmI(e.ctx, e.decide(access.APIKey(k.full), access.Authenticated(), "WhoAmI"))
	if err != nil {
		t.Fatal(err)
	}
	pk, isKey := p.Key()
	if !isKey || pk.ID != k.id || pk.Hash != nil || len(grants) != 1 || grants[0].Scope != access.TenantNamed("clinic") {
		t.Fatalf("whoami: %+v %+v", pk, grants)
	}
	lp, lg, err := e.access.WhoAmI(e.ctx, e.admin("WhoAmI"))
	if err != nil || !lp.IsLocalOperator() || lp.LocalUser() != "501" || len(lg) != 0 {
		t.Fatalf("local whoami: %+v %v %v", lp, lg, err)
	}
}

// TestCreatedKeyPrintsNoSecret guards that the one value carrying a
// secret, the result of CreateKey, prints only the key's id in every form.
func TestCreatedKeyPrintsNoSecret(t *testing.T) {
	e := newEnv(t)
	made, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "h", access.Expiry{}, master.KeyRules{})
	if err != nil {
		t.Fatal(err)
	}
	full := made.Secret.Key()
	_, sec, err := secret.ParseAPIKey(full)
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := sprintf(form, made); strings.Contains(out, sec) {
			t.Fatalf("%s prints the secret", form)
		}
	}
}

// TestRevokeAnswersWhatItDid guards the answer of a revocation that
// happened: when the master stops answering just after the revocation, the
// call still succeeds, with the key as read before and the revoke time the
// revocation returned, and is not reported as a failure.
func TestRevokeAnswersWhatItDid(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	d := e.admin("RevokeKey")
	e.store.keyDownAfterRevoke.Store(true)
	got, err := e.access.RevokeKey(e.ctx, d, k.id)
	if err != nil {
		t.Fatalf("a revocation that happened was answered %v", err)
	}
	if got.ID != k.id || got.Holder != "test" || !got.Revoked.Equal(t0) || got.Hash != nil {
		t.Fatalf("answer %+v", got)
	}
}
