package sqlkit

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant's retention rules and the sessions due under them
// (tenant.Retention) on a SQL database: one implementation, run in either
// driver's transactions. The rules of what a rule matches and the times it
// gives are the port's (tenant.RetentionTimesOf, tenant.ClampRetention);
// these statements read and write them.

// The lists' names, the first field of their cursors.
const (
	ListRetentionRules = "tenant-retention-rules"
	ListDueSessions    = "tenant-due-sessions"
)

// retentionRuleColumns are a rule's columns as the store reads them.
const retentionRuleColumns = `name, receiver, route, storage, keep_years, keep_days, dispose_years, dispose_days, action, created,
	updated, notice_until`

// scanRetentionRule reads one row of retentionRuleColumns.
func scanRetentionRule(r Row) (tenant.RetentionRule, error) {
	var out tenant.RetentionRule
	var action string
	var created, updated, notice DBTime
	err := r.Scan(&out.Name, &out.Selector.Receiver, &out.Selector.Route, &out.Selector.Storage, &out.KeepAtLeast.Years,
		&out.KeepAtLeast.Days, &out.DisposeAfter.Years, &out.DisposeAfter.Days, &action, &created, &updated, &notice)
	out.Action, out.Created, out.Updated, out.NoticeUntil = tenant.RetentionAction(action), created.T, updated.T, notice.T
	return out, err
}

// ruleIn reads the rule named, locked with lock; false when there is none.
func ruleIn(ctx context.Context, tx SendingTx, name, lock string) (tenant.RetentionRule, bool, error) {
	var out tenant.RetentionRule
	found := false
	err := tx.Query(ctx, `SELECT `+retentionRuleColumns+` FROM retention_rules WHERE name = ?`+lock, []any{name}, func(r Row) error {
		var err error
		out, err = scanRetentionRule(r)
		found = true
		return err
	})
	return out, found, err
}

// selectorFacts reads what the tenant has of what a selector names.
func selectorFacts(ctx context.Context, tx SendingTx, s tenant.RetentionSelector, lock string) (tenant.SelectorFacts, error) {
	var f tenant.SelectorFacts
	if s.Receiver != "" {
		var config string
		found, err := tx.QueryRow(ctx, `SELECT config FROM receivers WHERE name = ?`+lock, []any{s.Receiver}, &config)
		if err != nil {
			return f, err
		}
		f.ReceiverFound = found
		if found && s.Route != "" {
			var c dimseConfig
			if err := FromJSON(config, &c); err != nil {
				return f, err
			}
			for _, rc := range routingOf(c).Archive {
				f.RouteFound = f.RouteFound || rc.Name == s.Route
			}
		}
	}
	if s.Storage != "" {
		st, found, err := storageIn(ctx, tx, s.Storage, lock)
		if err != nil {
			return f, err
		}
		f.Storage, f.StorageFound = st, found
	}
	return f, nil
}

// checkRule checks a rule as it would stand against the port's rules and
// the tenant's receivers and storages.
func (k Sending) checkRule(ctx context.Context, tx SendingTx, r tenant.RetentionRule) error {
	f, err := selectorFacts(ctx, tx, r.Selector, k.D.Share)
	if err != nil {
		return err
	}
	return tenant.CheckRetentionSelector(r.Selector, f)
}

// lockRetention takes the tenant's configuration lock, then retention's
// row for update (orderRetention): a change of rules or holds waits for
// every commit that read the rules or the holds before it, and holds off
// those after it until it commits.
func (k Sending) lockRetention(ctx context.Context, tx SendingTx) error {
	if err := tx.LockConfig(ctx); err != nil {
		return err
	}
	return orderRetention(ctx, tx, k.D.Lock)
}

// lockRules takes retention's locks for a change of rules (lockRetention)
// and returns ch at the time it stores: its time, or the time trusted when
// the clock reads earlier (storeTime), so a rule shortened while the clock
// is set back gets its whole notice from the time trusted, never a notice
// already run out.
func (k Sending) lockRules(ctx context.Context, tx SendingTx, ch tenant.RetentionChange) (tenant.RetentionChange, error) {
	if err := k.lockRetention(ctx, tx); err != nil {
		return ch, err
	}
	var err error
	ch.Now, err = storeTime(ctx, tx, ch.Now)
	return ch, err
}

