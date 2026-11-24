package master

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
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

// KeyReinstatement lets suspended keys work again: the way back from the
// suspension a restore leaves on every key (KeySuspension), after a person
// has reviewed which keys deserve it. Store includes it, so the server
// serves it on any master.
type KeyReinstatement interface {
	// ReinstateKeys clears the suspension of the keys named by ids, or of
	// every suspended key when all is true, in one transaction, and returns
	// the keys it reinstated (with ids only, ordered by id, without their
	// hash) and how many.
	//
	// The request is checked first (ValidateReinstatement): only the local
	// operator may make it, Denied for any other actor, since every key may
	// be suspended and a key, however high its grants, must not lift its
	// own suspension; ids and all together, or neither, more than
	// MaxReinstateKeys ids, or an id that is not a key id (ValidateKeyRef,
	// which refuses a whole key given as an id without repeating it), is
	// Invalid. An id
	// named twice counts once. Every id named must be a key the master
	// holds, or the call is NotFound and changes nothing.
	//
	// Only a key that is suspended changes; a key named that is not is
	// left as it is, not returned and not audited, so asking again
	// changes nothing. A revoked or expired key has its suspension cleared
	// like any other and stays refused for what it is: the suspension is
	// one more reason to refuse a key, never one that hides the others.
	//
	// One audit entry is written per key reinstated, made from audit by
	// ReinstatedEntry (its own id, the key's id as its object), with the
	// change or not at all; nothing reinstated writes no entry.
	ReinstateKeys(ctx context.Context, ids []string, all bool, audit AuditEntry, by Actor) ([]Key, int64, error)
}

// MaxReinstateKeys bounds the ids one ReinstateKeys names, as the API
// contract does: a longer review asks for all, or is made in parts.
const MaxReinstateKeys = 1000

// reinstateIndexDigits is the room ReinstatedEntry's suffix may take after
// audit's id: a "-" and up to ten digits, far beyond the keys any master
// holds.
const reinstateIndexDigits = 11

// ValidateReinstatement returns ids sorted by their bytes with each id once,
// or an error, for ReinstateKeys: Denied unless by is the local operator;
// Invalid for ids and all together or neither, more than MaxReinstateKeys
// ids, an id that is not a key id (ValidateKeyRef), or an audit entry that
// cannot be stored
// once ReinstatedEntry has made its id and object. A driver calls it before
// it reads or writes anything.
func ValidateReinstatement(ids []string, all bool, audit AuditEntry, by Actor) ([]string, error) {
	if err := by.Validate(); err != nil {
		return nil, err
	}
	if !by.IsLocalOperator() {
		return nil, fault.New(fault.Denied, "only the local operator reinstates keys: any key may be suspended, and none lifts its own suspension")
	}
	switch {
	case all && len(ids) > 0:
		return nil, invalid("keys are reinstated by id or all of them, not both")
	case !all && len(ids) == 0:
		return nil, invalid("name the keys to reinstate, or ask for all of them")
	case len(ids) > MaxReinstateKeys:
		return nil, invalid("at most " + strconv.Itoa(MaxReinstateKeys) + " keys are reinstated by id at once")
	}
	for _, id := range ids {
		if err := ValidateKeyRef("key id", id); err != nil {
			return nil, err
		}
	}
	if err := audit.Validate(); err != nil {
		return nil, err
	}
	if len(audit.ID) > MaxIDLength-reinstateIndexDigits {
		return nil, invalid("the audit entry id of a reinstatement is at most " + strconv.Itoa(MaxIDLength-reinstateIndexDigits) + " bytes, to leave room for each key's entry")
	}
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	return slices.Compact(sorted), nil
}

// ReinstatedEntry returns the audit entry ReinstateKeys writes for the n-th
// key it reinstated (from 1, in the order of the keys' ids): audit with the
// id audit.ID, a "-" and n, so each key's entry has its own id and all of
// them read as one request's, and the key's id as its object. The copy has
// its own details map.
func ReinstatedEntry(audit AuditEntry, n int, keyID string) AuditEntry {
	e := audit.WithDetails()
	e.ID = audit.ID + "-" + strconv.Itoa(n)
	e.Object = keyID
	return e
}

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
