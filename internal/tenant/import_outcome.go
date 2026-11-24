package tenant

import "github.com/amrshadid/nopacs/internal/core/reception"

// The rules of what becomes of an import's files, which a store applies
// in the transactions that decide them (Importing): the change each
// outcome makes to the counts, which outcome each deciding transaction
// gives, and the state a unit's walk ends in. They are here, not in a
// driver, so every store counts alike.

// ImportOutcome is what the pipeline made of a file an import handed in,
// or of one whose reception failed (ImportOutcomeDiscarded,
// ImportOutcomeRetried).
type ImportOutcome string

// The outcomes.
const (
	// ImportOutcomeArchived is a file that became a new instance.
	ImportOutcomeArchived ImportOutcome = "archived"
	// ImportOutcomeAlreadyHeld is a file the tenant held already.
	ImportOutcomeAlreadyHeld ImportOutcome = "already_held"
	// ImportOutcomeQuarantined is a file quarantined, counted by reason.
	ImportOutcomeQuarantined ImportOutcome = "quarantined"
	// ImportOutcomeLost is a file whose copy was gone from the incoming
	// area.
	ImportOutcomeLost ImportOutcome = "lost"
	// ImportOutcomeFailed is a file whose reception failed, or ended with
	// the file still undecided.
	ImportOutcomeFailed ImportOutcome = "failed"
	// ImportOutcomeDiscarded is a failed file whose reception a person
	// discarded.
	ImportOutcomeDiscarded ImportOutcome = "discarded"
	// ImportOutcomeRetried is a failed file moved back to processing, into
	// the reception that runs its reception's files again.
	ImportOutcomeRetried ImportOutcome = "retried"
)

// ImportDelta is a change of an import unit's counts, and of its job's by
// the same amounts. Each change a store makes is one of the functions
// below, so both identities of ImportCounts hold after it.
type ImportDelta struct {
	Found, Skipped, HandedIn, Archived, AlreadyHeld, Quarantined, Lost, Failed, Discarded, Processing, Bytes int64
}

// Add returns d plus e.
func (d ImportDelta) Add(e ImportDelta) ImportDelta {
	return ImportDelta{d.Found + e.Found, d.Skipped + e.Skipped, d.HandedIn + e.HandedIn, d.Archived + e.Archived,
		d.AlreadyHeld + e.AlreadyHeld, d.Quarantined + e.Quarantined, d.Lost + e.Lost, d.Failed + e.Failed,
		d.Discarded + e.Discarded, d.Processing + e.Processing, d.Bytes + e.Bytes}
}

// ImportSkipDelta is the change an entry skipped makes: found and skipped.
func ImportSkipDelta() ImportDelta { return ImportDelta{Found: 1, Skipped: 1} }

// ImportArrivalDelta is the change a file handed in makes: found, handed
// in and its bytes, into processing; or, for a file held as it arrived,
// decided at once with held (ImportHeldOutcome).
func ImportArrivalDelta(size int64, held ImportOutcome) ImportDelta {
	d := ImportDelta{Found: 1, HandedIn: 1, Processing: 1, Bytes: size}
	if held == "" {
		return d
	}
	return d.Add(held.Delta())
}

// Delta returns the change o makes: a file processing moves to its
// outcome, a failed one to discarded or back to processing.
func (o ImportOutcome) Delta() ImportDelta {
	switch o {
	case ImportOutcomeArchived:
		return ImportDelta{Processing: -1, Archived: 1}
	case ImportOutcomeAlreadyHeld:
		return ImportDelta{Processing: -1, AlreadyHeld: 1}
	case ImportOutcomeQuarantined:
		return ImportDelta{Processing: -1, Quarantined: 1}
	case ImportOutcomeLost:
		return ImportDelta{Processing: -1, Lost: 1}
	case ImportOutcomeFailed:
		return ImportDelta{Processing: -1, Failed: 1}
	case ImportOutcomeDiscarded:
		return ImportDelta{Failed: -1, Discarded: 1}
	case ImportOutcomeRetried:
		return ImportDelta{Failed: -1, Processing: 1}
	}
	return ImportDelta{}
}

// Listed returns the kind a file with outcome o is listed as
// (ListImportFiles), or "" for one that is not listed: archived, already
// held, or retried.
func (o ImportOutcome) Listed() ImportFileKind {
	switch o {
	case ImportOutcomeQuarantined:
		return ImportFileQuarantined
	case ImportOutcomeLost:
		return ImportFileLost
	case ImportOutcomeFailed:
		return ImportFileFailed
	case ImportOutcomeDiscarded:
		return ImportFileDiscarded
	}
	return ""
}

// ImportCommitOutcomes returns the outcomes of the n arrivals of one
// instance a commit confirmed, in path order: the first archived, the
// others already held. One instance in two folders is two files and one
// new instance, whatever job handed each in.
func ImportCommitOutcomes(n int) []ImportOutcome {
	out := make([]ImportOutcome, n)
	for i := range out {
		out[i] = ImportOutcomeAlreadyHeld
	}
	if n > 0 {
		out[0] = ImportOutcomeArchived
	}
	return out
}

// ImportHeldOutcome returns the outcome a quarantine record gives the
// arrivals of its file, and the reason they are counted by: lost, with no
// reason, for a copy gone from the incoming area; quarantined by the
// record's reason otherwise.
func ImportHeldOutcome(r QuarantineReason) (ImportOutcome, QuarantineReason) {
	if r == QuarantineIncomingLost {
		return ImportOutcomeLost, ""
	}
	return ImportOutcomeQuarantined, r
}

// ImportEndOutcome returns the outcome a reception's end gives the
// arrivals still processing in it, or "" for a state that is no end:
//
//   - done: already held. A reception done archived, ignored as the same
//     file, or quarantined each received file, and the first and the last
//     are decided where they happen, so what is left was the same file.
//   - failed: failed, for RetryReception or DiscardReception to decide.
//   - quarantined: failed. The pipeline holds every file of a reception
//     before it quarantines it, so none is left; one left anyway has no
//     record to name, and is counted failed rather than claimed held.
func ImportEndOutcome(s reception.State) ImportOutcome {
	switch s {
	case reception.Done:
		return ImportOutcomeAlreadyHeld
	case reception.Failed, reception.Quarantined:
		return ImportOutcomeFailed
	}
	return ""
}

// ImportWalkEnd returns the state a unit's walk ends in: failed when it
// fails (failReason), canceled when its job has ended, done when nothing
// it handed in is processing, walked otherwise (done later, once its last
// file has an outcome).
func ImportWalkEnd(failReason string, jobEnded bool, processing int64) ImportUnitState {
	switch {
	case failReason != "":
		return ImportUnitFailed
	case jobEnded:
		return ImportUnitCanceled
	case processing == 0:
		return ImportUnitDone
	}
	return ImportUnitWalked
}
