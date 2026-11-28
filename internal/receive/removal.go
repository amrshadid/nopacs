package receive

import (
	"context"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// kindRemoval is the kind of the unit that removes bytes from the incoming
// area after a person's decision (or a purge) recorded that nothing keeps
// them. The unit lives in the tenant's database, so a removal owed while no
// pipeline runs, or one that failed, survives a restart and is not limited
// by what memory holds.
const kindRemoval = "incoming-removal"

// The kinds of removal, each named by the decision that says its bytes may
// go.
const (
	removeRejected  = "rejected"
	removeDiscarded = "discarded"
	removeWithdrawn = "withdrawn"
)

// Removal is one removal of bytes from the incoming area, owed after a
// decision the database recorded. Make one with RejectedRemoval,
// DiscardedRemoval or WithdrawnRemoval and queue it with QueueRemoval.
type Removal struct {
	kind, ref, path string
}

// RejectedRemoval is the removal of the bytes at path of the quarantined
// file record recordID, whose rejection the store recorded. The bytes are
// removed only if the record is rejected and, when it kept a plain checksum,
// the file at path has it.
func RejectedRemoval(recordID, path string) Removal {
	return Removal{kind: removeRejected, ref: recordID, path: path}
}

// DiscardedRemoval is the removal of the bytes of the files of the
// reception receptionID, whose discard the store recorded. The files are
// the ones the store lists for the discarded reception
// (tenant.Pipeline.DiscardedFiles).
func DiscardedRemoval(receptionID string) Removal {
	return Removal{kind: removeDiscarded, ref: receptionID}
}

// WithdrawnRemoval is the removal of the bytes at path of a held file whose
// record a purging delete correction cleared (ClearWithdrawn): only while
// the correction named is a delete.
func WithdrawnRemoval(correctionID, path string) Removal {
	return Removal{kind: removeWithdrawn, ref: correctionID, path: path}
}

// subject is what the unit keeps: the kind, the decision's id, and the
// path, separated by a space, which none of the three holds.
func (r Removal) subject() string {
	if r.path == "" {
		return r.kind + " " + r.ref
	}
	return r.kind + " " + r.ref + " " + r.path
}

// validate returns an Invalid error unless r names a decision by a valid id
// and, for the kinds that take one, a path of the incoming area: a unit
// queued with anything else would only be dropped when a worker met it.
func (r Removal) validate() error {
	switch r.kind {
	case removeRejected, removeWithdrawn:
		if _, _, err := incoming.ParsePath(r.path); err != nil {
			return fault.New(fault.Invalid, "the path of a removal is not one of the incoming area")
		}
	case removeDiscarded:
		if r.path != "" {
			return fault.New(fault.Invalid, "a discarded reception's removal takes no path")
		}
	default:
		return fault.New(fault.Invalid, "the removal is of no known kind")
	}
	if r.kind == removeWithdrawn {
		return tenant.ValidateCorrectionID(r.ref)
	}
	return master.ValidateID("removal decision id", r.ref)
}

// parseRemoval reads a unit's subject back.
func parseRemoval(subject string) (Removal, error) {
	parts := strings.Split(subject, " ")
	switch {
	case len(parts) == 2 && parts[0] == removeDiscarded:
		return Removal{kind: parts[0], ref: parts[1]}, nil
	case len(parts) == 3 && (parts[0] == removeRejected || parts[0] == removeWithdrawn):
		return Removal{kind: parts[0], ref: parts[1], path: parts[2]}, nil
	}
	return Removal{}, fault.New(fault.Internal, "a removal unit's subject is not of a known form")
}

// unit returns the unit that does r, due now. Its unique key is its subject,
// so queuing the same removal twice queues it once.
func (r Removal) unit(now time.Time) tenant.Work {
	s := r.subject()
	return tenant.Work{ID: newID(), Kind: kindRemoval, Subject: s, UniqueKey: kindRemoval + ":" + s, Due: now}
}

// QueueRemoval queues r in the tenant's database as a unit of work, which
// the tenant's reception pipeline runs once it runs: at once when it
// already does. It is the one way a removal after a decision is owed, so a
// stop, a restart or a pipeline that cannot start loses none of them, and
// none is dropped for being many. A crash between the decision and this call
// leaves bytes no row keeps, which the next start's Recover removes.
//
// It returns the unit, which is the one already queued when the same removal
// was.
func QueueRemoval(ctx context.Context, store tenant.Pipeline, r Removal, now time.Time) (tenant.Work, error) {
	if err := r.validate(); err != nil {
		return tenant.Work{}, err
	}
	now = now.UTC().Truncate(time.Microsecond)
	unit, _, err := store.EnqueueWork(ctx, r.unit(now), now)
	return unit, err
}

// WakeWork asks an idle worker to look for work now, so a unit queued from
// outside the pipeline (QueueRemoval) runs without waiting for the poll.
func (p *Pipeline) WakeWork() { p.wakeWorkers() }

// removal runs a removal unit: it reads the decision the unit names back
// from the database and removes the bytes only if that decision stands. A
// decision that does not stand (the record is not rejected, the reception
// not discarded, the bytes are not the ones rejected) removes nothing and
// ends the unit, since a retry would find the same: nothing is deleted
// without a recorded decision, and what no row keeps is the next start's
// Recover to remove. A database or disk that does not answer fails the
// attempt, which is tried again after its delay.
func (p *Pipeline) removal(ctx context.Context, w tenant.Work) error {
	r, err := parseRemoval(w.Subject)
	if err != nil {
		p.log.Error("a removal unit was not understood; it is dropped", "work", w.ID)
		return p.complete(ctx, w, nil)
	}
	switch r.kind {
	case removeRejected:
		err = p.removeRejectedBytes(ctx, r)
	case removeDiscarded:
		err = p.removeDiscarded(ctx, r.ref)
		if fault.KindOf(err) == fault.Precondition || fault.KindOf(err) == fault.NotFound {
			p.log.Warn("the bytes of a reception are not removed: it is not discarded", "reception", r.ref)
			err = nil
		}
	case removeWithdrawn:
		err = p.removeWithdrawnBytes(ctx, r)
	}
	if err != nil {
		return err
	}
	p.at("removal.done")
	return p.complete(ctx, w, nil)
}

// removeRejectedBytes removes a rejected file's bytes after reading its
// record: rejected, and, when the record kept the file's plain checksum, the
// file at the path has it.
func (p *Pipeline) removeRejectedBytes(ctx context.Context, r Removal) error {
	q, err := p.store.QuarantinedFile(ctx, r.ref)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		p.log.Warn("the bytes of a quarantine record are not removed: the record is not there", "record", r.ref)
		return nil
	case err != nil:
		return err
	case q.State != tenant.QuarantineRejected:
		p.log.Warn("the bytes of a quarantine record are not removed: it is not rejected", "record", r.ref)
		return nil
	}
	// The record keeps the place its bytes were held at after its
	// rejection. A unit that names any other path ends and deletes nothing,
	// so a unit queued for one record can never remove another file.
	if q.Incoming == "" || q.Incoming != r.path {
		p.log.Warn("the bytes at a path are not the rejected file's; they are left", "record", r.ref)
		return nil
	}
	if err := p.sameSize(q); err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return nil
		}
		if fault.KindOf(err) == fault.Conflict {
			p.log.Warn("the bytes of a rejected file are not its size; they are left", "record", r.ref)
			return nil
		}
		return err
	}
	// A record made at a receiver that de-identifies keeps a keyed
	// checksum, which this code cannot compute (the tenant's
	// de-identification keys are the reception pipeline's), so there the
	// size and the path stand for it.
	if !q.SHA256.IsZero() && !q.Deidentified {
		sum, err := p.sumIncoming(r.path)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return nil
		case fault.KindOf(err) == fault.Invalid:
			p.log.Warn("the bytes of a rejected file are not removed: the path is not one of the incoming area", "record", r.ref)
			return nil
		case err != nil:
			return err
		case sum != q.SHA256:
			p.log.Warn("the bytes at a rejected file's path are not the ones rejected; they are left", "record", r.ref)
			return nil
		}
	}
	p.at("reject.before-remove")
	return p.removeIncoming(q.Incoming)
}

