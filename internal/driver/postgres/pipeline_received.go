package postgres

import (
	"context"
	"encoding/hex"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// receivedInsertColumns are the columns a batch of received files recorded
// as received is written with: those of every schema since the
// pipeline's, so a database not yet migrated to the column deidentified
// takes it (a batch with a file recorded de-identified is written by
// insertDeidentifiedReceivedSQL).
const receivedInsertColumns = `reception_id, sop_instance_uid, sha256, series_uid, study_uid, sop_class_uid, transfer_syntax_uid,
	size, incoming, calling_ae, called_ae, remote_addr, association, released, received`

// receivedColumns are the columns a received file is read with.
const receivedColumns = receivedInsertColumns + `, deidentified`

// scanReceived reads one row of receivedColumns.
func scanReceived(row scanner) (tenant.ReceivedFile, error) {
	var f tenant.ReceivedFile
	var sum []byte
	var received time.Time
	err := row.Scan(&f.ReceptionID, &f.SOPInstanceUID, &sum, &f.SeriesUID, &f.StudyUID, &f.SOPClassUID, &f.TransferSyntaxUID,
		&f.Size, &f.Incoming, &f.Caller.CallingAETitle, &f.Caller.CalledAETitle, &f.Caller.Address, &f.Association,
		&f.Released, &received, &f.Deidentified)
	f.SHA256, f.Received = sqlkit.ToSHA256(sum), received.UTC()
	return f, err
}

// fileKey is what makes two received files of one reception the same file.
type fileKey struct {
	uid string
	sum tenant.SHA256
}

// insertReceivedSQL writes a set of received files in one statement, one
// array for each column (unnest), so a batch of any size is fifteen
// parameters and one round trip, far below Postgres's 65535; a file already
// recorded is skipped, and the files written are returned.
const insertReceivedSQL = `INSERT INTO received_files (` + receivedInsertColumns + `)
	SELECT * FROM unnest($1::text[], $2::text[], $3::bytea[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[],
		$9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::boolean[], $15::timestamptz[])
	ON CONFLICT DO NOTHING RETURNING sop_instance_uid, sha256`

// insertDeidentifiedReceivedSQL is insertReceivedSQL with each file's flag
// deidentified, a sixteenth array.
const insertDeidentifiedReceivedSQL = `INSERT INTO received_files (` + receivedColumns + `)
	SELECT * FROM unnest($1::text[], $2::text[], $3::bytea[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[],
		$9::text[], $10::text[], $11::text[], $12::text[], $13::text[], $14::boolean[], $15::timestamptz[], $16::boolean[])
	ON CONFLICT DO NOTHING RETURNING sop_instance_uid, sha256`

// insertReceived writes files (all of one reception, no key twice) with
// insertReceivedSQL, at the time given, and returns the keys of those that
// were new.
func insertReceived(ctx context.Context, tx pgx.Tx, files []tenant.ReceivedFile, at time.Time) (map[fileKey]bool, error) {
	n := len(files)
	text := func() []string { return make([]string, 0, n) }
	rids, uids, series, studies, classes, syntaxes, incoming, calling, called, addrs, assocs :=
		text(), text(), text(), text(), text(), text(), text(), text(), text(), text(), text()
	sums := make([][]byte, 0, n)
	sizes := make([]int64, 0, n)
	released, deidentified := make([]bool, 0, n), make([]bool, 0, n)
	times := make([]time.Time, 0, n)
	for _, f := range files {
		rids, uids, sums = append(rids, f.ReceptionID), append(uids, f.SOPInstanceUID), append(sums, sumBytes(f.SHA256))
		series, studies, classes = append(series, f.SeriesUID), append(studies, f.StudyUID), append(classes, f.SOPClassUID)
		syntaxes, sizes, incoming = append(syntaxes, f.TransferSyntaxUID), append(sizes, f.Size), append(incoming, f.Incoming)
		calling, called, addrs = append(calling, f.Caller.CallingAETitle), append(called, f.Caller.CalledAETitle), append(addrs, f.Caller.Address)
		assocs, released, times = append(assocs, f.Association), append(released, f.Released), append(times, micros(at))
		deidentified = append(deidentified, f.Deidentified)
	}
	args := []any{rids, uids, sums, series, studies, classes, syntaxes, sizes, incoming, calling, called, addrs, assocs, released, times}
	query := insertReceivedSQL
	if slices.Contains(deidentified, true) {
		query, args = insertDeidentifiedReceivedSQL, append(args, deidentified)
	}
	rows, err := tx.Query(ctx, query, args...)
	keys, err := collect(rows, err, func(row scanner) (fileKey, error) {
		var k fileKey
		var sum []byte
		err := row.Scan(&k.uid, &sum)
		k.sum = sqlkit.ToSHA256(sum)
		return k, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[fileKey]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

// batchReception is what AddReceivedFiles read of one reception it locked:
// its study and calling AE titles, or why it refuses the batch.
type batchReception struct {
	study, titles string
	truncated     bool
	refused       error
}

// lockBatchReceptions locks each reception of ids that is receiving, FOR NO
// KEY UPDATE, in the order given (sorted by id, the package's order), in the
// statement that checks it is receiving, and reads what the batch needs of
// it. One that is not receiving is not locked and is recorded as refusing
// the batch: NotFound when the tenant does not have it, ErrReceptionClosed
// when it has left receiving.
func lockBatchReceptions(ctx context.Context, tx pgx.Tx, ids []string) (map[string]batchReception, error) {
	out := make(map[string]batchReception, len(ids))
	for _, id := range ids {
		var h batchReception
		err := tx.QueryRow(ctx, `SELECT study_uid, calling_ae_titles, calling_truncated FROM receptions
			WHERE id = $1 AND state = 'receiving' FOR NO KEY UPDATE`, id).Scan(&h.study, &h.titles, &h.truncated)
		if isNoRow(err) {
			has, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = $1`, id)
			if err != nil {
				return nil, err
			}
			h.refused = tenant.ReceptionClosed()
			if !has {
				h.refused = errNoReception()
			}
		} else if err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, nil
}

// AddReceivedFiles implements tenant.Pipeline. Each reception the batch
// names is locked first, FOR NO KEY UPDATE (the lock its UPDATE of counts
// takes), in order of id, in the statement that checks it is receiving: a
// step and a batch of one reception are then ordered, and two batches naming
// two receptions take them in one order. When receptions refuse the batch,
// the answer is the first that refuses it in the order the batch names
// them, not in the order they were locked. Each reception's files are then
// written as one set, and its counts added by one UPDATE on the row it holds.
// The unit's row, when a fence is given, is locked first (checkFence), before
// any reception's, as every fenced write of the package locks it.
func (t *Tenant) AddReceivedFiles(ctx context.Context, files []tenant.ReceivedFile, at time.Time, fence *tenant.WorkFence) (int, error) {
	if err := tenant.ValidateReceivedBatch(files, at); err != nil {
		return 0, err
	}
	if fence != nil {
		if err := fence.Validate(); err != nil {
			return 0, err
		}
	}
	if len(files) == 0 {
		if fence == nil {
			return 0, t.open()
		}
		return 0, t.pipe(ctx, "the received files cannot be recorded", func(tx pgx.Tx) error { return checkFence(ctx, tx, *fence) })
	}
	// The files of each reception, in the batch's order, each key once (the
	// first kept, as a row-by-row insert would keep it), and the receptions
	// in the order the batch first names them.
	var named []string
	byReception := map[string][]tenant.ReceivedFile{}
	seen := map[string]map[fileKey]bool{}
	for _, f := range files {
		if seen[f.ReceptionID] == nil {
			seen[f.ReceptionID] = map[fileKey]bool{}
			named = append(named, f.ReceptionID)
		}
		k := fileKey{f.SOPInstanceUID, f.SHA256}
		if seen[f.ReceptionID][k] {
			continue
		}
		seen[f.ReceptionID][k] = true
		byReception[f.ReceptionID] = append(byReception[f.ReceptionID], f)
	}
	order := slices.Sorted(slices.Values(named))
	var inserted int
	err := t.pipe(ctx, "the received files cannot be recorded", func(tx pgx.Tx) error {
		if fence != nil {
			if err := checkFence(ctx, tx, *fence); err != nil {
				return err
			}
		}
		// An import unit's row before any reception's, as every fenced
		// write locks its unit first.
		x := sendingTx{tx: tx}
		if err := sqlkit.CheckImportFences(ctx, x, sqlkit.ReceivedArrivals(files)); err != nil {
			return err
		}
		held, err := lockBatchReceptions(ctx, tx, order)
		if err != nil {
			return err
		}
		// Every reception is locked before any is judged, and the first
		// that refuses the batch in the order the batch names them is the
		// answer: the order of the locks is the database's concern, and a
		// batch that two receptions refuse gets the same answer from every
		// driver.
		for _, id := range named {
			h := held[id]
			if h.refused != nil {
				return h.refused
			}
			for _, f := range byReception[id] {
				if f.StudyUID != h.study {
					return fault.New(fault.Invalid, "a received file's Study Instance UID is its reception's")
				}
			}
		}
		total := 0
		for _, id := range order {
			batch := byReception[id]
			titlesText, truncated := held[id].titles, held[id].truncated
			var titles []string
			if err := sqlkit.FromJSON(titlesText, &titles); err != nil {
				return err
			}
			added, err := insertReceived(ctx, tx, batch, at)
			if err != nil {
				return err
			}
			if len(added) == 0 {
				continue
			}
			var nFiles, nBytes, nReleased int64
			var assocs []string
			for _, f := range batch {
				if !added[fileKey{f.SOPInstanceUID, f.SHA256}] {
					continue
				}
				nFiles++
				nBytes += f.Size
				if f.Released {
					nReleased++
				}
				if f.ReleasedStorage != "" || !f.ReleasedProfile.IsZero() {
					if err := keepReleased(ctx, tx, id, f.ReleasedStorage, f.ReleasedProfile); err != nil {
						return err
					}
				}
				if f.Association != "" && !slices.Contains(assocs, f.Association) {
					assocs = append(assocs, f.Association)
				}
			}
			// The associations of the new files that the reception sees for
			// the first time, each with the calling AE title of the first new
			// file it brought, in the order the batch first names them.
			newAssoc := map[string]bool{}
			if len(assocs) > 0 {
				rows, err := tx.Query(ctx, `INSERT INTO reception_associations (reception_id, association)
					SELECT $1, a FROM unnest($2::text[]) AS a ON CONFLICT DO NOTHING RETURNING association`, id, assocs)
				names, err := collect(rows, err, func(row scanner) (string, error) {
					var a string
					err := row.Scan(&a)
					return a, err
				})
				if err != nil {
					return err
				}
				for _, a := range names {
					newAssoc[a] = true
				}
			}
			nAssoc := int64(len(newAssoc))
			for _, f := range batch {
				if newAssoc[f.Association] && added[fileKey{f.SOPInstanceUID, f.SHA256}] {
					delete(newAssoc, f.Association)
					titles, truncated = tenant.AddCallingAETitle(titles, truncated, f.Caller.CallingAETitle)
				}
			}
			if titles == nil {
				titles = []string{}
			}
			titlesText, err = sqlkit.JSONText(titles)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE receptions SET files_received = files_received + $1, bytes_received = bytes_received + $2,
				files_released = files_released + $3, associations = associations + $4, calling_ae_titles = $5, calling_truncated = $6,
				last_file = greatest(last_file, $7) WHERE id = $8`,
				nFiles, nBytes, nReleased, nAssoc, titlesText, truncated, micros(at), id); err != nil {
				return err
			}
			total += int(nFiles)
		}
		// Each file an import handed in, with its unit's cursor, in this
		// transaction, in the batch's order: a copy whose received file
		// was there already is still an arrival of its own path
		// (tenant.Importing).
		for _, f := range files {
			if f.Import == nil {
				continue
			}
			if err := importSending().RecordImportArrival(ctx, x, *f.Import, f.ReceptionID, f.SOPInstanceUID, f.SHA256, at); err != nil {
				return err
			}
		}
		inserted = total
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// ReceivedFiles implements tenant.Pipeline.
func (t *Tenant) ReceivedFiles(ctx context.Context, receptionID string, page master.Page) ([]tenant.ReceivedFile, string, error) {
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return nil, "", err
	}
	return t.pageReceived(ctx, listReceivedFiles, listReceivedSQL, receptionID, page)
}

// pageReceived pages a reception's received files with query, which takes
// the reception, the (SOP Instance UID, checksum) after which to start, and
// the limit, in the files' primary key order.
func (t *Tenant) pageReceived(ctx context.Context, list, query, receptionID string, page master.Page) ([]tenant.ReceivedFile, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.binding(receptionID)
	afterUID, afterSum := "", []byte{}
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, list, binding, 2)
		if err != nil {
			return nil, "", err
		}
		sum, err := hex.DecodeString(f[1])
		if err != nil {
			return nil, "", fault.New(fault.Invalid, "the page token is not one this list returned with these arguments")
		}
		afterUID, afterSum = f[0], sum
	}
	out, err := readRows(ctx, t, "the received files cannot be listed", query,
		[]any{receptionID, afterUID, afterSum, page.Limit + 1}, scanReceived)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.EncodeCursor(list, binding, last.SOPInstanceUID, hex.EncodeToString(last.SHA256[:])), nil
}
