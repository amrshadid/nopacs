package servecmd

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// drainPoll is how often a drain looks at whether a tenant's reception
// pipeline has stopped.
const drainPoll = 50 * time.Millisecond

// handoffs is what an export and a reclaim ask of the server
// (api.Handoffs): the tenant's writers drained, and its owner records
// checked.
type handoffs struct {
	sup     *supervise.Supervisor
	engines *correct.Engines
	owners  backup.OwnerCheck
}

var _ api.Handoffs = handoffs{}

// Drain implements api.Handoffs. The tenant is moved already, so the
// registry refuses its store to every writer: the reception supervisor,
// poked, stops its pipeline (the files it holds archived, or left in
// incoming with their place kept) while its listeners refuse new
// associations; the corrections' background work for it finishes the step
// it is at, and every later step's fence refuses it.
func (h handoffs) Drain(ctx context.Context, name string) error {
	// An export has no deadline of its own, so the drain has one: the
	// time a pipeline's own stop may take, after which the export gives
	// the tenant back rather than wait for ever.
	ctx, cancel := context.WithTimeout(ctx, ReceptionStopTimeout)
	defer cancel()
	h.sup.Poke(name)
	for !h.sup.Drained(name) {
		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Unavailable, ctx.Err(), "the tenant's reception pipeline did not stop in time")
		case <-time.After(drainPoll):
		}
		h.sup.Poke(name)
	}
	return h.engines.Idle(ctx, name)
}

// Advance implements api.Handoffs.
func (h handoffs) Advance(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	return h.owners.Advance(ctx, id, name, st)
}

// Reclaim implements api.Handoffs.
func (h handoffs) Reclaim(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	return h.owners.Reclaim(ctx, id, name, st)
}
