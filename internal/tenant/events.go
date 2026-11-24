package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Events is a tenant's event feed: one row for each thing that happened in
// the tenant that a system outside wants to follow, written in the
// transaction of the change it reports and read back in the order of the
// commits that wrote them. A driver that keeps a feed implements it beside
// Pipeline and proves it with the contract suite's event groups
// (tenanttest.Run, its event groups); once every driver does, it becomes part of Store.
//
// # Where events are written
//
// An event is written in the same transaction as its change, or not at all:
// a change that is undone takes its event with it, and an event is never
// written for a change that did not commit. A driver that implements Events
// writes these itself, inside the operations that make the change, with
// the operation's own time:
//
//   - OpenReception, when it opens a reception, and ReleaseQuarantined, when
//     its release opens one: the reception reaching receiving;
//   - StepReception: the step, every time, whatever hook the caller gives;
//     the hook runs after the event is written, in the same transaction, so
//     a hook that fails undoes both. A caller cannot take a step without its
//     event on a store that keeps a feed. A batch run again writes the event
//     again in the new transaction, and the write of the run that was undone
//     is undone with it: a step is recorded once however many times its
//     batch runs;
//   - RetryReception: the new reception reaching complete, with RetryOf;
//   - FailWork and ClaimWork, when a unit held as failed fails its
//     reception: the reception reaching failed;
//   - HoldFiles: one event per new record, with the quarantine's totals
//     after it; the time is the record's quarantine time, since HoldFiles
//     takes no other;
//   - ReleaseQuarantined and RejectQuarantined: one event per decision, with
//     the totals after it. A file already decided changes nothing and makes
//     no event;
//   - a job's changes, each its state reached or, at most once in
//     ProgressEventInterval, its counts, as Sending lists them; a forward's
//     jobs with the step that makes them.
//
// A reception step's event names the step left as well as the step reached
// (ReceptionStepReached.PreviousStep), so a client can keep the receptions in
// progress by step from a status and the events after its cursor.
//
// DiscardReception writes no event: the contract has no event for a discard
// (the reception stays failed), so a client that follows a status learns of
// the incoming bytes it freed only from a new status.
//
// What the server reports that is not a row of the pipeline (a failed
// association, a receiver's health, a storage's space) it writes with
// AppendEvents, which refuses the pipeline's own types: those are written
// with their change, never apart from it.
//
// # Reading never fails on what is stored
//
// Writing is strict and reading is tolerant. Every event is written through
// NewEvent, which validates it. A stored payload is read back ignoring fields
// this version does not know, so a later version may add fields to a type; a
// row that cannot be read back at all (not JSON, a value outside this
// version's lists, a field the type requires missing) is answered with its
// type, time and cursor and no payload, exactly as a type this version does
// not know is, and counted on its page (EventPage.Unreadable). A read never
// fails, and a cursor never stops, because of one row.
//
// # What a driver guarantees
//
// Every driver gives these properties, which the contract suite tests:
//
//   - a reader that resumes from a cursor it was given never misses an
//     event and never reads one twice: every event committed after the
//     point the cursor names is answered by a later read from it, once,
//     filtered or not;
//   - an event after a status snapshot's cursor is a change that snapshot's
//     counts do not hold, and an event at or before it is one they do;
//   - a cursor that names a point before what was pruned, or after the
//     newest event the feed holds (a database restored from an older
//     backup), is refused with ErrCursorExpired, never answered from another
//     point; a cursor of another tenant's feed holds ErrForeignCursor;
//   - pruning never removes an event that a reader whose cursor is at or
//     past the pruning's mark has not been offered: what is pruned is always
//     a prefix of the feed, in the feed's order.
//
// The SQLite driver has one writer, which runs one transaction at a time
// (sqlite.org/isolation.html), and takes ids that only grow (AUTOINCREMENT,
// sqlite.org/autoinc.html), so the order of ids is the order of commits; a
// cursor is one id, a read answers the ids after it from one snapshot, and
// the snapshot's newest id is a point that holds every commit before it and
// none after. A transaction undone and run again cannot break that: what it
// wrote, its ids included, is gone with it, no reader saw it, and the run
// again takes ids after every committed one.
//
// A driver on a database with several writers has more to solve, and the
// port prescribes no recipe. An id is taken when a row is written, not when
// its transaction commits, so a reader that follows ids passes an event whose
// transaction commits late (A takes id 10, B takes 11, B commits, a reader
// moves past 11, A commits, 10 is never answered: the suite's case LateCommit
// fails on such a driver). A snapshot on such a database is not one point in
// the order of ids but a bound and a set of transactions in progress, so a
// cursor of one position does not divide the feed the way a status needs; a
// page's cursor must not pass a transaction that is still running; pruning
// and its mark must follow the order the feed is read in, not the order of
// ids; and a bound taken from the oldest transaction running is shared by the
// whole database server, so one idle or forgotten transaction anywhere holds
// every tenant's feed back. The known families of solution are: ordering
// events by their transaction and carrying the reader's snapshot (its bound
// and the transactions it saw running) in the cursor; or making a tenant's
// event writers take turns, so that the order of ids is again the order of
// commits. Each costs something the driver's own design must state.
//
// # Cursors and retention
//
// A cursor is opaque, bound to the tenant and to the feed. A filtered read
// moves its cursor past the events its filter left out, so a filtered reader
// never reads them again, and every page carries the cursor to resume from,
// an empty one included. The type filter is not part of a cursor, so a
// reader may resume with another filter.
//
// Retention reads no event's own time: times come from callers' clocks, and
// one event dated far ahead must not keep every event after it forever, nor
// a run of wrong dates stall the pruning. The server records checkpoints
// with its own clock ("at this time the newest event was this one",
// CheckpointEvents), and pruning removes every event up to the newest
// checkpoint older than the retention period (PruneEvents). The store knows
// what was pruned by a mark, moved in the transaction that prunes.
type Events interface {
	// AppendEvents writes up to MaxEventBatch events the server reports,
	// all or none, and returns how many were new. Each has a key the
	// caller makes (master.ValidateID); an event whose key the feed holds
	// with the same type, time and payload is skipped, so a batch written
	// again after a failure writes nothing twice while its events are kept,
	// and a key held with another event, or given twice in one batch with
	// two events, is a Conflict holding ErrEventKeyReused. Only the
	// server's types are accepted (ServerEventType); a pipeline type is
	// Invalid.
	AppendEvents(ctx context.Context, events []EventAppend) (int, error)
	// ReadEvents returns up to limit (1 to MaxEventPage) events after the
	// cursor, in commit order, of the types given (all types when none; at
	// most MaxEventTypes, each one EventTypes lists), with the cursor to
	// resume from. An empty after reads from the oldest event kept. A
	// cursor before what was pruned or after the newest event holds
	// ErrCursorExpired; a cursor of another tenant's feed holds
	// ErrForeignCursor; any other cursor not made by this feed is Invalid.
	ReadEvents(ctx context.Context, after string, types []EventType, limit int) (EventPage, error)
	// EventCursor returns the cursor after the newest event committed: a
	// read from it answers the events committed later, which is where a
	// reader that starts "from now" starts.
	EventCursor(ctx context.Context) (string, error)
	// StatusSnapshot reads, in one snapshot of the database, the cursor of
	// the feed and the counts a status reports: an event after the cursor
	// is a change the counts do not hold, and one at or before it is one
	// they do.
	StatusSnapshot(ctx context.Context) (StatusSnapshot, error)
	// CheckpointEvents records that at the time at the newest event
	// committed was the one it is now, for PruneEvents to measure an
	// event's age by. A checkpoint is written only when events were
	// committed since the last one, so an idle feed adds none. One earlier
	// than the newest checkpoint is a precondition failure: the clock that
	// records them went back, and an age measured by it would prune too
	// soon.
	CheckpointEvents(ctx context.Context, at time.Time) error
	// PruneEvents removes every event up to the newest checkpoint whose
	// time is before before, in the feed's order, at most limit (1 to
	// MaxPruneBatch), and returns how many it removed and whether more are
	// due. No event's own time is read. It works in transactions of at
	// most PruneStep events, each moving the feed's mark of what was pruned
	// with what it removes, so the writer is never held for long and a
	// reader whose cursor it passed is answered ErrCursorExpired on its next
	// read, never a gap. A checkpoint that can prune nothing more is
	// removed with the last events it covered.
	PruneEvents(ctx context.Context, before time.Time, limit int) (removed int, more bool, err error)
}

