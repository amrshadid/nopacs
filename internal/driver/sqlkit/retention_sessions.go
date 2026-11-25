package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The retention times stored per session (tenant.Retention, "Times stored
// per session"): read, computed by the port's rules from the session's
// start and what its scans arrived through, and written again in the
// transaction of every change that moves them.

// retentionRow is a session's row of retention_sessions.
type retentionRow struct {
	study                  string
	start, archived, floor time.Time
	keep, due              time.Time
	rule, action, exempt   string
}

// times returns the row's stored times as the port's rules state them.
func (r retentionRow) times() tenant.RetentionTimes {
	return tenant.RetentionTimes{KeepUntil: r.keep, Due: r.due, Rule: r.rule, Action: tenant.RetentionAction(r.action)}
}

// retentionRowColumns are a row's columns as the store reads them.
const retentionRowColumns = `study_uid, start_at, archived_at, floor_at, keep_until, due_at, rule, action, exempt_rule`

// scanRetentionRow reads one row of retentionRowColumns.
func scanRetentionRow(r Row) (retentionRow, error) {
	var out retentionRow
	var start, archived, floor, keep, due DBTime
	err := r.Scan(&out.study, &start, &archived, &floor, &keep, &due, &out.rule, &out.action, &out.exempt)
	out.start, out.archived, out.floor, out.keep, out.due = start.T, archived.T, floor.T, keep.T, due.T
	return out, err
}

// retentionRowsIn reads the rows of the studies named, or of every session
// when studies is nil, by Study Instance UID.
func retentionRowsIn(ctx context.Context, tx SendingTx, studies []string) (map[string]retentionRow, error) {
	out := map[string]retentionRow{}
	read := func(q string, args []any) error {
		return tx.Query(ctx, q, args, func(r Row) error {
			row, err := scanRetentionRow(r)
			out[row.study] = row
			return err
		})
	}
	if studies == nil {
		return out, read(`SELECT `+retentionRowColumns+` FROM retention_sessions`, nil)
	}
	for _, s := range studies {
		if err := read(`SELECT `+retentionRowColumns+` FROM retention_sessions WHERE study_uid = ?`, []any{s}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// arrivalsOf reads what each scan of the sessions came through, by Study
// Instance UID: the receiver that first archived into it, the route that
// chose for its session, the storage it is on. study names one session, or
// "" for every session the catalog shows.
func arrivalsOf(ctx context.Context, tx SendingTx, study string) (map[string][]tenant.ScanArrival, error) {
	q := `SELECT s.study_uid, s.route, sc.receiver, sc.storage FROM sessions AS s JOIN scans AS sc ON sc.session_id = s.id`
	var args []any
	if study != "" {
		q, args = q+` WHERE s.study_uid = ?`, []any{study}
	}
	out := map[string][]tenant.ScanArrival{}
	routes := map[string]string{}
	err := tx.Query(ctx, q, args, func(r Row) error {
		var uid, routeText string
		var a tenant.ScanArrival
		if err := r.Scan(&uid, &routeText, &a.Receiver, &a.Storage); err != nil {
			return err
		}
		route, ok := routes[routeText]
		if !ok {
			rt, err := ReadReceptionRoute(routeText)
			if err != nil {
				return err
			}
			route, routes[routeText] = rt.Route, rt.Route
		}
		a.Route = route
		out[uid] = append(out[uid], a)
		return nil
	})
	return out, err
}

// retentionRulesIn reads every rule of the tenant, by name.
func retentionRulesIn(ctx context.Context, tx SendingTx) ([]tenant.RetentionRule, error) {
	var out []tenant.RetentionRule
	err := tx.Query(ctx, `SELECT `+retentionRuleColumns+` FROM retention_rules ORDER BY name`, nil, func(r Row) error {
		rule, err := scanRetentionRule(r)
		out = append(out, rule)
		return err
	})
	return out, err
}

// storedNotice reads the notice the last check of the clock gave, which a
// change the server makes by itself (a commit, a publish, an undo, an
// activation) is clamped by.
func storedNotice(ctx context.Context, tx SendingTx) (time.Duration, error) {
	var us int64
	if _, err := tx.QueryRow(ctx, `SELECT notice FROM retention_state WHERE one = 1`, nil, &us); err != nil {
		return 0, err
	}
	return time.Duration(us) * time.Microsecond, nil
}

// orderRetention takes retention_state's one row with lock, which orders a
// change of rules or holds against the commits beside it. A commit, or any
// change that stores a session's times from the rules, reads the row
// ShareAll before it reads the rules; a change of rules or a new hold reads
// it Lock before it writes. At read committed a change of rules cannot see
// a commit not yet committed, so without the order the commit would store
// times from the rules before the change while the change stored the times
// of every session but the commit's: a keep-at-least the change added
// would be lost for that session. With it, whichever runs second waits for
// the first, then reads what the first committed. SQLite's one writer
// orders them already ("" for both clauses).
func orderRetention(ctx context.Context, tx SendingTx, lock string) error {
	var one int64
	_, err := tx.QueryRow(ctx, `SELECT one FROM retention_state WHERE one = 1`+lock, nil, &one)
	return err
}

// storeRetention stores again the times of each session in arrivals (each
// one the catalog shows) from rules, clamped as a change at now with the
// notice: a session with no row gets one, started at its earliest held
// time, and is not clamped. It reports whether any session was clamped.
func (k Sending) storeRetention(ctx context.Context, tx SendingTx, rules []tenant.RetentionRule,
	arrivals map[string][]tenant.ScanArrival, rows map[string]retentionRow, now time.Time, notice time.Duration) (bool, error) {
	clampedAny := false
	for study, a := range arrivals {
		row, had := rows[study]
		if !had {
			var start DBTime
			if _, err := tx.QueryRow(ctx, `SELECT min(i.held_at) FROM instances AS i JOIN scans AS sc ON sc.id = i.scan_id
				JOIN sessions AS s ON s.id = sc.session_id WHERE s.study_uid = ?`, []any{study}, &start); err != nil {
				return false, err
			}
			if start.T.IsZero() {
				start.T = now
			}
			row = retentionRow{study: study, start: start.T, archived: start.T}
			if err := k.insertRetentionRow(ctx, tx, row); err != nil {
				return false, err
			}
		}
		raw := tenant.RetentionTimesOf(rules, row.start, row.floor, a)
		t, clamped := tenant.ClampRetention(row.times(), raw, had, now, notice)
		clampedAny = clampedAny || clamped
		if err := k.writeTimes(ctx, tx, row, t); err != nil {
			return false, err
		}
	}
	return clampedAny, nil
}

// insertRetentionRow writes a new session's row, with no times yet; a row
// another transaction made first for the session is left as it is.
func (k Sending) insertRetentionRow(ctx context.Context, tx SendingTx, r retentionRow) error {
	_, err := tx.Exec(ctx, `INSERT INTO retention_sessions (`+retentionRowColumns+`) VALUES (?, ?, ?, ?, NULL, NULL, '', '', '')
		ON CONFLICT (study_uid) DO NOTHING`, r.study, k.D.Time(r.start), k.D.Time(r.archived), k.nullTime(r.floor))
	return err
}

// writeTimes writes a session's times when they differ from the row's.
func (k Sending) writeTimes(ctx context.Context, tx SendingTx, r retentionRow, t tenant.RetentionTimes) error {
	if r.keep.Equal(t.KeepUntil) && r.due.Equal(t.Due) && r.rule == t.Rule && r.action == string(t.Action) {
		return nil
	}
	if t.Due.IsZero() {
		t.Rule, t.Action = "", ""
	}
	_, err := tx.Exec(ctx, `UPDATE retention_sessions SET keep_until = ?, due_at = ?, rule = ?, action = ? WHERE study_uid = ?`,
		k.nullTime(t.KeepUntil), k.nullTime(t.Due), t.Rule, string(t.Action), r.study)
	return err
}

// storeSessions stores again the times of the sessions of the studies
// named, those the catalog shows, as a change the server makes at now: a
// hidden one keeps its row as it is.
func (k Sending) storeSessions(ctx context.Context, tx SendingTx, studies []string, now time.Time) error {
	if len(studies) == 0 {
		return nil
	}
	if err := orderRetention(ctx, tx, k.D.ShareAll); err != nil {
		return err
	}
	rules, err := retentionRulesIn(ctx, tx)
	if err != nil {
		return err
	}
	notice, err := storedNotice(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := retentionRowsIn(ctx, tx, studies)
	if err != nil {
		return err
	}
	arrivals := map[string][]tenant.ScanArrival{}
	for _, s := range studies {
		a, err := arrivalsOf(ctx, tx, s)
		if err != nil {
			return err
		}
		if len(a[s]) > 0 {
			arrivals[s] = a[s]
		}
	}
	_, err = k.storeRetention(ctx, tx, rules, arrivals, rows, now, notice)
	return err
}

// correctionStudies returns the Study Instance UIDs of a correction's
// scans, in order.
func correctionStudies(ctx context.Context, tx SendingTx, id string) ([]string, error) {
	var out []string
	err := tx.Query(ctx, `SELECT DISTINCT study_uid FROM correction_scans WHERE correction_id = ? ORDER BY study_uid`, []any{id},
		func(r Row) error {
			var s string
			err := r.Scan(&s)
			out = append(out, s)
			return err
		})
	return out, err
}

// ImportRetentionStart answers, in a commit's transaction before the
// commit decides its files' arrivals, the retention start the import job
// that handed the reception's files in gave, or the zero time for none (or
// for a commit that makes no scan, whose start is its session's already).
func (k Sending) ImportRetentionStart(ctx context.Context, tx SendingTx, receptionID string, newScan bool) (time.Time, error) {
	if !newScan {
		return time.Time{}, nil
	}
	var given DBTime
	_, err := tx.QueryRow(ctx, `SELECT j.retention_start FROM import_arrivals AS a JOIN import_jobs AS j ON j.job_id = a.job_id
		WHERE a.reception_id = ? AND j.retention_start IS NOT NULL LIMIT 1`, []any{receptionID}, &given)
	return given.T, err
}

// CommitRetention stores, in a commit's transaction, the retention times
// of the session of c when the commit made a scan: a new session's row
// starts at the commit's time, or at given, the retention start of the
// import job that handed its files in (ImportRetentionStart), then never
// due before the commit's time plus the notice; a session that has a row
// keeps its start, and its times are stored again from what its scans now
// arrived through. A commit into a scan that exists changes nothing
// retention reads. It reads the rules only after it holds retention's row
// shared (orderRetention), so a change of rules beside it either waits for
// the commit and then sees its session, or ends before the commit reads.
func (k Sending) CommitRetention(ctx context.Context, tx SendingTx, c tenant.ScanCommit, newScan bool, given time.Time) error {
	if !newScan {
		return nil
	}
	if err := orderRetention(ctx, tx, k.D.ShareAll); err != nil {
		return err
	}
	rows, err := retentionRowsIn(ctx, tx, []string{c.StudyUID})
	if err != nil {
		return err
	}
	if _, had := rows[c.StudyUID]; !had {
		row := retentionRow{study: c.StudyUID, start: c.At, archived: c.At}
		if !given.IsZero() {
			notice, err := storedNotice(ctx, tx)
			if err != nil {
				return err
			}
			row.start, row.floor = given, c.At.Add(notice)
		}
		if err := k.insertRetentionRow(ctx, tx, row); err != nil {
			return err
		}
		// A new session's times are its first: nothing to clamp.
		rules, err := retentionRulesIn(ctx, tx)
		if err != nil || len(rules) == 0 {
			return err
		}
		arrivals, err := arrivalsOf(ctx, tx, c.StudyUID)
		if err != nil {
			return err
		}
		return k.writeTimes(ctx, tx, row, tenant.RetentionTimesOf(rules, row.start, row.floor, arrivals[c.StudyUID]))
	}
	return k.storeSessions(ctx, tx, []string{c.StudyUID}, c.At)
}

// ResyncRetention makes the retention rows agree with the catalog in use,
// in an activation's transaction: a session the catalog shows with no row
// gets one, started at its earliest held time; every session's times are
// stored again; a row whose session is neither shown nor hidden by a
// delete is dropped. A rebuild neither builds nor compares retention
// state; the rows outlive the catalog's ids since they are keyed by UID.
func (k Sending) ResyncRetention(ctx context.Context, tx SendingTx, now time.Time) error {
	rules, err := retentionRulesIn(ctx, tx)
	if err != nil {
		return err
	}
	notice, err := storedNotice(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := retentionRowsIn(ctx, tx, nil)
	if err != nil {
		return err
	}
	arrivals, err := arrivalsOf(ctx, tx, "")
	if err != nil {
		return err
	}
	if _, err := k.storeRetention(ctx, tx, rules, arrivals, rows, now, notice); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM retention_sessions WHERE NOT EXISTS (SELECT 1 FROM sessions AS s WHERE s.study_uid = retention_sessions.study_uid)
		AND NOT EXISTS (SELECT 1 FROM hidden_sessions AS h WHERE h.study_uid = retention_sessions.study_uid)`)
	return err
}

// retentionAfterPublish stores again, in a publish's or an undo's
// transaction, the times of the sessions a correction touches that the
// catalog shows: a delete of some of a session's scans changes what its
// scans arrived through.
func (k Sending) retentionAfterPublish(ctx context.Context, tx SendingTx, id string, now time.Time) error {
	studies, err := correctionStudies(ctx, tx, id)
	if err != nil {
		return err
	}
	return k.storeSessions(ctx, tx, studies, now)
}

// exemptAfterUndo marks, in an undo's transaction, the sessions of a
// retention delete exempt from its rule's daily pass: the pass never
// deletes again what a person brought back.
func (k Sending) exemptAfterUndo(ctx context.Context, tx SendingTx, c tenant.Correction) error {
	if c.Origin != tenant.OriginRetention || c.RetentionRule == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE retention_sessions SET exempt_rule = ? WHERE study_uid IN
		(SELECT study_uid FROM correction_scans WHERE correction_id = ?)`, c.RetentionRule, c.ID)
	return err
}

// dropPurgedRetention removes, in a delete's purge's end, the rows of the
// sessions it took whole: a session that arrives again starts anew.
func dropPurgedRetention(ctx context.Context, tx SendingTx, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM retention_sessions WHERE study_uid IN (SELECT study_uid FROM correction_scans WHERE correction_id = ?)
		AND NOT EXISTS (SELECT 1 FROM sessions AS s WHERE s.study_uid = retention_sessions.study_uid)
		AND NOT EXISTS (SELECT 1 FROM hidden_sessions AS h WHERE h.study_uid = retention_sessions.study_uid AND h.correction_id <> ?)`,
		id, id)
	return err
}
