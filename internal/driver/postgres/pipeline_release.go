package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReleaseQuarantined implements tenant.Pipeline. Its locks, in the
// package's order: the record FOR NO KEY UPDATE, read before anything is
// decided, so a decision made meanwhile is seen; the reception the file
// joins, FOR NO KEY UPDATE, before its received file is written
// (joinReception); the totals last.
func (t *Tenant) ReleaseQuarantined(ctx context.Context, id string, mode tenant.ReleaseMode, receiver, receptionID string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	return t.ReleaseFile(ctx, tenant.Release{ID: id, Mode: mode, Receiver: receiver, ReceptionID: receptionID, Now: now}, audit, guard)
}

// ReleaseFile implements tenant.QuarantineReleases, with ReleaseQuarantined's
// locks.
func (t *Tenant) ReleaseFile(ctx context.Context, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	if err := rel.Validate(); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if err := audit.ValidateWith(tenant.DetailReleaseReception); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	id, mode, receiver, receptionID, now := rel.ID, rel.Mode, rel.Receiver, rel.ReceptionID, rel.Now
	var out tenant.QuarantinedFile
	err := t.pipe(ctx, "the quarantined file cannot be released", func(tx pgx.Tx) error {
		q, err := quarantinedInTx(ctx, tx, id, "FOR NO KEY UPDATE")
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
			return t.audited(ctx, tx, a, guard)
		}
		if q.Reason == tenant.QuarantineDifferentFile && q.SOPInstanceUID != "" {
			// Released as it is, it would only be quarantined again while
			// the tenant holds an instance under its UID; replacing that
			// instance deletes archived data, which reception never does.
			// A held instance is never removed, so a read is enough.
			held, err := exists(ctx, tx, `SELECT 1 FROM instances WHERE sop_instance_uid = $1`, q.SOPInstanceUID)
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
		has, err := receiverExists(ctx, tx, via)
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
		if err := tx.QueryRow(ctx, `SELECT config FROM receivers WHERE name = $1`, via).Scan(&config); err != nil {
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
			if err := lockStorageShared(ctx, tx, rel.Storage); err != nil {
				return err
			}
		}
		joined := ""
		var events []tenant.EventRecord
		if q.JoinsAt(deidentifies) {
			r, openEvents, err := joinReception(ctx, tx, via, q.StudyUID, receptionID, now)
			if err != nil {
				return err
			}
			events = openEvents
			joined = r.ID
			if err := setReleased(ctx, tx, r, rel.Storage, rel.Profile); err != nil {
				return err
			}
			f := tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: q.SOPInstanceUID, SHA256: q.SHA256, SeriesUID: q.SeriesUID,
				StudyUID: q.StudyUID, SOPClassUID: q.SOPClassUID, TransferSyntaxUID: q.TransferSyntaxUID, Size: q.Size,
				Incoming: q.Incoming, Caller: q.Caller, Released: true, Deidentified: q.Deidentified}
			added, err := insertReceived(ctx, tx, []tenant.ReceivedFile{f}, now)
			if err != nil {
				return err
			}
			switch {
			case r.ID == q.ReceptionID:
				// Back into the reception it was quarantined from, which
				// counted it received already (by its received file or by its
				// hold): released there, and no longer quarantined.
				if _, err := tx.Exec(ctx, `UPDATE received_files SET released = true
					WHERE reception_id = $1 AND sop_instance_uid = $2 AND sha256 = $3`, r.ID, q.SOPInstanceUID, sumBytes(q.SHA256)); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE receptions SET files_released = files_released + 1,
					files_quarantined = files_quarantined - 1, bytes_quarantined = bytes_quarantined - $1, last_file = greatest(last_file, $2)
					WHERE id = $3`, q.Size, micros(now), r.ID); err != nil {
					return err
				}
			case len(added) == 1:
				if _, err := tx.Exec(ctx, `UPDATE receptions SET files_received = files_received + 1,
					bytes_received = bytes_received + $1, files_released = files_released + 1, last_file = greatest(last_file, $2)
					WHERE id = $3`, q.Size, micros(now), r.ID); err != nil {
					return err
				}
			}
		}
		// The record's state is in the statement that changes it, as well
		// as locked above, so two decisions at once make one.
		n, err := rowsChanged(tx.Exec(ctx, `UPDATE quarantine SET state = 'released', content_kept = false, decided_at = $1,
			release_mode = $2, release_reception = $3, release_receiver = $4, release_reason = $5, release_storage = $7,
			release_profile = $8 WHERE id = $6 AND state = 'held'`,
			micros(now), string(mode), joined, via, rel.Reason, id, rel.Storage, sqlkit.ProfileRefText(rel.Profile)))
		if err != nil {
			return err
		}
		if n != 1 {
			return fault.New(fault.Unavailable, "the quarantined file was decided at the same moment; try again")
		}
		if err := addTotals(ctx, tx, -1, -q.HeldBytes()); err != nil {
			return err
		}
		if out, err = quarantinedInTx(ctx, tx, id, ""); err != nil {
			return err
		}
		decided, err := decisionEventInTx(ctx, tx, out)
		if err != nil {
			return err
		}
		if err := writeEvents(ctx, tx, append(events, decided)...); err != nil {
			return err
		}
		a, err := sqlkit.ReleaseEntry(audit, joined)
		if err != nil {
			return err
		}
		return t.audited(ctx, tx, a, guard)
	})
	if err != nil {
		return tenant.QuarantinedFile{}, err
	}
	return out, nil
}

// joinReception finds or opens the reception a released file joins
// (openReceptionInTx) and locks its row FOR NO KEY UPDATE in the statement
// that checks it is still receiving, as a received batch does. A reception
// found receiving whose quiet step committed while this change waited for
// its row is receiving no more, and the file belongs to the next arrival:
// the next round, a statement of its own, does not find it receiving and
// opens that arrival.
//
// A reception it opened comes with its first event, made as it was opened
// and for the caller to write last in its transaction.
func joinReception(ctx context.Context, tx pgx.Tx, via, studyUID, receptionID string, now time.Time) (tenant.Reception, []tenant.EventRecord, error) {
	for range openTries {
		r, opened, err := openReceptionInTx(ctx, tx, via, studyUID, receptionID, now)
		if err != nil {
			return r, nil, err
		}
		var events []tenant.EventRecord
		if opened {
			e, err := tenant.ReceptionStepEvent("", r)
			if err != nil {
				return r, nil, err
			}
			events = append(events, e)
		}
		receiving, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = $1 AND state = 'receiving' FOR NO KEY UPDATE`, r.ID)
		if err != nil || receiving {
			return r, events, err
		}
	}
	return tenant.Reception{}, nil, fault.New(fault.Unavailable, "receptions of the study kept closing at the same moment; try again")
}

// RejectQuarantined implements tenant.Pipeline. The record's row is read FOR
// NO KEY UPDATE before anything is decided, so a decision made meanwhile is
// seen.
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
	err := t.pipe(ctx, "the quarantined file cannot be rejected", func(tx pgx.Tx) error {
		q, err := quarantinedInTx(ctx, tx, id, "FOR NO KEY UPDATE")
		if err != nil {
			return err
		}
		switch q.State {
		case tenant.QuarantineReleased:
			return tenant.Precondition("the file was released")
		case tenant.QuarantineHeld:
			n, err := rowsChanged(tx.Exec(ctx, `UPDATE quarantine SET state = 'rejected', content_kept = false, decided_at = $1,
				reject_reason = $2 WHERE id = $3 AND state = 'held'`, micros(now), reason, id))
			if err != nil {
				return err
			}
			if n != 1 {
				return fault.New(fault.Unavailable, "the quarantined file was decided at the same moment; try again")
			}
			if err := addTotals(ctx, tx, -1, -q.HeldBytes()); err != nil {
				return err
			}
			if q, err = quarantinedInTx(ctx, tx, id, ""); err != nil {
				return err
			}
			decided, err := decisionEventInTx(ctx, tx, q)
			if err != nil {
				return err
			}
			if err := writeEvents(ctx, tx, decided); err != nil {
				return err
			}
		}
		a, err := sqlkit.AddDetails(audit, tenant.DetailReason, string(q.Reason), tenant.DetailSize, strconv.FormatInt(q.Size, 10),
			tenant.DetailSHA256, q.SHA256.String())
		if err != nil {
			return err
		}
		out = q
		return t.audited(ctx, tx, a, guard)
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
func setReleased(ctx context.Context, tx pgx.Tx, r tenant.Reception, storage string, profile tenant.ProfileRef) error {
	route, changed, err := sqlkit.ReleasedRoute(r.Route, storage, profile)
	if err != nil || !changed {
		return err
	}
	text, err := sqlkit.ReceptionRouteText(route)
	if err != nil {
		return err
	}
	receptionID := r.ID
	_, err = tx.Exec(ctx, `UPDATE receptions SET route = $1 WHERE id = $2`, text, receptionID)
	return err
}

// keepReleased keeps with a reception still receiving the storage and the
// profile version the release of a file that lands again named
// (tenant.ReceivedFile.ReleasedStorage and ReleasedProfile), unless it
// keeps a route already.
func keepReleased(ctx context.Context, tx pgx.Tx, receptionID, storage string, profile tenant.ProfileRef) error {
	text, err := sqlkit.ReceptionRouteText(tenant.ReceptionRoute{ReleasedStorage: storage, ReleasedProfile: profile})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE receptions SET route = $1 WHERE id = $2 AND route = ''`, text, receptionID)
	return err
}
