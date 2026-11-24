package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReleaseQuarantined implements tenant.Pipeline: a release with no reason.
func (t *Tenant) ReleaseQuarantined(ctx context.Context, id string, mode tenant.ReleaseMode, receiver, receptionID string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	return t.ReleaseFile(ctx, tenant.Release{ID: id, Mode: mode, Receiver: receiver, ReceptionID: receptionID, Now: now}, audit, guard)
}

// ReleaseFile implements tenant.QuarantineReleases.
func (t *Tenant) ReleaseFile(ctx context.Context, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	if err := rel.Validate(); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if err := audit.ValidateWith(tenant.DetailReleaseReception); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	id, mode, receiver, receptionID, now := rel.ID, rel.Mode, rel.Receiver, rel.ReceptionID, rel.Now
	var out tenant.QuarantinedFile
	err := t.write(ctx, "the quarantined file cannot be released", func(tx *sql.Tx, st *tenantStatements) error {
		// The record's row is locked before it is read, so on a database
		// with several writers a decision made meanwhile is seen.
		if _, err := tx.ExecContext(ctx, `UPDATE quarantine SET decided_at = decided_at WHERE id = ?`, id); err != nil {
			return err
		}
		q, err := quarantinedInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch q.State {
		case tenant.QuarantineRejected:
			return tenant.Precondition("the file was rejected")
		case tenant.QuarantineReleased:
			a, err := sqlkit.ReleaseEntry(audit, q.ReleaseReception)
			if err != nil {
				return err
			}
			out = q
			return audited(ctx, tx, st, a, guard)
		}
		if q.Reason == tenant.QuarantineDifferentFile && q.SOPInstanceUID != "" {
			// Released as it is, it would only be quarantined again while
			// the tenant holds an instance under its UID; replacing that
			// instance deletes archived data, which reception never does.
			held, err := exists(ctx, tx, `SELECT 1 FROM instances WHERE sop_instance_uid = ?`, q.SOPInstanceUID)
			if err != nil {
				return err
			}
			if held {
				return tenant.Precondition("the tenant holds an instance under this file's SOP Instance UID; in this version the file can only be rejected")
			}
		}
		if !q.ContentKept {
			return tenant.Precondition("the file's content was never kept")
		}
		via := receiver
		if via == "" {
			via = q.Receiver
		}
		has, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = ?`, via)
		if err != nil {
			return err
		}
		if !has && receiver == "" {
			return tenant.Precondition("the receiver the file came to has been removed; name the receiver to run it by")
		}
		if !has {
			return tenant.Precondition("the tenant has no receiver named " + receiver)
		}
		var config string
		if err := tx.QueryRowContext(ctx, `SELECT config FROM receivers WHERE name = ?`, via).Scan(&config); err != nil {
			return err
		}
		deidentifies, err := sqlkit.ReceiverDeidentifies(config)
		if err != nil {
			return err
		}
		if err := tenant.ReleaseAllowed(q, rel, deidentifies); err != nil {
			return err
		}
		if rel.Storage != "" {
			// The storage named is held to the commit, so its removal
			// cannot pass the release unseen.
			if err := lockStorage(ctx, tx, rel.Storage); err != nil {
				return err
			}
		}
		joined := ""
		if q.JoinsAt(deidentifies) {
			r, _, err := openReceptionInTx(ctx, tx, via, q.StudyUID, receptionID, now)
			if err != nil {
				return err
			}
			joined = r.ID
			if err := setReleased(ctx, tx, r, rel.Storage, rel.Profile); err != nil {
				return err
			}
			args := []any{r.ID, q.SOPInstanceUID, q.SHA256[:], q.SeriesUID,
				q.StudyUID, q.SOPClassUID, q.TransferSyntaxUID, q.Size, q.Incoming, q.Caller.CallingAETitle,
				q.Caller.CalledAETitle, q.Caller.Address, "", 1, micros(now)}
			var res sql.Result
			if q.Deidentified {
				res, err = tx.ExecContext(ctx, insertDeidentifiedReceivedSQL, args...)
			} else {
				res, err = tx.StmtContext(ctx, st.insertReceived).ExecContext(ctx, args...)
			}
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			switch {
			case r.ID == q.ReceptionID:
				// Back into the reception it was quarantined from, which
				// counted it received already (by its received file or by
				// its hold): released there, and no longer quarantined.
				if _, err := tx.ExecContext(ctx, `UPDATE received_files SET released = 1
					WHERE reception_id = ? AND sop_instance_uid = ? AND sha256 = ?`, r.ID, q.SOPInstanceUID, q.SHA256[:]); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE receptions SET files_released = files_released + 1,
					files_quarantined = files_quarantined - 1, bytes_quarantined = bytes_quarantined - ?, last_file = max(last_file, ?)
					WHERE id = ?`, q.Size, micros(now), r.ID); err != nil {
					return err
				}
			case n == 1:
				if _, err := tx.ExecContext(ctx, `UPDATE receptions SET files_received = files_received + 1,
					bytes_received = bytes_received + ?, files_released = files_released + 1, last_file = max(last_file, ?)
					WHERE id = ?`, q.Size, micros(now), r.ID); err != nil {
					return err
				}
			}
		}
		// The record's state is in the statement that changes it, so two
		// decisions at once make one.
		res, err := tx.ExecContext(ctx, `UPDATE quarantine SET state = 'released', content_kept = 0, decided_at = ?,
			release_mode = ?, release_reception = ?, release_receiver = ?, release_reason = ?, release_storage = ?,
			release_profile = ? WHERE id = ? AND state = 'held'`,
			micros(now), string(mode), joined, via, rel.Reason, rel.Storage, sqlkit.ProfileRefText(rel.Profile), id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return fault.New(fault.Unavailable, "the quarantined file was decided at the same moment; try again")
		}
		if err := addTotals(ctx, tx, -1, -q.HeldBytes()); err != nil {
			return err
		}
		if out, err = quarantinedInTx(ctx, tx, id); err != nil {
			return err
		}
		if err := writeDecisionEvent(ctx, tx, out); err != nil {
			return err
		}
		a, err := sqlkit.ReleaseEntry(audit, joined)
		if err != nil {
			return err
		}
		return audited(ctx, tx, st, a, guard)
	})
	if err != nil {
		return tenant.QuarantinedFile{}, err
	}
	return out, nil
}

