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
	// locks serializes each advance of a tenant's owner records with a
	// switch's taking of the record on its replica's place (ownerLocks).
	locks *ownerLocks
	// work is the tenant's background work (the replicator, the recalls,
	// the repairs, retention), stopped and waited for by each drain.
	work quiescer
}

// quiescer stops a tenant's background work and waits for it
// (tenantWork.Quiesce).
type quiescer interface {
	Quiesce(ctx context.Context, name string) error
}

// workQuiet stops and waits for the tenant's background work, when there is
// any: its workers hold the store they were started over, so the registry's
// refusal of the store alone does not stop a copy or a repair in flight
// from writing after the drain.
func (h handoffs) workQuiet(ctx context.Context, name string) error {
	if h.work == nil {
		return nil
	}
	return h.work.Quiesce(ctx, name)
}

var _ api.Handoffs = handoffs{}

// Drain implements api.Handoffs. The tenant is moved already, so the
// registry refuses its store to every writer: the reception supervisor,
// poked, stops its pipeline (the files it holds archived, or left in
// incoming with their place kept) while its listeners refuse new
// associations; the tenant's background work (the replicator, the
// recalls, the repairs, retention) is stopped and waited for; the
// corrections' background work for it finishes the step it is at, and
// every later step's fence refuses it.
func (h handoffs) Drain(ctx context.Context, name string) error {
	// An export has no deadline of its own, so the drain has one: the
	// time a pipeline's own stop may take, after which the export gives
	// the tenant back rather than wait for ever.
	ctx, cancel := context.WithTimeout(ctx, ReceptionStopTimeout)
	defer cancel()
	if err := h.receptionStopped(ctx, name); err != nil {
		return err
	}
	if err := h.workQuiet(ctx, name); err != nil {
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
// queued or running (within ReceptionStopTimeout), and the tenant's
// background work is stopped and waited for, not started again while the
// rebuild runs (tenantWork.Quiesce, rebuilding); then every correction
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
	if err == nil {
		err = h.workQuiet(rctx, name)
	}
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

// Advance implements api.Handoffs, under the tenant's owner lock.
func (h handoffs) Advance(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	release, err := h.locks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.owners.Advance(ctx, id, name, st)
}

// Reclaim implements api.Handoffs, under the tenant's owner lock.
func (h handoffs) Reclaim(ctx context.Context, id, name string, st tenant.Store) ([]backup.Owner, error) {
	release, err := h.locks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.owners.Reclaim(ctx, id, name, st)
}
