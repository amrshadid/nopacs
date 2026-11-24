// Package events is the server's live feed of tenants' events: it lets a
// stream follow one tenant's event feed, or every tenant's, as events are
// committed, without the server holding the events for it.
//
// A tenant's events are rows of its database, written in the transaction of
// the change they report and read back in commit order from a cursor
// (tenant.Events). This package adds what makes that a live stream:
//
//   - a Broadcaster, one per server, that a writer tells after a commit that
//     wrote an event (Wake), and that wakes every watch of that tenant. A
//     watch is a cursor, a filter and a wake signal of one slot: a wake that
//     finds the slot full is folded into it, so the server keeps no queue of
//     events per watch, and a watch that does not read costs a few words
//     whatever is written meanwhile;
//   - a timer, one second by default, that wakes every watch anyway, which
//     covers a wake missed and events another process wrote;
//   - the loop of a watch (Broadcaster.Watch): read a page from its cursor,
//     deliver each event to the caller's send function, which may block
//     (that is the stream's back-pressure, and while it blocks nothing
//     accumulates), and when a read answers nothing, wait for a wake, the
//     timer or the end of the context, sending a keep-alive with the cursor
//     when nothing was sent for the keep-alive interval (15 seconds by
//     default). No read transaction is held between reads;
//   - the stream of every tenant (Broadcaster.WatchAll): a cursor per tenant,
//     kept for as long as the stream lives, read through a few readers the
//     stream owns, each read bounded by a deadline, over an interface that
//     lists tenants and gives each one's reader (Tenants). A tenant whose
//     database fails or does not answer in time is skipped, named in the
//     keep-alives, and tried again after a wait; a tenant created joins from
//     its first event; a tenant that leaves the list is paused and goes on
//     from its cursor if it comes back; the opening keep-alive names every
//     tenant followed and a later one those whose cursor changed, so a
//     client that keeps the map they make resumes a stream from it losing
//     no tenant's events.
//
// The bounds the API contract states are kept here: a most of watches per
// caller, per tenant and in all, refused beyond with ErrTooManyWatches.
//
// # What this package must never do
//
// It must never import a database driver, the registry, or anything that
// opens a network connection: it is handed readers through Reader and
// Tenants, so it is tested with fakes and serves any database type. It
// holds no event in memory beyond the page a watch is delivering, keeps no
// clock of its own (time comes through Clock), keeps no global state, and
// starts no goroutine it does not end: the timer's goroutine belongs to the
// Broadcaster and ends with Close, each watch runs in its caller's
// goroutine, and the readers of a stream of every tenant end before it
// returns. A reader it is handed must return when its context ends: that is
// how a read that does not answer is given up. Nothing about a patient reaches it: events carry the archive's
// labels, ids and counts only.
package events
