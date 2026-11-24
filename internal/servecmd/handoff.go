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
	if err := h.receptionStopped(ctx, name); err != nil {
		return err
	}
	return h.engines.Idle(ctx, name)
}

// CorrectionsQuiesceTimeout bounds a rebuild's wait for the corrections of
// its tenant to stop, apart from the wait for its reception: a purge in
// flight stops at the end of its page of objects (up to 500 removed from a
// storage), and a relabel's copy in flight runs to its publish, which the
// store then refuses; on a slow storage either takes minutes.
const CorrectionsQuiesceTimeout = 15 * time.Minute

// DrainForRebuild is a rebuild's drain (rebuild.Config.Drain), asked once
// its job is in the tenant's database. The tenant answers its store, and
// the reception supervisor stops the pipeline because a replace job is
// queued or running (within ReceptionStopTimeout); then every correction
// request in flight and the corrections' background work end, and what a
// stop or a failure left is settled under the engine's gate: a delete or a
// replace left prepared abandoned with what it wrote undone, and every
// record that lags its row rewritten (correct.Engines.QuiesceForRebuild,
// within CorrectionsQuiesceTimeout). After that nothing of the corrections
// writes until the rebuild ends: the store refuses a new correction, a
// publish, an undo, and every step of a purge, and the engines' pass and
// background work wait.
func (h handoffs) DrainForRebuild(ctx context.Context, name string) error {
	rctx, cancel := context.WithTimeout(ctx, ReceptionStopTimeout)
	err := h.receptionStopped(rctx, name)
	cancel()
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, CorrectionsQuiesceTimeout)
	defer cancel()
	return h.engines.QuiesceForRebuild(cctx, name)
}

// receptionStopped pokes the reception supervisor and waits until it runs
// no pipeline of the tenant named name (supervise.Supervisor.Drained), or
// ctx ends.
func (h handoffs) receptionStopped(ctx context.Context, name string) error {
	h.sup.Poke(name)
	for !h.sup.Drained(name) {
		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Unavailable, ctx.Err(), "the tenant's reception pipeline did not stop in time")
		case <-time.After(drainPoll):
		}
		h.sup.Poke(name)
	}
	return nil
}

// Advance implements api.Handoffs.
func (h handoffs) Advance(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	return h.owners.Advance(ctx, id, name, st)
}

// Reclaim implements api.Handoffs.
func (h handoffs) Reclaim(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	return h.owners.Reclaim(ctx, id, name, st)
}
