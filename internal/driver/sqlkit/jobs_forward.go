package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Forward makes, in the transaction of a reception's step that reached
// done, one job per sender its receiver forwards to (tenant.Sending), or
// its route when the route chosen names its own senders (even when the
// receiver was removed meanwhile), and
// returns their events, which the driver writes with the step's. A sender
// removed meanwhile is skipped; one forwarded to already is not made twice
// (tenant.ForwardKey). A reception not done, or one that archived nothing
// (every file a duplicate or quarantined), makes nothing: its job would
// send nothing.
func (k Sending) Forward(ctx context.Context, tx SendingTx, r tenant.Reception) ([]tenant.EventRecord, error) {
	if r.State != reception.Done {
		return nil, nil
	}
	if archived, err := exists(ctx, tx, receptionArchivedSQL, r.ID); err != nil || !archived {
		return nil, err
	}
	var config string
	found, err := tx.QueryRow(ctx, `SELECT config FROM receivers WHERE name = ?`, []any{r.Receiver}, &config)
	if err != nil {
		return nil, err
	}
	// A receiver removed meanwhile forwards to none of its own senders;
	// a route chosen for the reception still names its own, which were
	// chosen for this reception's study when it was identified.
	var c dimseConfig
	if found {
		if err := FromJSON(config, &c); err != nil {
			return nil, err
		}
	} else if !r.Route.ReplacesForwardTo {
		return nil, nil
	}
	var events []tenant.EventRecord
	for _, sender := range r.Route.Senders(tenant.Receiver{ForwardTo: c.ForwardTo}) {
		has, err := k.lockSender(ctx, tx, sender)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		_, e, err := k.insertJob(ctx, tx, newJob{kind: tenant.JobSend, id: tenant.ForwardJobID(r.ID, sender), sender: sender,
			target: tenant.JobTarget{SubjectLabel: r.SubjectLabel, SessionLabel: r.SessionLabel}, receptionID: r.ID,
			receiver: r.Receiver, uniqueKey: tenant.ForwardKey(r.ID, sender), created: r.StepTime, bound: r.StepTime,
			retryListed: true})
		if err != nil {
			return nil, err
		}
		if e != nil {
			events = append(events, *e)
		}
	}
	return events, nil
}
