package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's event feed (tenant.Events), on the events, event_marks and
// event_checkpoints tables of migrations/tenant/0001_baseline.sql.
//
// Commit order. Changes run at once on Postgres, so an id taken by one
// transaction can commit after an id another took later, and a reader could
// pass an event that commits behind it. A change that writes events takes
// the feed's lock (lockFeed: the row of event_marks, FOR UPDATE) just before
// it writes the first one and holds it to its commit, and writes nothing
// after its events but what it must: the writers of events then commit one
// after another, each takes its ids once the one before has committed, and
// the order of ids is the order of commits. A reader reads in one snapshot
// (readFeed: repeatable read, read only), which then holds every event up to
// some id and none after it. Changes that write no event, such as a batch of
// received files, never take the lock and are not slowed by it.

var _ tenant.Events = (*Tenant)(nil)

// listEvents names the feed in its cursors, apart from every other list.
const listEvents = "tenant-events"

// The feed's statements, named so the plan test reads the very text the
// driver runs.
const (
	// lockFeedSQL takes the feed's lock, held to the commit.
	lockFeedSQL = `SELECT pruned_through FROM event_marks WHERE only_row FOR UPDATE`
	// insertEventSQL writes one event; ON CONFLICT DO NOTHING skips an
	// appended event whose key the feed holds (events_by_key).
	insertEventSQL = `INSERT INTO events (type, at, payload, key) VALUES ($1, $2, $3, $4) ON CONFLICT (key) WHERE key IS NOT NULL DO NOTHING`
	// eventsAfterSQL reads the events after an id: the table's own key.
	eventsAfterSQL = `SELECT id, type, at, payload FROM events WHERE id > $1 ORDER BY id LIMIT $2`
	// eventHighSQL reads the feed's place: the newest id committed, or the
	// mark of what was pruned when every event is gone.
	eventHighSQL = `SELECT greatest(coalesce((SELECT max(id) FROM events), 0), pruned_through), pruned_through FROM event_marks WHERE only_row`
	// lastCheckpointSQL reads the newest checkpoint, if any.
	lastCheckpointSQL = `SELECT at, through FROM event_checkpoints ORDER BY at DESC LIMIT 1`
	// pruneTargetSQL reads the newest event a checkpoint older than a time
	// says was committed by then: what retention may remove.
	pruneTargetSQL = `SELECT coalesce(max(through), -1) FROM event_checkpoints WHERE at < $1`
	// spentCheckpointsSQL removes the checkpoints that can prune nothing
	// more than what was pruned, keeping the newest of them.
	spentCheckpointsSQL = `DELETE FROM event_checkpoints WHERE through <= $1 AND at < (SELECT max(at) FROM event_checkpoints WHERE through <= $1)`
	// pruneStepSQL reads the ids of the oldest events up to one, a step's
	// worth: the table's own key.
	pruneStepSQL = `SELECT id FROM events WHERE id <= $1 ORDER BY id LIMIT $2`
	// incomingBytesSQL sums the bytes of the receptions whose files are in
	// the incoming area: those not ended, and those failed and not retried
	// or discarded.
	incomingBytesSQL = `SELECT coalesce(sum(bytes_received - bytes_quarantined), 0)::bigint FROM receptions
		WHERE NOT ended OR (state = 'failed' AND retry_reception = '' AND discard_reason = '')`
	// stepCountsSQL counts the receptions not ended by step; they are few by
	// nature (receptions_in_progress).
	stepCountsSQL = `SELECT state, count(*) FROM receptions WHERE NOT ended GROUP BY state`
)

// eventTime is how an event's time is stored: in UTC, cut to the
// microseconds Postgres keeps, so that the time read back is the time
// written (Postgres rounds where this cuts) and a key's event compares equal.
func eventTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// lockFeed takes the feed's lock in tx (the file's doc says why), once per
// transaction is enough and more is harmless.
func lockFeed(ctx context.Context, tx pgx.Tx) error {
	var pruned int64
	return tx.QueryRow(ctx, lockFeedSQL).Scan(&pruned)
}

// writeEvent writes e in tx, with key for an appended event or nil for one
// the store writes with its change, and reports whether it was new. The
// caller has taken the feed's lock.
func writeEvent(ctx context.Context, tx pgx.Tx, e tenant.EventRecord, key *string) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, insertEventSQL, string(e.Type()), eventTime(e.Time()), e.Payload(), key)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// writeEvents takes the feed's lock and writes the events a change made,
// last in its transaction.
func writeEvents(ctx context.Context, tx pgx.Tx, events ...tenant.EventRecord) error {
	if len(events) == 0 {
		return nil
	}
	if err := lockFeed(ctx, tx); err != nil {
		return err
	}
	for _, e := range events {
		if _, err := writeEvent(ctx, tx, e, nil); err != nil {
			return err
		}
	}
	return nil
}

