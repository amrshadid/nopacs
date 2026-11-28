package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestQuotaIsSetByTheInstallation guards who sets a tenant's storage quota:
// the installation's admin alone sets quota.bytes and quota.instances, on
// the installation or on one tenant; a tenant's own admin is refused, and
// so is a masked key whatever its grants, setting or unsetting; the
// warning's share is the tenant admin's to set; a bound of zero is refused.
func TestQuotaIsSetByTheInstallation(t *testing.T) {
	f := newFixture(t)
	maskedInst, _ := f.maskedKey(onInstallation(master.RoleAdmin))
	f.run(t, []tcase{
		{name: "the tenant's admin", as: f.lab1Admin, call: setSetting(settings.QuotaBytes, tenantScope("lab1"), "1GiB"),
			code: connect.CodePermissionDenied, audit: refused()},
		{name: "a masked admin of the installation", as: maskedInst, call: setSetting(settings.QuotaBytes, tenantScope("lab1"), "1GiB"),
			code: connect.CodePermissionDenied, msg: "masked key", audit: refused()},
		{name: "a masked admin, on the installation", as: maskedInst, call: setSetting(settings.QuotaInstances, installation(), "10"),
			code: connect.CodePermissionDenied, msg: "masked key"},
		{name: "the installation's admin", as: f.instAdmin, call: setSetting(settings.QuotaBytes, tenantScope("lab1"), "1GiB"),
			check: wantSetting(settings.QuotaBytes, "lab1", "1GiB", fromTen)},
		{name: "the installation's admin, every tenant's default", as: f.instAdmin, call: setSetting(settings.QuotaInstances, installation(), "500"),
			check: wantSetting(settings.QuotaInstances, "installation", "500", fromInst)},
		{name: "a masked admin unsets", as: maskedInst, call: unsetSetting(settings.QuotaBytes, tenantScope("lab1")),
			code: connect.CodePermissionDenied, msg: "masked key"},
		{name: "the tenant's admin unsets", as: f.lab1Admin, call: unsetSetting(settings.QuotaBytes, tenantScope("lab1")),
			code: connect.CodePermissionDenied},
		{name: "a bound of zero", as: f.instAdmin, call: setSetting(settings.QuotaBytes, tenantScope("lab1"), "0"),
			code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "the warning, by the tenant's admin", as: f.lab1Admin, call: setSetting(settings.QuotaWarn, tenantScope("lab1"), "80%"),
			check: wantSetting(settings.QuotaWarn, "lab1", "80%", fromTen)},
	})
	st, err := f.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := st.Setting(ctx, settings.QuotaBytes); err != nil || v != "1GiB" {
		t.Errorf("the tenant holds quota.bytes %q, %v, want what its installation's admin set", v, err)
	}
}

// TestQuotaIsAnswered guards where a tenant's quota is read: GetTenant and
// GetStatus answer its use against its bounds, with the warning's share and
// the state, and ListTenants leaves it out.
func TestQuotaIsAnswered(t *testing.T) {
	f := newFixture(t)
	call(f.as(f.instAdmin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.QuotaBytes,
		Scope: tenantScope("lab1"), Value: "1MiB"}})
	call(f.as(f.instAdmin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.QuotaInstances,
		Scope: tenantScope("lab1"), Value: "7"}})
	want := func(t *testing.T, where string, q *v1.TenantQuota) {
		t.Helper()
		if q == nil || q.GetBytesBound() != 1<<20 || q.GetInstancesBound() != 7 || q.GetWarnPercent() != 90 ||
			q.GetState() != v1.QuotaState_QUOTA_STATE_UNDER || q.GetUsedBytes() != 0 || q.GetUsedInstances() != 0 {
			t.Errorf("%s answers the quota %+v, want 1 MiB and 7 instances, warned at 90%%, nothing used", where, q)
		}
	}
	got := call(f.as(f.instReader).tenants.GetTenant, &v1.GetTenantRequest{Name: "lab1"})
	if got.err != nil {
		t.Fatal(got.err)
	}
	want(t, "GetTenant", got.res.(*v1.GetTenantResponse).GetTenant().GetQuota())
	st := call(f.as(f.lab1Reader).monitor.GetStatus, &v1.GetStatusRequest{Scope: tenantScope("lab1")})
	if st.err != nil {
		t.Fatal(st.err)
	}
	want(t, "GetStatus", st.res.(*v1.GetStatusResponse).GetTenants()[0].GetQuota())
	list := call(f.as(f.instReader).tenants.ListTenants, &v1.ListTenantsRequest{})
	if list.err != nil {
		t.Fatal(list.err)
	}
	for _, tn := range list.res.(*v1.ListTenantsResponse).GetTenants() {
		if tn.GetQuota() != nil {
			t.Errorf("ListTenants answers %s's quota", tn.GetName())
		}
	}
}
