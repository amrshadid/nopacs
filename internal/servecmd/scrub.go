package servecmd

import (
	"context"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// newScrubs returns the supervisor of every tenant's scrub jobs, over the
// registry's tenants, which also makes each storage's scheduled scrub every
// scrub.interval. Its reads of archived bytes count in reads, the server's
// one count, under their own owner; storages are opened for reading only,
// as the senders open them, so a scrub cannot write to one.
func newScrubs(d Deps, reg *registry.Registry, wake func(tenantName string), reads *checkedread.Slots) (*scrub.Supervisor, error) {
	values := settingvalue.Reader{Master: d.Master}
	opener := place.Opener{Values: values, Keys: d.Keys, StaleUploadAge: -1}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	return scrub.New(scrub.Config{
		Tenants:        registryTenants{master: d.Master, registry: reg},
		TenantSettings: func(st tenant.Store) scrub.TenantSettings { return values.OnTenant(st) },
		OpenStorages: func(name string, p tenant.Pipeline) scrub.Storages {
			return checkedread.NewStores(opener, tenantID, p, name)
		},
		Reads:    reads,
		Now:      clock.Now,
		NewID:    server.NewID,
		Wake:     wake,
		Logger:   d.Log,
		Keys:     d.Keys,
		TenantID: tenantID,
	})
}
