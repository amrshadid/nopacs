package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a restore asks of a tenant's database around its rebuild
// (tenant.Restoring): the quarantined files whose bytes are gone, the
// labels a subject's delete holds, the rebuilds a copy left not ended.

// MarkQuarantineLost implements tenant.Restoring: every record held whose
// bytes were kept, but those keep names, becomes incoming lost, with no
// details and no bytes kept (the reason's own rules:
// tenant.QuarantinedFile.Validate), and the quarantine's totals stop
// counting the bytes kept for them.
func (k Sending) MarkQuarantineLost(ctx context.Context, keep []string) (int64, error) {
	kept := make(map[string]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}
	const lost = `UPDATE quarantine SET reason = ?, details = '{}', content_kept = ?, incoming = '', kept_sha256 = NULL
		WHERE state = ? AND content_kept = ?`
	var n int64
	err := k.Host.Write(ctx, "the quarantined files whose bytes are gone cannot be marked", func(tx SendingTx) error {
		n = 0
		var bytes int64
		args := []any{string(tenant.QuarantineIncomingLost), k.D.Bool(false), string(tenant.QuarantineHeld), k.D.Bool(true)}
		if len(kept) == 0 {
			if _, err := tx.QueryRow(ctx, `SELECT count(*), CAST(coalesce(sum(size), 0) AS BIGINT) FROM quarantine WHERE state = ? AND content_kept = ?`,
				args[2:], &n, &bytes); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			if _, err := tx.Exec(ctx, lost, args...); err != nil {
				return err
			}
		} else {
			type held struct {
				id   string
				size int64
			}
			var all []held
			if err := tx.Query(ctx, `SELECT id, size FROM quarantine WHERE state = ? AND content_kept = ? ORDER BY id`, args[2:], func(r Row) error {
				var h held
				err := r.Scan(&h.id, &h.size)
				all = append(all, h)
				return err
			}); err != nil {
				return err
			}
			for _, h := range all {
				if kept[h.id] {
					continue
				}
				if _, err := tx.Exec(ctx, lost+` AND id = ?`, append(args, h.id)...); err != nil {
					return err
				}
				n++
				bytes += h.size
			}
			if n == 0 {
				return nil
			}
		}
		_, err := tx.Exec(ctx, `UPDATE quarantine_totals SET bytes = CASE WHEN bytes > ? THEN bytes - ? ELSE 0 END`, bytes, bytes)
		return err
	})
	return n, err
}

// HoldEarlierSubjectLabels implements tenant.Restoring, by the rule a
// delete's publish names a subject's labels by (subjectLabels), following
// only the relabels that came before each delete.
func (k Sending) HoldEarlierSubjectLabels(ctx context.Context) (int64, error) {
	var n int64
	err := k.Host.Write(ctx, "the labels a subject's delete holds cannot be completed", func(tx SendingTx) error {
		n = 0
		type named struct{ id, label string }
		var targets []named
		if err := tx.Query(ctx, `SELECT t.correction_id, t.value FROM deleted_targets AS t JOIN corrections AS c ON c.id = t.correction_id
			WHERE t.kind = 'subject' AND c.kind = 'delete' AND c.state = 'committed' ORDER BY t.correction_id, t.value`, nil, func(r Row) error {
			var x named
			err := r.Scan(&x.id, &x.label)
			targets = append(targets, x)
			return err
		}); err != nil {
			return err
		}
		for _, x := range targets {
			labels, err := subjectLabels(ctx, tx, x.label, x.id)
			if err != nil {
				return err
			}
			for _, l := range labels[1:] {
				added, err := tx.Exec(ctx, `INSERT INTO deleted_targets (kind, value, correction_id) VALUES ('subject', ?, ?) ON CONFLICT DO NOTHING`,
					l, x.id)
				if err != nil {
					return err
				}
				n += added
			}
		}
		return nil
	})
	return n, err
}

// EndRebuilds implements tenant.Restoring: each rebuild job not ended is
// canceled as a cancel cancels it (cancelRebuildInTx), its unit left for a
// worker that meets it ended, with its event.
func (k Sending) EndRebuilds(ctx context.Context, now time.Time) ([]string, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return nil, err
	}
	var out []string
	err := k.Host.Write(ctx, "the rebuilds a restore ends cannot be ended", func(tx SendingTx) error {
		out = nil
		var ids []string
		if err := tx.Query(ctx, `SELECT id FROM jobs WHERE kind = 'rebuild' AND `+k.D.NotEnded+` ORDER BY id`, nil, func(r Row) error {
			var id string
			err := r.Scan(&id)
			ids = append(ids, id)
			return err
		}); err != nil {
			return err
		}
		for _, id := range ids {
			_, e, err := k.cancelRebuildInTx(ctx, tx, id, tenant.JobReasonEndedByRestore, now)
			if err != nil {
				return err
			}
			if e != nil {
				if err := tx.WriteEvents(ctx, *e); err != nil {
					return err
				}
			}
			out = append(out, id)
		}
		return nil
	})
	return out, err
}
