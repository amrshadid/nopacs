package rebuild

import (
	"context"
	"errors"
	"fmt"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
)

// adoptHolds makes, once a replace's shadow is the catalog in use, an
// active hold of each hold whose record is found active on the storages
// read with no row (holds.Adopt), unless Config.LeaveHolds: a catalog made
// again after its database was lost is held as before, and the hold's
// storages are those of the catalog just made. It runs after the
// activation, whose job has ended, so a failure is logged, not answered:
// the activation made the feed's history again, and the hold-record worker
// and retention's disposals each wait for an adoption of their own that
// succeeds after it (holds.Adopted), so no disposal passes a hold this try
// did not adopt.
func (u *unitRun) adoptHolds() {
	if u.sup.cfg.LeaveHolds {
		return
	}
	ad, ok := u.p.(holds.Adopter)
	if !ok {
		u.log.Error("rebuilds: the tenant's database keeps no holds; no hold record was read")
		return
	}
	ctx, cancel := context.WithTimeout(u.ctx, activationTimeout)
	defer cancel()
	a, err := u.readAndAdopt(ctx, ad)
	for _, id := range a.Adopted {
		u.log.Warn("rebuilds: a hold record was found active with no row; the hold holds again", "hold", id)
	}
	if err != nil {
		u.log.Warn("rebuilds: the hold records could not all be adopted; they are read again before any disposal",
			"reason", fault.Message(err))
	}
}

// readAndAdopt reads the hold records of every storage the catalog was
// made from, each through the storage it was read from, and adopts the
// holds found active with no row, then answers the storages that could not
// be read: one storage that does not answer must not cost the holds
// recorded on every other.
func (u *unitRun) readAndAdopt(ctx context.Context, ad holds.Adopter) (holds.Adoption, error) {
	names, readFrom, err := u.storageNames()
	if err != nil {
		return holds.Adoption{}, err
	}
	tenantID, err := u.sup.cfg.TenantID(ctx, u.t.name)
	if err != nil {
		return holds.Adoption{}, err
	}
	stores := u.sup.cfg.OpenStorages(u.t.name, u.p)
	defer stores.Close()
	found, readErr := readHolds(ctx, names, readFrom, func(ctx context.Context, name string) (Storage, error) { return stores.Get(ctx, name) },
		tenantID, u.sup.cfg.Keys)
	now := u.sup.cfg.Now()
	a, err := holds.Adopt(ctx, ad, found, now, func(id, storage string) master.AuditEntry {
		return holds.AdoptEntry(u.t.name, "server", id, storage, now)
	})
	return a, errors.Join(readErr, err)
}

// readHolds reads the hold records of each storage of names through the
// storage it is read from (readFrom, the storage itself unless named
// there), as the catalog was built: a record found is of the storage that
// holds it, so a hold adopted from it names where it was found. A storage
// that cannot be opened or read is joined into the error, and the others
// are read still.
func readHolds(ctx context.Context, names []string, readFrom map[string]string, get func(ctx context.Context, name string) (Storage, error),
	tenantID string, keys record.Keyring) ([]holds.Found, error) {
	var found []holds.Found
	var errs []error
	for _, name := range names {
		source := name
		if r, ok := readFrom[name]; ok {
			source = r
		}
		st, err := get(ctx, source)
		if err != nil {
			errs = append(errs, fmt.Errorf("the storage %s cannot be opened: %w", source, err))
			continue
		}
		f, err := holds.ReadRecords(ctx, st, source, tenantID, keys)
		if err != nil {
			errs = append(errs, fmt.Errorf("the hold records of %s cannot be read: %w", source, err))
			continue
		}
		found = append(found, f)
	}
	return found, errors.Join(errs...)
}
