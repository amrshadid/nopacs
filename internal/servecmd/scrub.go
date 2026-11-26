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
	return scrub.New(scrubConfig(d, values, opener, registryTenants{master: d.Master, registry: reg}, wake, reads))
}

// scrubConfig returns the scrubs' configuration as serve wires it, over the
// tenants given, their storages opened for reading through opener.
func scrubConfig(d Deps, values settingvalue.Reader, opener place.Opener, tenants scrub.Tenants, wake func(tenantName string),
	reads *checkedread.Slots) scrub.Config {
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	return scrub.Config{
		Tenants:        tenants,
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
		// When each scan was last read whole on each storage, the ground of
		// a storage's coverage, kept by a tenant database that records it;
		// one that does not records nothing and its coverage is not told.
		Verifications: func(st tenant.Store) tenant.VerificationRecorder {
			if v, ok := st.(tenant.VerificationRecorder); ok {
				return v
			}
			return nil
		},
	}
}
