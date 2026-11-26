package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// keyGrant is one grant of the key a test rig connects with.
type keyGrant struct {
	tenant string // empty for the installation
	role   v1.Role
}

// newKeyRig is newRig for a caller that holds a key with the given grants
// instead of the local operator, so the menu and the keys of each screen are
// drawn from what the key may do. The fake accepts only that key over TLS.
func newKeyRig(t *testing.T, w, h int, grants []keyGrant, setup func(*fakeserver.Server)) *rig {
	t.Helper()
	fake := fakeserver.New()
	fake.Now = func() time.Time { return fixedNow }
	fake.Settings = testSettings()
	fake.RequireKey = testSecret
	fake.CallerKeyID = "k00000000001"
	for _, g := range grants {
		scope := &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
		if g.tenant != "" {
			scope = &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: g.tenant}}
		}
		fake.AddGrant("k00000000001", scope, g.role)
	}
	if setup != nil {
		setup(fake)
	}
	addr := fake.ServeTLS(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, fake.CertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(client.Config{Server: addr, CAFile: ca, Getenv: func(k string) string {
		if k == client.EnvKey {
			return testSecret
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("making a client: %v", err)
	}
	t.Cleanup(cl.Close)
	m := newModel(context.Background(), Options{Client: cl, Where: addr, Now: func() time.Time { return fixedNow }, Location: zone, NoRefresh: true})
	r := &rig{t: t, fake: fake, m: m, w: w, h: h}
	r.send(tea.WindowSizeMsg{Width: w, Height: h})
	r.exec(m.Init())
	r.record()
	return r
}

// TestMenuIsBuiltFromTheCallersGrants guards the menu: the local operator and
// an admin on the installation are offered every screen; a key that is a reader
// on one tenant is offered the tenants and nothing that is the installation's
// (no Keys, no Settings, no Audit log); a key with no grant is offered nothing
// and told why. The menu is a convenience: the server checks every call.
func TestMenuIsBuiltFromTheCallersGrants(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		all := []string{"Tenants", "Keys", "Settings", "Audit log"}
		r := newRig(t, w, h, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.expect(all...)

		r = newKeyRig(t, w, h, []keyGrant{{role: v1.Role_ROLE_ADMIN}}, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.expect(all...)

		r = newKeyRig(t, w, h, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_READER}}, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.expect("Tenants")
		r.refuse("Keys", "Settings", "Audit log")
		r.key("2")
		r.expect("Menu")

		r = newKeyRig(t, w, h, []keyGrant{{role: v1.Role_ROLE_READER}}, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.expect("Tenants")
		r.refuse("Keys", "Settings", "Audit log")

		r = newKeyRig(t, w, h, nil, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.expect("holds no grant")
		r.refuse("Tenants", "Keys", "Settings", "Audit log")
		r.key("enter", "1")
		r.expect("Menu")
	})
}

// TestATenantsKeysFollowTheRoleOnIt guards the keys of a tenant's screen: a
// reader is offered what it may look at (status, storages, replicas,
// retention rules) and none of what changes or exposes more (grants, audit,
// holds, corrections, findings, the audit seal, removal); an admin on the tenant
// is offered the tenant's own screens but still not its removal, which is the
// installation admin's; the installation admin is offered everything.
func TestATenantsKeysFollowTheRoleOnIt(t *testing.T) {
	setup := func(f *fakeserver.Server) { addTenants(f, 1) }
	// The hint line is one line wide and cuts a long list of keys, so the keys
	// are read from the help screen, which lists them all.
	open := func(r *rig) {
		r.key("enter", "enter")
		r.expect("Tenant t000")
		r.key("?")
		r.expect("Keys of: Tenant t000")
	}
	r := newKeyRig(t, 120, 34, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_READER}}, setup)
	open(r)
	r.expect("status", "storages", "replicas", "retention")
	r.refuse("grants", "audit", "holds", "corrections", "findings", "remove")

	r = newKeyRig(t, 120, 34, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, setup)
	open(r)
	r.expect("grants", "audit", "holds", "corrections", "findings", "audit seal")
	r.refuse("remove")

	r = newKeyRig(t, 120, 34, []keyGrant{{role: v1.Role_ROLE_ADMIN}}, setup)
	open(r)
	r.expect("grants", "audit", "holds", "corrections", "remove")
}

// TestATenantsAdminSeesTheGrantsButNotTheKeys guards the grants of a tenant
// as a tenant's admin sees them: the list, without a way into the key, which
// only an admin on the installation may read.
func TestATenantsAdminSeesTheGrantsButNotTheKeys(t *testing.T) {
	setup := func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.AddKey(&v1.Key{Id: "k00000000001", Holder: "the caller"})
		f.AddGrant("k00000000001", &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: "t000"}}, v1.Role_ROLE_ADMIN)
	}
	r := newKeyRig(t, 120, 30, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, setup)
	r.key("enter", "enter", "g")
	r.expect("Grants on tenant t000", "k00000000001", "admin", "s set a role", "d remove")
	keys := r.calls("GetKey")
	r.key("enter")
	if r.calls("GetKey") != keys {
		t.Errorf("a tenant's admin read a key through its grant")
	}
}
