package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const receptionColumns = `id, receiver, study_uid, arrival, state, files_received, bytes_received, files_archived,
	bytes_archived, files_duplicate, bytes_duplicate, files_quarantined, bytes_quarantined, files_released,
	subject_label, session_label, storage, failure, started, last_file, step_at, ended_at, associations,
	calling_ae_titles, calling_truncated, retry_of, retry_reception, discard_reason, discard_at, route`

// scanReception reads one row of receptionColumns.
func scanReception(row interface{ Scan(...any) error }) (tenant.Reception, error) {
	var r tenant.Reception
	var state, titles, route string
	var started, lastFile, stepAt int64
	var endedAt, discardAt sql.NullInt64
	c := &r.Counts
	if err := row.Scan(&r.ID, &r.Receiver, &r.StudyUID, &r.Arrival, &state, &c.FilesReceived, &c.BytesReceived,
		&c.FilesArchived, &c.BytesArchived, &c.FilesDuplicate, &c.BytesDuplicate, &c.FilesQuarantined, &c.BytesQuarantined,
		&c.FilesReleased, &r.SubjectLabel, &r.SessionLabel, &r.Storage, &r.Failure, &started, &lastFile, &stepAt, &endedAt,
		&r.Associations, &titles, &r.CallingAETitlesTruncated, &r.RetryOf, &r.RetryReception, &r.DiscardReason, &discardAt, &route); err != nil {
		return r, err
	}
	r.State = reception.State(state)
	r.Started, r.LastFile, r.StepTime = fromMicros(started), fromMicros(lastFile), fromMicros(stepAt)
	r.Ended, r.DiscardTime = fromNullMicros(endedAt), fromNullMicros(discardAt)
	if err := sqlkit.FromJSON(titles, &r.CallingAETitles); err != nil {
		return r, err
	}
	if len(r.CallingAETitles) == 0 {
		r.CallingAETitles = nil
	}
	var err error
	r.Route, err = sqlkit.ReadReceptionRoute(route)
	return r, err
}

