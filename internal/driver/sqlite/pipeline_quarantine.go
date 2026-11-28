package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const quarantineColumns = `id, reason, details, state, receiver, reception_id, calling_ae, called_ae, remote_addr, size,
	sha256, kept_sha256, sop_class_uid, sop_instance_uid, study_uid, series_uid, transfer_syntax_uid, incoming,
	content_kept, at, decided_at, release_mode, release_reception, release_receiver, reject_reason, released_from, release_reason,
	deidentified`

// quarantineReadColumns are the columns a record is read with: those it
// is written with, the storage and profile version its release named,
// which only a release writes, and the import job and path of a file an
// import handed in, which the store writes (sqlkit.ImportHeld).
const quarantineReadColumns = quarantineColumns + `, release_storage, release_profile, import_job, import_path`

// scanQuarantined reads one row of quarantineReadColumns.
func scanQuarantined(row interface{ Scan(...any) error }) (tenant.QuarantinedFile, error) {
	var q tenant.QuarantinedFile
	var reason, details, state, mode, releaseProfile string
	var sum, kept []byte
	var at int64
	var decided sql.NullInt64
	if err := row.Scan(&q.ID, &reason, &details, &state, &q.Receiver, &q.ReceptionID, &q.Caller.CallingAETitle,
		&q.Caller.CalledAETitle, &q.Caller.Address, &q.Size, &sum, &kept, &q.SOPClassUID, &q.SOPInstanceUID, &q.StudyUID,
		&q.SeriesUID, &q.TransferSyntaxUID, &q.Incoming, &q.ContentKept, &at, &decided, &mode, &q.ReleaseReception,
		&q.ReleaseReceiver, &q.RejectReason, &q.ReleasedFrom, &q.ReleaseReason, &q.Deidentified, &q.ReleaseStorage, &releaseProfile,
		&q.ImportJob, &q.ImportPath); err != nil {
		return q, err
	}
	q.Reason, q.State, q.ReleaseMode = tenant.QuarantineReason(reason), tenant.QuarantineState(state), tenant.ReleaseMode(mode)
	q.SHA256, q.KeptSHA256 = sqlkit.ToSHA256(sum), sqlkit.ToSHA256(kept)
	q.Quarantined, q.Decided = fromMicros(at), fromNullMicros(decided)
	var err error
	if q.ReleaseProfile, err = sqlkit.ReadProfileRef(releaseProfile); err != nil {
		return q, err
	}
	q.Details, err = sqlkit.DecodeDetails(details)
	return q, err
}

