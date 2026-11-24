package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
)

// TestInstallationSettingsCheckAMovedTenant guards an installation setting
// whose check visits every tenant, with one of them moved by an export:
// a change that keeps what the moved tenant uses is made (its being read
// only does not refuse it), and one that takes away the root its storage
// is on is still refused, naming the tenant, since a reclaim would bring
// the tenant back with that storage.
func TestInstallationSettingsCheckAMovedTenant(t *testing.T) {
	f := newPlaceFixture(t)
	if o := call(f.as(local).storages.AddStorage, &v1.AddStorageRequest{Tenant: "lab1", Storage: dirStorage("main", "fast", "main")}); o.err != nil {
		t.Fatalf("AddStorage: %v", o.err)
	}
	if x := export(f.as(f.instAdmin), "lab1"); x.err != nil || x.summary == nil {
		t.Fatalf("the export: %v", x.err)
	}
	if s := f.tenantState(t, "lab1"); s != v1.TenantState_TENANT_STATE_MOVED {
		t.Fatalf("after the export lab1 is %v", s)
	}
	set := func(value string) outcome {
		return call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.StorageDirectoryRoots,
			Scope: installation(), Value: value}})
	}
	if o := set("fast=" + f.root + "\nslow=" + t.TempDir()); o.err != nil {
		t.Errorf("a root added, the moved tenant's kept: %v, want it made", o.err)
	}
	o := set("slow=" + t.TempDir())
	if connect.CodeOf(o.err) != connect.CodeFailedPrecondition || !strings.Contains(o.err.Error(), "the tenant lab1") ||
		strings.Contains(o.err.Error(), "TENANT_MOVED") {
		t.Errorf("the moved tenant's root taken away: %v, want a precondition naming its storage", o.err)
	}
}