// The bounds of the feed's operations.
const (
	// MaxEventBatch is the most events one AppendEvents writes.
	MaxEventBatch = 1000
	// MaxEventPage is the most events one ReadEvents answers.
	MaxEventPage = 1000
	// MaxEventTypes is the most types a read's filter names, the
	// contract's 16.
	MaxEventTypes = 16
	// MaxPruneBatch is the most events one PruneEvents removes.
	MaxPruneBatch = 10000
	// PruneStep is the most events one transaction of PruneEvents removes.
	// Removing an appended event (with its key) costs about 50 to 80
	// microseconds with ten million kept, so a step holds the writer some
	// tens of milliseconds at most: measured on a laptop with ten million
	// kept, a change made beside a pruning waited at most 68 ms, its own
	// synced commit included (85 ms with steps of 500).
	PruneStep = 200
	// CheckpointInterval is how often the server records a checkpoint of
	// each tenant's feed, and so how close to its retention period an
	// event is pruned.
	CheckpointInterval = time.Hour
	// DefaultEventRetention is how long events are kept when the setting
	// events.retention is not set, the contract's 90 days.
	DefaultEventRetention = 90 * 24 * time.Hour
)

// ErrCursorExpired is in the chain of a read whose cursor the feed can no
// longer answer from: it points before the oldest event kept (events after
// it were pruned), or after the newest event the feed holds (the database
// was restored from an older backup, so the events at and before it are not
// the ones the reader saw). Either way the events after it can no longer all
// be answered, and the reader does the same thing: it reads a status again
// and follows from its cursor. Its kind is fault.OutOfRange: the cursor was
// well formed and this feed's own, and sending it again fails again. The
// sentinel names this one case of that kind, which a watch tells apart from
// a feed that fails (package events).
var ErrCursorExpired = errors.New("the feed can no longer answer from this cursor")