// quarantinedInTx reads a record inside a change's transaction, or NotFound.
func quarantinedInTx(ctx context.Context, tx *sql.Tx, id string) (tenant.QuarantinedFile, error) {
	q, err := scanQuarantined(tx.QueryRowContext(ctx, `SELECT `+quarantineReadColumns+` FROM quarantine WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return q, fault.New(fault.NotFound, "the tenant has no quarantined file with this id")
	}
	return q, err
}

// addTotals changes the quarantine's running totals in the transaction of
// the change that moved them.
func addTotals(ctx context.Context, tx *sql.Tx, files, bytes int64) error {
	if files == 0 && bytes == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE quarantine_totals SET files = files + ?, bytes = bytes + ? WHERE one = 1`, files, bytes)
	return err
}

// HoldFiles implements tenant.Pipeline.
func (t *Tenant) HoldFiles(ctx context.Context, files []tenant.QuarantinedFile, fence *tenant.WorkFence) (int, error) {
	if err := tenant.ValidateQuarantineBatch(files); err != nil {
		return 0, err
	}
	if fence != nil {
		if err := fence.Validate(); err != nil {
			return 0, err
		}
	}
	if len(files) == 0 && fence == nil {
		return 0, t.open()
	}
	var inserted int
	err := t.write(ctx, "the quarantined files cannot be recorded", func(tx *sql.Tx, _ *tenantStatements) error {
		if fence != nil {
			if err := checkFence(ctx, tx, *fence); err != nil {
				return err
			}
		}
		if err := sqlkit.CheckImportFences(ctx, sendingTx{tx: tx}, sqlkit.HeldArrivals(files)); err != nil {
			return err
		}
		n := 0
		var heldFiles, heldBytes int64
		// The totals before the batch, so each record's event carries the
		// totals with it and those before it in the batch.
		startFiles, startBytes, err := quarantineTotalsInTx(ctx, tx)
		if err != nil {
			return err
		}
		type delta struct{ files, bytes, receivedFiles, receivedBytes int64 }
		var order []string
		var importEvents []tenant.EventRecord
		perReception := map[string]delta{}
		for _, q := range files {
			if q.ReceptionID != "" {
				if _, seen := perReception[q.ReceptionID]; !seen {
					// The reception's row is locked first, as a received
					// batch locks it, so a file counted received here and
					// a batch of the same file are ordered.
					res, err := tx.ExecContext(ctx, `UPDATE receptions SET last_file = last_file WHERE id = ?`, q.ReceptionID)
					if err != nil {
						return err
					}
					locked, err := res.RowsAffected()
					if err != nil {
						return err
					}
					if locked == 0 {
						return tenant.Precondition("a quarantined file names a reception the tenant does not have")
					}
					order = append(order, q.ReceptionID)
					perReception[q.ReceptionID] = delta{}
				}
			}
			q.State = tenant.QuarantineHeld
			details, err := sqlkit.EncodeDetails(q.Details)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO quarantine (`+quarantineColumns+`)
				VALUES (?, ?, ?, 'held', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', '', '', '', ?, '', ?)
				ON CONFLICT (id) DO NOTHING`,
				q.ID, string(q.Reason), details, q.Receiver, q.ReceptionID, q.Caller.CallingAETitle, q.Caller.CalledAETitle,
				q.Caller.Address, q.Size, nullBlob(q.SHA256), nullBlob(q.KeptSHA256), q.SOPClassUID, q.SOPInstanceUID,
				q.StudyUID, q.SeriesUID, q.TransferSyntaxUID, q.Incoming, boolInt(q.ContentKept), micros(q.Quarantined),
				q.ReleasedFrom, boolInt(q.Deidentified))
			if err != nil {
				return err
			}
			added, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if added == 0 {
				continue
			}
			n++
			heldFiles++
			heldBytes += q.HeldBytes()
			e, err := tenant.FileQuarantinedEvent(q, startFiles+heldFiles, startBytes+heldBytes)
			if err != nil {
				return err
			}
			if _, err := writeEvent(ctx, tx, e, nil); err != nil {
				return err
			}
			// What the record decides of an import's file, in its
			// transaction (tenant.Importing).
			more, err := importInTx(tx, func(k sqlkit.Sending, x sqlkit.SendingTx) ([]tenant.EventRecord, error) {
				return k.ImportHeld(ctx, x, q, q.Quarantined)
			})
			if err != nil {
				return err
			}
			importEvents = append(importEvents, more...)
			if q.ReceptionID != "" {
				d := perReception[q.ReceptionID]
				d.files, d.bytes = d.files+1, d.bytes+q.Size
				// A file that arrived and has no received file in its
				// reception (too large, not DICOM, no UIDs) is counted
				// received here, once, as the record is made once.
				recorded := false
				if !q.SHA256.IsZero() && q.SOPInstanceUID != "" {
					if recorded, err = exists(ctx, tx, `SELECT 1 FROM received_files
						WHERE reception_id = ? AND sop_instance_uid = ? AND sha256 = ?`, q.ReceptionID, q.SOPInstanceUID, q.SHA256[:]); err != nil {
						return err
					}
				}
				if !recorded {
					d.receivedFiles, d.receivedBytes = d.receivedFiles+1, d.receivedBytes+q.Size
				}
				perReception[q.ReceptionID] = d
			}
		}
		if err := addTotals(ctx, tx, heldFiles, heldBytes); err != nil {
			return err
		}
		for _, id := range order {
			d := perReception[id]
			if d.files == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE receptions SET files_quarantined = files_quarantined + ?,
				bytes_quarantined = bytes_quarantined + ?, files_received = files_received + ?, bytes_received = bytes_received + ?
				WHERE id = ?`, d.files, d.bytes, d.receivedFiles, d.receivedBytes, id); err != nil {
				return err
			}
		}
		// The events of the import jobs the records ended, last.
		if err := writeImportEvents(ctx, tx, importEvents); err != nil {
			return err
		}
		inserted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// QuarantinedFile implements tenant.Pipeline.
func (t *Tenant) QuarantinedFile(ctx context.Context, id string) (tenant.QuarantinedFile, error) {
	if err := master.ValidateID("quarantine id", id); err != nil {
		return tenant.QuarantinedFile{}, err
	}
	return readOne(ctx, t, "the tenant has no quarantined file with this id", "the quarantined file cannot be read",
		`SELECT `+quarantineReadColumns+` FROM quarantine WHERE id = ?`, []any{id}, scanQuarantined)
}

// ListQuarantined implements tenant.Pipeline.
func (t *Tenant) ListQuarantined(ctx context.Context, filter tenant.QuarantineFilter, page master.Page) ([]tenant.QuarantinedFile, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.binding(string(filter.State), string(filter.Reason), filter.Receiver, filter.ReceptionID, filter.StudyUID,
		filter.SOPInstanceUID, sqlkit.MicrosText(filter.Since), sqlkit.MicrosText(filter.Until))
	pos, err := sqlkit.PageAfterTime(page, listQuarantine, binding)
	if err != nil {
		return nil, "", err
	}
	after := afterArgs(pos)
	query, args := quarantineQuery(filter, after, page.Limit+1)
	out, err := readRows(ctx, t, "the quarantined files cannot be listed", query, args, scanQuarantined)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.TimeCursor(listQuarantine, binding, last.Quarantined, last.ID), nil
}

// QuarantineTotals implements tenant.Pipeline.
func (t *Tenant) QuarantineTotals(ctx context.Context) (int64, int64, error) {
	type totals struct{ files, bytes int64 }
	v, err := readOne(ctx, t, "the quarantine has no totals", "the quarantine's totals cannot be read",
		`SELECT files, bytes FROM quarantine_totals WHERE one = 1`, nil, func(row interface{ Scan(...any) error }) (totals, error) {
			var v totals
			err := row.Scan(&v.files, &v.bytes)
			return v, err
		})
	return v.files, v.bytes, err
}

// IncomingNamed implements tenant.Pipeline.
func (t *Tenant) IncomingNamed(ctx context.Context, path string) (bool, error) {
	return readOne(ctx, t, "the incoming path has no answer", "the incoming path cannot be looked up",
		`SELECT EXISTS (SELECT 1 FROM quarantine WHERE incoming = ? AND content_kept = 1)
		     OR EXISTS (SELECT 1 FROM received_files WHERE incoming = ?)`, []any{path, path},
		func(row interface{ Scan(...any) error }) (bool, error) {
			var n int
			err := row.Scan(&n)
			return n == 1, err
		})
}

// quarantineQuery returns the statement and arguments of one page of
// ListQuarantined, put together as receptionsQuery's is, so each equality
// filter reads its index in migrations/tenant/0001_baseline.sql.
func quarantineQuery(filter tenant.QuarantineFilter, after []any, limit int) (string, []any) {
	var where []string
	var args []any
	for _, eq := range []struct{ column, value string }{
		{"state", string(filter.State)}, {"reason", string(filter.Reason)}, {"receiver", filter.Receiver},
		{"reception_id", filter.ReceptionID}, {"study_uid", filter.StudyUID}, {"sop_instance_uid", filter.SOPInstanceUID},
	} {
		if eq.value != "" {
			where, args = append(where, eq.column+" = ?"), append(args, eq.value)
		}
	}
	if !filter.Since.IsZero() {
		where, args = append(where, "at >= ?"), append(args, micros(filter.Since))
	}
	if !filter.Until.IsZero() {
		where, args = append(where, "at < ?"), append(args, micros(filter.Until))
	}
	if after != nil {
		where, args = append(where, "(at, id) < (?, ?)"), append(args, after...)
	}
	query := "SELECT " + quarantineReadColumns + " FROM quarantine"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
	}
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	return query, append(args, limit)
}
