package receive

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The kinds of unit the pipeline queues, one per step a worker runs. A
// reception past receiving always has one of them queued or leased (the
// store's rule): each step queues the next in the transaction that
// completes its own unit (CompleteWork with next).
const (
	kindQuiet    = "receive-quiet"
	kindIdentify = "receive-identify"
	kindArchive  = "receive-archive"
	kindVerify   = "receive-verify"
)

// claimOrder is the order a worker asks for work: the steps nearest the end
// first, so receptions finish before new ones start and the incoming area
// empties.
var claimOrder = []string{kindVerify, kindArchive, kindIdentify, kindQuiet}

// kindFor returns the kind of unit a reception in state s needs, or "" for
// one that has ended.
func kindFor(s reception.State) string {
	switch s {
	case reception.Receiving:
		return kindQuiet
	case reception.Complete:
		return kindIdentify
	case reception.Identified:
		return kindArchive
	case reception.Archived, reception.Verified:
		return kindVerify
	}
	return ""
}

// unitFor returns the unit of kind for reception id, due at due. Its unique
// key is the kind and the reception, so queuing it twice queues it once.
func unitFor(kind, receptionID string, due time.Time) tenant.Work {
	return tenant.Work{ID: newID(), Kind: kind, ReceptionID: receptionID, UniqueKey: kind + ":" + receptionID, Due: due}
}

// ensureQuiet queues the quiet unit of a receiving reception, due when its
// shortest quiet period could first end. It is queued once whatever the
// number of calls.
func (p *Pipeline) ensureQuiet(ctx context.Context, r tenant.Reception) error {
	if r.State != reception.Receiving {
		return nil
	}
	return p.enqueue(ctx, unitFor(kindQuiet, r.ID, r.Started.Add(tenant.MinQuietPeriod)))
}

// ensureNext queues the unit a reception's state needs, due now.
func (p *Pipeline) ensureNext(ctx context.Context, r tenant.Reception) error {
	k := kindFor(r.State)
	if k == "" {
		return nil
	}
	if k == kindQuiet {
		return p.ensureQuiet(ctx, r)
	}
	return p.enqueue(ctx, unitFor(k, r.ID, p.now()))
}

// enqueue queues a unit and wakes a worker.
func (p *Pipeline) enqueue(ctx context.Context, w tenant.Work) error {
	if _, _, err := p.store.EnqueueWork(ctx, w, p.now()); err != nil {
		return err
	}
	p.wakeWorkers()
	return nil
}

// worker is one of the pool's loops: it claims a unit of each kind in turn
// and runs it, and waits for a poke or the poll interval when there is none.
func (p *Pipeline) worker(owner string) {
	defer p.wg.Done()
	for p.workCtx.Err() == nil {
		did := false
		for _, kind := range claimOrder {
			if p.workCtx.Err() != nil {
				return
			}
			c, err := p.store.ClaimNextWork(p.workCtx, kind, owner, p.now(), p.cfg.Lease)
			if err != nil {
				if p.workCtx.Err() == nil {
					p.log.Warn("work could not be claimed", "kind", kind, "reason", safe(err))
				}
				break
			}
			p.gaveUp(c.GaveUp)
			if !c.Found {
				continue
			}
			did = true
			p.stepping.Add(1)
			p.runUnit(c.Work)
			p.stepping.Add(-1)
		}
		if did {
			continue
		}
		select {
		case <-p.workCtx.Done():
			return
		case <-p.poke:
		case <-p.cfg.Clock.After(p.cfg.PollInterval):
		}
	}
}

// gaveUp reports the units a claim held as failed on the way, their last
// attempt's lease having ended, and each reception that failed with them:
// a failed reception keeps every incoming copy for a person's retry or
// discard, and its fail step wrote its event, so the live feed is woken at
// once rather than at its next timer.
func (p *Pipeline) gaveUp(units []tenant.GaveUpWork) {
	if len(units) == 0 {
		return
	}
	for _, u := range units {
		if u.ReceptionFailed {
			p.log.Error("a reception failed: its work gave up after its last attempt; its files are kept for a retry or a discard",
				"reception", u.ReceptionID, "work", u.WorkID)
		} else {
			p.log.Warn("a unit of work gave up after its last attempt", "work", u.WorkID)
		}
	}
	p.wake()
}

// errLeaseLost is the cause a step's context is ended with when its lease
// was taken over.
var errLeaseLost = errors.New("the lease on the unit was lost")