// restoreAll stores again every session's times after a change of rules at
// ch, and marks the rule named with the notice's end when the change made
// anything due sooner or kept it less. Every change of a rule ends the
// exemptions a person's undo gave under it.
func (k Sending) restoreAll(ctx context.Context, tx SendingTx, name string, ch tenant.RetentionChange) error {
	rules, err := retentionRulesIn(ctx, tx)
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
	clamped, err := k.storeRetention(ctx, tx, rules, arrivals, rows, ch.Now, ch.Notice)
	if err != nil {
		return err
	}
	if clamped {
		if _, err := tx.Exec(ctx, `UPDATE retention_rules SET notice_until = ? WHERE name = ?`, k.D.Time(ch.Now.Add(ch.Notice)), name); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE retention_sessions SET exempt_rule = '' WHERE exempt_rule = ?`, name)
	return err
}

// AddRetentionRule implements tenant.Retention.
func (k Sending) AddRetentionRule(ctx context.Context, r tenant.RetentionRule, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	if err := r.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := ch.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	var out tenant.RetentionRule
	err := k.Host.Write(ctx, "the retention rule cannot be added", func(tx SendingTx) error {
		ch, err := k.lockRules(ctx, tx, ch)
		if err != nil {
			return err
		}
		if _, found, err := ruleIn(ctx, tx, r.Name, k.D.Lock); err != nil || found {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "the tenant has a retention rule named "+r.Name)
		}
		if err := k.checkRule(ctx, tx, r); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO retention_rules (`+retentionRuleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
			r.Name, r.Selector.Receiver, r.Selector.Route, r.Selector.Storage, r.KeepAtLeast.Years, r.KeepAtLeast.Days,
			r.DisposeAfter.Years, r.DisposeAfter.Days, string(r.Action), k.D.Time(ch.Now), k.D.Time(ch.Now)); err != nil {
			return err
		}
		if err := k.restoreAll(ctx, tx, r.Name, ch); err != nil {
			return err
		}
		if out, err = k.ruleWithDue(ctx, tx, r.Name, ch.Now); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}

