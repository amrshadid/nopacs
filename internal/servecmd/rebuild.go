package servecmd

import (
	"context"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/rebuild"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// newRebuilds returns the supervisor of every tenant's rebuild jobs, over
// the registry's tenants. Storages are opened for reading only, as the
// scrubs open them, so a rebuild cannot write to one; what it writes is
// the tenant's shadow catalog, through the tenant's database. The
// installation keys check each correction record's MAC, so a record that
// someone without the key file put on a storage hides nothing.
func newRebuilds(d Deps, reg *registry.Registry, wake func(tenantName string)) (*rebuild.Supervisor, error) {
	values := settingvalue.Reader{Master: d.Master}
	opener := place.Opener{Values: values, Keys: d.Keys, StaleUploadAge: -1}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	return rebuild.New(rebuild.Config{
		Tenants: registryTenants{master: d.Master, registry: reg},
		OpenStorages: func(name string, p tenant.Pipeline) rebuild.Storages {
			return checkedread.NewStores(opener, tenantID, p, name)
		},
		Wake:     wake,
		Logger:   d.Log,
		Keys:     d.Keys,
		TenantID: tenantID,
	})
}
