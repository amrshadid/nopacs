package supervise

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Decisions is what a tenant's running pipeline does for a person's
// decision on its quarantine and its failed receptions: the release and the
// retry run through it whole, so the bytes in the incoming area and the work
// queue follow the record in the order that survives a crash. The removal of
// bytes after a rejection or a discard the store recorded is not one of
// them: it is queued as work in the tenant's database (RemoveRejected,
// RemoveDiscarded, RemoveWithdrawn). The reception pipeline
// (receive.Pipeline) is one.
type Decisions interface {
	// Release releases a held file (receive.Pipeline.Release).
	Release(ctx context.Context, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error)
	// Retry makes a new reception of a failed one's files and queues its
	// first unit (receive.Pipeline.Retry).
	Retry(ctx context.Context, id, newReceptionID, receiver string, audit master.AuditEntry, guard tenant.Guard) (failed, retry tenant.Reception, err error)
}

// The reception pipeline takes decisions.
var _ Decisions = (*receive.Pipeline)(nil)

// Decisions returns the running pipeline of the tenant named name, for a
// decision, and whether one runs. It reads memory only: the pipeline the
// tenant's worker published, which it withdraws before it stops one and
// replaces as soon as the next has started.
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

// RemoveRejected queues the removal of the bytes of the quarantined file
// record recordID, whose rejection the store recorded, at path in the
// tenant's incoming area. The removal is a unit of work in the tenant's
// database (receive.QueueRemoval), so it is owed across a stop, a restart or
// a pipeline that cannot start, however many there are, and the tenant's
// pipeline runs it once it runs, reading the decision back first. A tenant
// the installation does not have is left alone.
func (s *Supervisor) RemoveRejected(ctx context.Context, name, recordID, path string) error {
	return s.remove(ctx, name, receive.RejectedRemoval(recordID, path))
}

// RemoveDiscarded queues the removal of the bytes of the files of a
// reception whose discard the store recorded, as RemoveRejected does for
// one file.
func (s *Supervisor) RemoveDiscarded(ctx context.Context, name, receptionID string) error {
	return s.remove(ctx, name, receive.DiscardedRemoval(receptionID))
}

// RemoveWithdrawn queues the removal of the bytes at path of a held file
// whose record the purging delete correction correctionID cleared, as
// RemoveRejected does for a rejected one.
func (s *Supervisor) RemoveWithdrawn(ctx context.Context, name, correctionID, path string) error {
	return s.remove(ctx, name, receive.WithdrawnRemoval(correctionID, path))
}

// waker is what a running pipeline offers to run queued work now
// (receive.Pipeline.WakeWork).
type waker interface{ WakeWork() }

// remove queues r in the tenant's database and wakes the tenant's pipeline,
// if one runs, to do it now.
func (s *Supervisor) remove(ctx context.Context, name string, r receive.Removal) error {
	st, err := s.cfg.Tenants.Store(name)
	if fault.KindOf(err) == fault.NotFound {
		return nil
	}
	if err != nil {
		return err
	}
	p, ok := st.(tenant.Pipeline)
	if !ok {
		return nil
	}
	if _, err := receive.QueueRemoval(ctx, p, r, s.cfg.Now()); err != nil {
		return err
	}
	// Asked of the tenant's worker, which may not have started yet, since
	// the unit is in the database whatever the supervisor has begun.
	if t := s.tenant(name); t != nil {
		if v := t.view.Load(); v != nil {
			if w, ok := v.pipeline.(waker); ok {
				w.WakeWork()
			}
		}
	}
	return nil
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
