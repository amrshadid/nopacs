package sqlkit

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/amrshadid/nopacs/internal/search"
)

// The search tables' writes (search.Tables), as SQL both drivers run in the
// transaction of the change that makes them necessary: the archive step
// that makes or fills a scan, an edit and its undo, a delete and its undo
// and purge, and a rebuild. A row is never written by a transaction of its
// own, so a catalog and its search table agree at every commit.

// WriteSearch writes the search row of the scan with the given id in the
// caller's transaction, with the rows of its values tables, as mode says
// (search.Mode). An empty row (search.Row.IsZero) is written as a row with
// no values from the source that any header replaces.
func WriteSearch(ctx context.Context, tx SendingTx, scanID int64, row search.Row, mode search.Mode) error {
	if row.IsZero() {
		row = search.Empty()
	}
	n, err := tx.Exec(ctx, search.UpsertSQL(mode), search.Args(scanID, row)...)
	if err != nil || n == 0 {
		return err
	}
	for _, q := range search.DeleteValuesSQL() {
		if _, err := tx.Exec(ctx, q, scanID); err != nil {
			return err
		}
	}
	for _, v := range search.Values(row) {
		if _, err := tx.Exec(ctx, search.InsertValueSQL(v), scanID, v.Text, v.FoldedText); err != nil {
			return err
		}
	}
	return nil
}

// ReadSearch returns the search row of the scan with the given id, and
// whether it has one.
func ReadSearch(ctx context.Context, tx SendingTx, scanID int64) (search.Row, bool, error) {
	var g search.Reading
	found, err := tx.QueryRow(ctx, search.SelectSQL(), []any{scanID}, g.Dest()...)
	if err != nil || !found {
		return search.Row{}, false, err
	}
	multi := map[string][]string{}
	for _, a := range search.Attributes() {
		if !a.Multi {
			continue
		}
		err := tx.Query(ctx, search.SelectValuesSQL(a), []any{scanID}, func(r Row) error {
			var v sql.NullString
			if err := r.Scan(&v); err != nil {
				return err
			}
			if v.Valid {
				multi[a.Keyword] = append(multi[a.Keyword], v.String)
			}
			return nil
		})
		if err != nil {
			return search.Row{}, false, err
		}
	}
	return g.Finish(multi), true, nil
}

// scanIDOf returns the id of the scan of a series, and whether the catalog
// holds one.
func scanIDOf(ctx context.Context, tx SendingTx, series string) (int64, bool, error) {
	var id int64
	found, err := tx.QueryRow(ctx, `SELECT id FROM scans WHERE series_uid = ?`, []any{series}, &id)
	return id, found, err
}

// ApplyEditSearch gives the scan of series the search row an edit made of
// its file, in the transaction that publishes the edit: the row as it was
// is kept with the correction first, so its undo can give it back. The row
// changes only if the file it was read from is the one edited, or one with
// a higher UID (search.LowestOrSame).
func ApplyEditSearch(ctx context.Context, tx SendingTx, correction, series string, row search.Row) error {
	id, found, err := scanIDOf(ctx, tx, series)
	if err != nil || !found {
		return err
	}
	prior, held, err := ReadSearch(ctx, tx, id)
	if err != nil {
		return err
	}
	if held {
		text, err := json.Marshal(prior)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO correction_search (correction_id, series_uid, prior) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			correction, series, string(text)); err != nil {
			return err
		}
	}
	return WriteSearch(ctx, tx, id, row, search.LowestOrSame)
}

// RestoreEditSearch gives each scan an edit changed the search row it had
// before, in the transaction that undoes the edit, and drops what was kept.
// A row is given back only where its source is the current row's or lower
// (search.LowestOrSame).
func RestoreEditSearch(ctx context.Context, tx SendingTx, correction string) error {
	type kept struct{ series, text string }
	var all []kept
	err := tx.Query(ctx, `SELECT series_uid, prior FROM correction_search WHERE correction_id = ?`, []any{correction}, func(r Row) error {
		var k kept
		err := r.Scan(&k.series, &k.text)
		all = append(all, k)
		return err
	})
	if err != nil {
		return err
	}
	for _, k := range all {
		id, found, err := scanIDOf(ctx, tx, k.series)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var row search.Row
		if err := json.Unmarshal([]byte(k.text), &row); err != nil {
			return err
		}
		// The row an undo gives back was read from the file with the lowest UID
		// there was then; a file with a lower one that arrived since has
		// given the scan its row, which the lowest-instance rule keeps, as at
		// archive.
		if err := WriteSearch(ctx, tx, id, row.Normal(), search.LowestOrSame); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM correction_search WHERE correction_id = ?`, correction)
	return err
}

// ApplyDeleteSearch gives the scan of series, in the transaction of an
// instance delete, the search row read from the next lowest instance it
// holds, when the row it has was read from the instance deleted (uid): the
// row as it was is kept with the correction for its undo.
func ApplyDeleteSearch(ctx context.Context, tx SendingTx, correction, series, uid string, row search.Row) error {
	id, found, err := scanIDOf(ctx, tx, series)
	if err != nil || !found {
		return err
	}
	prior, held, err := ReadSearch(ctx, tx, id)
	if err != nil || !held || prior.Source != uid {
		return err
	}
	text, err := json.Marshal(prior)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO correction_search (correction_id, series_uid, prior) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		correction, series, string(text)); err != nil {
		return err
	}
	return WriteSearch(ctx, tx, id, row, search.Replace)
}
