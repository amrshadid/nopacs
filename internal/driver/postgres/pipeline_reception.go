package postgres

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
func scanReception(row scanner) (tenant.Reception, error) {
	var r tenant.Reception
	var state, titles, route string
	var started, lastFile, stepAt time.Time
	var endedAt, discardAt *time.Time
	c := &r.Counts
	if err := row.Scan(&r.ID, &r.Receiver, &r.StudyUID, &r.Arrival, &state, &c.FilesReceived, &c.BytesReceived,
		&c.FilesArchived, &c.BytesArchived, &c.FilesDuplicate, &c.BytesDuplicate, &c.FilesQuarantined, &c.BytesQuarantined,
		&c.FilesReleased, &r.SubjectLabel, &r.SessionLabel, &r.Storage, &r.Failure, &started, &lastFile, &stepAt, &endedAt,
		&r.Associations, &titles, &r.CallingAETitlesTruncated, &r.RetryOf, &r.RetryReception, &r.DiscardReason, &discardAt, &route); err != nil {
		return r, err
	}
	r.State = reception.State(state)
	r.Started, r.LastFile, r.StepTime = started.UTC(), lastFile.UTC(), stepAt.UTC()
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

// errNoReception is the NotFound of a reception the tenant does not have.
func errNoReception() error {
	return fault.New(fault.NotFound, "the tenant has no reception with this id")
}

// receptionInTx reads a reception inside a change's transaction, with the
// lock given ("" for none), or NotFound.
func receptionInTx(ctx context.Context, tx pgx.Tx, id, lock string) (tenant.Reception, error) {
	r, err := scanReception(tx.QueryRow(ctx, `SELECT `+receptionColumns+` FROM receptions WHERE id = $1 `+lock, id))
	if isNoRow(err) {
		return r, errNoReception()
	}
	return r, err
}

// openTries bounds the rounds of openReceptionInTx and of a retry's insert.
// Each round that inserts nothing met a row another transaction committed
// meanwhile (the open reception, or the arrival number), so every round makes
// progress; the bound is generous for many receivers opening one study at
// once, and only keeps a broken table from looping forever.
const openTries = 64

// openReceptionInTx finds the reception of the study receiving at the
// receiver, or makes one with id at the next arrival of the study. Two opens at
// once meet at receptions_open, the partial unique index that holds one open
// reception per receiver and study, or at receptions_arrival: the second
// insert waits for the first's commit and then does nothing (ON CONFLICT DO
// NOTHING), and the next round, in statements of its own that see what was
// committed, finds the reception that won or the next free arrival number.
func openReceptionInTx(ctx context.Context, tx pgx.Tx, receiver, studyUID, id string, at time.Time) (tenant.Reception, bool, error) {
	has, err := receiverExists(ctx, tx, receiver)
	if err != nil {
		return tenant.Reception{}, false, err
	}
	if !has {
		return tenant.Reception{}, false, tenant.Precondition("the tenant has no receiver named " + receiver)
	}
	for range openTries {
		r, err := scanReception(tx.QueryRow(ctx, openReceptionSQL, receiver, studyUID))
		if err == nil {
			return r, false, nil
		}
		if !isNoRow(err) {
			return r, false, err
		}
		inserted, err := insertReception(ctx, tx, id, receiver, studyUID, reception.Receiving, "", at)
		if err != nil {
			return tenant.Reception{}, false, err
		}
		if inserted {
			r, err = receptionInTx(ctx, tx, id, "")
			return r, true, err
		}
	}
	return tenant.Reception{}, false, fault.New(fault.Unavailable, "receptions of the study kept opening at the same moment; try again")
}

// insertReception writes a new reception in state at the next arrival of
// its study, and reports whether it did: a row it meets under one of the
// unique keys (its id, the open reception of its receiver and study, the
// arrival number another took meanwhile) leaves nothing written. An id taken
// by another reception is a Conflict, since ids are the caller's to make
// unique, with one exception: two opens at once that carry the same id (a
// sender's association reconnecting, a receiver's call run again) meet at
// the id, and the row they met is the very reception both asked for, open
// at the same receiver for the same study. That is reported as nothing
// written, so the caller's next round reads it as the open reception, as
// it would have had the two not overlapped.
func insertReception(ctx context.Context, tx pgx.Tx, id, receiver, studyUID string, state reception.State, retryOf string, at time.Time) (bool, error) {
	var arrival int
	if err := tx.QueryRow(ctx, nextArrivalSQL, studyUID).Scan(&arrival); err != nil {
		return false, err
	}
	var endedAt any
	if state.Ended() {
		endedAt = micros(at)
	}
	n, err := rowsChanged(tx.Exec(ctx, `INSERT INTO receptions (id, receiver, study_uid, arrival, state, ended,
		files_received, bytes_received, files_archived, bytes_archived, files_duplicate, bytes_duplicate,
		files_quarantined, bytes_quarantined, files_released, subject_label, session_label, storage, failure,
		started, last_file, step_at, ended_at, associations, calling_ae_titles, calling_truncated,
		retry_of, retry_reception, discard_reason, discard_at)
		VALUES ($1, $2, $3, $4, $5, $6, 0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '', $7, $7, $7, $8, 0, '[]', false, $9, '', '', NULL)
		ON CONFLICT DO NOTHING`,
		id, receiver, studyUID, arrival, string(state), state.Ended(), micros(at), endedAt, retryOf))
	if err != nil || n == 1 {
		return n == 1, err
	}
	// A statement of its own, so it sees the row the insert waited for.
	var same bool
	err = tx.QueryRow(ctx, `SELECT receiver = $2 AND study_uid = $3 AND state = 'receiving' FROM receptions WHERE id = $1`,
		id, receiver, studyUID).Scan(&same)
	switch {
	case isNoRow(err):
		return false, nil
	case err != nil:
		return false, err
	case same && state == reception.Receiving:
		return false, nil
	}
	return false, fault.New(fault.Conflict, "a reception with this id exists")
}

// OpenReception implements tenant.Pipeline.
func (t *Tenant) OpenReception(ctx context.Context, receiver, studyUID, id string, at time.Time) (tenant.Reception, bool, error) {
	if err := tenant.ValidateOpen(receiver, studyUID, id, at); err != nil {
		return tenant.Reception{}, false, err
	}
	var out tenant.Reception
	var opened bool
	err := t.pipe(ctx, "the reception cannot be opened", func(tx pgx.Tx) error {
		var err error
		out, opened, err = openReceptionInTx(ctx, tx, receiver, studyUID, id, at)
		if err != nil || !opened {
			return err
		}
		// The reception reaching receiving is its first event, last in the
		// transaction that opens it.
		e, err := tenant.ReceptionStepEvent("", out)
		if err != nil {
			return err
		}
		return writeEvents(ctx, tx, e)
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
		`SELECT `+receptionColumns+` FROM receptions WHERE id = $1`, []any{id}, scanReception)
	if err != nil {
		return tenant.Reception{}, nil, err
	}
	arrivals, err := readRows(ctx, t, "the reception's arrivals cannot be read", arrivalsSQL,
		[]any{r.StudyUID, tenant.MaxArrivals}, func(row scanner) (tenant.Arrival, error) {
			var a tenant.Arrival
			var state string
			var started time.Time
			err := row.Scan(&a.ID, &a.Arrival, &state, &started)
			a.State, a.Started = reception.State(state), started.UTC()
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
	after, err := sqlkit.PageAfterTime(page, listReceptions, binding)
	if err != nil {
		return nil, "", err
	}
	query, args := receptionsQuery(filter, after, page.Limit+1)
	if query == "" {
		return nil, "", t.open()
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
// took it over. The check is an UPDATE of the unit's row with the attempt in
// its condition, so the row stays locked (FOR NO KEY UPDATE) to the end of
// the worker's transaction: a takeover skips it while it is held (a claim
// takes rows with SKIP LOCKED) or comes after and changes the attempt, and a
// write after a takeover finds the condition false. A unit's row is the
// first row any transaction locks (the package documentation's order).
func checkFence(ctx context.Context, tx pgx.Tx, f tenant.WorkFence) error {
	n, err := rowsChanged(tx.Exec(ctx, `UPDATE work SET updated = updated WHERE id = $1 AND attempt = $2 AND state = 'leased'`, f.ID, f.Attempt))
	if err != nil || n == 1 {
		return err
	}
	has, err := exists(ctx, tx, `SELECT 1 FROM work WHERE id = $1`, f.ID)
	if err != nil {
		return err
	}
	if !has {
		return fault.New(fault.NotFound, "the tenant has no unit of work with this id")
	}
	return tenant.LeaseLost()
}

// StepReception implements tenant.Pipeline. The reception's row is read FOR
// NO KEY UPDATE, the lock the step's UPDATE takes, before anything is
// decided, so the state and last file time compared are those no other step
// or batch can change until this one commits; the compare is also in the
// UPDATE's condition. An identify then holds its storage's row FOR SHARE
// (lockStorageShared), after the reception's, in the package's order.
func (t *Tenant) StepReception(ctx context.Context, step tenant.ReceptionStep, hook tenant.StepHook) (tenant.Reception, error) {
	if err := step.Validate(); err != nil {
		return tenant.Reception{}, err
	}
	var out tenant.Reception
	err := t.pipe(ctx, "the reception's step cannot be written", func(tx pgx.Tx) error {
		if step.Fence != nil {
			if err := checkFence(ctx, tx, *step.Fence); err != nil {
				return err
			}
		}
		before, err := receptionInTx(ctx, tx, step.ID, "FOR NO KEY UPDATE")
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
		if before.State != step.From || (!step.LastFile.IsZero() && !before.LastFile.Equal(micros(step.LastFile))) {
			return tenant.StateChanged()
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
		var chosen tenant.ReceptionRoute
		if step.Event == reception.Identify {
			if err := lockStorageShared(ctx, tx, step.Storage); err != nil {
				return err
			}
			subject, session, storage = step.SubjectLabel, step.SessionLabel, step.Storage
			// The routing's choice is kept with the step; a storage a
			// release named stays, so the record says why the storage is
			// what it is.
			chosen = step.Route
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
		n, err := rowsChanged(tx.Exec(ctx, `UPDATE receptions SET state = $1, ended = $2, step_at = $3, ended_at = $4,
			files_duplicate = $5, bytes_duplicate = $6, subject_label = $7, session_label = $8, storage = $9, failure = $10,
			route = $13 WHERE id = $11 AND state = $12`,
			string(next), next.Ended(), micros(step.At), endedAt, c.FilesDuplicate, c.BytesDuplicate,
			subject, session, storage, failure, step.ID, string(step.From), routeText))
		if err != nil {
			return err
		}
		if n != 1 {
			return tenant.StateChanged()
		}
		if step.Event == reception.Identify {
			// The rules the reception was routed by, with the step that
			// keeps the choice: what hit counts and a dry run read.
			if err := sqlkit.RecordRouteMatches(ctx, sendingTx{tx: tx}, sendingDialect, step.ID, before.Receiver, chosen, step.At); err != nil {
				return err
			}
		}
		var ended []tenant.EventRecord
		if next.Ended() {
			if ended, err = endReception(ctx, tx, step.ID, next, step.At); err != nil {
				return err
			}
		}
		after, err := receptionInTx(ctx, tx, step.ID, "")
		if err != nil {
			return err
		}
		// A step that reached done forwards the reception to each sender
		// its receiver names, in this transaction (tenant.Sending); each
		// sender's row is held after the reception's, in the package's
		// order.
		forwards, err := forwardInTx(ctx, tx, after)
		if err != nil {
			return err
		}
		if hook != nil {
			if err := hook(ctx, before, after); err != nil {
				return err
			}
		}
		// The step's event, whatever hook the caller gave: a step is never
		// taken without it. It is written after the hook, last in the
		// transaction, so the feed's lock is held only to the commit.
		e, err := tenant.ReceptionStepEvent(before.State, after)
		if err != nil {
			return err
		}
		if err := writeEvents(ctx, tx, append(append([]tenant.EventRecord{e}, forwards...), ended...)...); err != nil {
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

// giveUp takes a reception whose unit of work was held as failed through the
// Fail step, in the transaction that held the unit, with
// tenant.FailureWorkGaveUp, and releases its claims, so no claim outlives
// every worker that could end its reception. The reception's row is read
// FOR NO KEY UPDATE first, after the unit's (the package's order). A
// reception that has ended, or is not there, is left as it is, and it
// reports whether the reception failed. The step is
// the core's (reception.Step).
func giveUp(ctx context.Context, tx pgx.Tx, receptionID string, now time.Time) (bool, error) {
	if receptionID == "" {
		return false, nil
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM receptions WHERE id = $1 FOR NO KEY UPDATE`, receptionID).Scan(&state)
	if isNoRow(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next, refused := reception.Step(reception.State(state), reception.Fail)
	if refused != nil {
		// Ended already: nothing is held for it, and the unit is still held
		// as failed.
		return false, nil
	}
	n, err := rowsChanged(tx.Exec(ctx, `UPDATE receptions SET state = $1, ended = $2, step_at = $3, ended_at = $3, failure = $4
		WHERE id = $5 AND state = $6`, string(next), next.Ended(), micros(now), tenant.FailureWorkGaveUp, receptionID, state))
	if err != nil || n != 1 {
		return false, err
	}
	ended, err := endReception(ctx, tx, receptionID, next, now)
	if err != nil {
		return false, err
	}
	// The step's event, last: both callers only read after giving up.
	e, err := stepEventInTx(ctx, tx, receptionID, reception.State(state))
	if err != nil {
		return false, err
	}
	return true, writeEvents(ctx, tx, append([]tenant.EventRecord{e}, ended...)...)
}

// ReceptionsWithoutWork implements tenant.Pipeline.
func (t *Tenant) ReceptionsWithoutWork(ctx context.Context, page master.Page) ([]tenant.Reception, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := t.binding()
	afterAt, afterID := time.UnixMicro(0).UTC(), ""
	if page.Cursor != "" {
		f, err := sqlkit.DecodeCursor(page.Cursor, listWithoutWork, binding, 2)
		if err != nil {
			return nil, "", err
		}
		at, err := sqlkit.DecodeCursorInt(f[0])
		if err != nil {
			return nil, "", err
		}
		afterAt, afterID = microsTime(at), f[1]
	} else {
		// Before every reception: a start time is after 1970
		// (master.ValidateTime), so the first page starts below it.
		afterAt = afterAt.Add(-time.Microsecond)
	}
	out, err := readRows(ctx, t, "the receptions without work cannot be listed", receptionsWithoutWorkSQL,
		[]any{afterAt, afterID, page.Limit + 1}, scanReception)
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
func receptionsQuery(filter tenant.ReceptionFilter, after *sqlkit.TimePosition, limit int) (string, []any) {
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
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	// where returns the conditions of one part, with the state given ("" for
	// none). Each part numbers its own parameters, so the parts of a union
	// share none and each reads its index with its own values.
	where := func(state string) string {
		var w []string
		if state != "" {
			w = append(w, "state = "+arg(state))
		}
		if filter.Receiver != "" {
			w = append(w, "receiver = "+arg(filter.Receiver))
		}
		if filter.StudyUID != "" {
			w = append(w, "study_uid = "+arg(filter.StudyUID))
		}
		if filter.InProgress && len(states) == 0 {
			// The literal is receptions_in_progress's own condition. With
			// steps it is left out: the steps that remain have not ended.
			w = append(w, "NOT ended")
		}
		if !filter.Since.IsZero() {
			w = append(w, "started >= "+arg(micros(filter.Since)))
		}
		if !filter.Until.IsZero() {
			w = append(w, "started < "+arg(micros(filter.Until)))
		}
		if after != nil {
			w = append(w, "(started, id) < ("+arg(after.At)+", "+arg(after.ID)+")")
		}
		if len(w) == 0 {
			return ""
		}
		return " WHERE " + strings.Join(w, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
	}
	const order = " ORDER BY started DESC, id DESC LIMIT "
	if len(states) <= 1 {
		state := ""
		if len(states) == 1 {
			state = states[0]
		}
		q := "SELECT " + receptionColumns + " FROM receptions" + where(state)
		return q + order + arg(limit), args
	}
	// Several steps: one part per step, each read from its index in order
	// and cut to the limit, then merged and cut again. A single
	// "state IN (...)" reads one index in state order, and the rows of all
	// the steps asked for are then sorted whole, which costs as much as
	// those receptions are many.
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = "(SELECT " + receptionColumns + " FROM receptions" + where(s) + order + arg(limit) + ")"
	}
	return strings.Join(parts, " UNION ALL ") + order + arg(limit), args
}

// releaseClaims removes the claims a reception did not confirm. A held
// instance is never touched.
func releaseClaims(ctx context.Context, tx pgx.Tx, receptionID string) error {
	_, err := tx.Exec(ctx, releaseClaimedSQL, receptionID)
	return err
}

// endReception does what a step to an end does besides writing the
// reception's row: it releases the claims the reception did not confirm,
// gives the import's files still processing in it their outcome
// (tenant.Importing) and, for an end that is not a failure, removes its
// received files (tenant.Pipeline's ReceivedFiles says why), in the step's
// transaction, at now. It returns the events of the import jobs that
// ended, which the step writes last with its own.
func endReception(ctx context.Context, tx pgx.Tx, receptionID string, state reception.State, now time.Time) ([]tenant.EventRecord, error) {
	if err := releaseClaims(ctx, tx, receptionID); err != nil {
		return nil, err
	}
	events, err := importSending().ImportEnded(ctx, sendingTx{tx: tx}, receptionID, state, now)
	if err != nil {
		return nil, err
	}
	if !tenant.KeepsReceivedFiles(state) {
		_, err := tx.Exec(ctx, `DELETE FROM received_files WHERE reception_id = $1`, receptionID)
		return events, err
	}
	return events, nil
}
