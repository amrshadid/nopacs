package api

import (
	"context"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxWholeListPages bounds how many pages of master.MaxPageLimit items a
// read of a whole list takes (wholeList, eachTenant): a list longer than
// that is the server's fault, not something to read on without end.
const maxWholeListPages = 100

// installationPlaces answers package place the installation's settings it
// checks a storage against, read from the master as they apply on the
// installation (package settingvalue).
type installationPlaces struct{ c *core }

var _ place.Values = installationPlaces{}

// DirectoryRoots returns the installation's storage.directory-roots.
func (v installationPlaces) DirectoryRoots(ctx context.Context) (settings.DirectoryRoots, error) {
	return v.c.values().DirectoryRoots(ctx)
}

// S3Endpoints returns the installation's storage.s3-endpoints.
func (v installationPlaces) S3Endpoints(ctx context.Context) (settings.S3Endpoints, error) {
	return v.c.values().S3Endpoints(ctx)
}

// tenantReceiverValues answers package place the settings a receiver of the
// tenant whose store is st is checked against.
type tenantReceiverValues struct {
	c  *core
	st tenant.Store
}

var _ place.ReceiverValues = tenantReceiverValues{}

// Ports returns receiver.ports as it applies on the tenant.
func (v tenantReceiverValues) Ports(ctx context.Context) (settings.Ports, error) {
	return v.c.values().OnTenant(v.st).Ports(ctx)
}

// BindAddresses returns the installation's receiver.bind-addresses.
func (v tenantReceiverValues) BindAddresses(ctx context.Context) (settings.BindAddresses, error) {
	return v.c.values().BindAddresses(ctx)
}

// opener returns the opener of a storage for a test or a read. A test
// touches nothing but its own object, so it leaves the cleanup of old S3
// uploads to the reception pipeline, which opens the storage to use it; it
// shares the server's record of cleaned storages, so a directory's crash
// leftovers are removed by the test only when it is the server's first open
// of the storage, before any Put there, and never under a store already
// writing (place.Cleanups).
func (c *core) opener() place.Opener {
	return place.Opener{Values: installationPlaces{c}, Keys: c.keys, StaleUploadAge: -1, Cleanups: c.cleanups}
}

// openToTest opens the storage s of t for TestStorage, with the server's
// record of cleaned storages (opener): the open removes nothing under a
// store already writing there, and the test's own Put starts after any
// removal it does as the server's first open of the storage.
func (c *core) openToTest(ctx context.Context, t place.Tenant, s tenant.Storage) (blob.Store, error) {
	return c.opener().Open(ctx, t, s)
}

// placeTenant returns the tenant whose pipeline is p as package place opens
// its storages.
func placeTenant(id, name string, p tenant.Pipeline) place.Tenant { return place.TenantOf(id, name, p) }

// wholeList returns every item of a tenant's list, page after page; what
// names the list in the error of one that does not end.
func wholeList[T any](ctx context.Context, what string, list func(context.Context, master.Page) ([]T, string, error)) ([]T, error) {
	var out []T
	page := master.Page{Limit: master.MaxPageLimit}
	for i := 0; ; i++ {
		if i == maxWholeListPages {
			return nil, fault.New(fault.Internal, "api: "+what+" did not end")
		}
		batch, next, err := list(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// tenantStorages returns every storage of a tenant.
func tenantStorages(ctx context.Context, p tenant.Pipeline) ([]tenant.Storage, error) {
	return wholeList(ctx, "a tenant's storages", p.ListStorages)
}

// tenantReceivers returns every receiver of a tenant.
func tenantReceivers(ctx context.Context, p tenant.Pipeline) ([]tenant.Receiver, error) {
	return wholeList(ctx, "a tenant's receivers", p.ListReceivers)
}
