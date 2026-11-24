package servecmd

import (
	"context"
	"path/filepath"

	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// importGuarded returns the server's own directories no import root may
// be, contain or lie inside: the data directory, its incoming area and the
// directory of the installation key file. The data directory holds the
// other two today; each is named, so a later layout that moves one out
// keeps it guarded.
func importGuarded(dataDir string) []string {
	return []string{dataDir, filepath.Join(dataDir, IncomingDirName), filepath.Dir(filepath.Join(dataDir, KeyFileName))}
}

// importRoots returns where an import unit finds its root: by the settings
// that name and allot it, resolved again at each unit's start and refused
// when it now resolves elsewhere than when import.roots was set, or now
// touches a storage root, a guarded directory or another import root
// (place.ImportRoots). The API checks a job's root with the same rules.
func importRoots(values settingvalue.Reader, dataDir string) place.ImportRoots {
	return place.ImportRoots{
		Values: values,
		Allotted: func(ctx context.Context, st tenant.Store) ([]string, error) {
			return values.OnTenant(st).ImportAllowedRoots(ctx)
		},
		Guarded: importGuarded(dataDir),
	}
}

// newImports returns the supervisor of every tenant's import units, over
// the registry's tenants, handing each file to the tenant's running
// reception pipeline through the sink its listeners are given (sup.Sink),
// so an import is one more source of the same pipeline.
func newImports(d Deps, reg *registry.Registry, sup *supervise.Supervisor, wake func(tenantName string)) (*dirimport.Supervisor, error) {
	values := settingvalue.Reader{Master: d.Master}
	return dirimport.New(dirimport.Config{
		Tenants:        registryTenants{master: d.Master, registry: reg},
		Installation:   values,
		TenantSettings: func(st tenant.Store) dirimport.TenantSettings { return values.OnTenant(st) },
		Roots:          importRoots(values, d.DataDir),
		Sink:           func(tenantName string) receive.Sink { return sup.Sink(tenantName) },
		Wake:           wake,
		Logger:         d.Log,
	})
}
