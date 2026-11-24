package sqlite

// The operations on a reception that failed: running its files again in a
// new reception, and discarding them, with the list of what a discard
// removes from the incoming area.

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RetryReception implements tenant.Pipeline.
func (t *Tenant) RetryReception(ctx context.Context, id, newID, receiver string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Reception, tenant.Reception, error) {
	for _, v := range []struct{ what, id string }{{"reception id", id}, {"new reception id", newID}} {
		if err := master.ValidateID(v.what, v.id); err != nil {
			return tenant.Reception{}, tenant.Reception{}, err
		}
	}
	if receiver != "" {
		if err := tenant.ValidateName("receiver", receiver); err != nil {
			return tenant.Reception{}, tenant.Reception{}, err
		}
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Reception{}, tenant.Reception{}, err
	}
	if err := audit.ValidateWith(tenant.DetailRetryReception); err != nil {
		return tenant.Reception{}, tenant.Reception{}, err
	}
	var failed, retry tenant.Reception
	err := t.write(ctx, "the reception cannot be retried", func(tx *sql.Tx, st *tenantStatements) error {
		// The failed reception is marked first, in the statement that
		// checks it may be retried, so two retries at once make one new
		// reception: on a database with several writers the second waits
		// for the first and then finds it retried.
		res, err := tx.ExecContext(ctx, `UPDATE receptions SET retry_reception = ?
			WHERE id = ? AND state = 'failed' AND retry_reception = '' AND discard_reason = ''`, newID, id)
		if err != nil {
			return err
		}
		marked, err := res.RowsAffected()
		if err != nil {
			return err
		}
		f, err := receptionInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if marked == 0 {
			switch {
			case f.RetryReception != "":
				r, err := receptionInTx(ctx, tx, f.RetryReception)
				if err != nil {
					return err
				}
				a, err := sqlkit.AddDetails(audit, tenant.DetailRetryReception, r.ID)
				if err != nil {
					return err
				}
				failed, retry = f, r
				return audited(ctx, tx, st, a, guard)
			case f.State != reception.Failed:
				return tenant.Precondition("the reception has not failed")
			default:
				return tenant.Precondition("the reception's files were discarded")
			}
		}
		via := receiver
		if via == "" {
			via = f.Receiver
		}
		has, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = ?`, via)
		if err != nil {
			return err
		}
		if !has && receiver == "" {
			return tenant.Precondition("the reception's receiver has been removed; name the receiver to run it by")
		}
		if !has {
			return tenant.Precondition("the tenant has no receiver named " + receiver)
		}
		if err := releaseClaims(ctx, tx, f.ID); err != nil {
			return err
		}
		inserted := false
		for try := 0; try < 8 && !inserted; try++ {
			if inserted, err = insertReception(ctx, tx, newID, via, f.StudyUID, reception.Complete, f.ID, now); err != nil {
				return err
			}
		}
		if !inserted {
			return fault.New(fault.Unavailable, "receptions of the study kept opening at the same moment; try again")
		}
		// What the failed reception's routing chose, or the storage a
		// release into it named, goes with its files.
		routeText, err := sqlkit.ReceptionRouteText(f.Route)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE receptions SET route = ? WHERE id = ?`, routeText, newID); err != nil {
			return err
		}
		// The failed reception's files, as they are in the incoming area,
		// become the new one's; a file it confirmed is held, and the new
		// reception's claim answers it SameFile.
		if _, err := tx.ExecContext(ctx, `INSERT INTO received_files (`+receivedColumns+`)
			SELECT ?, sop_instance_uid, sha256, series_uid, study_uid, sop_class_uid, transfer_syntax_uid, size, incoming,
				calling_ae, called_ae, remote_addr, association, released, ?, deidentified
			FROM received_files WHERE reception_id = ?`, newID, micros(now), f.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE receptions SET
			files_received = (SELECT count(*) FROM received_files WHERE reception_id = ?1),
			bytes_received = (SELECT coalesce(sum(size), 0) FROM received_files WHERE reception_id = ?1)
			WHERE id = ?1`, newID); err != nil {
			return err
		}
		// An import's files that failed with it go back to processing,
		// with their files, into the new reception (tenant.Importing).
		importEvents, err := importInTx(tx, func(k sqlkit.Sending, x sqlkit.SendingTx) ([]tenant.EventRecord, error) {
			return k.ImportRetried(ctx, x, f.ID, newID, now)
		})
		if err != nil {
			return err
		}
		if failed, err = receptionInTx(ctx, tx, f.ID); err != nil {
			return err
		}
		if retry, err = receptionInTx(ctx, tx, newID); err != nil {
			return err
		}
		// The new reception reaching complete, with the reception it runs
		// again, is its first event.
		e, err := tenant.ReceptionStepEvent("", retry)
		if err != nil {
			return err
		}
		if _, err := writeEvent(ctx, tx, e, nil); err != nil {
			return err
		}
		if err := writeImportEvents(ctx, tx, importEvents); err != nil {
			return err
		}
		a, err := sqlkit.AddDetails(audit, tenant.DetailRetryReception, newID)
		if err != nil {
			return err
		}
		return audited(ctx, tx, st, a, guard)
	})
	if err != nil {
		return tenant.Reception{}, tenant.Reception{}, err
	}
	return failed, retry, nil
}

// DiscardReception implements tenant.Pipeline.
func (t *Tenant) DiscardReception(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Reception, error) {
	if err := master.ValidateID("reception id", id); err != nil {
		return tenant.Reception{}, err
	}
	if err := tenant.ValidateDiscardReason(reason); err != nil {
		return tenant.Reception{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Reception{}, err
	}
	if err := audit.ValidateWith(tenant.DetailFiles, tenant.DetailBytes); err != nil {
		return tenant.Reception{}, err
	}
	var out tenant.Reception
	err := t.write(ctx, "the reception's files cannot be discarded", func(tx *sql.Tx, st *tenantStatements) error {
		// Marked in the statement that checks it may be discarded, so a
		// retry and a discard at once do not both happen.
		res, err := tx.ExecContext(ctx, `UPDATE receptions SET discard_reason = ?, discard_at = ?
			WHERE id = ? AND state = 'failed' AND retry_reception = '' AND discard_reason = ''`, reason, micros(now), id)
		if err != nil {
			return err
		}
		marked, err := res.RowsAffected()
		if err != nil {
			return err
		}
		r, err := receptionInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if marked == 0 {
			switch {
			case r.State != reception.Failed:
				return tenant.Precondition("the reception has not failed")
			case r.RetryReception != "":
				return tenant.Precondition("the reception was retried; its files belong to the reception that runs them")
			}
		}
		if err := releaseClaims(ctx, tx, id); err != nil {
			return err
		}
		// An import's files that failed with it are discarded with it
		// (tenant.Importing).
		importEvents, err := importInTx(tx, func(k sqlkit.Sending, x sqlkit.SendingTx) ([]tenant.EventRecord, error) {
			return k.ImportDiscarded(ctx, x, id, now)
		})
		if err != nil {
			return err
		}
		var files, bytes int64
		if err := tx.QueryRowContext(ctx, discardCountSQL, id).Scan(&files, &bytes); err != nil {
			return err
		}
		if err := writeImportEvents(ctx, tx, importEvents); err != nil {
			return err
		}
		a, err := sqlkit.AddDetails(audit, tenant.DetailFiles, strconv.FormatInt(files, 10), tenant.DetailBytes, strconv.FormatInt(bytes, 10))
		if err != nil {
			return err
		}
		out = r
		return audited(ctx, tx, st, a, guard)
	})
	if err != nil {
		return tenant.Reception{}, err
	}
	return out, nil
}

// notKept is the condition, on a received file r, that the bytes at its
// place in the incoming area are needed by nothing else: the files a
// discard removes. The one condition serves the count in the audit entry
// and the list the caller removes, so the two agree, and a restart that
// removes the list deletes nothing another part of the pipeline still
// reads. A place is needed when:
//
//   - a quarantine record keeps its bytes there (quarantine_kept);
//   - a record released from it gave the place to another reception that
//     has not ended, or that failed and whose files were not discarded
//     (quarantine_released_places, then the receptions' key).
//
// Every reception other than r's that has a received file at the place got
// it through a released record (a release writes the record's place as the
// new reception's) or through a retry of a reception that did, and a
// failed reception that was retried counts as needing it, so the condition
// also covers a chain of releases and retries: what it may leave behind is
// a place no reception needs any more, never a place one still does.
const notKept = `NOT EXISTS (SELECT 1 FROM quarantine AS q WHERE q.incoming = r.incoming AND q.content_kept = 1)
	AND NOT EXISTS (SELECT 1 FROM quarantine AS q JOIN receptions AS p ON p.id = q.release_reception
		WHERE q.incoming = r.incoming AND q.state = 'released' AND p.id <> r.reception_id
		AND (p.ended = 0 OR (p.state = 'failed' AND p.discard_reason = '')))`

// discardCountSQL counts the files and bytes a discard removes.
const discardCountSQL = `SELECT count(*), coalesce(sum(r.size), 0) FROM received_files AS r WHERE r.reception_id = ? AND ` + notKept

// discardedSQL pages the files a discard removes: the received files'
// primary key.
const discardedSQL = `SELECT ` + receivedColumns + ` FROM received_files AS r
	WHERE r.reception_id = ? AND (r.sop_instance_uid, r.sha256) > (?, ?) AND ` + notKept + `
	ORDER BY r.sop_instance_uid, r.sha256 LIMIT ?`

// DiscardedFiles implements tenant.Pipeline.
func (t *Tenant) DiscardedFiles(ctx context.Context, receptionID string, page master.Page) ([]tenant.ReceivedFile, string, error) {
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return nil, "", err
	}
	r, _, err := t.Reception(ctx, receptionID)
	if err != nil {
		return nil, "", err
	}
	if r.DiscardReason == "" {
		return nil, "", tenant.Precondition("the reception's files were not discarded")
	}
	return t.pageReceived(ctx, listDiscarded, discardedSQL, receptionID, page)
}
