package sqlkit

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/amrshadid/nopacs/internal/search"
)

// searchShadowPage is how many scans' rows one read of the shadow gives:
// the rows are written between the reads, and a transaction holds one
// statement's rows open at a time.
const searchShadowPage = 500

// copyShadowSearch makes the search rows of the catalog in use those the
// shadow of prefix sp holds, in the activation's transaction, after its
// scans are the shadow's: every row is removed and each scan the shadow
// names is given the row the build read from its metadata, so no scan
// the rebuild removed keeps a row and every scan has the one an archive
// of its files makes.
func (k Rebuild) copyShadowSearch(ctx context.Context, tx SendingTx, sp string) error {
	for _, t := range search.Tables() {
		// #nosec G202 -- the table names are the registry's.
		if _, err := tx.Exec(ctx, `DELETE FROM `+t); err != nil {
			return err
		}
	}
	type kept struct{ series, content string }
	after := ""
	for {
		var page []kept
		err := tx.Query(ctx, `SELECT series_uid, content FROM `+sp+`search_shadow WHERE series_uid > ? ORDER BY series_uid LIMIT `+
			strconv.Itoa(searchShadowPage), []any{after}, func(r Row) error {
			var s kept
			err := r.Scan(&s.series, &s.content)
			page = append(page, s)
			return err
		})
		if err != nil {
			return err
		}
		for _, s := range page {
			id, found, err := scanIDOf(ctx, tx, s.series)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			var row search.Row
			if err := json.Unmarshal([]byte(s.content), &row); err != nil {
				return err
			}
			if err := WriteSearch(ctx, tx, id, row.Normal(), search.Replace); err != nil {
				return err
			}
		}
		if len(page) < searchShadowPage {
			return nil
		}
		after = page[len(page)-1].series
	}
}
