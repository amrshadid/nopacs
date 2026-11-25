package tenant

import (
	"strconv"
	"strings"
	"time"
)

// What a correction does on the replicas of its scans' storages: the copy
// set, the records owed to replicas, and the purge's progress per copy.
//
// A replica holds the same keys as its primary, and every object the
// committed manifest names, a hidden instance too, until the purge of the
// correction that hid it. So a correction reaches each replica as it
// reaches the primary:
//
//   - Its record is put on the primary and on each replica. The publishing
//     transaction owes the record to every replica of the scans' storages
//     (a row each, OweRecord), and the row is cleared once the replica
//     holds the record at the correction's state (MarkRecordWritten); a
//     replica that does not answer keeps its row, and the record is
//     written again later (RecordsOwed). A record owed to a replica keeps
//     the correction's purge from every copy, and a switch to that replica
//     from its swap: a rebuild or a switch that reads the replica alone
//     would not know the correction.
//   - Attaching a replica owes it the record of every committed or purging
//     correction with a scan on its primary, in the attach's transaction;
//     starting and ending a purge owe the record again to every replica, in
//     their own transactions, so a stop before the replicas are written
//     leaves no replica with a stale record.
//   - Its purge removes each object from the primary, then from each
//     replica attached now (CorrectionCopies), at the same key, and records
//     what each storage did, at the switch generation it acted at. A copy
//     that does not answer leaves the object not settled, its other copies'
//     removals recorded (PurgeSettled with OutcomePending, PurgeTarget.Done,
//     each at its generation), and the correction purging with
//     StateReasonStorage, never purged. Before the correction is purged,
//     every key it removed is checked absent on every copy attached then
//     (PurgedTargets), and one found again is removed and recorded.
//   - A purge and a switch exclude each other: a switch is not begun nor
//     swapped while a purge of its storages has an object not settled
//     (BlockerPurgeUnfinished), and a purge's targets are neither read nor
//     settled while a switch of its storages runs. A replica is not
//     attached while a correction on its primary is purging
//     (RefusalAttachPurging): the purge took its copies when it began, and
//     a backfill would copy what it hid. The replicator skips an instance a
//     purging correction hides (ReplicaClaim.Purging), and starting a purge
//     counts a change on every replica's row of its scans being copied, so
//     a copy claimed before cannot end in step.
//   - A replica detached with abandon_purges while a correction counts it
//     has its objects not yet removed recorded unknown and is named in the
//     correction's CopiesAbandoned.

// StateReasonStorage is the state reason of a purge that waits for a copy
// that does not answer, or is still owed the correction's record.
const StateReasonStorage = "storage does not answer"

// StateReasonHeld is the state reason of a purge that waits for a hold.
const StateReasonHeld = "held"

// ReplicaCopy is a replica of a correction's storage.
type ReplicaCopy struct {
	// Replica names the replica; Primary its primary.
	Replica, Primary string
}

// CorrectionCopy is a replica a correction reaches.
type CorrectionCopy struct {
	ReplicaCopy
	// InStep are the Series Instance UIDs of the correction's scans the
	// replica is in step for at the catalog's manifest digest now, in
	// order.
	InStep []string
}

// HoldsScan reports whether the copy is in step for the scan of series.
func (c CorrectionCopy) HoldsScan(series string) bool {
	for _, s := range c.InStep {
		if s == series {
			return true
		}
	}
	return false
}

// RecordOwed is a correction's record a replica is owed.
type RecordOwed struct {
	// Correction and Replica name it.
	Correction, Replica string
	// Since is when it was first owed.
	Since time.Time
}

// MaxRecordsOwed is the most records owed one RecordsOwed answers.
const MaxRecordsOwed = 1000

// ValidateRecordsOwed returns an Invalid error unless a page of records
// owed can be read after the pair given, limit at a time.
func ValidateRecordsOwed(after RecordOwed, limit int) error {
	if limit < 1 || limit > MaxRecordsOwed {
		return invalid("a page of records owed is 1 to " + strconv.Itoa(MaxRecordsOwed))
	}
	if after.Correction == "" && after.Replica == "" {
		return nil
	}
	if err := ValidateCorrectionID(after.Correction); err != nil {
		return err
	}
	return ValidateName("storage", after.Replica)
}

// ValidatePurgeNote returns an Invalid error unless why is a state reason
// NotePurge sets: one of the purge's, or empty.
func ValidatePurgeNote(why string) error {
	if why != "" && why != StateReasonStorage && why != StateReasonHeld {
		return invalid("a purge waits for a storage or a hold")
	}
	return nil
}

// OutcomePending is a target whose removal some of its copies answered and
// another did not: the removals answered are recorded and named in the
// target's Done, and the target stays to settle.
const OutcomePending PurgeOutcome = ""

// CopyDone is a copy whose removal of a target not yet settled is
// recorded: the storage's name and its switch generation then. A name
// stands for a place only at one generation: a switch swaps the places of
// two names, so a removal recorded under a name at another generation was
// made at the place the other name holds now.
type CopyDone struct {
	Storage    string
	Generation int64
}

// ParseCopiesDone parses a target's copies done as a store keeps them:
// name@generation, separated by spaces. A word that does not parse is
// answered as a copy at generation -1, which no storage is at, so the
// target is asked again on every copy rather than taken as removed.
func ParseCopiesDone(s string) []CopyDone {
	var out []CopyDone
	for _, w := range strings.Fields(s) {
		name, gen, ok := strings.Cut(w, "@")
		n, err := strconv.ParseInt(gen, 10, 64)
		if !ok || err != nil || n < 0 {
			out = append(out, CopyDone{Storage: name, Generation: -1})
			continue
		}
		out = append(out, CopyDone{Storage: name, Generation: n})
	}
	return out
}

// JoinCopiesDone returns copies as a store keeps a target's copies done,
// each storage once (its first), in the order first given.
func JoinCopiesDone(copies ...CopyDone) string {
	var out []string
	seen := map[string]bool{}
	for _, c := range copies {
		if c.Storage != "" && !seen[c.Storage] {
			seen[c.Storage] = true
			out = append(out, c.Storage+"@"+strconv.FormatInt(c.Generation, 10))
		}
	}
	return strings.Join(out, " ")
}

// CurrentCopiesDone answers the names of copies whose removal stands at
// the switch generation each storage is at now (generation, by name), or
// none when any one was recorded at another: the place a name held when it
// removed the object is no longer the place it names, so every copy is
// asked again.
func CurrentCopiesDone(copies []CopyDone, generation func(storage string) (int64, error)) ([]string, error) {
	var out []string
	for _, c := range copies {
		g, err := generation(c.Storage)
		if err != nil {
			return nil, err
		}
		if g != c.Generation {
			return nil, nil
		}
		out = append(out, c.Storage)
	}
	return out, nil
}
