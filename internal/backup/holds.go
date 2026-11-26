package backup

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// adoptHolds reads the hold records of every storage of the restored
// tenant, replicas too, once its catalog is made again, and makes an
// active hold of each found active with no row (holds.Adopt): a hold placed
// after the recovery point outlives the restore. It runs after the rebuild,
// so the storages a hold adopted is owed on are those of the catalog made
// again. A record that does not verify for the tenant makes no hold and is
// named in the report; a storage that cannot be read fails the reconcile,
// since a hold it may hold would otherwise be lost.
func adoptHolds(ctx context.Context, cfg RestoreConfig, t openedTenant, opener place.Opener, storages []tenant.Storage, now time.Time,
	res *TenantResult) error {
	ad, ok := t.store.(holds.Adopter)
	if !ok {
		return fault.New(fault.Internal, "backup: this database type keeps no holds")
	}
	pt := place.TenantOf(t.row.ID, t.row.Name, t.store)
	var found []holds.Found
	for _, s := range storages {
		st, err := opener.Open(ctx, pt, s)
		if err != nil {
			return prefixed(err, "the storage "+s.Name+" cannot be opened: ")
		}
		f, err := holds.ReadRecords(ctx, st, s.Name, t.row.ID, cfg.Keys)
		_ = st.Close()
		if err != nil {
			return prefixed(err, "the hold records of "+s.Name+" cannot be read: ")
		}
		found = append(found, f)
	}
	user := restoreUser
	switch {
	case cfg.method != "":
		user = cfg.localUser
	case cfg.reconcileOnly:
		user = reconcileUser
	}
	a, err := holds.Adopt(ctx, ad, found, now, func(id, storage string) master.AuditEntry {
		return holds.AdoptEntry(t.row.Name, user, id, storage, now)
	})
	res.HoldsAdopted = a.Adopted
	res.HoldsRefused = len(a.Refused)
	res.HoldsRefusedRecords = a.Refused[:min(len(a.Refused), maxDisagreements)]
	return err
}
