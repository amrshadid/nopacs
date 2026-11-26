package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Disposals (tenant.Disposal) on a SQL database: the preview of a rule's
// due, unheld sessions, the dispose job a confirmed preview or the daily
// pass makes, and what became of each session. Each session is deleted by
// the ordinary delete (PrepareCorrection with its Retention set), never
// here.

// disposePage is how many due rows a preview reads at a time.
const disposePage = 1000

// disposalIn makes the preview of a disposal of rule at now: at most limit
// due sessions not held (and, for the daily pass, not exempt), oldest due
// first, and the counts of the rest. A rule the tenant does not have is
// NotFound; a paused retention a precondition failure.
func (k Sending) disposalIn(ctx context.Context, tx SendingTx, rule string, limit int, now time.Time, scheduled bool) (tenant.DisposalPreview, error) {
	p := tenant.DisposalPreview{Rule: rule, Max: limit, Issued: now.UTC().Truncate(time.Microsecond)}
	if _, found, err := ruleIn(ctx, tx, rule, ""); err != nil || !found {
		if err != nil {
			return p, err
		}
		return p, notFound("the tenant has no retention rule named " + rule)
	}
	clock, err := retentionClockIn(ctx, tx, "")
	if err != nil {
		return p, err
	}
	if clock.Paused {
		return p, tenant.RetentionPausedError()
	}
	holds, err := activeHolds(ctx, tx)
	if err != nil {
		return p, err
	}
	eff := clock.Effective(p.Issued)
	d, s := dueBefore, ""
	for {
		rows, err := k.readDue(ctx, tx, rule, eff, d, s, disposePage)
		if err != nil {
			return p, err
		}
		for _, r := range rows {
			switch {
			case !r.live:
			case holds.covering(r.subjectFolded, r.sessionFolded) != "":
				p.Held++
			case scheduled && r.s.Undone:
			case len(p.Sessions) < limit:
				p.Sessions = append(p.Sessions, r.s)
				p.Counts.Sessions++
				p.Counts.Scans += r.scans
				p.Counts.Instances += r.s.Instances
				p.Counts.Bytes += r.s.Bytes
			default:
				p.NotListed++
			}
		}
		if len(rows) < disposePage {
			break
		}
		last := rows[len(rows)-1]
		d, s = last.s.Due, last.s.StudyUID
	}
	p.Confirm = tenant.DisposeConfirm(p)
	return p, nil
}