// sameSize checks that the bytes at the record's place are as large as the
// record says: NotFound when nothing is there, Conflict when the size
// differs. It is the check left for a record made at a receiver that
// de-identifies, whose keyed checksum is not computed here.
func (p *Pipeline) sameSize(q tenant.QuarantinedFile) error {
	f, size, err := p.area.Open(q.Incoming)
	if err != nil {
		return err
	}
	_ = f.Close()
	if size != q.Size {
		return fault.New(fault.Conflict, "the file's size is not the record's")
	}
	return nil
}

// removeWithdrawnBytes removes the bytes of a held file a purging delete
// cleared, after reading that the correction is a delete.
func (p *Pipeline) removeWithdrawnBytes(ctx context.Context, r Removal) error {
	c, err := p.store.Correction(ctx, r.ref)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		p.log.Warn("the bytes of a purged delete are not removed: the correction is not there", "correction", r.ref)
		return nil
	case err != nil:
		return err
	case c.Kind != record.KindDelete:
		p.log.Warn("the bytes of a correction are not removed: it is not a delete", "correction", r.ref)
		return nil
	}
	// What a purge cleared is no row's any more; a path a live record
	// still names is that record's, whoever asked.
	named, err := p.store.IncomingNamed(ctx, r.path)
	if err != nil {
		return err
	}
	if named {
		p.log.Warn("the bytes at a path are not removed: a record still keeps them", "correction", r.ref)
		return nil
	}
	return p.removeIncoming(r.path)
}

// removeIncoming removes the bytes at path; a path not of the incoming area
// is left, as the unit would find it again.
func (p *Pipeline) removeIncoming(path string) error {
	err := p.area.Remove(path)
	if fault.KindOf(err) == fault.Invalid {
		p.log.Warn("bytes are not removed: the path is not one of the incoming area")
		return nil
	}
	return err
}

// sumIncoming returns the SHA-256 of the file at path in the incoming area,
// NotFound when it is not there.
func (p *Pipeline) sumIncoming(path string) (tenant.SHA256, error) {
	f, _, err := p.area.Open(path)
	if err != nil {
		return tenant.SHA256{}, err
	}
	defer func() { _ = f.Close() }()
	return sumReader(f)
}
