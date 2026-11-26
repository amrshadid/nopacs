package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What holds and retention refuse of a delete, checked in its preview and
// in its prepare's transaction: an active hold that covers any of the
// target; a rule that keeps any of it at least until a time to come; and,
// for a delete a disposal makes, the session no longer due under its rule.

// targetStudies returns the Study Instance UIDs of r's sessions, in order.
func (r targetRows) targetStudies() []string {
	var out []string
	for _, s := range r.scans {
		if len(out) == 0 || out[len(out)-1] != s.study {
			out = append(out, s.study)
		}
	}
	return out
}

// keptAt returns how many of rows a rule keeps beyond at, and the latest
// time it keeps any of them until (zero for none).
func keptAt(rows map[string]retentionRow, at time.Time) (int64, time.Time) {
	var n int64
	var until time.Time
	for _, r := range rows {
		if r.keep.After(at) {
			n++
			if r.keep.After(until) {
				until = r.keep
			}
		}
	}
	return n, until
}

// previewRetention fills a delete's preview with the active holds that
// cover any of it and what retention keeps of it at the preview's time.
func (k Sending) previewRetention(ctx context.Context, tx SendingTx, r targetRows, t tenant.CorrectionTarget, p *tenant.DeletePreview) error {
	holds, err := activeHolds(ctx, tx)
	if err != nil {
		return err
	}
	p.Holds = nil
	for _, h := range holds {
		if h.target.Covers(t.Path()) {
			p.Holds = append(p.Holds, h.id)
		}
	}
	rows, err := retentionRowsIn(ctx, tx, r.targetStudies())
	if err != nil {
		return err
	}
	p.SessionsUnderMinimum, p.KeepUntil = keptAt(rows, p.Issued)
	return nil
}

// checkRetentionDelete refuses a delete p of r that a hold covers (HELD),
// then gives tenant.CheckRetentionDelete what it decides retention's
// refusal on: each session's times as stored and as the rules standing
// under the configuration lock this prepare holds give them again, and,
// for a disposal's delete, where retention's clock stands. The times are
// read again from the rules because a stored keep is only as good as the
// commit that stored it: one that raced a rule's change, or a row written
// by hand, never lets a delete through that the rules now refuse.
func (k Sending) checkRetentionDelete(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare, r targetRows) error {
	holds, err := activeHolds(ctx, tx)
	if err != nil {
		return err
	}
	if id := holds.coveringPath(p.Target.Path()); id != "" {
		return tenant.HoldRefusalError(id)
	}
	studies := r.targetStudies()
	c := tenant.RetentionDeleteCheck{Origin: p.Retention, Override: p.OverrideRetention, Now: p.Now}
	if p.Retention != nil {
		// A disposal's delete takes one session, and the check refuses it
		// unless the target holds exactly the one the job listed.
		c.Studies = studies
		studies = studies[:min(len(studies), 1)]
	}
	rows, err := retentionRowsIn(ctx, tx, studies)
	if err != nil {
		return err
	}
	rules, err := retentionRulesIn(ctx, tx)
	if err != nil {
		return err
	}
	for _, study := range studies {
		row, found := rows[study]
		s := tenant.SessionKept{Found: found, Stored: row.times(), Exempt: row.exempt}
		if found && len(rules) > 0 {
			arrivals, err := arrivalsOf(ctx, tx, study)
			if err != nil {
				return err
			}
			s.Rules = tenant.RetentionTimesOf(rules, row.start, row.floor, arrivals[study])
		}
		c.Sessions = append(c.Sessions, s)
	}
	if p.Retention != nil {
		clock, err := retentionClockIn(ctx, tx, "")
		if err != nil {
			return err
		}
		c.Paused, c.Effective = clock.Paused, clock.Effective(p.Now)
	}
	return tenant.CheckRetentionDelete(c)
}

// checkDisposeJobOpen refuses a disposal's step (ErrDisposeJobEnded) unless
// the dispose job of that id exists and has not ended. The job's row is
// read under the dialect's row lock, which a cancel's update of it takes
// too, so a cancel committed before this transaction reads the row stops
// the step, and one after it waits for the step's commit: a canceled job
// never deletes another session.
func (k Sending) checkDisposeJobOpen(ctx context.Context, tx SendingTx, id string) error {
	if err := master.ValidateID("dispose job id", id); err != nil {
		return err
	}
	if has, err := exists(ctx, tx, `SELECT 1 FROM dispose_jobs WHERE job_id = ?`, id); err != nil || !has {
		if err != nil {
			return err
		}
		return tenant.DisposeJobEndedError()
	}
	j, err := jobInTx(ctx, tx, id, k.D.Lock)
	if err != nil {
		return err
	}
	if j.job.State.Ended() {
		return tenant.DisposeJobEndedError()
	}
	return nil
}

// markRetention records on a prepared delete its origin, rule and due time
// when a disposal made it, and an override of a rule's keep-at-least with
// its reason.
func (k Sending) markRetention(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	if o := p.Retention; o != nil {
		if _, err := tx.Exec(ctx, `UPDATE corrections SET origin = ?, retention_rule = ?, retention_due = ? WHERE id = ?`,
			tenant.OriginRetention, o.Rule, k.D.Time(o.Due), p.ID); err != nil {
			return err
		}
	}
	if p.OverrideRetention {
		if _, err := tx.Exec(ctx, `UPDATE corrections SET override_retention = ?, override_reason = ? WHERE id = ?`, k.D.Bool(true),
			p.OverrideReason, p.ID); err != nil {
			return err
		}
	}
	return nil
}
