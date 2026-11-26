package servecmd

import (
	"context"
	"log/slog"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/place"
)

// holdRecords returns the run of the tenant's hold-record worker
// (holds.Writer.Run) over st, the tenant's database as its background work
// writes it: at its start it adopts each hold found active on the tenant's
// storages with no row, then writes the records owed. Every record it
// writes opens its storage through open (fenced as the other workers'
// writes are) and asks the switch gate first. A writer that cannot be made
// is logged, and the tenant's next start tries again.
func (w *tenantWork) holdRecords(tenantID, name string, st tenantDB, open func(ctx context.Context, storage string) (blob.Store, error),
	gate holds.WritePause, log *slog.Logger) func(ctx context.Context) {
	hw, err := holds.New(holds.Config{Tenant: name, TenantID: tenantID, Keys: w.d.Keys, Store: st, Open: open, Pause: gate, Now: w.now, Log: log})
	if err != nil {
		return func(context.Context) {
			log.Error("holds: the tenant's hold-record worker could not start", "reason", fault.Message(err))
		}
	}
	return hw.Run
}

// HoldWriter returns the hold writer of the tenant named, over its database
// as the registry answers it to a writer now: what the API's PlaceHold and
// ReleaseHold run through, so a hold's record is on every primary its
// target touches before its row commits. Its writes are fenced by the
// registry and ask the tenant's switch gate, as the background work's do.
func (w *tenantWork) HoldWriter(ctx context.Context, name string) (*holds.Writer, error) {
	raw, err := w.tenants.Store(name)
	if err != nil {
		return nil, err
	}
	st, err := dbOf(raw)
	if err != nil {
		return nil, err
	}
	return w.holdWriterOver(ctx, name, st, st)
}

// holdWriterOver returns the hold writer of the tenant named whose database
// writes go to st, and whose storages are opened by their rows in base, the
// database as the registry answered it (st itself, or st fenced).
func (w *tenantWork) holdWriterOver(ctx context.Context, name string, base, st tenantDB) (*holds.Writer, error) {
	id, err := w.tenantID(ctx, name)
	if err != nil {
		return nil, err
	}
	sw, err := w.switcherFor(ctx, name, base)
	if err != nil {
		return nil, err
	}
	open := fencedOpen(w.openFor(place.TenantOf(id, name, base), base), w.fenceOf(name))
	return holds.New(holds.Config{Tenant: name, TenantID: id, Keys: w.d.Keys, Store: st, Open: open, Pause: sw.Gate(), Now: w.now,
		Log: w.log.With("tenant", name)})
}

// WriteHoldsOwed writes at once the hold records the tenant's storages are
// owed (holds.Writer.WriteOwed), rather than at the hold-record worker's
// next pass: a relabel's publish that moved a held target owes its holds'
// records under the new labels, and a rebuild or restore that read a
// storage before they were written would find a hold on labels the catalog
// no longer has. A record not written stays owed, and the worker writes it.
func (w *tenantWork) WriteHoldsOwed(ctx context.Context, name string) {
	hw, err := w.HoldWriter(ctx, name)
	if err == nil {
		_, err = hw.WriteOwed(ctx)
	}
	if err != nil && ctx.Err() == nil {
		w.log.Warn("holds: hold records owed could not all be written at once; the worker writes them", "tenant", name,
			"kind", fault.KindOf(err).String(), "reason", fault.Message(err))
	}
}
