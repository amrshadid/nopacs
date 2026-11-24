package api_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
)

// TestRootPathsAreTheInstallations guards who reads the paths of
// storage.directory-roots: the installation's admin reads them whole; any
// other caller (a tenant's reader or admin, a reader on the installation)
// reads the roots' names alone, from GetSetting on either scope and from the
// installation's ListSettings, and no answer, line of the log or audit
// entry holds a path for them.
func TestRootPathsAreTheInstallations(t *testing.T) {
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageDirectoryRoots, "fast="+f.root+"\nslow="+f.dir+"/slow")
	get := func(c caller, scope *v1.Scope) string {
		o := call(f.as(c).settings.GetSetting, &v1.GetSettingRequest{Name: settings.StorageDirectoryRoots, Scope: scope})
		if o.err != nil {
			t.Fatalf("refused: %v", o.err)
		}
		return o.res.(*v1.GetSettingResponse).GetSetting().GetValue()
	}
	list := func(c caller) string {
		o := call(f.as(c).settings.ListSettings, &v1.ListSettingsRequest{Scope: installation(), PageSize: 100})
		if o.err != nil {
			t.Fatalf("refused: %v", o.err)
		}
		for _, s := range o.res.(*v1.ListSettingsResponse).GetSettings() {
			if s.GetName() == settings.StorageDirectoryRoots {
				return s.GetValue()
			}
		}
		t.Fatal("the roots are not listed")
		return ""
	}
	for name, v := range map[string]string{
		"a tenant's reader":            get(f.lab1Reader, tenantScope("lab1")),
		"a tenant's admin":             get(f.lab1Admin, tenantScope("lab1")),
		"a reader on the installation": get(f.instReader, installation()),
		"a reader listing":             list(f.instReader),
	} {
		if v != "fast\nslow" {
			t.Errorf("%s reads %q, want the names alone", name, v)
		}
	}
	for name, v := range map[string]string{
		"the installation's admin":          get(f.instAdmin, installation()),
		"the installation's admin, listing": list(f.instAdmin),
		"the local operator, on lab1":       get(local, tenantScope("lab1")),
	} {
		if !strings.Contains(v, "fast="+f.root) {
			t.Errorf("%s reads %q, want the paths", name, v)
		}
	}
	f.flush()
	for _, e := range append(f.masterLog(), f.tenantLog("lab1")...) {
		for _, d := range e.Details {
			if strings.Contains(d, f.dir) {
				t.Errorf("an audit entry holds a root's path: %+v", e)
			}
		}
	}
	if strings.Contains(f.log.String(), f.dir) {
		t.Error("the log holds a root's path")
	}
}
