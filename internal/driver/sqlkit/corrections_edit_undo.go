package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// BeginEditUndo implements tenant.Edits.
func (k Sending) BeginEditUndo(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, error) {
	if err := tenant.ValidateUndoReason(reason); err != nil {
		return tenant.Correction{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Correction{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, id, "the edit's undo cannot be begun", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind == corr.KindEdit && (c.State == corr.StateUndone || c.State == corr.StateCommitted && c.StateReason == tenant.StateReasonUndoing) {
			return tx.Audited(ctx, audit, guard)
		}
		if c.State != corr.StateCommitted {
			return tenant.Precondition("the correction is " + string(c.State) + ", not a committed edit")
		}
		if err := k.checkEditMayUndo(ctx, tx, *c, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE corrections SET state_reason = ?, undo_reason = ? WHERE id = ?`, tenant.StateReasonUndoing, reason,
			c.ID); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
}

// UndoEdit implements tenant.Edits.
func (k Sending) UndoEdit(ctx context.Context, u tenant.EditUndo) (tenant.Correction, error) {
	if err := u.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	return k.step(ctx, u.ID, "the edit cannot be undone", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind == corr.KindEdit && c.State == corr.StateUndone {
			return nil
		}
		if c.Kind != corr.KindEdit || c.State != corr.StateCommitted || c.StateReason != tenant.StateReasonUndoing {
			return fault.Wrap(fault.Precondition, tenant.ErrNotBeingUndone, "the correction is not an edit being undone")
		}
		if err := k.checkEditMayUndo(ctx, tx, *c, u.Now); err != nil {
			return err
		}
		rows, err := editRowsIn(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if err := countOwned(ctx, tx, c.ID, func() error {
			_, err := tx.Exec(ctx, `DELETE FROM correction_keys WHERE correction_id = ?`, c.ID)
			return err
		}); err != nil {
			return err
		}
		for _, row := range rows {
			if err := k.undoEditRow(ctx, tx, c.ID, row); err != nil {
				return err
			}
		}
		if err := restoreEditValues(ctx, tx, c.ID); err != nil {
			return err
		}
		if err := RestoreEditSearch(ctx, tx, c.ID); err != nil {
			return err
		}
		if err := k.afterEditRows(ctx, tx, *c, rows, u.Manifests); err != nil {
			return err
		}
		for series, m := range u.Manifests {
			if err := SetSidecarInTx(ctx, tx, series, m, u.Sidecars[series]); err != nil {
				return err
			}
		}
		// The replicas hold the new revisions the publish queued them for:
		// each scan is queued again as the catalog now says, a copy under way
		// is ended, and the removal of the new revisions waits for a put it
		// had under way.
		due, err := k.endCopies(ctx, tx, c.ID, u.Now)
		if err != nil {
			return err
		}
		if err := k.keepCopiesInFlight(ctx, tx, c.ID, due); err != nil {
			return err
		}
		if err := k.QueueCorrectionCopies(ctx, tx, c.ID, u.Now, nil); err != nil {
			return err
		}
		if err := k.oweAllRecords(ctx, tx, c.ID, u.Now); err != nil {
			return err
		}
		if err := k.setState(ctx, tx, c, corr.StateUndone, u.Now, ""); err != nil {
			return err
		}
		return changedEvent(ctx, tx, *c, u.Now)
	})
}

// CheckEditUndo implements tenant.Edits.
func (k Sending) CheckEditUndo(ctx context.Context, id string, now time.Time) error {
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	return k.stepRow(ctx, id, "the edit's undo cannot be checked", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind != corr.KindEdit || c.State != corr.StateCommitted || c.StateReason != tenant.StateReasonUndoing {
			return fault.Wrap(fault.Precondition, tenant.ErrNotBeingUndone, "the correction is not an edit being undone")
		}
		return k.checkEditMayUndo(ctx, tx, *c, now)
	})
}

// AbandonEditUndo implements tenant.Edits.
func (k Sending) AbandonEditUndo(ctx context.Context, u tenant.EditUndo) (tenant.Correction, error) {
	if err := u.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	if u.Audit == nil {
		return tenant.Correction{}, fault.New(fault.Invalid, "an undo taken back carries its audit entry")
	}
	if err := u.Audit.Validate(); err != nil {
		return tenant.Correction{}, err
	}
	for series := range u.Manifests {
		if err := tenant.ValidateSeriesUID(series); err != nil {
			return tenant.Correction{}, err
		}
	}
	return k.step(ctx, u.ID, "the edit's undo cannot be abandoned", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind != corr.KindEdit || c.State != corr.StateCommitted || c.StateReason != tenant.StateReasonUndoing {
			return nil
		}
		for series, m := range u.Manifests {
			ours, err := exists(ctx, tx, `SELECT 1 FROM correction_scans WHERE correction_id = ? AND series_uid = ?`, c.ID, series)
			if err != nil {
				return err
			}
			if !ours || m.IsZero() {
				return tenant.Precondition("a scan whose digests are given is not one of the edit's")
			}
			if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE series_uid = ?`, m[:], series); err != nil {
				return err
			}
			if err := SetSidecarInTx(ctx, tx, series, m, u.Sidecars[series]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE corrections SET state_reason = '', undo_reason = '' WHERE id = ?`, c.ID); err != nil {
			return err
		}
		if err := tx.Audited(ctx, *u.Audit, nil); err != nil {
			return err
		}
		c.StateReason, c.UndoReason = "", ""
		return changedEvent(ctx, tx, *c, u.Now)
	})
}

// undoEditRow puts one instance of an edit back at its old revision: its
// row, its history (the old revision current again, the new one superseded
// and the edit's own to remove), and its bytes' counts.
func (k Sending) undoEditRow(ctx context.Context, tx SendingTx, id string, row editRow) error {
	newKey, err := layout.RevisionKey(row.Path, row.UID, row.NewRevision)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "a revision key cannot be made")
	}
	if row.key != newKey || row.archived != row.NewSHA256 {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "an instance changed since the edit was published")
	}
	raw := row.OldKey
	if plain, err := layout.InstanceKey(row.Path, row.UID); err == nil && plain == raw {
		raw = ""
	}
	if err := k.setInstanceBytes(ctx, tx, row, row.Storage, raw, row.OldSHA256, row.OldSize); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM instance_revisions WHERE sop_instance_uid = ? AND revision = ? AND correction_id = ?`, row.UID,
		row.OldRevision, id); err != nil {
		return err
	}
	return k.ownSuperseded(ctx, tx, id, row.Storage, row.Path, row.UID, newKey, newKey, row.NewSize, row.NewSHA256, row.NewRevision)
}

// restoreEditValues gives each scan of an edit the catalog values it had
// before the edit.
func restoreEditValues(ctx context.Context, tx SendingTx, id string) error {
	type scan struct {
		series, modality, description string
		number                        *int64
	}
	var scans []scan
	err := tx.Query(ctx, `SELECT series_uid, modality, series_description, series_number FROM correction_edit_scans WHERE correction_id = ?`,
		[]any{id}, func(r Row) error {
			var s scan
			err := r.Scan(&s.series, &s.modality, &s.description, &s.number)
			scans = append(scans, s)
			return err
		})
	if err != nil {
		return err
	}
	for _, s := range scans {
		var n any
		if s.number != nil {
			n = *s.number
		}
		if _, err := tx.Exec(ctx, `UPDATE scans SET modality = ?, series_description = ?, description_folded = ?, series_number = ?
			WHERE series_uid = ?`, s.modality, s.description, tenant.FoldDescription(s.description), n, s.series); err != nil {
			return err
		}
	}
	return nil
}