// CursorExpired returns the refusal of a read whose cursor the feed can no
// longer answer from.
func CursorExpired() error {
	return fault.Wrap(fault.OutOfRange, ErrCursorExpired,
		"the cursor is outside the events the feed keeps; read the status again and watch from its event cursor")
}

// ErrForeignCursor is in the chain of a read whose cursor was made by the
// feed of another tenant: as of a tenant removed and created again under the
// same name, which is another tenant with its own feed. Its kind is Invalid.
// A stream that follows tenants by name starts such a tenant from its first
// event.
var ErrForeignCursor = errors.New("the cursor was made by another tenant's feed")

// ForeignCursor returns the refusal of a cursor of another tenant's feed.
func ForeignCursor() error {
	return fault.Wrap(fault.Invalid, ErrForeignCursor, "the cursor belongs to another tenant's event feed")
}

// ErrEventKeyReused is in the chain of an append whose key the feed holds
// with another event, or that a batch gives to two events. Its kind is
// Conflict.
var ErrEventKeyReused = errors.New("the event key is held by another event")

// EventKeyReused returns the refusal of an append that reuses key.
func EventKeyReused(key string) error {
	return fault.Wrap(fault.Conflict, ErrEventKeyReused, "the event key "+key+" was appended with another event")
}

// EventAppend is one event the server reports, with the key that makes its
// append idempotent.
type EventAppend struct {
	// Key names the event among those the server appends, made by the
	// caller (master.ValidateID).
	Key string
	// Event is the event, made by NewEvent.
	Event EventRecord
}

// Event is one event as a read answers it.
type Event struct {
	// Cursor is its place in the feed: a read from it answers the events
	// after this one.
	Cursor string
	// Type is its type.
	Type EventType
	// Time is the time of the change it reports, as its writer gave it.
	Time time.Time
	// Payload is what happened, of the type Type names; nil for a type
	// this version does not know (written by a later one) and for a row
	// that cannot be read back (EventPage.Unreadable), which a reader
	// skips, taking its cursor.
	Payload EventPayload
}

// EventPage is a page of events and the cursor after it.
type EventPage struct {
	// Events are the events read, in commit order.
	Events []Event
	// Cursor is where the next read starts: after the last event answered,
	// or after the events the filter left out, whichever is later. It is
	// set whether or not the page has events.
	Cursor string
	// Unreadable counts the events of the page of a type this version
	// knows whose payload could not be read back, answered with no
	// payload.
	Unreadable int
}

// StepCount is how many receptions are in one step.
type StepCount struct {
	// Step is the step.
	Step reception.State
	// Receptions counts the tenant's receptions in it.
	Receptions int64
}

// StatusSnapshot is what a status reads from the tenant's database, all of
// it in one snapshot.
type StatusSnapshot struct {
	// EventCursor is the feed's place in the snapshot: a read from it
	// answers exactly the events whose changes the counts do not hold.
	EventCursor string
	// Catalog are the catalog's running totals.
	Catalog CatalogCounts
	// QuarantinedFiles and QuarantinedBytes are the quarantine's totals:
	// files held, and the bytes kept for them.
	QuarantinedFiles, QuarantinedBytes int64
	// InProgress are the receptions that have not ended, one entry per
	// step that has any, in the order of reception.States.
	InProgress []StepCount
	// IncomingBytes are the bytes the tenant holds in the incoming area:
	// the received bytes of each reception that has not ended, or that
	// failed and was neither retried nor discarded, less what it
	// quarantined; and the quarantine's held bytes.
	IncomingBytes int64
	// StorageBytes are the bytes of the instances the catalog holds on each
	// storage, by name; a storage that holds none is left out.
	StorageBytes map[string]int64
	// JobsInProgress are the jobs that have not ended, one entry per state
	// that has any, in the contract's order (InProgressJobs).
	JobsInProgress []JobStateCount
}