// PreviewDisposal implements tenant.Disposal.
func (k Sending) PreviewDisposal(ctx context.Context, rule string, limit int, now time.Time) (tenant.DisposalPreview, error) {
	if err := tenant.ValidateName("rule", rule); err != nil {
		return tenant.DisposalPreview{}, err
	}
	if err := tenant.ValidateDisposeMax(limit); err != nil {
		return tenant.DisposalPreview{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.DisposalPreview{}, err
	}
	var out tenant.DisposalPreview
	err := k.Host.Read(ctx, "the disposal cannot be previewed", func(tx SendingTx) error {
		var err error
		out, err = k.disposalIn(ctx, tx, rule, limit, now, false)
		return err
	})
	return out, err
}

// QueueDispose implements tenant.Disposal.
func (k Sending) QueueDispose(ctx context.Context, d tenant.DisposeQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job,
	tenant.DisposalPreview, bool, error) {
	if err := d.Validate(); err != nil {
		return tenant.Job{}, tenant.DisposalPreview{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, tenant.DisposalPreview{}, false, err
	}
	var job tenant.Job
	var preview tenant.DisposalPreview
	var made bool
	err := k.Host.Write(ctx, "the disposal cannot be queued", func(tx SendingTx) error {
		made = false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		if !d.Scheduled {
			id, rule := "", ""
			if _, err := tx.QueryRow(ctx, `SELECT job_id, rule FROM dispose_jobs WHERE confirm_token = ?`, []any{d.Confirm}, &id, &rule); err != nil {
				return err
			}
			if id != "" {
				// A token answers only the disposal it was given for: a
				// repeat naming another rule would otherwise be told a job
				// of a rule it did not ask about was queued for it.
				if rule != d.Rule {
					return tenant.Precondition("the confirm token was used for a disposal under another rule")
				}
				r, err := jobInTx(ctx, tx, id, "")
				if err != nil {
					return err
				}
				job, preview = r.job, tenant.DisposalPreview{Rule: d.Rule, Confirm: d.Confirm}
				return tx.Audited(ctx, answeredEntry(audit, d.ID, id), guard)
			}
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, d.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		var err error
		if d.Scheduled {
			busy, err := exists(ctx, tx, `SELECT 1 FROM dispose_jobs WHERE rule = ? AND job_id IN (SELECT id FROM jobs WHERE `+
				k.D.NotEnded+`)`, d.Rule)
			if err != nil {
				return err
			}
			if busy {
				return tenant.Precondition("the rule " + d.Rule + " has a dispose job not ended")
			}
			if preview, err = k.disposalIn(ctx, tx, d.Rule, d.Max, d.Now, true); err != nil {
				return err
			}
		} else {
			issued, limit, ok := tenant.DisposeConfirmParts(d.Confirm)
			if !ok || d.Now.Sub(issued) > tenant.MaxConfirmAge || issued.After(d.Now.Add(time.Minute)) {
				return tenant.Precondition("the confirm token is not one of a disposal's preview made within the hour")
			}
			// The preview is made again at the token's time and bound, from
			// the rows, holds and clock as they stand now, and must list what
			// the person saw: a hold placed, a rule changed, a session
			// deleted, or retention paused since make it list otherwise (or
			// refuse). Made at the token's time, not now, it does not count
			// the sessions that only became due since, which on a busy
			// tenant would refuse nearly every token.
			if preview, err = k.disposalIn(ctx, tx, d.Rule, limit, issued, false); err != nil {
				return err
			}
			if preview.Confirm != d.Confirm {
				return tenant.Precondition("the confirm token does not match what the disposal would now delete")
			}
		}
		if len(preview.Sessions) == 0 {
			return tenant.Precondition("nothing is due under the rule " + d.Rule)
		}
		if job, err = k.insertDisposeJob(ctx, tx, d, preview); err != nil {
			return err
		}
		made = true
		return tx.Audited(ctx, audit, guard)
	})
	return job, preview, made, err
}

// answeredEntry returns audit naming the job answered in place of the one
// the caller would have made: a repeated token makes no job, so wherever
// the entry names the new id (its object or a detail) it names answered,
// and the log never points at a job that does not exist.
func answeredEntry(audit master.AuditEntry, made, answered string) master.AuditEntry {
	if audit.Object == made {
		audit.Object = answered
	}
	details := make(map[string]string, len(audit.Details))
	for k, v := range audit.Details {
		if v == made {
			v = answered
		}
		details[k] = v
	}
	audit.Details = details
	return audit
}

// insertDisposeJob writes a dispose job of preview's sessions, queued,
// with its event.
func (k Sending) insertDisposeJob(ctx context.Context, tx SendingTx, d tenant.DisposeQueue, p tenant.DisposalPreview) (tenant.Job, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
		receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
		retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
		scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
		failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
		VALUES (?, 'dispose', '', '', '', '', '', '', NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
		?, 0, ?, ?, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
		d.ID, k.D.Bool(false), d.KeyID, k.D.Time(d.Now), k.D.Time(d.Now), k.D.Time(d.Now), k.D.Bool(true), k.D.Bool(true), k.D.Bool(true),
		int64(len(p.Sessions)), p.Counts.Instances, p.Counts.Bytes); err != nil {
		return tenant.Job{}, err
	}
	var confirm any
	if !d.Scheduled {
		confirm = d.Confirm
	}
	if _, err := tx.Exec(ctx, `INSERT INTO dispose_jobs (job_id, rule, reason, scheduled, confirm_token) VALUES (?, ?, ?, ?, ?)`, d.ID,
		d.Rule, d.Reason, k.D.Bool(d.Scheduled), confirm); err != nil {
		return tenant.Job{}, err
	}
	for i, s := range p.Sessions {
		if _, err := tx.Exec(ctx, `INSERT INTO dispose_sessions (job_id, ord, study_uid, subject_label, session_label, due_at, instances,
			bytes, outcome, correction_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '')`, d.ID, i+1, s.StudyUID, s.SubjectLabel, s.SessionLabel,
			k.D.Time(s.Due), s.Instances, s.Bytes); err != nil {
			return tenant.Job{}, err
		}
	}
	r, err := jobInTx(ctx, tx, d.ID, "")
	if err != nil {
		return tenant.Job{}, err
	}
	e, err := tenant.JobStepEvent("", r.job, d.Now)
	if err != nil {
		return tenant.Job{}, err
	}
	return r.job, tx.WriteEvents(ctx, e)
}

// DisposeJob implements tenant.Disposal.
func (k Sending) DisposeJob(ctx context.Context, id string) (tenant.DisposeJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.DisposeJob{}, err
	}
	var out tenant.DisposeJob
	err := k.Host.Read(ctx, "the dispose job cannot be read", func(tx SendingTx) error {
		out = tenant.DisposeJob{}
		found, err := tx.QueryRow(ctx, `SELECT rule, reason, scheduled FROM dispose_jobs WHERE job_id = ?`, []any{id}, &out.Rule,
			&out.Reason, &out.Scheduled)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no dispose job with this id")
		}
		r, err := jobInTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		out.Job = r.job
		return tx.Query(ctx, `SELECT study_uid, subject_label, session_label, due_at, instances, bytes, outcome, correction_id
			FROM dispose_sessions WHERE job_id = ? ORDER BY ord`, []any{id}, func(row Row) error {
			var s tenant.DisposeSession
			var due DBTime
			var outcome string
			err := row.Scan(&s.StudyUID, &s.SubjectLabel, &s.SessionLabel, &due, &s.Instances, &s.Bytes, &outcome, &s.Correction)
			s.Due, s.Outcome = due.T, tenant.DisposeOutcome(outcome)
			out.Sessions = append(out.Sessions, s)
			return err
		})
	})
	return out, err
}

// RecordDisposed implements tenant.Disposal.
func (k Sending) RecordDisposed(ctx context.Context, jobID, studyUID string, outcome tenant.DisposeOutcome, correction string) error {
	if err := master.ValidateID("job id", jobID); err != nil {
		return err
	}
	if err := tenant.ValidateStudyUID(studyUID); err != nil {
		return err
	}
	if !outcome.Valid() {
		return fault.New(fault.Invalid, "a disposed session's outcome is deleted or one of the skips")
	}
	if (outcome == tenant.DisposeDeleted) != (correction != "") {
		return fault.New(fault.Invalid, "a session deleted names its delete, and only then")
	}
	if correction != "" {
		if err := tenant.ValidateCorrectionID(correction); err != nil {
			return err
		}
	}
	return k.Host.Write(ctx, "the disposed session cannot be recorded", func(tx SendingTx) error {
		if has, err := exists(ctx, tx, `SELECT 1 FROM dispose_sessions WHERE job_id = ? AND study_uid = ?`, jobID, studyUID); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the dispose job does not list this session")
		}
		// An ended job's tallies are final: a cancel stops what the runner
		// records, as it stops what it deletes.
		if err := k.checkDisposeJobOpen(ctx, tx, jobID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE dispose_sessions SET outcome = ?, correction_id = ? WHERE job_id = ? AND study_uid = ? AND outcome = ''`,
			string(outcome), correction, jobID, studyUID)
		return err
	})
}
