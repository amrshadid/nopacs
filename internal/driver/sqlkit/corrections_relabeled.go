package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Where the relabels and moves of a tenant moved labels asked for after
// them.

// relabelRow is a committed relabel or move of the table relabels: a move
// of one session names the subject it went under as newSubject; a merge is
// a subject's row, its new label the subject merged into.
type relabelRow struct {
	id, level, to, newSubject string
	old                       label.Path
}

// moves reports whether the row moved the labels p, and returns p as it
// left them (tenant.RelabelMoves, then a move's new subject).
func (r relabelRow) moves(p label.Path) (label.Path, bool) {
	at, ok := tenant.RelabelMoves(r.level, r.old, r.to, p)
	if ok && r.newSubject != "" {
		at.Subject = r.newSubject
	}
	return at, ok
}

// RelabeledTo implements tenant.Corrections. It reads in one transaction,
// so the relabels it follows and the catalog row it ends on are one state
// of the tenant. Each step reads the relabels of the subject the labels
// name at that step; a tenant holds few relabels, made by a person each.
func (k Sending) RelabeledTo(ctx context.Context, p label.Path) (tenant.Relabeled, bool, error) {
	if err := tenant.ValidateRelabelLookup(p); err != nil {
		return tenant.Relabeled{}, false, err
	}
	var out tenant.Relabeled
	var found bool
	err := k.Host.Read(ctx, "the relabels cannot be read", func(tx SendingTx) error {
		out, found = tenant.Relabeled{}, false
		first, err := relabelsMoving(ctx, tx, p, "")
		if err != nil {
			return err
		}
		// Each relabel that moved p, newest first, is a start: the labels
		// may have been given up more than once (new data arrived under
		// them and was relabeled again), and a later relabel of a level
		// above may have moved what an earlier one left. Each start is
		// followed forward; the first chain whose end the catalog holds
		// answers. Every relabel read counts against one bound.
		budget := tenant.MaxRelabelSteps
		for i := len(first) - 1; i >= 0 && budget > 0; i-- {
			at, _ := first[i].moves(p)
			last := first[i].id
			budget--
			for budget > 0 {
				next, err := relabelsMoving(ctx, tx, at, last)
				if err != nil {
					return err
				}
				if len(next) == 0 {
					break
				}
				at, _ = next[0].moves(at)
				last = next[0].id
				budget--
			}
			held, err := catalogHolds(ctx, tx, at)
			if err != nil {
				return err
			}
			if held {
				out, found = tenant.Relabeled{Correction: last, To: at}, true
				return nil
			}
		}
		return nil
	})
	return out, found, err
}

// relabelsMoving returns the committed relabels after the id after (all,
// when it is empty) that moved the labels p, oldest first: the first of
// them is where a chain goes next, in the order its relabels were
// committed, so it ends.
func relabelsMoving(ctx context.Context, tx SendingTx, p label.Path, after string) ([]relabelRow, error) {
	var out []relabelRow
	err := tx.Query(ctx, `SELECT r.correction_id, r.level, r.old_subject, r.old_session, r.old_scan, r.new_label, r.new_subject
		FROM relabels AS r JOIN corrections AS c ON c.id = r.correction_id
		WHERE r.old_subject = ? AND r.correction_id > ? AND c.state IN ('committed', 'purging', 'purged')
		ORDER BY r.correction_id`, []any{p.Subject, after}, func(row Row) error {
		var r relabelRow
		if err := row.Scan(&r.id, &r.level, &r.old.Subject, &r.old.Session, &r.old.Scan, &r.to, &r.newSubject); err != nil {
			return err
		}
		if _, moves := r.moves(p); moves {
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// catalogHolds reports whether the catalog holds now the subject, session
// or scan the labels p name, compared exactly: a delete published since
// the relabel has hidden it otherwise.
func catalogHolds(ctx context.Context, tx SendingTx, p label.Path) (bool, error) {
	var one int
	switch {
	case p.Scan != "":
		return tx.QueryRow(ctx, `SELECT 1 FROM scans AS c JOIN sessions AS s ON s.id = c.session_id JOIN subjects AS u ON u.id = s.subject_id
			WHERE u.folded = ? AND u.label = ? AND s.folded = ? AND s.label = ? AND c.folded = ? AND c.label = ?`,
			[]any{label.Fold(p.Subject), p.Subject, label.Fold(p.Session), p.Session, label.Fold(p.Scan), p.Scan}, &one)
	case p.Session != "":
		return tx.QueryRow(ctx, `SELECT 1 FROM sessions AS s JOIN subjects AS u ON u.id = s.subject_id
			WHERE u.folded = ? AND u.label = ? AND s.folded = ? AND s.label = ?`,
			[]any{label.Fold(p.Subject), p.Subject, label.Fold(p.Session), p.Session}, &one)
	}
	return tx.QueryRow(ctx, `SELECT 1 FROM subjects WHERE folded = ? AND label = ?`, []any{label.Fold(p.Subject), p.Subject}, &one)
}
