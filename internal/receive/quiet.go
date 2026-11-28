package receive

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// quiet decides whether a receiving reception is complete, and completes it.
//
// Order of effects, each safe to repeat or to stop after:
//
//  1. Read the reception. One past receiving took this step before a
//     crash: its unit is completed with the unit its state needs (3 again).
//  2. Decide with reception.Completion from the stored start and last file
//     times, the last file time taken no earlier than this process's start:
//     a sender could not send while the server was down, so a reception
//     that was receiving across a restart gets a whole quiet period from
//     the restart before it is called complete, instead of being cut at
//     once and splitting a study whose sender is still retrying. Not yet
//     complete: the unit is completed with a new quiet unit due when the
//     decision says, in one transaction (CompleteWork with next), so the
//     reception always has its unit.
//  3. Wait while the batcher holds files whose senders were answered and whose
//     rows are not written yet: they belong to this reception, and its
//     closing first would split the study. Then take the quiet step, guarded by the last file time the decision read
//     (ReceptionStep.LastFile): a batch of files written after the decision
//     moves that time, the step is refused with ErrStateChanged, and the
//     decision is made again. A batch written after the step is refused by
//     the store as closed, and its files open the next arrival.
//  4. Complete the unit with the identify unit, in one transaction. A crash
//     between 3 and 4 leaves the unit leased; when its lease ends it is
//     claimed again and 1 completes it.
func (p *Pipeline) quiet(ctx context.Context, w tenant.Work) error {
	for {
		r, _, err := p.store.Reception(ctx, w.ReceptionID)
		if err != nil {
			return err
		}
		if r.State != reception.Receiving {
			p.receptions.drop(r.ID)
			return p.completeFor(ctx, w, r)
		}
		quiet, maxOpen := tenant.DefaultQuietPeriod, tenant.DefaultMaxOpenTime
		rcv, err := p.receivers.get(ctx, r.Receiver)
		switch {
		case err == nil:
			quiet, maxOpen = rcv.QuietPeriod, rcv.MaxOpenTime
		case fault.KindOf(err) != fault.NotFound:
			return err
		}
		now := p.now()
		last := r.LastFile
		if last.Before(p.started) {
			last = p.started
		}
		d, err := reception.Completion(reception.Timing{Opened: r.Started, LastFile: last, Quiet: quiet, MaxOpen: maxOpen}, now)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "the receiver's quiet period cannot be applied")
		}
		if !d.Complete {
			next := unitFor(kindQuiet, r.ID, d.Due)
			return p.complete(ctx, w, &next)
		}
		// A file the sender was answered for may have no row yet: the batcher
		// writes the rows a moment after the answer, and a reception closed in
		// that moment would take the file as the first of another. The decision
		// is made again when the batcher has written what it holds.
		if p.batch.pending() {
			next := unitFor(kindQuiet, r.ID, now.Add(p.cfg.BatchInterval))
			return p.complete(ctx, w, &next)
		}
		p.at("quiet.before-step")
		_, err = p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, LastFile: r.LastFile})
		if errors.Is(err, tenant.ErrStateChanged) {
			continue
		}
		if err != nil {
			return err
		}
		p.receptions.drop(r.ID)
		p.at("quiet.after-step")
		next := unitFor(kindIdentify, r.ID, p.now())
		return p.complete(ctx, w, &next)
	}
}
