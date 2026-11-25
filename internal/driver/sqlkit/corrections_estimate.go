package sqlkit

import (
	"context"
	"time"

	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A delete's preview: what it would remove, the senders any of it was sent
// to, the jobs that would meet it, what its purge clears besides its
// objects, what stays held after, and the token a delete must carry.

// previewIn makes the preview of a delete of target in tx.
func (k Sending) previewIn(ctx context.Context, tx SendingTx, target tenant.CorrectionTarget, now time.Time) (tenant.DeletePreview, error) {
	r, err := resolveTarget(ctx, tx, target, true)
	if err != nil {
		return tenant.DeletePreview{}, err
	}
	p := tenant.DeletePreview{Target: target, Counts: r.counts(), Storages: r.storages(), Issued: now.UTC().Truncate(time.Microsecond)}
	scope, arg := r.scanScope()
	// The senders it was sent to: the send jobs' scans that answered any of
	// it sent. One instance is in its scan, and the jobs count a scan's
	// instances, not which: its scan's senders are those it may have
	// reached.
	// #nosec G202 -- the scope is a constant of resolveTarget; values are parameters.
	err = tx.Query(ctx, `SELECT j.sender, sum(js.sent), max(j.state_at) FROM job_scans AS js JOIN jobs AS j ON j.id = js.job_id
		WHERE j.kind = 'send' AND js.sent > 0 AND js.scan_id IN `+scope+` GROUP BY j.sender ORDER BY j.sender`, []any{arg},
		func(row Row) error {
			var pc tenant.PeerCopy
			var at DBTime
			err := row.Scan(&pc.Sender, &pc.Instances, &at)
			pc.LastSend = at.T
			p.SentTo = append(p.SentTo, pc)
			return err
		})
	if err != nil {
		return p, err
	}
	// The jobs not ended that name its subject, and its session and scan
	// when they name one. A job over the whole tenant may reach it too, but
	// is only counted: such jobs start and end all the time, and the token
	// is of what the delete takes, not of the tenant's traffic.
	t := target
	err = tx.Query(ctx, `SELECT id FROM jobs WHERE `+k.D.NotEnded+` AND kind IN ('send', 'deid', 'scrub')
		AND subject_label = ?
		AND (session_label = '' OR ? = '' OR session_label = ?)
		AND (scan_label = '' OR ? = '' OR scan_label = ?)
		ORDER BY created DESC, id DESC`, []any{t.Subject, t.Session, t.Session, t.Scan, t.Scan}, func(row Row) error {
		var id string
		if err := row.Scan(&id); err != nil {
			return err
		}
		if len(p.Jobs) < tenant.MaxPreviewJobs {
			p.Jobs = append(p.Jobs, id)
		} else {
			p.JobsNotListed++
		}
		return nil
	})
	if err != nil {
		return p, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE `+k.D.NotEnded+` AND kind IN ('send', 'deid', 'scrub')
		AND subject_label = ''`, nil, &p.TenantJobs); err != nil {
		return p, err
	}
	if err := k.previewClears(ctx, tx, r, target, &p); err != nil {
		return p, err
	}
	if err := k.previewRetention(ctx, tx, r, target, &p); err != nil {
		return p, err
	}
	p.Confirm = tenant.DeleteConfirm(p)
	return p, nil
}

// previewClears counts what the purge of a delete of r clears besides its
// objects (ClearWithdrawn), as the catalog holds it now, and the subject
// labels held after.
func (k Sending) previewClears(ctx context.Context, tx SendingTx, r targetRows, target tenant.CorrectionTarget, p *tenant.DeletePreview) error {
	var labels []string
	if r.level == corr.LevelSubject {
		var err error
		if labels, err = subjectLabels(ctx, tx, target.Subject, ""); err != nil {
			return err
		}
		p.HeldAfterPurge = labels
	}
	scope, arg := r.scanScope()
	var where string
	args := []any{}
	switch {
	case r.instance != nil:
		where, args = `q.sop_instance_uid = ?`, append(args, r.instance.uid)
	case r.level == corr.LevelScan:
		where, args = `q.series_uid IN (SELECT s.series_uid FROM scans AS s WHERE s.id IN `+scope+`)`, append(args, arg)
	default:
		where, args = `q.study_uid IN (SELECT s.study_uid FROM scans AS s WHERE s.id IN `+scope+`)`, append(args, arg)
	}
	labelWhere := ""
	for i, l := range labels {
		if i == 0 {
			labelWhere = `?`
		} else {
			labelWhere += `, ?`
		}
		args = append(args, l)
	}
	if labelWhere != "" {
		where = `(` + where + ` OR q.reception_id IN (SELECT r.id FROM receptions AS r WHERE r.subject_label IN (` + labelWhere + `)))`
	}
	// #nosec G202 -- every fragment is a constant; values are parameters.
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM quarantine AS q WHERE `+where+` AND NOT EXISTS
		(SELECT 1 FROM receptions AS rr WHERE rr.id = q.reception_id AND rr.ended = ?)`, append(args, k.D.Bool(false)), &p.Quarantined); err != nil {
		return err
	}
	if r.level != corr.LevelSubject && r.level != corr.LevelSession {
		return nil
	}
	rargs := []any{k.D.Bool(true), arg}
	rwhere := `r.study_uid IN (SELECT s.study_uid FROM scans AS s WHERE s.id IN ` + scope + `)`
	if labelWhere != "" {
		rwhere = `(` + rwhere + ` OR r.subject_label IN (` + labelWhere + `))`
		for _, l := range labels {
			rargs = append(rargs, l)
		}
	}
	// #nosec G202 -- as above.
	_, err := tx.QueryRow(ctx, `SELECT count(*) FROM receptions AS r WHERE r.ended = ? AND `+rwhere, rargs, &p.Receptions)
	return err
}

// PreviewDelete implements tenant.Corrections.
func (k Sending) PreviewDelete(ctx context.Context, target tenant.CorrectionTarget, now time.Time) (tenant.DeletePreview, error) {
	if err := target.Validate(); err != nil {
		return tenant.DeletePreview{}, err
	}
	if err := master.ValidateTime("preview time", now); err != nil {
		return tenant.DeletePreview{}, err
	}
	var out tenant.DeletePreview
	err := k.Host.Read(ctx, "the delete cannot be previewed", func(tx SendingTx) error {
		var err error
		out, err = k.previewIn(ctx, tx, target, now)
		return err
	})
	return out, err
}
