package correct

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// What the engines do for a rebuild of a tenant: its build reads the
// storages' manifests, metadata and correction records, so while a rebuild
// is queued or running nothing of the corrections writes them, and before
// its build what was left half done is settled.

// current reports whether r is the tenant's engine now.
func (es *Engines) current(name string, r *engineRun) bool {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.runs[name] == r
}

// rebuilding reports whether a rebuild of the tenant named name is queued
// or running (tenant.Rebuilding's RebuildRunning), or whether that cannot be
// read: either way the tenant's background work writes nothing now, since a
// rebuild's build reads the storages' manifests, metadata and correction
// records, which a purge, a relabel's copy or Recover rewrites, and would
// see them torn or miss the change. A database that keeps no rebuilds has
// none.
func (es *Engines) rebuilding(name string, r *engineRun) bool {
	rb, ok := r.store.(interface {
		RebuildRunning(ctx context.Context) (bool, error)
	})
	if !ok {
		return false
	}
	running, err := rb.RebuildRunning(es.ctx)
	if err != nil {
		if es.ctx.Err() == nil {
			es.cfg.Log.Warn("corrections: whether a rebuild runs could not be read; background work waits", "tenant", name,
				"error", fault.Message(err))
		}
		return true
	}
	return running
}

// Quiesce waits until nothing of the tenant named name acts through its
// corrections engines: no request in flight on its current engine (each
// holds that engine's gate), none on an engine it replaced (each such
// engine is held until its last request lets it go), and no background
// work (each holds the tenant's work lock), all at once, or ctx ends. A
// request or background work begun after a rebuild was made finds it and
// changes nothing (the store refuses a new correction, a publish and a
// purge; the work waits).
func (es *Engines) Quiesce(ctx context.Context, name string) error {
	for {
		if es.quiet(name) {
			return nil
		}
		if err := pollWait(ctx); err != nil {
			return err
		}
	}
}

// QuiesceForRebuild is a rebuild's wait for the corrections of the tenant
// named name, asked once its job is made: Quiesce, then, holding the
// engine's gate and the work lock so no request or background work runs,
// the settle of what a stop or a failure left (Engine.settleForRebuild),
// which fails unless every correction is then in a state the build reads
// right. A request in flight when the job was made may have written a
// correction's records, or a replace's manifest, that the build would read
// before the request's publish is refused and it abandons; a correction
// left prepared with no request holding it, or a record that lags its row,
// would be read as it is.
func (es *Engines) QuiesceForRebuild(ctx context.Context, name string) error {
	for {
		r, err := es.acquireCurrent(ctx, name)
		if err != nil {
			return err
		}
		held, replaced := es.holdQuiet(name, r)
		if held {
			err := r.engine.settleForRebuild(ctx)
			r.gate.Unlock()
			r.work.Unlock()
			r.holds.Done()
			return err
		}
		r.holds.Done()
		if replaced && ctx.Err() == nil {
			// The engine was replaced while it waited (the tenant's
			// database opened anew): the next round waits on the new one.
			continue
		}
		if err := pollWait(ctx); err != nil {
			return err
		}
	}
}

// acquireCurrent holds the tenant's engine over the database the tenant
// answers now, made when there is none; the caller lets it go.
func (es *Engines) acquireCurrent(ctx context.Context, name string) (*engineRun, error) {
	st, err := es.cfg.Tenants.Store(name)
	if err != nil {
		return nil, err
	}
	return es.acquire(ctx, name, st)
}

// holdQuiet is quiet for the engine r: when r is the tenant's current one
// and the tenant is quiet, it answers held, holding r's gate and the work
// lock for the caller to let go; replaced when r is no longer the tenant's
// engine, which waiting on would never see quiet.
func (es *Engines) holdQuiet(name string, r *engineRun) (held, replaced bool) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.runs[name] != r {
		return false, true
	}
	if es.retiring[name] > 0 || !r.work.TryLock() {
		return false, false
	}
	if !r.gate.TryLock() {
		r.work.Unlock()
		return false, false
	}
	return true, false
}

// quiet reports whether, at one moment, the tenant named name has no engine
// replaced but still held, no request on its current engine and no
// background work. The locks are tried, never waited for: a writer waiting
// on the gate would hold every new request of the tenant, reads included,
// behind it.
func (es *Engines) quiet(name string) bool {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.retiring[name] > 0 {
		return false
	}
	r, work := es.runs[name], es.works[name]
	if work != nil {
		if !work.TryLock() {
			return false
		}
		defer work.Unlock()
	}
	if r != nil {
		if !r.gate.TryLock() {
			return false
		}
		r.gate.Unlock()
	}
	return true
}

// pollWait waits idlePoll, or answers unavailable once ctx ends.
func pollWait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the tenant's corrections did not finish what they were doing in time")
	case <-time.After(idlePoll):
		return nil
	}
}
