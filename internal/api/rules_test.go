package api_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
)

// TestRecreatedTenantIsNotTheCheckedOne guards the rule that a handler acts
// only on the tenant the check allowed: when the tenant is removed and
// created again under the same name between the check and the handler, the
// grant the check read was on the old tenant, so the request is answered as
// for a tenant that is gone: PERMISSION_DENIED with the message of no grant
// to a key, NOT_FOUND to a caller who may list every tenant.
func TestRecreatedTenantIsNotTheCheckedOne(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.tenant("lab1")
	admin, _ := e.key(e.onTenant("lab1", master.RoleAdmin))
	recreate := func() {
		if _, err := e.registry.Remove(ctx, "lab1", e.setupEntry(), master.LocalOperator()); err != nil {
			t.Error(err)
			return
		}
		nt := registry.NewTenant{ID: e.newID(), Name: "lab1", Driver: "sqlite"}
		entry := e.setupEntry()
		entry.TenantName = "lab1"
		if _, _, err := e.registry.Create(ctx, nt, entry, master.LocalOperator()); err != nil {
			t.Error(err)
		}
	}
	calls := map[string]func(clients) outcome{
		"GetTenant":    func(c clients) outcome { return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: "lab1"}) },
		"GetSetting":   getSetting(testRetention, tenantScope("lab1")),
		"SetSetting":   setSetting(testRetention, tenantScope("lab1"), "1d"),
		"UnsetSetting": unsetSetting(testRetention, tenantScope("lab1")),
		"ListSettings": func(c clients) outcome {
			return call(c.settings.ListSettings, &v1.ListSettingsRequest{Scope: tenantScope("lab1")})
		},
		"ListAuditEntries": func(c clients) outcome {
			return call(c.audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: tenantScope("lab1")})
		},
	}
	for name, m := range calls {
		e.afterCheck.Store(&recreate)
		if o := m(e.as(admin)); o.code() != connect.CodePermissionDenied || o.message() != access.MessageDenied {
			t.Errorf("%s by a key of the old tenant: %v %q, want permission denied %q", name, o.code(), o.message(), access.MessageDenied)
		}
		e.afterCheck.Store(&recreate)
		if o := m(e.as(local)); o.code() != connect.CodeNotFound || o.message() != access.MessageNoTenant {
			t.Errorf("%s by the local operator: %v %q, want not found", name, o.code(), o.message())
		}
	}
	// Without the race, the key of the tenant it holds a grant on is served.
	fresh, _ := e.key(e.onTenant("lab1", master.RoleAdmin))
	if o := setSetting(testRetention, tenantScope("lab1"), "1d")(e.as(fresh)); o.err != nil {
		t.Errorf("a key of the current tenant: %v", o.err)
	}
}

// TestSetSettingTheMasterFails guards the record of a change the master
// refuses: SetSetting on the installation, with only that one write of the
// master failing, answers UNAVAILABLE and leaves one failed entry in the
// installation's log.
func TestSetSettingTheMasterFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.faulty.failSetSetting.Store(true)
	o := setSetting(testRetention, installation(), "30d")(e.as(local))
	if o.code() != connect.CodeUnavailable {
		t.Fatalf("code %v (%v)", o.code(), o.err)
	}
	got := entriesOf(e.masterLog(), o.requestID)
	if len(got) != 1 || got[0].Outcome != master.OutcomeFailed || got[0].Object != testRetention {
		t.Errorf("the entries of the request: %+v, want one, failed", got)
	}
}

