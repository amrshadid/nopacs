package correct

import (
	"context"
	"encoding/json"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// searchAfterInstanceDelete returns, by Series Instance UID, the search row
// of the scan of a prepared instance delete read from the next lowest
// instance the scan holds, for the publish to give the scan when its row
// was read from the instance deleted. Nil when the scan holds no other
// instance, or its metadata cannot be read: the row then stays, as it was,
// which is the values of a file of the same series.
func (e *Engine) searchAfterInstanceDelete(ctx context.Context, c tenant.Correction) map[string]search.Row {
	reads, ok := e.cfg.Store.(tenant.InstanceReads)
	if !ok {
		return nil
	}
	scans, err := e.cfg.Store.CorrectionScans(ctx, c.ID)
	if err != nil || len(scans) != 1 {
		return nil
	}
	s := scans[0]
	held, _, err := reads.ListHeldInstances(ctx, s.Path, master.Page{Limit: 2})
	if err != nil {
		return nil
	}
	next := ""
	for _, in := range held {
		if in.SOPInstanceUID != c.Target.Instance {
			next = in.SOPInstanceUID
			break
		}
	}
	if next == "" {
		return nil
	}
	st, err := e.cfg.OpenStorage(ctx, s.Storage)
	if err != nil {
		return nil
	}
	raw, err := lineOf(ctx, st, s.Path, next)
	if err != nil {
		return nil
	}
	var line sidecar.Instance
	if json.Unmarshal(raw, &line) != nil || len(line.Dataset) == 0 {
		return nil
	}
	return map[string]search.Row{s.SeriesUID: search.ExtractJSON(line.Dataset, next)}
}