// stepEventInTx returns the event of a reception as it is in tx after a
// step from previous (empty for its first).
func stepEventInTx(ctx context.Context, tx pgx.Tx, id string, previous reception.State) (tenant.EventRecord, error) {
	r, err := receptionInTx(ctx, tx, id, "")
	if err != nil {
		return tenant.EventRecord{}, err
	}
	return tenant.ReceptionStepEvent(previous, r)
}

// decisionEventInTx returns the event of a record just decided, with the
// quarantine's totals after it.
func decisionEventInTx(ctx context.Context, tx pgx.Tx, q tenant.QuarantinedFile) (tenant.EventRecord, error) {
	var files, bytes int64
	if err := tx.QueryRow(ctx, `SELECT files, bytes FROM quarantine_totals WHERE only_row`).Scan(&files, &bytes); err != nil {
		return tenant.EventRecord{}, err
	}
	return tenant.QuarantineDecidedEvent(q, files, bytes)
}

// eventCursor returns the cursor after the event with this id. The tenant
// it belongs to is a field of the cursor rather than the cursor's own
// binding, so a cursor of another tenant's feed is told apart from one
// altered or of another list (tenant.ErrForeignCursor).
func (t *Tenant) eventCursor(id int64) string {
	return sqlkit.EncodeCursor(listEvents, "", t.binding(), strconv.FormatInt(id, 10))
}

// eventPosition decodes a feed cursor into the id it is after, or -1 for
// the empty cursor, which reads from the oldest event kept.
func (t *Tenant) eventPosition(cursor string) (int64, error) {
	if cursor == "" {
		return -1, nil
	}
	f, err := sqlkit.DecodeCursor(cursor, listEvents, "", 2)
	if err != nil {
		return 0, err
	}
	if f[0] != t.binding() {
		return 0, tenant.ForeignCursor()
	}
	id, err := sqlkit.DecodeCursorInt(f[1])
	if err != nil || id < 0 {
		return 0, fault.New(fault.Invalid, "the page token is not one this list returned with these arguments")
	}
	return id, nil
}

// readFeed runs fn in one read-only transaction at repeatable read, so
// every query in it reads one snapshot, and ends it before returning.
func (t *Tenant) readFeed(ctx context.Context, message string, fn func(tx pgx.Tx) error) error {
	if err := t.open(); err != nil {
		return err
	}
	tx, err := t.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return t.fail(ctx, err, message)
	}
	defer t.rollback(tx) // A read changes nothing; ending it is all that matters.
	if err := fn(tx); err != nil {
		return t.fail(ctx, err, message)
	}
	return nil
}

// feedHigh reads the feed's place in tx.
func feedHigh(ctx context.Context, tx pgx.Tx) (high, pruned int64, err error) {
	err = tx.QueryRow(ctx, eventHighSQL).Scan(&high, &pruned)
	return high, pruned, err
}

