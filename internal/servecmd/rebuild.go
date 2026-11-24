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
//
// A replace waits, before it builds, for handoffs.DrainForRebuild: the
// reception supervisor, which reads the replace job in the tenant's
// database, stops the tenant's pipeline while its receivers refuse, and
// every correction request in flight and the corrections' background work
// end. When a unit stops, the supervisor is poked, so receivers held back
// start again as soon as the job has ended.
func newRebuilds(d Deps, reg *registry.Registry, wake func(tenantName string), drain handoffs) (*rebuild.Supervisor, error) {
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
		Drain:    drain.DrainForRebuild,
		// Each correction record is applied as its row says: a delete a
		// stop left prepared, or one undone or abandoned whose record lags,
		// hides nothing; the drain has settled those already, and this
		// holds if a settle missed one.
		Decide: rebuild.ByRow,
		Resume: drain.sup.Poke,
	})
}
