package receive

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons an Outcome gives a file an import hands in, besides the
// others: fixed phrases a source compares, safe to log.
const (
	// ReasonTooLarge is a file larger than the receiver's largest file. An
	// import skips it as too large; nothing of it is kept or held.
	ReasonTooLarge = reasonTooLarge
	// ReasonImportEnded is a file handed in to an import job that has
	// ended (tenant.ErrImportEnded): nothing was recorded, the copy landed
	// is let go, and the unit ends its walk.
	ReasonImportEnded = "the import job has ended"
	// ReasonLeaseLost is a file handed in by an import unit whose lease no
	// longer holds (tenant.ErrLeaseLost): nothing was recorded, the copy
	// landed is let go, and the unit stops; the unit's next attempt hands
	// the file in again.
	ReasonLeaseLost = "the import unit's lease was lost"
)

// maxClosedRetries bounds how many times a file an import hands in is
// checked again because the reception it was checked into closed before
// its row was written: each check opens or finds the study's next
// reception, so a second closing in a row is already rare.
const maxClosedRetries = 4

// intakeImport records a file an import unit handed in, landed as l, on
// its own and before it answers: its received file, or its hold, written
// with its arrival imp, the unit's cursor and its counts in one fenced
// transaction (tenant.Importing). A file an import hands in is never put
// in a batch: a lost lease refuses the whole of a batch, and the unit must
// know its file was recorded before it judges the next, since its cursor
// moves only with each record. It answers Received once the row is
// written; otherwise nothing is kept, the copy landed is let go, and the
// unit hands the file in again after its cursor: Failed with
// ReasonImportEnded for a job that has ended, Refused with ReasonLeaseLost
// for a lease lost, and Refused for anything else (the database, a check
// that could not decide).
func (p *Pipeline) intakeImport(ctx context.Context, l landed, imp tenant.ImportArrival) Outcome {
	for range maxClosedRetries {
		d, err := p.check(ctx, l)
		if err != nil {
			p.log.Warn("a file an import handed in could not be checked; it is let go and handed in again", "reason", safe(err))
			return p.letGoImport(l, Outcome{Status: Refused, Reason: reasonUnavailable})
		}
		if d.hold != nil {
			d.hold.Import = &imp
			return p.holdImport(ctx, l, *d.hold)
		}
		d.received.Import = &imp
		out, closed := p.recordImport(ctx, l, *d.received)
		if !closed {
			return out
		}
		p.receptions.drop(d.received.ReceptionID)
	}
	return p.letGoImport(l, Outcome{Status: Refused, Reason: reasonUnavailable})
}

// recordImport writes one received file an import handed in, and reports
// closed when its reception closed first, for the caller to check it
// again. A file the catalog holds under the same UID and checksum, or one
// its reception holds already (the same file in two folders), is still an
// arrival of its own path: its row and its arrival are written, and its
// bytes let go, since nothing will ever be written from them.
func (p *Pipeline) recordImport(ctx context.Context, l landed, f tenant.ReceivedFile) (Outcome, bool) {
	early := p.batch.earlyDuplicates(ctx, []batchItem{{l: l, d: decision{received: &f}}})
	p.at("import.before-add")
	added, err := p.store.AddReceivedFiles(ctx, []tenant.ReceivedFile{f}, p.now(), nil)
	if errors.Is(err, tenant.ErrReceptionClosed) {
		return Outcome{}, true
	}
	if err != nil {
		return p.importRefusal(l, err), false
	}
	p.at("import.after-add")
	p.stats.Recorded.Add(1)
	if added == 0 || early[0] {
		if early[0] {
			p.stats.EarlyDuplicates.Add(1)
		}
		if err := p.area.Remove(l.file.Path); err != nil {
			p.log.Warn("the bytes of a file held already could not be removed; a restart removes them", "reason", safe(err))
			p.batch.commitName(l)
		}
		return Outcome{Status: Received}, false
	}
	p.batch.commitName(l)
	return Outcome{Status: Received}, false
}

// holdImport writes the hold of one file an import handed in, held as it
// arrived (not DICOM, a placing UID missing...), with its arrival.
func (p *Pipeline) holdImport(ctx context.Context, l landed, q tenant.QuarantinedFile) Outcome {
	p.at("import.before-hold")
	written, err := p.store.HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil)
	if err != nil {
		return p.importRefusal(l, err)
	}
	p.wake()
	if written == 0 && !p.batch.heldAsWritten(ctx, q, l.file.Path) {
		// Its record's id is taken by one that does not keep this file:
		// the bytes keep their landed name, which a restart checks again,
		// as the batch does.
		p.log.Error("a held file's record was not written, since its id is taken by another; the file keeps its landed name",
			"record", q.ID)
		return Outcome{Status: Received}
	}
	p.stats.Held.Add(1)
	if !q.ContentKept {
		if err := p.area.Remove(l.file.Path); err != nil {
			p.log.Warn("the bytes of a file held without them could not be removed; a restart removes them", "reason", safe(err))
		}
		return Outcome{Status: Received}
	}
	p.batch.commitName(l)
	return Outcome{Status: Received}
}

// importRefusal returns the answer to a file whose record the store
// refused, and lets its landed copy go: nothing of it was recorded.
func (p *Pipeline) importRefusal(l landed, err error) Outcome {
	switch {
	case errors.Is(err, tenant.ErrImportEnded):
		return p.letGoImport(l, Outcome{Status: Failed, Reason: ReasonImportEnded})
	case errors.Is(err, tenant.ErrLeaseLost):
		return p.letGoImport(l, Outcome{Status: Refused, Reason: ReasonLeaseLost})
	}
	p.log.Warn("a file an import handed in could not be recorded; it is let go and handed in again", "reason", safe(err))
	p.stats.Refused.Add(1)
	return p.letGoImport(l, Outcome{Status: Refused, Reason: reasonUnavailable})
}

// letGoImport removes the landed copy of a file an import handed in that
// was not recorded, and returns out. A copy that cannot be removed now is
// let go by the next start (Recover), which never records a landed file of
// an import receiver.
func (p *Pipeline) letGoImport(l landed, out Outcome) Outcome {
	if err := p.area.Remove(l.file.Path); err != nil {
		p.log.Warn("the landed copy of a file an import handed in could not be removed; a restart removes it", "reason", safe(err))
	}
	return out
}
