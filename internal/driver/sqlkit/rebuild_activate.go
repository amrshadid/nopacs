package sqlkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The end of a rebuild: a compare job's (EndRebuild), and the activation
// that makes the shadow the catalog in use (ActivateCatalog), both from
// the tables as rebuild_compare.go describes them.

// movedPrefix begins the folded label a row whose label or parent an
// activation changes holds for the moment between the two writes of the
// activation (copyShadow): "~" is no character of a label, so the value
// is unique (with the row's id after it) and never one a label folds to.
const movedPrefix = "~rebuild~"

// closeHistorySQL moves the feed's mark of what was pruned to just before
// the newest event, the new history's first, written just before it in the
// same transaction (tenant.Events, "Histories"): no read answers an event of
// the old history again, and the empty cursor reads from the new one's
// first. The newest id is that event's: SQLite runs one writer, and on
// Postgres the feed's writers take its lock before they take an id and the
// transaction holds it, so every other event's id is lower, and one less than it is at or past every one of
// them however the ids skip. The old rows are left for the pruning.
const closeHistorySQL = `UPDATE event_marks SET pruned_through = (SELECT max(id) FROM events) - 1
	WHERE pruned_through < (SELECT max(id) FROM events) - 1`

// endRebuildIn ends the rebuild job of r with the build's counts in st and
// its tallies with more, writes its event, and returns it as it is after.
func (k Rebuild) endRebuildIn(ctx context.Context, tx SendingTx, r rebuildRow, st tenant.RebuildProgress, more map[string]int64,
	now time.Time) (tenant.Job, error) {
	c := tenant.JobCounts{Instances: st.Instances + st.LeftOut, Bytes: st.Bytes + st.LeftOutBytes, InstancesSent: st.Instances,
		BytesSent: st.Bytes, InstancesRefused: st.LeftOut, BytesRefused: st.LeftOutBytes, Scans: st.Scans, ScansDone: st.Scans, Listed: true}
	state, reason := tenant.EndState(c)
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state = ?, ended = ?, reason = ?, state_at = ?, ended_at = ?, listed = ?, scans = ?,
		scans_done = ?, instances = ?, bytes = ?, sent = ?, sent_bytes = ?, refused = ?, refused_bytes = ? WHERE id = ?`,
		string(state), k.D.Bool(true), reason, k.D.Time(now), k.D.Time(now), k.D.Bool(true), c.Scans, c.ScansDone, c.Instances, c.Bytes,
		c.InstancesSent, c.BytesSent, c.InstancesRefused, c.BytesRefused, r.job.job.ID); err != nil {
		return tenant.Job{}, err
	}
	tallies := map[string]int64{}
	for key, n := range st.Tallies {
		tallies[key] += n
	}
	for key, n := range more {
		tallies[key] += n
	}
	if err := k.addTallies(ctx, tx, r.job.job.ID, tallies); err != nil {
		return tenant.Job{}, err
	}
	after, err := jobInTx(ctx, tx, r.job.job.ID, "")
	if err != nil {
		return tenant.Job{}, err
	}
	e, err := tenant.JobStepEvent(r.job.job.State, after.job, now)
	if err != nil {
		return tenant.Job{}, err
	}
	return after.job, tx.WriteEvents(ctx, e)
}

// EndRebuild implements tenant.Rebuilding.
func (k Rebuild) EndRebuild(ctx context.Context, fence tenant.WorkFence, cmp tenant.CatalogComparison, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	if err := cmp.Validate(); err != nil {
		return tenant.Job{}, err
	}
	tallies := cmp.Tallies()
	if len(tallies) > 0 {
		if err := tenant.ValidateScrubTallies(tallies); err != nil {
			return tenant.Job{}, err
		}
	}
	var scratch string
	err := k.Host.Write(ctx, "the rebuild job cannot be read", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, "")
		scratch = r.rebuild.Scratch
		return err
	})
	if err != nil {
		return tenant.Job{}, err
	}
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err = k.Host.Write(ctx, "the rebuild job cannot be ended", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, k.D.Lock)
		if err != nil {
			return err
		}
		if r.job.job.State.Ended() {
			out = r.job.job
			return nil
		}
		if r.rebuild.Mode != tenant.RebuildCompare {
			return tenant.Precondition("a rebuild of replace mode ends by making its shadow the catalog in use")
		}
		st, err := k.stateIn(ctx, tx, scratch)
		if err != nil {
			return err
		}
		if st.Job != r.job.job.ID || !st.Complete {
			return tenant.Precondition("the build of the shadow is not complete")
		}
		// The differences by name are kept with the job, in the order the
		// comparison found them, so an admin reads which rows differ after
		// the shadow is gone; the tallies count them all.
		for i, d := range cmp.Differences {
			if _, err := tx.Exec(ctx, `INSERT INTO rebuild_differences (job_id, seq, table_name, kind, row_key) VALUES (?, ?, ?, ?, ?)`,
				r.job.job.ID, i, d.Table, d.Kind, d.Key); err != nil {
				return err
			}
		}
		out, err = k.endRebuildIn(ctx, tx, r, st, tallies, now)
		return err
	})
	return out, err
}

// ActivateCatalog implements tenant.Rebuilding.
func (k Rebuild) ActivateCatalog(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var scratch string
	err := k.Host.Write(ctx, "the rebuild job cannot be read", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, "")
		scratch = r.rebuild.Scratch
		return err
	})
	if err != nil {
		return tenant.Job{}, err
	}
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.Job{}, err
	}
	history, err := newHistoryID()
	if err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err = k.Host.Write(ctx, "the shadow cannot be made the catalog in use", func(tx SendingTx) error {
		r, err := k.rebuildOf(ctx, tx, fence, k.D.Lock)
		if err != nil {
			return err
		}
		switch {
		case r.rebuild.Activated:
			out = r.job.job
			return nil
		case r.rebuild.Mode != tenant.RebuildReplace:
			return tenant.Precondition("only a rebuild of replace mode makes its shadow the catalog in use")
		case r.job.job.State.Ended():
			return tenant.Precondition("the rebuild job has ended " + string(r.job.job.State))
		}
		st, err := k.stateIn(ctx, tx, scratch)
		if err != nil {
			return err
		}
		if st.Job != r.job.job.ID || !st.Complete {
			return tenant.Precondition("the build of the shadow is not complete")
		}
		sp := k.Shadow.Prefix(scratch)
		if err := k.checkJobScans(ctx, tx, sp); err != nil {
			return err
		}
		if err := k.copyShadow(ctx, tx, sp); err != nil {
			return err
		}
		// A replica's state is not the catalog's, but it names the
		// catalog's scans: it is made to agree with the new catalog in the
		// same transaction (tenant.Replication, "Rebuilds and restores").
		if err := k.ResyncReplicas(ctx, tx, now); err != nil {
			return err
		}
		// So are the retention rows, keyed by Study Instance UID: a session
		// the new catalog shows with no row gets one (tenant.Retention).
		if err := k.ResyncRetention(ctx, tx, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE rebuild_jobs SET activated = ? WHERE job_id = ?`, k.D.Bool(true), r.job.job.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE event_history SET id = ?, started = ?`, history, k.D.Time(now)); err != nil {
			return err
		}
		// The new history's first event says what began it, before the
		// job's own.
		e, err := tenant.NewEvent(&tenant.CatalogRebuilt{Job: r.job.job.ID, Instances: st.Instances, NewHistory: true}, now)
		if err != nil {
			return err
		}
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, closeHistorySQL); err != nil {
			return err
		}
		out, err = k.endRebuildIn(ctx, tx, r, st, map[string]int64{tenant.TallyActivated: 1}, now)
		return err
	})
	return out, err
}

// jobScanTables are the tables that name a scan of a job by the scan's
// integer id: what a send job lists, resends and owes, and the failures a
// retry sends again.
var jobScanTables = []string{"job_scans", "job_resends", "job_owed", "job_failures"}

// checkJobScans refuses the activation while a job not ended names, by
// integer id, a scan of the catalog in use that the shadow of prefix sp
// does not hold, or holds with other values: that job would send by the
// id what the catalog no longer says, or, were the id given to another
// scan, another scan's files. The refusal ends the rebuild (it holds
// tenant.ErrRebuildRefused): the operator lets those jobs end, or cancels
// them, and runs the rebuild again.
func (k Rebuild) checkJobScans(ctx context.Context, tx SendingTx, sp string) error {
	var c catalogTable
	for _, t := range catalogTables {
		if t.name == "scans" {
			c = t
		}
	}
	cols, err := k.Shadow.Columns(ctx, tx, c.name)
	if err != nil {
		return err
	}
	named := make([]string, len(jobScanTables))
	args := make([]any, len(jobScanTables))
	for i, t := range jobScanTables {
		named[i] = "SELECT n.scan_id FROM " + t + " AS n JOIN jobs AS j ON j.id = n.job_id WHERE j.ended = ?"
		args[i] = k.D.Bool(false)
	}
	busy, err := exists(ctx, tx, `SELECT 1 FROM scans AS l WHERE l.id IN (`+strings.Join(named, " UNION ")+`)
		AND NOT EXISTS (SELECT 1 FROM `+sp+`scans AS s WHERE s.series_uid = l.series_uid AND `+c.same(sp, "l", cols)+`)`, args...)
	if err != nil {
		return err
	}
	if busy {
		return tenant.RebuildRefused("a job not ended names a scan the rebuilt catalog removes or changes; let it end, or cancel it, and rebuild again")
	}
	return nil
}

// copyShadow makes the shadow of prefix sp the catalog in use, in the
// transaction: the merged tables' rows the shadow does not name are
// removed, those it names take its values and keep their ids, and its
// others are added; the other tables are emptied and copied whole, each
// reference written as the id of the row of the catalog in use that bears
// the name the shadow's reference points at.
//
// A row both catalogs name keeps its integer id because other tables name
// subjects, sessions and scans by id and are not rebuilt (a job's scans,
// its resends and failures; a correction's hidden rows): kept, the id
// still names the same subject, session or scan. A row added takes an id
// the database makes, never one a removed row had: a Postgres identity
// never gives an id twice, and SQLite's tables of subjects, sessions and
// scans are AUTOINCREMENT, whose ids only grow (sqlite.org/autoinc.html),
// so an id a job or a correction still holds never comes to name another
// row.
//
// A row whose label or parent changes is first given a folded label of
// its own (movedPrefix and its id), and then its final values: two rows
// that trade labels (a relabel the storages hold that the catalog does
// not) would otherwise meet the unique index of labels between their two
// updates, and the activation would fail every attempt.
func (k Rebuild) copyShadow(ctx context.Context, tx SendingTx, sp string) error {
	for i := len(catalogTables) - 1; i >= 0; i-- {
		c := catalogTables[i]
		stmt := "DELETE FROM " + c.name
		switch {
		case c.name == "catalog_totals":
			continue
		case c.merged:
			stmt += " WHERE NOT EXISTS (SELECT 1 FROM " + sp + c.name + " AS s WHERE " + c.match(sp, c.name) + ")"
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	for _, c := range catalogTables {
		cols, err := k.Shadow.Columns(ctx, tx, c.name)
		if err != nil {
			return err
		}
		vals := c.values(cols)
		targets, sources := slices.Clone(vals), make([]string, len(vals))
		for i, v := range vals {
			sources[i] = "s." + v
		}
		if c.ref != "" {
			// The id of the row of the catalog in use named as the shadow's
			// reference names its own: written after its table.
			targets = append(targets, c.ref)
			sources = append(sources, "(SELECT t.id FROM "+c.refTable+" AS t WHERE t."+c.refKey+" = "+c.refExpr(sp, "s")+")")
		}
		list, from := strings.Join(targets, ", "), " FROM "+sp+c.name+" AS s"
		if c.name == "catalog_totals" {
			if _, err := tx.Exec(ctx, "UPDATE catalog_totals SET ("+list+") = (SELECT "+strings.Join(sources, ", ")+from+")"); err != nil {
				return err
			}
			continue
		}
		if c.merged {
			own := c.match(sp, c.name)
			if c.name != "subjects" {
				// What names a subject is its folded label, so a subject
				// both catalogs name keeps it; a session or a scan is named
				// by its UID, and may change label or parent.
				if err := moveChanged(ctx, tx, sp, c, cols); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, "UPDATE "+c.name+" SET ("+list+") = (SELECT "+strings.Join(sources, ", ")+from+" WHERE "+own+
				") WHERE EXISTS (SELECT 1"+from+" WHERE "+own+")"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO "+c.name+" ("+list+") SELECT "+strings.Join(sources, ", ")+from+
				" WHERE NOT EXISTS (SELECT 1 FROM "+c.name+" AS l WHERE "+c.match(sp, "l")+")"); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+c.name+" ("+list+") SELECT "+strings.Join(sources, ", ")+from); err != nil {
			return err
		}
	}
	return nil
}

// moveChanged gives each row of the merged table c that the shadow names
// with other values a folded label of its own (movedPrefix and its id):
// every unique index of labels of sessions and scans holds the folded
// label, so no two rows meet while their final values are written.
func moveChanged(ctx context.Context, tx SendingTx, sp string, c catalogTable, cols []string) error {
	_, err := tx.Exec(ctx, "UPDATE "+c.name+" SET folded = ? || CAST(id AS TEXT) WHERE EXISTS (SELECT 1 FROM "+sp+c.name+" AS s WHERE "+
		c.match(sp, c.name)+" AND NOT ("+c.same(sp, c.name, cols)+"))", movedPrefix)
	return err
}

// newHistoryID makes an event history id: 32 lowercase hexadecimal digits
// of a random value.
func newHistoryID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fault.Wrap(fault.Internal, err, "a random history id cannot be made")
	}
	return hex.EncodeToString(b), nil
}
