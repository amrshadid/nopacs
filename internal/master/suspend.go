package master

import (
	"context"
	"time"
)

// KeySuspension suspends keys: what a restore asks of the master once it
// is loaded. A driver implements it beside Store; a restore runs it with
// no server running.
type KeySuspension interface {
	// SuspendKeys suspends, at now, every key the master holds, revoked
	// included, whatever its row held before (so a key a later backup or a
	// person reinstates cannot be told apart from one this restore found,
	// and no value a backup carries can leave a key working), and writes
	// audit with the count in its details (DetailSuspended), both or
	// neither. It returns how many keys it suspended: every key.
	SuspendKeys(ctx context.Context, now time.Time, audit AuditEntry) (int64, error)
}

// DetailSuspended is the detail SuspendKeys sets to the count of keys it
// suspended.
const DetailSuspended = "suspended"

// earliestSet is what a time that is set reads as when its stored value is
// the zero time.Time: one microsecond past it.
var earliestSet = time.Time{}.Add(time.Microsecond)

// SetTime returns t, a time a driver read from a column that is not NULL,
// such that it is never the zero time: the zero time means "never" (a key
// not revoked, not suspended, that does not expire), so a stored value that
// decodes to it (the microseconds of year 1, which a backup can carry) would
// leave a key working that its row says is revoked or suspended. Such a
// value reads as one microsecond past the zero time, set and long past.
func SetTime(t time.Time) time.Time {
	if t.IsZero() {
		return earliestSet
	}
	return t
}
