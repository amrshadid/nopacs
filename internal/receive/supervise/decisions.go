package supervise

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Decisions is what a tenant's running pipeline does for a person's
// decision on its quarantine and its failed receptions: the release and the
// retry run through it whole, so the bytes in the incoming area and the work
// queue follow the record in the order that survives a crash; after a
// rejection or a discard the store recorded, it removes the bytes. The
// reception pipeline (receive.Pipeline) is one.
type Decisions interface {
	// Release releases a held file (receive.Pipeline.Release).
	Release(ctx context.Context, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error)
	// Retry makes a new reception of a failed one's files and queues its
	// first unit (receive.Pipeline.Retry).
	Retry(ctx context.Context, id, newReceptionID, receiver string, audit master.AuditEntry, guard tenant.Guard) (failed, retry tenant.Reception, err error)
	// RemoveRejected removes a rejected file's bytes at path.
	RemoveRejected(path string) error
	// RemoveDiscarded removes the bytes of a discarded reception's files.
	RemoveDiscarded(ctx context.Context, receptionID string) error
}

// The reception pipeline takes decisions.
var _ Decisions = (*receive.Pipeline)(nil)

// Decisions returns the running pipeline of the tenant named name, for a
// decision, and whether one runs. It reads memory only: the pipeline the
// tenant's worker published, which it withdraws before it stops one and
// replaces as soon as the next has started. The removal of bytes after a
// rejection or a discard goes through RemoveRejected and RemoveDiscarded
// instead, which keep it while no pipeline runs.
func (s *Supervisor) Decisions(name string) (Decisions, bool) {
	t := s.tenant(name)
	if t == nil {
		return nil, false
	}
	return t.decisions()
}

// Sink returns the sink of the tenant named name, which hands each file to
// whichever of its pipelines runs (the one its listeners are given), or nil
// while no pipeline of it runs: an import unit then waits rather than have
// every file refused. It reads memory only.
func (s *Supervisor) Sink(name string) receive.Sink {
	t := s.tenant(name)
	if t == nil || !t.sink.running() {
		return nil
	}
	return t.sink
}

// tenant returns the worker of the tenant named name, or nil.
func (s *Supervisor) tenant(name string) *tenantRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenants[name]
}

// decisions returns the pipeline the worker published, if it takes
// decisions.
func (t *tenantRun) decisions() (Decisions, bool) {
	v := t.view.Load()
	if v == nil || v.pipeline == nil {
		return nil, false
	}
	d, ok := v.pipeline.(Decisions)
	return d, ok
}

// RemoveRejected removes the bytes of a file whose rejection the store
// recorded, at path in the tenant's incoming area, through the tenant's
// running pipeline. While none runs (one stopping, one starting, one that
// could not start) the removal is kept and done by the next pipeline once it
// has started. Without that, a rejection recorded after a starting
// pipeline's Recover had read the database kept the rejected bytes on disk,
// held by no row, until the start after, which could be weeks later. A
// tenant this supervisor does not run, or a removal past maxDeferred, is
// left to the next start's Recover, which reads the decision recorded.
func (s *Supervisor) RemoveRejected(ctx context.Context, name, path string) error {
	return s.remove(ctx, name, removal{rejected: path})
}

// RemoveDiscarded removes the bytes of the files of a reception whose
// discard the store recorded, as RemoveRejected does for one file.
func (s *Supervisor) RemoveDiscarded(ctx context.Context, name, receptionID string) error {
	return s.remove(ctx, name, removal{discarded: receptionID})
}

// removal is one removal of bytes after a recorded decision: of a rejected
// file at its incoming path, or of a discarded reception's files.
type removal struct {
	rejected, discarded string
}

// do hands the removal to a running pipeline.
func (r removal) do(ctx context.Context, d Decisions) error {
	if r.rejected != "" {
		return d.RemoveRejected(r.rejected)
	}
	return d.RemoveDiscarded(ctx, r.discarded)
}

// maxDeferred bounds the removals a tenant keeps while no pipeline runs. A
// removal past it is left to the next start's Recover: nothing is lost,
// the bytes only stay longer.
const maxDeferred = 4096