// receptionInTx reads a reception inside a change's transaction, or
// NotFound.
func receptionInTx(ctx context.Context, tx *sql.Tx, id string) (tenant.Reception, error) {
	r, err := scanReception(tx.QueryRowContext(ctx, `SELECT `+receptionColumns+` FROM receptions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fault.New(fault.NotFound, "the tenant has no reception with this id")
	}
	return r, err
}

// openReceptionInTx finds the reception of the study receiving at the
// receiver, or makes one with id at the next arrival of the study. Two opens
// at once meet at receptions_open, the partial unique index that holds one
// open reception per receiver and study, or at receptions_arrival: the
// insert that meets the other's row changes nothing (ON CONFLICT DO
// NOTHING), and the loop reads again, finding the reception that won, or
// the arrival number taken. On a database with several writers the second
// insert waits for the first's commit, then reads what it committed.
func openReceptionInTx(ctx context.Context, tx *sql.Tx, receiver, studyUID, id string, at time.Time) (tenant.Reception, bool, error) {
	// The receiver first, so a receiver removed is refused whether or not
	// a reception of it is still open, as on every driver.
	has, err := exists(ctx, tx, `SELECT 1 FROM receivers WHERE name = ?`, receiver)
	if err != nil {
		return tenant.Reception{}, false, err
	}
	if !has {
		return tenant.Reception{}, false, tenant.Precondition("the tenant has no receiver named " + receiver)
	}
	for try := 0; try < 8; try++ {
		r, err := scanReception(tx.QueryRowContext(ctx, openReceptionSQL, receiver, studyUID))
		if err == nil {
			return r, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return r, false, err
		}
		inserted, err := insertReception(ctx, tx, id, receiver, studyUID, reception.Receiving, "", at)
		if err != nil {
			return r, false, err
		}
		if inserted {
			// The reception reaching receiving is its first event, written
			// with the row that opens it.
			if err := writeStepEvent(ctx, tx, id, ""); err != nil {
				return r, false, err
			}
			r, err = receptionInTx(ctx, tx, id)
			return r, true, err
		}
	}
	return tenant.Reception{}, false, fault.New(fault.Unavailable, "receptions of the study kept opening at the same moment; try again")
}

// insertReception writes a new reception in state at the next arrival of
// its study, and reports whether it did: a row it meets under one of the
// unique keys (its id, the open reception of its receiver and study, the
// arrival number another took meanwhile) leaves nothing written. An id
// taken by another reception is a Conflict, since ids are the caller's to
// make unique.
func insertReception(ctx context.Context, tx *sql.Tx, id, receiver, studyUID string, state reception.State, retryOf string, at time.Time) (bool, error) {
	var arrival int
	if err := tx.QueryRowContext(ctx, nextArrivalSQL, studyUID).Scan(&arrival); err != nil {
		return false, err
	}
	ts := micros(at)
	res, err := tx.ExecContext(ctx, `INSERT INTO receptions (id, receiver, study_uid, arrival, state, ended,
		files_received, bytes_received, files_archived, bytes_archived, files_duplicate, bytes_duplicate,
		files_quarantined, bytes_quarantined, files_released, subject_label, session_label, storage, failure,
		started, last_file, step_at, ended_at, associations, calling_ae_titles, calling_truncated,
		retry_of, retry_reception, discard_reason, discard_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '', ?, ?, ?, NULL, 0, '[]', 0, ?, '', '', NULL)
		ON CONFLICT DO NOTHING`,
		id, receiver, studyUID, arrival, string(state), boolInt(state.Ended()), ts, ts, ts, retryOf)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 1 {
		return n == 1, err
	}
	taken, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	if taken {
		return false, fault.New(fault.Conflict, "a reception with this id exists")
	}
	return false, nil
}

// OpenReception implements tenant.Pipeline.
func (t *Tenant) OpenReception(ctx context.Context, receiver, studyUID, id string, at time.Time) (tenant.Reception, bool, error) {
	if err := tenant.ValidateOpen(receiver, studyUID, id, at); err != nil {
		return tenant.Reception{}, false, err
	}
	var out tenant.Reception
	var opened bool
	err := t.write(ctx, "the reception cannot be opened", func(tx *sql.Tx, _ *tenantStatements) error {
		var err error
		out, opened, err = openReceptionInTx(ctx, tx, receiver, studyUID, id, at)
		return err
	})
	if err != nil {
		return tenant.Reception{}, false, err
	}
	return out, opened, nil
}

// Reception implements tenant.Pipeline.
func (t *Tenant) Reception(ctx context.Context, id string) (tenant.Reception, []tenant.Arrival, error) {
	if err := master.ValidateID("reception id", id); err != nil {
		return tenant.Reception{}, nil, err
	}
	r, err := readOne(ctx, t, "the tenant has no reception with this id", "the reception cannot be read",
		`SELECT `+receptionColumns+` FROM receptions WHERE id = ?`, []any{id}, scanReception)
	if err != nil {
		return tenant.Reception{}, nil, err
	}
	// The latest arrivals, from receptions_arrival read backward, then put
	// in order of arrival number.
	arrivals, err := readRows(ctx, t, "the reception's arrivals cannot be read",
		arrivalsSQL,
		[]any{r.StudyUID, tenant.MaxArrivals}, func(row interface{ Scan(...any) error }) (tenant.Arrival, error) {
			var a tenant.Arrival
			var state string
			var started int64
			err := row.Scan(&a.ID, &a.Arrival, &state, &started)
			a.State, a.Started = reception.State(state), fromMicros(started)
			return a, err
		})
	if err != nil {
		return tenant.Reception{}, nil, err
	}
	slices.Reverse(arrivals)
	return r, arrivals, nil
}

// receptionBinding is what a ListReceptions cursor is bound to: the tenant
// and every field of the filter.
func (t *Tenant) receptionBinding(f tenant.ReceptionFilter) string {
	states := make([]string, len(f.States))
	for i, s := range f.States {
		states[i] = string(s)
	}
	return t.binding(f.Receiver, f.StudyUID, strings.Join(states, ","), strconv.FormatBool(f.InProgress),
		sqlkit.MicrosText(f.Since), sqlkit.MicrosText(f.Until))
}

// ListReceptions implements tenant.Pipeline.
func (t *Tenant) ListReceptions(ctx context.Context, filter tenant.ReceptionFilter, page master.Page) ([]tenant.Reception, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.receptionBinding(filter)
	pos, err := sqlkit.PageAfterTime(page, listReceptions, binding)
	if err != nil {
		return nil, "", err
	}
	after := afterArgs(pos)
	query, args := receptionsQuery(filter, after, page.Limit+1)
	if query == "" {
		if _, err := t.ready(ctx); err != nil {
			return nil, "", err
		}
		return nil, "", nil
	}
	out, err := readRows(ctx, t, "the receptions cannot be listed", query, args, scanReception)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.TimeCursor(listReceptions, binding, last.Started, last.ID), nil
}

// checkFence refuses a worker's write whose lease no longer holds: the unit
// is not leased, or its attempt is no longer the fence's, because a claim
// took it over. The check is an update of the unit's row with the attempt
// in its condition, so on a database with several writers the row stays
// locked to the end of the worker's transaction: a takeover waits for it,
// and a write after a takeover finds the condition false.
func checkFence(ctx context.Context, tx *sql.Tx, f tenant.WorkFence) error {
	res, err := tx.ExecContext(ctx, `UPDATE work SET updated = updated WHERE id = ? AND attempt = ? AND state = 'leased'`, f.ID, f.Attempt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	has, err := exists(ctx, tx, `SELECT 1 FROM work WHERE id = ?`, f.ID)
	if err != nil {
		return err
	}
	if !has {
		return fault.New(fault.NotFound, "the tenant has no unit of work with this id")
	}
	return tenant.LeaseLost()
}

// StepReception implements tenant.Pipeline.
func (t *Tenant) StepReception(ctx context.Context, step tenant.ReceptionStep, hook tenant.StepHook) (tenant.Reception, error) {
	if err := step.Validate(); err != nil {
		return tenant.Reception{}, err
	}
	var out tenant.Reception
	err := t.write(ctx, "the reception's step cannot be written", func(tx *sql.Tx, _ *tenantStatements) error {
		if step.Fence != nil {
			if err := checkFence(ctx, tx, *step.Fence); err != nil {
				return err
			}
		}
		before, err := receptionInTx(ctx, tx, step.ID)
		if err != nil {
			return err
		}
		next, err := reception.Step(step.From, step.Event)
		if err != nil {
			if before.State != step.From {
				return tenant.StateChanged()
			}
			return fault.Wrap(fault.Conflict, err, "the reception cannot take this step: "+err.Error())
		}
		c := before.Counts
		if step.Counts != nil {
			c.FilesDuplicate, c.BytesDuplicate = step.Counts.FilesDuplicate, step.Counts.BytesDuplicate
		}
		subject, session, storage, failure := before.SubjectLabel, before.SessionLabel, before.Storage, before.Failure
		routeText, err := sqlkit.ReceptionRouteText(before.Route)
		if err != nil {
			return err
		}
		if step.Event == reception.Identify {
			// The storage's row is held to the commit, so its removal or
			// move cannot pass this step unseen (lockStorage).
			if err := lockStorage(ctx, tx, step.Storage); err != nil {
				return err
			}
			subject, session, storage = step.SubjectLabel, step.SessionLabel, step.Storage
			// The routing's choice is kept with the step; a storage a
			// release named stays, so the record says why the storage is
			// what it is.
			chosen := step.Route
			chosen.ReleasedStorage, chosen.ReleasedProfile = before.Route.ReleasedStorage, before.Route.ReleasedProfile
			if routeText, err = sqlkit.ReceptionRouteText(chosen); err != nil {
				return err
			}
		}
		if step.Event == reception.Fail {
			failure = step.Failure
		}
		var endedAt any
		if next.Ended() {
			endedAt = micros(step.At)
		}
		// The compare of the compare-and-set is in the statement that
		// changes the row: the state, and the last file time when the step
		// was decided from it. On a database with several writers that is
		// what holds the rule against a batch or a step committed
		// meanwhile; here the one writer holds it as well.
		lastFile := int64(-1)
		if !step.LastFile.IsZero() {
			lastFile = micros(step.LastFile)
		}
		res, err := tx.ExecContext(ctx, `UPDATE receptions SET state = ?, ended = ?, step_at = ?, ended_at = ?,
			files_duplicate = ?, bytes_duplicate = ?, subject_label = ?, session_label = ?, storage = ?, failure = ?, route = ?
			WHERE id = ? AND state = ? AND (? < 0 OR last_file = ?)`,
			string(next), boolInt(next.Ended()), micros(step.At), endedAt, c.FilesDuplicate, c.BytesDuplicate,
			subject, session, storage, failure, routeText, step.ID, string(step.From), lastFile, lastFile)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return tenant.StateChanged()
		}
		var ended []tenant.EventRecord
		if next.Ended() {
			if ended, err = endReception(ctx, tx, step.ID, next, step.At); err != nil {
				return err
			}
		}
		after, err := receptionInTx(ctx, tx, step.ID)
		if err != nil {
			return err
		}
		// The step's event, in the step's transaction, whatever hook the
		// caller gave: a step is never taken without it. The hook runs
		// after, so a hook that fails undoes both.
		e, err := tenant.ReceptionStepEvent(before.State, after)
		if err != nil {
			return err
		}
		if _, err := writeEvent(ctx, tx, e, nil); err != nil {
			return err
		}
		// A step that reached done forwards the reception to each sender
		// its receiver names, in this transaction (tenant.Sending).
		if err := forwardInTx(ctx, tx, after); err != nil {
			return err
		}
		if hook != nil {
			if err := hook(ctx, before, after); err != nil {
				return err
			}
		}
		// The events of the import jobs the end ended, last.
		if err := writeImportEvents(ctx, tx, ended); err != nil {
			return err
		}
		out = after
		return nil
	})
	if err != nil {
		return tenant.Reception{}, err
	}
	return out, nil
}

// giveUp takes a reception whose unit of work was held as failed through
// the Fail step, in the transaction that held the unit, with
// tenant.FailureWorkGaveUp, releases its claims, so no claim outlives every
// worker that could end its reception, and writes the step's event. It
// reports whether the reception failed: one that has ended, or is not
// there, is left as it is. The step is the core's (reception.Step),
// compared and set like any other.
func giveUp(ctx context.Context, tx *sql.Tx, receptionID string, now time.Time) (bool, error) {
	if receptionID == "" {
		return false, nil
	}
	var state string
	found, err := catalogID(ctx, tx, `SELECT state FROM receptions WHERE id = ?`, []any{receptionID}, &state)
	if err != nil || !found {
		return false, err
	}
	next, err := reception.Step(reception.State(state), reception.Fail)
	if err != nil {
		return false, nil // ended already: nothing is held for it
	}
	res, err := tx.ExecContext(ctx, `UPDATE receptions SET state = ?, ended = 1, step_at = ?, ended_at = ?, failure = ?
		WHERE id = ? AND state = ?`, string(next), micros(now), micros(now), tenant.FailureWorkGaveUp, receptionID, state)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	ended, err := endReception(ctx, tx, receptionID, next, now)
	if err != nil {
		return false, err
	}
	if err := writeStepEvent(ctx, tx, receptionID, reception.State(state)); err != nil {
		return false, err
	}
	return true, writeImportEvents(ctx, tx, ended)
}

// ReceptionsWithoutWork implements tenant.Pipeline.
func (t *Tenant) ReceptionsWithoutWork(ctx context.Context, page master.Page) ([]tenant.Reception, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.binding()
	after := []any{int64(-1), ""}
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listWithoutWork, binding, 2)
		if err != nil {
			return nil, "", err
		}
		at, err := sqlkit.DecodeCursorInt(f[0])
		if err != nil {
			return nil, "", err
		}
		after = []any{at, f[1]}
	}
	out, err := readRows(ctx, t, "the receptions without work cannot be listed", receptionsWithoutWorkSQL,
		append(after, page.Limit+1), scanReception)
	if err != nil {
		return nil, "", err
	}
	out, last, more := sqlkit.Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, sqlkit.TimeCursor(listWithoutWork, binding, last.Started, last.ID), nil
}

// receptionsQuery returns the statement and arguments of one page of
// ListReceptions. It is put together from fixed fragments by which filters
// are set, so each reads the index named for it in migrations/tenant/0001_baseline.sql; every
// value is a parameter. after is the (started, id) of the last row
// returned, or nil. The empty statement means no reception can match.
func receptionsQuery(filter tenant.ReceptionFilter, after []any, limit int) (string, []any) {
	var where []string
	var args []any
	if filter.Receiver != "" {
		where, args = append(where, "receiver = ?"), append(args, filter.Receiver)
	}
	if filter.StudyUID != "" {
		where, args = append(where, "study_uid = ?"), append(args, filter.StudyUID)
	}
	narrowed, none := filter.NarrowStates()
	if none {
		// Only ended steps were asked for, with in progress: nothing can
		// match, and no statement is run.
		return "", nil
	}
	states := make([]string, len(narrowed))
	for i, st := range narrowed {
		states[i] = string(st)
	}
	if filter.InProgress && len(states) == 0 {
		// The literal matches receptions_in_progress's condition. With
		// steps it is left out: the steps that remain have not ended.
		where = append(where, "ended = 0")
	}
	if !filter.Since.IsZero() {
		where, args = append(where, "started >= ?"), append(args, micros(filter.Since))
	}
	if !filter.Until.IsZero() {
		where, args = append(where, "started < ?"), append(args, micros(filter.Until))
	}
	if after != nil {
		where, args = append(where, "(started, id) < (?, ?)"), append(args, after...)
	}
	const order = " ORDER BY started DESC, id DESC LIMIT ?"
	page := func(where []string) string {
		query := "SELECT " + receptionColumns + " FROM receptions"
		if len(where) > 0 {
			query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
		}
		return query + order
	}
	if len(states) <= 1 {
		if len(states) == 1 {
			where, args = append([]string{"state = ?"}, where...), append([]any{states[0]}, args...)
		}
		return page(where), append(args, limit)
	}
	// Several steps: one page per step, each read from receptions_by_state
	// and bounded by the limit, merged and cut to the limit. A single
	// "state IN (...)" would make SQLite walk the time index and test every
	// reception, which costs as much as the receptions are many when the
	// steps asked for are rare.
	parts := make([]string, len(states))
	var all []any
	for i, s := range states {
		parts[i] = "SELECT * FROM (" + page(append([]string{"state = ?"}, where...)) + ")"
		all = append(append(append(all, s), args...), limit)
	}
	return strings.Join(parts, " UNION ALL ") + order, append(all, limit)
}
