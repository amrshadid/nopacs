package rebuild

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ByRow is a Config.Decide that applies each correction record as the
// correction's row in the tenant's database says, for the records that
// verify under the installation keys (the rebuild applies none that does
// not). The row is the authority and the record follows it: a record
// rewritten after its row's step may lag it (a stop between the two, or a
// rewrite that failed), and a record says nothing of a request that never
// answered. So:
//
//   - a record with a row is applied with the row's state: a delete undone
//     or abandoned hides nothing, whatever its record still says, and a
//     delete committed, purging or purged hides what it names;
//   - a delete whose row is still prepared hides nothing: no request
//     answered it, and a start abandons it;
//   - a record of another kind than its row is applied not at all;
//   - a record with no row is applied as it reads, by the rule a restore
//     adopts it by (record.Correction.Alone): a delete committed, purging,
//     purged or still prepared hides what it names (the last the
//     fail-closed side, counted unresolved), one abandoned or undone shows
//     it; a relabel or a move published hides its old tree, one prepared
//     or abandoned (or a move undone) its new tree; a replace hides
//     nothing.
//
// A row that cannot be read is an error: the unit's attempt fails rather
// than apply a record the database may contradict.
func ByRow(ctx context.Context, p tenant.Pipeline, c record.Correction) (record.Correction, bool, error) {
	row, err := p.Correction(ctx, c.ID)
	if fault.KindOf(err) == fault.NotFound {
		c.State, _ = c.Alone()
		return c, true, nil
	}
	if err != nil {
		return c, false, err
	}
	if row.Kind != c.Kind {
		return c, false, nil
	}
	c.State = row.State
	if c.Kind == record.KindDelete && c.State == record.StatePrepared {
		c.State = record.StateAbandoned
	}
	return c, true, nil
}