// SetRetentionRule implements tenant.Retention.
func (k Sending) SetRetentionRule(ctx context.Context, r tenant.RetentionRule, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	if err := r.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := ch.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	var out tenant.RetentionRule
	err := k.Host.Write(ctx, "the retention rule cannot be changed", func(tx SendingTx) error {
		ch, err := k.lockRules(ctx, tx, ch)
		if err != nil {
			return err
		}
		old, found, err := ruleIn(ctx, tx, r.Name, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no retention rule named " + r.Name)
		}
		if !old.SameAs(r) {
			if err := k.checkRule(ctx, tx, r); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE retention_rules SET receiver = ?, route = ?, storage = ?, keep_years = ?, keep_days = ?,
				dispose_years = ?, dispose_days = ?, action = ?, updated = ? WHERE name = ?`, r.Selector.Receiver, r.Selector.Route,
				r.Selector.Storage, r.KeepAtLeast.Years, r.KeepAtLeast.Days, r.DisposeAfter.Years, r.DisposeAfter.Days, string(r.Action),
				k.D.Time(ch.Now), r.Name); err != nil {
				return err
			}
			if err := k.restoreAll(ctx, tx, r.Name, ch); err != nil {
				return err
			}
		}
		if out, err = k.ruleWithDue(ctx, tx, r.Name, ch.Now); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}

// RemoveRetentionRule implements tenant.Retention.
func (k Sending) RemoveRetentionRule(ctx context.Context, name string, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	if err := tenant.ValidateName("rule", name); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := ch.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RetentionRule{}, err
	}
	var out tenant.RetentionRule
	err := k.Host.Write(ctx, "the retention rule cannot be removed", func(tx SendingTx) error {
		ch, err := k.lockRules(ctx, tx, ch)
		if err != nil {
			return err
		}
		if out, err = k.ruleWithDue(ctx, tx, name, ch.Now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM retention_rules WHERE name = ?`, name); err != nil {
			return err
		}
		if err := k.restoreAll(ctx, tx, name, ch); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}

// ruleWithDue reads the rule named with its sessions due at now; NotFound
// when there is none.
func (k Sending) ruleWithDue(ctx context.Context, tx SendingTx, name string, now time.Time) (tenant.RetentionRule, error) {
	r, found, err := ruleIn(ctx, tx, name, "")
	if err != nil {
		return r, err
	}
	if !found {
		return r, notFound("the tenant has no retention rule named " + name)
	}
	eff, err := effectiveNow(ctx, tx, now)
	if err != nil {
		return r, err
	}
	_, err = tx.QueryRow(ctx, `SELECT count(*) FROM retention_sessions AS r WHERE r.due_at IS NOT NULL AND r.rule = ? AND r.due_at <= ?
		AND EXISTS (SELECT 1 FROM sessions AS s WHERE s.study_uid = r.study_uid)`, []any{name, k.D.Time(eff)}, &r.SessionsDue)
	return r, err
}

// RetentionRule implements tenant.Retention.
func (k Sending) RetentionRule(ctx context.Context, name string, now time.Time) (tenant.RetentionRule, error) {
	if err := tenant.ValidateName("rule", name); err != nil {
		return tenant.RetentionRule{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.RetentionRule{}, err
	}
	var out tenant.RetentionRule
	err := k.Host.Read(ctx, "the retention rule cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.ruleWithDue(ctx, tx, name, now)
		return err
	})
	return out, err
}

// ListRetentionRules implements tenant.Retention.
func (k Sending) ListRetentionRules(ctx context.Context, now time.Time, page master.Page) ([]tenant.RetentionRule, string, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding()
	after := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListRetentionRules, binding, 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	var out []tenant.RetentionRule
	err := k.Host.Read(ctx, "the retention rules cannot be listed", func(tx SendingTx) error {
		out = nil
		var names []string
		if err := tx.Query(ctx, `SELECT name FROM retention_rules WHERE name > ? ORDER BY name LIMIT ?`, []any{after, page.Limit + 1},
			func(r Row) error {
				var n string
				err := r.Scan(&n)
				names = append(names, n)
				return err
			}); err != nil {
			return err
		}
		for _, n := range names {
			r, err := k.ruleWithDue(ctx, tx, n, now)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListRetentionRules, binding, last.Name), nil
}

// The due sessions are read as a range of the index of due times: under a
// rule (retention_sessions_due) or under any (retention_sessions_next), due
// at a time, after a position; each row's session is then looked up by its
// unique Study Instance UID, so a session a delete hides is passed over and
// no planner is tempted to read the catalog's sessions whole for a join.
const (
	dueColumns   = `study_uid, start_at, due_at, rule, action, exempt_rule`
	dueByRuleSQL = `SELECT ` + dueColumns + ` FROM retention_sessions
		WHERE due_at IS NOT NULL AND rule = ? AND due_at <= ? AND (due_at, study_uid) > (?, ?) ORDER BY due_at, study_uid LIMIT ?`
	dueAllSQL = `SELECT ` + dueColumns + ` FROM retention_sessions
		WHERE due_at IS NOT NULL AND due_at <= ? AND (due_at, study_uid) > (?, ?) ORDER BY due_at, study_uid LIMIT ?`
	dueSessionSQL = `SELECT subject_label, subject_folded, label, folded, instance_count, bytes, scan_count FROM sessions WHERE study_uid = ?`
)

// dueRow is one row of dueColumns, with what a hold is matched by.
type dueRow struct {
	s                            tenant.DueSession
	subjectFolded, sessionFolded string
	exempt                       string
	scans                        int64
	// live says the catalog shows its session; a session a delete hides is
	// read for the position only.
	live bool
}

// readDue reads at most limit due rows after (due, study), under rule or
// any when rule is "".
func (k Sending) readDue(ctx context.Context, tx SendingTx, rule string, eff, afterDue time.Time, afterStudy string,
	limit int) ([]dueRow, error) {
	var q string
	var args []any
	if rule != "" {
		q, args = dueByRuleSQL, []any{rule}
	} else {
		q = dueAllSQL
	}
	args = append(args, k.D.Time(eff), k.D.Time(afterDue), afterStudy, limit)
	var rows []dueRow
	err := tx.Query(ctx, q, args, func(r Row) error {
		var d dueRow
		var start, due DBTime
		var action string
		if err := r.Scan(&d.s.StudyUID, &start, &due, &d.s.Rule, &action, &d.exempt); err != nil {
			return err
		}
		d.s.Start, d.s.Due, d.s.Action = start.T, due.T, tenant.RetentionAction(action)
		d.s.Undone = d.exempt != "" && d.exempt == d.s.Rule
		rows = append(rows, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		d := &rows[i]
		if d.live, err = tx.QueryRow(ctx, dueSessionSQL, []any{d.s.StudyUID}, &d.s.SubjectLabel, &d.subjectFolded, &d.s.SessionLabel,
			&d.sessionFolded, &d.s.Instances, &d.s.Bytes, &d.scans); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// dueBefore is the position before every due row: a due time earlier than
// any stored.
var dueBefore = time.Unix(0, 0).UTC()

// DueSessions implements tenant.Retention.
func (k Sending) DueSessions(ctx context.Context, f tenant.DueFilter, page master.Page) ([]tenant.DueSession, string, error) {
	if err := f.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(f.Rule, strconv.FormatBool(f.IncludeHeld))
	afterDue, afterStudy := dueBefore, ""
	if page.Cursor != "" {
		fs, err := DecodeCursor(page.Cursor, ListDueSessions, binding, 2)
		if err != nil {
			return nil, "", err
		}
		us, err := DecodeCursorInt(fs[0])
		if err != nil {
			return nil, "", err
		}
		afterDue, afterStudy = time.UnixMicro(us).UTC(), fs[1]
	}
	var out []tenant.DueSession
	err := k.Host.Read(ctx, "the due sessions cannot be listed", func(tx SendingTx) error {
		out = nil
		eff, err := effectiveNow(ctx, tx, f.Now)
		if err != nil {
			return err
		}
		holds, err := activeHolds(ctx, tx)
		if err != nil {
			return err
		}
		d, s := afterDue, afterStudy
		for len(out) <= page.Limit {
			rows, err := k.readDue(ctx, tx, f.Rule, eff, d, s, page.Limit+1)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if !r.live {
					continue
				}
				r.s.Hold = holds.covering(r.subjectFolded, r.sessionFolded)
				r.s.Held = r.s.Hold != ""
				if !r.s.Held || f.IncludeHeld {
					out = append(out, r.s)
				}
			}
			if len(rows) <= page.Limit {
				return nil
			}
			last := rows[len(rows)-1]
			d, s = last.s.Due, last.s.StudyUID
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListDueSessions, binding, MicrosText(last.Due), last.StudyUID), nil
}

// SessionRetention implements tenant.Retention.
func (k Sending) SessionRetention(ctx context.Context, studyUID string) (tenant.SessionRetention, error) {
	if err := tenant.ValidateStudyUID(studyUID); err != nil {
		return tenant.SessionRetention{}, err
	}
	var out tenant.SessionRetention
	err := k.Host.Read(ctx, "the session's retention cannot be read", func(tx SendingTx) error {
		rows, err := retentionRowsIn(ctx, tx, []string{studyUID})
		if err != nil {
			return err
		}
		r, found := rows[studyUID]
		if !found {
			return notFound("the tenant has no retention row of this study")
		}
		out = tenant.SessionRetention{StudyUID: r.study, Start: r.start, Archived: r.archived, Floor: r.floor, KeepUntil: r.keep,
			Due: r.due, Rule: r.rule, Action: tenant.RetentionAction(r.action), ExemptRule: r.exempt}
		return nil
	})
	return out, err
}