// TestReadyTenantHasNoReason guards the contract's "empty when it is ready":
// a tenant whose master row is ready with a reason left from an earlier
// state, and that the registry does not hold (so the row is what is shown),
// is answered with no reason.
func TestReadyTenantHasNoReason(t *testing.T) {
	t.Parallel()
	e := newEnvChecking(t, time.Hour)
	row := master.Tenant{ID: "old-id", Name: "old", Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady,
		StateReason: "the database does not answer", Created: t0}
	if err := e.master.CreateTenant(ctx, row, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	o := call(e.as(local).tenants.GetTenant, &v1.GetTenantRequest{Name: "old"})
	if o.err != nil {
		t.Fatal(o.err)
	}
	if tn := o.res.(*v1.GetTenantResponse).GetTenant(); tn.GetState() != v1.TenantState_TENANT_STATE_READY || tn.GetStateReason() != "" {
		t.Errorf("got %v, want ready with no reason", tn)
	}
}

// TestSettingsLimitsAreTheMasters guards the limits the settings list
// restates, because the core imports nothing of NoPACs': a key's longest
// lifetime, a value's length and the rule for a name must be those of the
// access package and the master, or a value one accepts the other refuses.
func TestSettingsLimitsAreTheMasters(t *testing.T) {
	if settings.MaxKeyLifetime != access.MaxKeyLifetime {
		t.Errorf("settings.MaxKeyLifetime %v, access.MaxKeyLifetime %v", settings.MaxKeyLifetime, access.MaxKeyLifetime)
	}
	if settings.MaxValueLength != master.MaxSettingValueLength {
		t.Errorf("settings.MaxValueLength %d, master.MaxSettingValueLength %d", settings.MaxValueLength, master.MaxSettingValueLength)
	}
	if settings.MaxNameLength != master.MaxSettingNameLength {
		t.Errorf("settings.MaxNameLength %d, master.MaxSettingNameLength %d", settings.MaxNameLength, master.MaxSettingNameLength)
	}
	ok := func(string) error { return nil }
	names := []string{"a", "a.b", "a_b-c.9", "", "A", "a b", "a/b", "é", "a\x00", strings.Repeat("n", 128), strings.Repeat("n", 129)}
	for b := 0; b < 256; b++ {
		names = append(names, "x"+string(rune(b)))
	}
	for _, n := range names {
		_, err := settings.New(settings.Definition{Name: n, Validate: ok})
		if (err == nil) != (master.ValidateSettingName(n) == nil) {
			t.Errorf("the name %q: the settings list accepts it %v, the master %v", n, err == nil, master.ValidateSettingName(n) == nil)
		}
	}
}

// TestReservedSettingsDoNotExist guards the settings the system keeps for
// itself under settings.ReservedPrefix, the installation's id and its key
// check: with a value held in the master and in a tenant's database, every
// settings method on every scope answers each as a name that does not exist,
// no listing shows it, and nothing changes it. The names are the server's
// InstallationIDSetting and KeyCheckSetting, written out here because this
// package may not import the server.
func TestReservedSettingsDoNotExist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	st, err := f.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{settings.ReservedPrefix + "installation-id", settings.ReservedPrefix + "key-check"}
	for _, name := range names {
		if err := f.master.SetSetting(ctx, name, "01-not-for-callers", f.setupEntry(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSetting(ctx, name, "01-not-for-callers", f.setupEntry(), nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		for _, scope := range []*v1.Scope{installation(), tenantScope("lab1")} {
			for method, m := range map[string]func(clients) outcome{
				"GetSetting":   getSetting(name, scope),
				"SetSetting":   setSetting(name, scope, "x"),
				"UnsetSetting": unsetSetting(name, scope),
			} {
				o := m(f.as(local))
				if o.code() != connect.CodeNotFound || o.message() != "no setting has this name" {
					t.Errorf("%s of %s on %v: %v %q, want the answer of any unknown name", method, name, scope, o.code(), o.message())
				}
			}
		}
		if v, err := f.master.Setting(ctx, name); err != nil || v != "01-not-for-callers" {
			t.Errorf("the master's value of %s is %q, %v: something changed it", name, v, err)
		}
	}
	for _, scope := range []*v1.Scope{installation(), tenantScope("lab1")} {
		o := call(f.as(local).settings.ListSettings, &v1.ListSettingsRequest{Scope: scope, PageSize: 1000})
		if o.err != nil {
			t.Fatal(o.err)
		}
		for _, s := range o.res.(*v1.ListSettingsResponse).GetSettings() {
			if strings.HasPrefix(s.GetName(), settings.ReservedPrefix) {
				t.Errorf("ListSettings on %v shows %s", scope, s.GetName())
			}
		}
	}
}