// AppendEvents implements tenant.Events.
func (t *Tenant) AppendEvents(ctx context.Context, events []tenant.EventAppend) (int, error) {
	if err := tenant.ValidateEventAppends(events); err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, t.open()
	}
	var added int
	err := t.pipe(ctx, "the events cannot be written", func(tx pgx.Tx) error {
		added = 0
		if err := lockFeed(ctx, tx); err != nil {
			return err
		}
		for _, e := range events {
			key := e.Key
			fresh, err := writeEvent(ctx, tx, e.Event, &key)
			if err != nil {
				return err
			}
			if fresh {
				added++
				continue
			}
			// The key is held: by the same event, written before (or
			// earlier in this batch), which is skipped; or by another,
			// which is refused rather than kept silently.
			var typ, payload string
			var at time.Time
			if err := tx.QueryRow(ctx, `SELECT type, at, payload FROM events WHERE key = $1`, e.Key).Scan(&typ, &at, &payload); err != nil {
				return err
			}
			if typ != string(e.Event.Type()) || !at.Equal(eventTime(e.Event.Time())) || payload != e.Event.Payload() {
				return tenant.EventKeyReused(e.Key)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// eventsOfType returns the statement that reads the events of one type
// after an id, from events_by_type. The type is in the statement's text, not
// a parameter: one type is most of a feed and another a few events in it,
// and the plan Postgres keeps for a prepared statement after a few runs, one
// for any value, walks the table's key and tests each event, which for a
// rare type reads the feed. Written in, each type has a statement and a plan
// of its own. A type is one word of a closed list, checked by
// tenant.ValidateEventTypes before any statement is made, and quoted here
// all the same.
func eventsOfType(typ tenant.EventType) string {
	return `SELECT id, type, at, payload FROM events WHERE type = ` + quoteLiteral(string(typ)) + ` AND id > $1 ORDER BY id LIMIT $2`
}

// quoteLiteral quotes s as an SQL string constant: in single quotes, each
// one inside doubled (standard_conforming_strings, on in every Postgres the
// driver supports).
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// eventsQuery returns the statement and arguments of a page of events after
// id, of the types given. Several types read one page per type, each from
// events_by_type and bounded by the limit, merged and cut to the limit: a
// single "type = ANY" would walk every id and test each event.
func eventsQuery(types []tenant.EventType, after int64, limit int) (string, []any) {
	args := []any{after, limit}
	switch len(types) {
	case 0:
		return eventsAfterSQL, args
	case 1:
		return eventsOfType(types[0]), args
	}
	parts := make([]string, len(types))
	for i, typ := range types {
		parts[i] = "(" + eventsOfType(typ) + ")"
	}
	return strings.Join(parts, " UNION ALL ") + " ORDER BY id LIMIT $2", args
}

// ReadEvents implements tenant.Events.
func (t *Tenant) ReadEvents(ctx context.Context, after string, types []tenant.EventType, limit int) (tenant.EventPage, error) {
	if err := tenant.ValidateEventRead(types, limit); err != nil {
		return tenant.EventPage{}, err
	}
	pos, err := t.eventPosition(after)
	if err != nil {
		return tenant.EventPage{}, err
	}
	var page tenant.EventPage
	err = t.readFeed(ctx, "the events cannot be read", func(tx pgx.Tx) error {
		high, pruned, err := feedHigh(ctx, tx)
		if err != nil {
			return err
		}
		from := pos
		switch {
		case pos < 0:
			from = pruned
		case pos < pruned, pos > high:
			// Before what was pruned, or after the newest event the feed
			// holds: a database restored from an older backup, whose events
			// at and before the cursor are not those the reader saw.
			return tenant.CursorExpired()
		}
		query, args := eventsQuery(sqlkit.DistinctTypes(types), from, limit)
		rows, err := tx.Query(ctx, query, args...)
		var ids []int64
		unreadable := 0
		events, err := collect(rows, err, func(row scanner) (tenant.Event, error) {
			var id int64
			var at time.Time
			var typ, payload string
			if err := row.Scan(&id, &typ, &at, &payload); err != nil {
				return tenant.Event{}, err
			}
			p, ok := tenant.DecodeEvent(tenant.EventType(typ), payload)
			if !ok {
				unreadable++
			}
			ids = append(ids, id)
			return tenant.Event{Cursor: t.eventCursor(id), Type: tenant.EventType(typ), Time: at.UTC(), Payload: p}, nil
		})
		if err != nil {
			return err
		}
		// A full page ends at its last event; a short one has every event
		// of the snapshot after the cursor that the filter keeps, so the
		// reader is at the snapshot's newest event.
		next := max(from, high)
		if len(events) == limit {
			next = ids[len(ids)-1]
		}
		page = tenant.EventPage{Events: events, Cursor: t.eventCursor(next), Unreadable: unreadable}
		return nil
	})
	if err != nil {
		return tenant.EventPage{}, err
	}
	return page, nil
}

// EventCursor implements tenant.Events.
func (t *Tenant) EventCursor(ctx context.Context) (string, error) {
	var cursor string
	err := t.readFeed(ctx, "the event feed's place cannot be read", func(tx pgx.Tx) error {
		high, _, err := feedHigh(ctx, tx)
		cursor = t.eventCursor(high)
		return err
	})
	return cursor, err
}

// StatusSnapshot implements tenant.Events. Every query runs in one
// snapshot, so the cursor and the counts are of one commit.
func (t *Tenant) StatusSnapshot(ctx context.Context) (tenant.StatusSnapshot, error) {
	var s tenant.StatusSnapshot
	err := t.readFeed(ctx, "the tenant's status cannot be read", func(tx pgx.Tx) error {
		high, _, err := feedHigh(ctx, tx)
		if err != nil {
			return err
		}
		c := &s.Catalog
		if err := tx.QueryRow(ctx, `SELECT subjects, sessions, scans, instances FROM catalog_totals WHERE only_row`).
			Scan(&c.Subjects, &c.Sessions, &c.Scans, &c.Instances); err != nil {
			return err
		}
		var files, bytes, incoming int64
		if err := tx.QueryRow(ctx, `SELECT files, bytes FROM quarantine_totals WHERE only_row`).Scan(&files, &bytes); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, incomingBytesSQL).Scan(&incoming); err != nil {
			return err
		}
		type stepRow struct {
			state string
			n     int64
		}
		rows, err := tx.Query(ctx, stepCountsSQL)
		steps, err := collect(rows, err, func(row scanner) (stepRow, error) {
			var r stepRow
			err := row.Scan(&r.state, &r.n)
			return r, err
		})
		if err != nil {
			return err
		}
		counts := make(map[reception.State]int64, len(steps))
		for _, r := range steps {
			counts[reception.State(r.state)] = r.n
		}
		type storageRow struct {
			storage string
			bytes   int64
		}
		rows, err = tx.Query(ctx, `SELECT storage, bytes FROM storage_totals`)
		list, err := collect(rows, err, func(row scanner) (storageRow, error) {
			var v storageRow
			err := row.Scan(&v.storage, &v.bytes)
			return v, err
		})
		if err != nil {
			return err
		}
		storages := make(map[string]int64, len(list))
		for _, v := range list {
			storages[v.storage] = v.bytes
		}
		jobs, err := jobsInProgress(ctx, tx)
		if err != nil {
			return err
		}
		s = tenant.StatusSnapshot{EventCursor: t.eventCursor(high), Catalog: *c, QuarantinedFiles: files,
			QuarantinedBytes: bytes, InProgress: tenant.InProgressSteps(counts), IncomingBytes: incoming + bytes,
			StorageBytes: storages, JobsInProgress: jobs}
		return nil
	})
	if err != nil {
		return tenant.StatusSnapshot{}, err
	}
	return s, nil
}

// CheckpointEvents implements tenant.Events. It takes the feed's lock, so
// the newest event it records is one no writer can pass, and two
// checkpoints at once are put in order.
func (t *Tenant) CheckpointEvents(ctx context.Context, at time.Time) error {
	if err := tenant.ValidateCheckpoint(at); err != nil {
		return err
	}
	when := eventTime(at)
	return t.pipe(ctx, "the event feed's checkpoint cannot be written", func(tx pgx.Tx) error {
		if err := lockFeed(ctx, tx); err != nil {
			return err
		}
		high, _, err := feedHigh(ctx, tx)
		if err != nil {
			return err
		}
		var lastAt time.Time
		var lastThrough int64
		err = tx.QueryRow(ctx, lastCheckpointSQL).Scan(&lastAt, &lastThrough)
		switch {
		case isNoRow(err):
		case err != nil:
			return err
		case when.Before(lastAt):
			return tenant.Precondition("a checkpoint of the event feed is earlier than the one before it; the clock that records them went back")
		case high == lastThrough:
			// Nothing was committed since: the last checkpoint already
			// says as much, and an idle feed adds no rows.
			return nil
		case when.Equal(lastAt):
			return tenant.Precondition("a checkpoint of the event feed has the time of the one before it")
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_checkpoints (at, through) VALUES ($1, $2)`, when, high)
		return err
	})
}

// PruneEvents implements tenant.Events. The newest checkpoint older than
// before says up to which event to remove; every event up to it is removed
// in order, in transactions of at most PruneStep events, each holding the
// feed's lock while it moves the mark with what it removes.
func (t *Tenant) PruneEvents(ctx context.Context, before time.Time, limit int) (int, bool, error) {
	if err := tenant.ValidatePrune(before, limit); err != nil {
		return 0, false, err
	}
	var target int64
	err := t.readFeed(ctx, "the events cannot be pruned", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, pruneTargetSQL, eventTime(before)).Scan(&target)
	})
	if err != nil || target < 0 {
		return 0, false, err
	}
	removed, more := 0, true
	for removed < limit && more {
		step := min(tenant.PruneStep, limit-removed)
		var n int
		err := t.pipe(ctx, "the events cannot be pruned", func(tx pgx.Tx) error {
			n = 0
			if err := lockFeed(ctx, tx); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, pruneStepSQL, target, step)
			ids, err := collect(rows, err, func(row scanner) (int64, error) {
				var id int64
				err := row.Scan(&id)
				return id, err
			})
			if err != nil {
				return err
			}
			through := target
			if len(ids) > 0 {
				through = ids[len(ids)-1]
				if _, err := tx.Exec(ctx, `DELETE FROM events WHERE id <= $1`, through); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE event_marks SET pruned_through = greatest(pruned_through, $1) WHERE only_row`, through); err != nil {
					return err
				}
			}
			if len(ids) < step {
				// The last step: the checkpoints at or below what is now
				// pruned can remove nothing more.
				if _, err := tx.Exec(ctx, spentCheckpointsSQL, through); err != nil {
					return err
				}
			}
			n = len(ids)
			return nil
		})
		if err != nil {
			return removed, false, err
		}
		removed += n
		more = n == step
	}
	if removed < limit {
		return removed, false, nil
	}
	err = t.readFeed(ctx, "the events cannot be pruned", func(tx pgx.Tx) error {
		var remains bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id <= $1)`, target).Scan(&remains)
		more = remains
		return err
	})
	if err != nil {
		return removed, false, err
	}
	return removed, more, nil
}
