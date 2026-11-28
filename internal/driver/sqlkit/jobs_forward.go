package sqlkit

import (
	"context"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Forward makes, in the transaction of a reception's step that reached
// done, the send jobs of its forwards, and returns their events, which the
// driver writes with the step's:
//   - one per sender its receiver forwards to (tenant.Receiver.ForwardTo),
//     for the reception's whole session;
//   - one per sender and series of each forward rule that held for it
//     (tenant.ReceptionRoute.Forwards), chosen when it was identified, so
//     even a receiver removed meanwhile forwards what its rules chose: one
//     per series the reception archived, or only those a series filter
//     chose, each with the rule's destination profile, which the sender
//     applies to every copy. A session's earlier arrivals are never sent.
//
// A sender removed meanwhile is skipped; one forwarded to already is not
// made twice (tenant.ForwardKey, tenant.RuleForwardKey). A reception not
// done, or one that archived nothing (every file a duplicate or
// quarantined), makes nothing: its job would send nothing.
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
	var c dimseConfig
	if found {
		if err := FromJSON(config, &c); err != nil {
			return nil, err
		}
	}
	session := tenant.JobTarget{SubjectLabel: r.SubjectLabel, SessionLabel: r.SessionLabel}
	var events []tenant.EventRecord
	add := func(sender, id, key string, target tenant.JobTarget, profile tenant.ProfileRef) error {
		has, err := k.lockSender(ctx, tx, sender)
		if err != nil || !has {
			return err
		}
		_, e, err := k.insertJob(ctx, tx, newJob{kind: tenant.JobSend, id: id, sender: sender, target: target, receptionID: r.ID,
			receiver: r.Receiver, uniqueKey: key, created: r.StepTime, bound: r.StepTime, retryListed: true, profile: profile})
		if err != nil {
			return err
		}
		if e != nil {
			events = append(events, *e)
		}
		return nil
	}
	// A receiver removed meanwhile forwards to none of its own senders.
	for _, sender := range c.ForwardTo {
		if err := add(sender, tenant.ForwardJobID(r.ID, sender), tenant.ForwardKey(r.ID, sender), session, tenant.ProfileRef{}); err != nil {
			return nil, err
		}
	}
	// The series this reception brought: a forward rule was judged on this
	// reception's files, so a session's earlier arrivals are not its to send.
	type brought struct{ uid, scan string }
	var arrived []brought
	err = tx.Query(ctx, `SELECT s.series_uid, s.label FROM scans AS s JOIN reception_scans AS r ON r.scan_id = s.id
		WHERE r.reception_id = ? AND r.instances > 0 ORDER BY s.series_uid`, []any{r.ID}, func(row Row) error {
		var b brought
		err := row.Scan(&b.uid, &b.scan)
		arrived = append(arrived, b)
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, f := range r.Route.Forwards {
		for _, sender := range f.Senders {
			for _, b := range arrived {
				// A filter chose some series; a series that was held or whose
				// every file was a duplicate is not among those arrived.
				if f.Filtered && !slices.Contains(f.Series, b.uid) {
					continue
				}
				target := session
				target.ScanLabel = b.scan
				if err := add(sender, tenant.RuleForwardJobID(r.ID, f.Rule, sender, b.uid), tenant.RuleForwardKey(r.ID, f.Rule, sender, b.uid),
					target, f.Profile); err != nil {
					return nil, err
				}
			}
		}
	}
	return events, nil
}