// RejectQuarantined implements tenant.Pipeline.
func (t *Tenant) RejectQuarantined(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	if err := master.ValidateID("quarantine id", id); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if err := tenant.ValidateDiscardReason(reason); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if err := audit.ValidateWith(tenant.DetailReason, tenant.DetailSize, tenant.DetailSHA256); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	var out tenant.QuarantinedFile
	err := t.write(ctx, "the quarantined file cannot be rejected", func(tx *sql.Tx, st *tenantStatements) error {
		// The record's row is locked before it is read, so on a database
		// with several writers a decision made meanwhile is seen.
		if _, err := tx.ExecContext(ctx, `UPDATE quarantine SET decided_at = decided_at WHERE id = ?`, id); err != nil {
			return err
		}
		q, err := quarantinedInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch q.State {
		case tenant.QuarantineReleased:
			return tenant.Precondition("the file was released")
		case tenant.QuarantineHeld:
			res, err := tx.ExecContext(ctx, `UPDATE quarantine SET state = 'rejected', content_kept = 0, decided_at = ?,
				reject_reason = ? WHERE id = ? AND state = 'held'`, micros(now), reason, id)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return fault.New(fault.Unavailable, "the quarantined file was decided at the same moment; try again")
			}
			if err := addTotals(ctx, tx, -1, -q.HeldBytes()); err != nil {
				return err
			}
			if q, err = quarantinedInTx(ctx, tx, id); err != nil {
				return err
			}
			if err := writeDecisionEvent(ctx, tx, q); err != nil {
				return err
			}
		}
		a, err := sqlkit.AddDetails(audit, tenant.DetailReason, string(q.Reason), tenant.DetailSize, strconv.FormatInt(q.Size, 10),
			tenant.DetailSHA256, q.SHA256.String())
		if err != nil {
			return err
		}
		out = q
		return audited(ctx, tx, st, a, guard)
	})
	if err != nil {
		return tenant.QuarantinedFile{}, err
	}
	return out, nil
}

// setReleased keeps, with a reception still receiving, the storage and the
// profile version a release into it named (tenant.Release.Storage and
// Profile); its identify step archives a new session on that storage, and
// its archive step de-identifies with that version. One that keeps another
// a release named is a precondition failure (sqlkit.ReleasedRoute).
func setReleased(ctx context.Context, tx *sql.Tx, r tenant.Reception, storage string, profile tenant.ProfileRef) error {
	route, changed, err := sqlkit.ReleasedRoute(r.Route, storage, profile)
	if err != nil || !changed {
		return err
	}
	text, err := sqlkit.ReceptionRouteText(route)
	if err != nil {
		return err
	}
	receptionID := r.ID
	_, err = tx.ExecContext(ctx, `UPDATE receptions SET route = ? WHERE id = ?`, text, receptionID)
	return err
}

// keepReleased keeps with a reception still receiving the storage and the
// profile version the release of a file that lands again named
// (tenant.ReceivedFile.ReleasedStorage and ReleasedProfile), unless it
// keeps a route already.
func keepReleased(ctx context.Context, tx *sql.Tx, receptionID, storage string, profile tenant.ProfileRef) error {
	text, err := sqlkit.ReceptionRouteText(tenant.ReceptionRoute{ReleasedStorage: storage, ReleasedProfile: profile})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE receptions SET route = ? WHERE id = ? AND route = ''`, text, receptionID)
	return err
}
