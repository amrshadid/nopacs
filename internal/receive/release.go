package receive

import (
	"context"
	"crypto/sha256"
	"io"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The operations below release, reject, retry and discard. Each is the
// store's operation, which records the decision and its reason with its
// audit entry, with what the pipeline does around it to the bytes in the
// incoming area and to the work queue. The store always records first: no
// byte is removed before the reason is committed.
//
// The API releases and retries through the tenant's running pipeline, so
// that the bytes are given their landed name before the store records a
// release and a retry's first unit is queued at once. It records a
// rejection or a discard through the store itself and then hands the
// removal of the bytes here (RemoveRejected, RemoveDiscarded); a removal
// that does not happen is done by Recover at the next start, which finds
// the decision recorded. A pipeline that stops between a step and the next
// loses nothing either: a released file that joined nothing is landed, and
// Recover checks it again as a release of its record; a reception with no
// unit gets one from the repair.

// Release releases a held file as it is (tenant.Pipeline.ReleaseQuarantined)
// and puts its bytes back in line.
//
// Order of effects:
//
//  1. Give the file's bytes their landed name (incoming.Area.Unrecord) and
//     make that durable. While the record is held, a restart that finds
//     them so gives them back their recorded name, since the record names
//     them.
//  2. Release in the store. A file with its UIDs and checksum joins a
//     reception of its study as a received file (the store's rule); one
//     without them joins nothing, and its record no longer keeps its bytes.
//  3. A file that joined a reception gets its recorded name back, and its
//     reception its quiet unit. One that joined nothing is checked again by
//     the receiver's rules now, as a file that just arrived, and recorded or
//     held again with ReleasedFrom naming this record. A crash between 2
//     and 3 leaves its bytes under the landed name with no record keeping
//     them, which a restart checks the same way.
func (p *Pipeline) Release(ctx context.Context, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	leave, err := p.enterDecision()
	if err != nil {
		return tenant.QuarantinedFile{}, err
	}
	defer leave()
	if rel.ReceptionID == "" {
		rel.ReceptionID = newID()
	}
	if rel.Now.IsZero() {
		rel.Now = p.now()
	}
	q, err := p.store.QuarantinedFile(ctx, rel.ID)
	if err != nil {
		return tenant.QuarantinedFile{}, err
	}
	if q.State != tenant.QuarantineHeld {
		return p.store.ReleaseFile(ctx, rel, audit, guard)
	}
	if q.ContentKept {
		if err := p.area.Unrecord(q.Incoming); err != nil {
			return tenant.QuarantinedFile{}, err
		}
	}
	p.at("release.before-store")
	out, err := p.store.ReleaseFile(ctx, rel, audit, guard)
	if err != nil {
		if q.ContentKept {
			_ = p.area.Commit(q.Incoming)
		}
		return tenant.QuarantinedFile{}, err
	}
	p.at("release.after-store")
	return out, p.afterRelease(ctx, q, out)
}

// afterRelease does what follows a release the store recorded: before is
// the record as it was held, after as the release left it. A file that
// joined a reception gets its recorded name and its reception the unit of
// its state; one that joined nothing is given its landed name and checked
// again by the receiver's rules, as a file that just arrived, recorded or
// held again with ReleasedFrom naming the released record. A record that
// kept no bytes, or was not held, asks nothing more.
func (p *Pipeline) afterRelease(ctx context.Context, before, after tenant.QuarantinedFile) error {
	if before.State != tenant.QuarantineHeld || !before.ContentKept {
		return nil
	}
	p.wake()
	if after.ReleaseReception != "" {
		if err := p.area.Commit(before.Incoming); err != nil {
			p.log.Warn("a released file keeps its landed name until a restart", "reason", safe(err))
		}
		r, _, err := p.store.Reception(ctx, after.ReleaseReception)
		if err != nil {
			return err
		}
		return p.ensureNext(ctx, r)
	}
	if !p.running.Load() {
		return nil
	}
	f, err := p.landedFile(incoming.Entry{Receiver: before.Receiver, Path: before.Incoming, ID: pathID(before.Incoming)})
	if err != nil {
		return err
	}
	via := after.ReleaseReceiver
	if via == "" {
		via = before.Receiver
	}
	p.intake(ctx, landed{file: f, receiver: via, caller: before.Caller, releasedFrom: before.ID})
	return nil
}

// RemoveRejected removes the bytes of a held file at path in the incoming
// area, after the store recorded its rejection.
// A pipeline stopping removes nothing: Recover at the next start does.
func (p *Pipeline) RemoveRejected(path string) error {
	leave, err := p.enterDecision()
	if err != nil {
		return err
	}
	defer leave()
	return p.removeRejected(path)
}

// removeRejected removes a rejected file's bytes, inside a decision.
func (p *Pipeline) removeRejected(path string) error {
	p.at("reject.before-remove")
	return p.area.Remove(path)
}

// Retry makes a new reception of a failed one's files
// (tenant.Pipeline.RetryReception) and queues its first unit. A crash
// between the two leaves a reception past receiving with no unit, which
// Start's repair queues.
// newReceptionID, when not empty, is the id of the new reception.
func (p *Pipeline) Retry(ctx context.Context, id, newReceptionID, receiver string, audit master.AuditEntry, guard tenant.Guard) (failed, retry tenant.Reception, err error) {
	leave, err := p.enterDecision()
	if err != nil {
		return failed, retry, err
	}
	defer leave()
	if newReceptionID == "" {
		newReceptionID = newID()
	}
	failed, retry, err = p.store.RetryReception(ctx, id, newReceptionID, receiver, p.now(), audit, guard)
	if err != nil {
		return failed, retry, err
	}
	p.wake()
	return failed, retry, p.ensureNext(ctx, retry)
}

// RemoveDiscarded removes the bytes the store lists for a reception whose
// discard it recorded (tenant.Pipeline.DiscardedFiles).
// A pipeline stopping removes nothing: Recover at the next start does.
func (p *Pipeline) RemoveDiscarded(ctx context.Context, receptionID string) error {
	leave, err := p.enterDecision()
	if err != nil {
		return err
	}
	defer leave()
	return p.removeDiscarded(ctx, receptionID)
}

// removeDiscarded removes a discarded reception's bytes, inside a decision.
func (p *Pipeline) removeDiscarded(ctx context.Context, receptionID string) error {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		files, next, err := p.store.DiscardedFiles(ctx, receptionID, page)
		if err != nil {
			return err
		}
		for _, f := range files {
			p.at("discard.before-remove")
			if err := p.area.Remove(f.Incoming); err != nil && fault.KindOf(err) != fault.Invalid {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// errStopping answers a decision made while the pipeline stops or has not
// started: nothing is recorded, and the person tries again once the next
// pipeline runs.
var errStopping = fault.New(fault.Unavailable, "the tenant's reception pipeline is stopping or not started; try again shortly")

// enterDecision admits a decision as Accept admits a file: refused once the
// pipeline stops, and counted until it returns, so Stop waits for it before
// it closes the incoming area and before another pipeline's Recover can
// run. Without it a release in flight across a restart could give the file
// its landed name, see the next pipeline's Recover give back the recorded
// name the held record named, then record the release: a recorded file no
// row keeps, which the start after removes.
func (p *Pipeline) enterDecision() (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping || !p.running.Load() {
		return nil, errStopping
	}
	p.accepting.Add(1)
	return p.accepting.Done, nil
}

// pathID returns the id of an incoming path, empty for one not of the form.
func pathID(path string) string {
	_, id, err := incoming.ParsePath(path)
	if err != nil {
		return ""
	}
	return id
}

// sumReader returns the SHA-256 of what r yields.
func sumReader(r io.Reader) (tenant.SHA256, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return tenant.SHA256{}, fault.Wrap(fault.Unavailable, err, "an incoming file cannot be read")
	}
	var out tenant.SHA256
	copy(out[:], h.Sum(nil))
	return out, nil
}

// ReadHeld reads the header of the held file at path in the incoming area,
// within the header bound. It is admitted as a decision is, so the incoming
// area is not closed under it; while the pipeline stops or has not started
// it is answered unavailable. A file that is not there is NotFound; a header
// that runs past the bound is a *header.TooLargeError and a file that is not
// a whole DICOM file a *header.NotDICOMError, both unwrapped, so the caller
// can tell them from a failure of the disk.
func (p *Pipeline) ReadHeld(ctx context.Context, path string) (*header.Header, error) {
	done, err := p.enterDecision()
	if err != nil {
		return nil, err
	}
	defer done()
	f, size, err := p.area.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return header.ReadWithin(f, size, p.headerLimit(ctx))
}

// ReadHeldFile returns the bytes of the held file at path in the incoming
// area, as the sender sent them, for a trial run of a profile over it. It
// is admitted as ReadHeld is. A file that is not there is NotFound; one
// longer than limit bytes is a precondition failure, never read in part.
func (p *Pipeline) ReadHeldFile(_ context.Context, path string, limit int64) ([]byte, error) {
	done, err := p.enterDecision()
	if err != nil {
		return nil, err
	}
	defer done()
	f, size, err := p.area.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if size > limit {
		return nil, fault.New(fault.Precondition, "the held file is larger than a trial run reads")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, size), b); err != nil {
		return nil, fault.Wrap(fault.Unavailable, err, "an incoming file cannot be read")
	}
	return b, nil
}