// runUnit runs one claimed unit with a heartbeat that renews its lease, and
// records the attempt's end: a step that returns nil has completed its unit
// itself; one that fails records the failure (FailWork), so the unit is due
// again after its retry delay or, after its last attempt, held as failed
// with its reception; one whose lease was lost, or that the pipeline's stop
// cut short, records nothing, and its unit is taken up when its lease ends.
func (p *Pipeline) runUnit(w tenant.Work) {
	ctx, cancel := context.WithCancelCause(p.stepCtx)
	defer cancel(nil)
	done := make(chan struct{})
	beat := make(chan struct{})
	go p.heartbeat(w.Fence(), done, beat, cancel)
	p.at("step.start:" + w.Kind)
	err := p.runStep(ctx, w)
	close(done)
	<-beat
	switch {
	case err == nil:
		p.stats.StepsDone.Add(1)
		p.at("step.done:" + w.Kind)
		return
	case errors.Is(err, tenant.ErrLeaseLost) || errors.Is(context.Cause(ctx), errLeaseLost):
		p.log.Info("a step stopped: its lease was taken over", "kind", w.Kind, "reception", w.ReceptionID)
		p.at("step.lease-lost:" + w.Kind)
		return
	case p.stepCtx.Err() != nil:
		p.at("step.stopped:" + w.Kind)
		return
	}
	p.stats.StepsFailed.Add(1)
	p.log.Warn("a step failed; it is tried again after its delay", "kind", w.Kind, "reception", w.ReceptionID,
		"attempt", w.Attempt, "reason", safe(err))
	if _, ferr := p.store.FailWork(context.WithoutCancel(ctx), w.Fence(), reasonText(err), p.now()); ferr != nil {
		p.log.Warn("a failed attempt could not be recorded; its lease ends on its own", "kind", w.Kind, "reason", safe(ferr))
	} else {
		// A unit that gave up fails its reception in the same transaction,
		// which writes an event.
		p.wake()
	}
	p.at("step.failed:" + w.Kind)
}

// heartbeat renews a unit's lease every third of the lease until done is
// closed. A renewal refused because the lease was taken over ends the step's
// context, so the step stops at its next call; every write it would still
// make is refused by its fence anyway.
func (p *Pipeline) heartbeat(fence tenant.WorkFence, done <-chan struct{}, beat chan<- struct{}, cancel context.CancelCauseFunc) {
	defer close(beat)
	for {
		select {
		case <-done:
			return
		case <-p.cfg.Clock.After(p.cfg.Lease / 3):
		}
		_, err := p.store.RenewWork(p.stepCtx, fence, p.now(), p.cfg.Lease)
		if errors.Is(err, tenant.ErrLeaseLost) {
			cancel(errLeaseLost)
			return
		}
		if err != nil && p.stepCtx.Err() == nil {
			p.log.Warn("a lease could not be renewed; trying again", "reason", safe(err))
		}
	}
}

// runStep runs the step a unit's kind names.
func (p *Pipeline) runStep(ctx context.Context, w tenant.Work) error {
	switch w.Kind {
	case kindQuiet:
		return p.quiet(ctx, w)
	case kindIdentify:
		return p.identify(ctx, w)
	case kindArchive:
		return p.archive(ctx, w)
	case kindVerify:
		return p.verify(ctx, w)
	}
	return fault.New(fault.Internal, "a unit of an unknown kind was claimed")
}

// step takes one step of a reception under the unit's fence, from the state
// the caller read. A step the reception has taken already (a run again
// after a crash) is not an error: the reception is read again and returned.
func (p *Pipeline) step(ctx context.Context, w tenant.Work, s tenant.ReceptionStep) (tenant.Reception, error) {
	fence := w.Fence()
	s.Fence = &fence
	s.At = p.now()
	r, err := p.store.StepReception(ctx, s, p.cfg.StepHook)
	var refusal *reception.Refusal
	if errors.As(err, &refusal) && refusal.Kind == reception.Already {
		r, _, err = p.store.Reception(ctx, s.ID)
		return r, err
	}
	if err != nil {
		return tenant.Reception{}, err
	}
	p.wake()
	return r, nil
}

// complete marks the unit done and queues next with it, in one transaction.
func (p *Pipeline) complete(ctx context.Context, w tenant.Work, next *tenant.Work) error {
	if err := p.store.CompleteWork(ctx, w.Fence(), p.now(), next); err != nil {
		return err
	}
	if next != nil {
		p.wakeWorkers()
	}
	return nil
}

// completeFor completes the unit of a reception found already past the
// unit's step (the step was taken before a crash): the unit the reception's
// state needs is queued with it, or none for one that ended.
func (p *Pipeline) completeFor(ctx context.Context, w tenant.Work, r tenant.Reception) error {
	k := kindFor(r.State)
	if k == "" || k == w.Kind {
		return p.complete(ctx, w, nil)
	}
	next := unitFor(k, r.ID, p.now())
	return p.complete(ctx, w, &next)
}