// deferredRemovals are the removals handed over while no pipeline ran.
type deferredRemovals struct {
	mu   sync.Mutex
	list []removal
}

// removeTries bounds how many pipelines one removal is handed to: one
// withdrawn while the removal reached it is followed by none or the next.
// A removal still not done after that is left to the next start's Recover.
const removeTries = 3

// remove hands r to the tenant's running pipeline, or keeps it for the next
// one. The worker publishes a new pipeline before it takes the kept
// removals (runDeferred), and a removal is kept only under the lock and
// after it saw no pipeline published, so none is kept after the last take.
func (s *Supervisor) remove(ctx context.Context, name string, r removal) error {
	t := s.tenant(name)
	if t == nil {
		return nil
	}
	for range removeTries {
		t.deferred.mu.Lock()
		d, ok := t.decisions()
		if !ok {
			if len(t.deferred.list) < maxDeferred {
				t.deferred.list = append(t.deferred.list, r)
			} else {
				t.log.Warn("receivers: too many removals wait for the tenant's pipeline; this one is left to the next start")
			}
			t.deferred.mu.Unlock()
			return nil
		}
		t.deferred.mu.Unlock()
		err := r.do(ctx, d)
		if err == nil {
			return nil
		}
		if now, ok := t.decisions(); ok && now == d {
			return err
		}
		// The pipeline was withdrawn while the removal reached it: it
		// refused as stopping, or removed nothing for certain. Hand it to
		// the next, or keep it.
	}
	return nil
}

// runDeferred hands the removals kept while no pipeline ran to d, the
// pipeline just published. One that fails is logged and left to the next
// start's Recover.
func (t *tenantRun) runDeferred(d Decisions) {
	t.deferred.mu.Lock()
	list := t.deferred.list
	t.deferred.list = nil
	t.deferred.mu.Unlock()
	for _, r := range list {
		ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
		err := r.do(ctx, d)
		cancel()
		if err != nil {
			t.log.Warn("receivers: the bytes of a recorded rejection or discard could not be removed; the next start removes them", "reason", fault.Message(err))
		}
	}
}

// Held reads the header of a file held in the incoming area, through the
// tenant's running pipeline, which owns that area. The reception pipeline
// (receive.Pipeline) is one.
type Held interface {
	// ReadHeld reads the header of the file at path (receive.Pipeline.ReadHeld).
	ReadHeld(ctx context.Context, path string) (*header.Header, error)
	// ReadHeldFile reads the bytes of the file at path, at most limit
	// (receive.Pipeline.ReadHeldFile).
	ReadHeldFile(ctx context.Context, path string, limit int64) ([]byte, error)
}

// The reception pipeline reads held files.
var _ Held = (*receive.Pipeline)(nil)

// HeldHeader reads the header of the held file at path in the incoming area
// of the tenant named name, through its running pipeline. ok is false when
// none runs, which reads memory only, as Decisions does; the errors are
// ReadHeld's.
func (s *Supervisor) HeldHeader(ctx context.Context, name, path string) (h *header.Header, ok bool, err error) {
	t := s.tenant(name)
	if t == nil {
		return nil, false, nil
	}
	v := t.view.Load()
	if v == nil || v.pipeline == nil {
		return nil, false, nil
	}
	held, ok := v.pipeline.(Held)
	if !ok {
		return nil, false, nil
	}
	h, err = held.ReadHeld(ctx, path)
	return h, true, err
}

// HeldFile reads the bytes of the held file at path in the incoming area of
// the tenant named name, at most limit, through its running pipeline. ok is
// false when none runs; the errors are ReadHeldFile's.
func (s *Supervisor) HeldFile(ctx context.Context, name, path string, limit int64) (b []byte, ok bool, err error) {
	t := s.tenant(name)
	if t == nil {
		return nil, false, nil
	}
	v := t.view.Load()
	if v == nil || v.pipeline == nil {
		return nil, false, nil
	}
	held, ok := v.pipeline.(Held)
	if !ok {
		return nil, false, nil
	}
	b, err = held.ReadHeldFile(ctx, path, limit)
	return b, true, err
}
