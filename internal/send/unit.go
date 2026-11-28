package send

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons a unit is given back, the last error the store keeps on it.
const (
	reasonConnect     = "the connection to the peer failed"
	reasonSenderDown  = "sender down"
	reasonDisabled    = "sender disabled"
	reasonStopping    = "the server is stopping"
	reasonChanged     = "the sender's destination or credential changed"
	reasonRemoved     = "sender removed"
	reasonNoSlot      = "no read slot free"
	reasonResources   = "the peer is out of resources"
	reasonLost        = "the association ended before the peer answered"
	reasonStorage     = "the storage did not answer"
	reasonTimeout     = "the peer did not answer in time"
	reasonPeerFailing = "the peer answered a temporary failure"
	reasonInternal    = "the unit could not be worked"
)

// outcome is how a scan's sending ended, short of its end: given back with
// an attempt spent (temporary), without one (release), or with nothing
// more to write (stopped: the lease is lost, or the job ended).
type outcome struct {
	temporary, release, stopped bool
	reason                      string
	due                         time.Time
}

// unitRun is one claimed unit being worked.
type unitRun struct {
	sup   *Supervisor
	sr    *senderRun
	w     *worker
	p     tenant.Pipeline
	row   tenant.Sender
	fence tenant.WorkFence
	log   *slog.Logger
	// ctx ends when the lease is lost or the supervisor stops hard.
	ctx  context.Context
	lost atomic.Bool
	// invalid says the store refused a write as malformed: a fault of
	// this package, never of the peer or the data.
	invalid atomic.Bool
	// sealed is the sender's sealed credential, read with row as one
	// (storedSender); nil for none.
	sealed []byte
	// stores and bufs serve the unit's reads; scan is the labels of the
	// scan it sends, which a read that turns to a replica looks up.
	scan   label.Path
	stores Storages
	bufs   [2][]byte
	prefix []byte
	// profile is the destination profile of the unit's job (a forward
	// rule's), whose copy of each instance is sent in its place; zero
	// sends each as archived. copier makes the copies; nil refuses them.
	profile tenant.ProfileRef
	copier  Copier
}

// work works one claimed unit to its end or until it is given back.
//
// The unit works with the sender as stored now, its row and its sealed
// credential read as one, never the row the supervisor last read: a sender
// moved, or given another credential, since then sends to where it is now
// and with what it holds now, and a credential only ever goes with the URL
// it was stored with.
func (w *worker) work(p tenant.Pipeline, unit tenant.Work) {
	sr := w.s
	sup := sr.t.sup
	ctx, cancel := context.WithCancel(sup.hard)
	defer cancel()
	u := &unitRun{sup: sup, sr: sr, w: w, p: p, fence: unit.Fence(), ctx: ctx,
		log: sr.t.log.With("sender", sr.name, "unit", unit.ID)}
	row, sealed, err := storedSender(ctx, p, sr.name)
	var reason string
	switch {
	case fault.KindOf(err) == fault.NotFound:
		reason = reasonRemoved
	case errors.Is(err, errSenderChanged):
		reason = reasonChanged
	case err != nil:
		u.write("Sender", err)
		return
	case !row.Enabled:
		reason = reasonDisabled
	}
	if reason != "" {
		rctx, rcancel := context.WithTimeout(ctx, opTimeout)
		defer rcancel()
		u.write("ReleaseJobUnit", p.ReleaseJobUnit(rctx, u.fence, sup.cfg.Now(), reason, sup.cfg.Now()))
		return
	}
	u.row, u.sealed = row, sealed
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		u.renew(cancel)
	}()
	defer func() {
		cancel()
		<-renewed
	}()
	start, err := p.StartJobUnit(ctx, u.fence, sup.cfg.Now())
	if err != nil {
		u.failStart(err)
		return
	}
	u.log = u.log.With("job", start.Job.ID)
	defer sup.cfg.Wake(sr.t.name)
	if start.Job.State.Ended() {
		// Canceled meanwhile: the unit is ended at once.
		if start.Listing {
			u.write("CompleteWork", p.CompleteWork(ctx, u.fence, sup.cfg.Now(), nil))
		} else {
			_, err := p.EndJobScan(ctx, tenant.JobProgress{Fence: u.fence, From: start.Scan.Cursor, To: start.Scan.Cursor,
				FailuresKept: int(sr.t.kept.Load()), Now: sup.cfg.Now()})
			u.write("EndJobScan", err)
		}
		return
	}
	if start.Listing {
		u.list()
		return
	}
	u.stores = sup.cfg.OpenStorages(sr.t.name, p)
	defer u.stores.Close()
	u.bufs = [2][]byte{make([]byte, readBuffer), make([]byte, readBuffer)}
	u.prefix = make([]byte, metaPrefix)
	u.scan = start.Scan.Path
	if u.profile = start.Job.Profile; u.copies() && sup.cfg.Copies != nil {
		u.copier = sup.cfg.Copies(sr.t.name, p)
	}
	u.log = u.log.With("scan", start.Scan.Path.Subject+"/"+start.Scan.Path.Session+"/"+start.Scan.Path.Scan)
	rec := newRecorder(p, u.fence, start.Scan.Cursor, int(sr.t.kept.Load()), sup.cfg.Now, func() { sup.cfg.Wake(sr.t.name) })
	var out outcome
	if row.Type == tenant.SenderSTOW {
		out = u.sendStow(start.Scan, rec)
	} else {
		out = u.sendDIMSE(start.Scan, rec)
	}
	u.finish(rec, out)
}

// renew renews the unit's lease every renew interval until ctx ends, and
// cancels the unit when the lease is lost.
func (u *unitRun) renew(cancel context.CancelFunc) {
	t := time.NewTicker(u.sup.cfg.seams.renew)
	defer t.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-t.C:
		}
		ctx, c := context.WithTimeout(u.ctx, opTimeout)
		_, err := u.p.RenewWork(ctx, u.fence, u.sup.cfg.Now(), Lease)
		c()
		if errors.Is(err, tenant.ErrLeaseLost) {
			u.lost.Store(true)
			u.log.Info("sender: the unit's lease was lost; it stops and writes nothing more")
			cancel()
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("sender: the unit's lease could not be renewed", "reason", fault.Message(err))
		}
	}
}

// write logs a write of the unit that failed, and marks the unit lost when
// its lease was.
func (u *unitRun) write(what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, tenant.ErrLeaseLost):
		u.lost.Store(true)
		u.log.Info("sender: the unit's lease was lost; it writes nothing more", "write", what)
	case fault.KindOf(err) == fault.Invalid:
		u.invalid.Store(true)
		u.log.Error("sender: the store refused a write of the unit as malformed; the unit's scan is ended without it, never sent again in a loop",
			"write", what, "reason", fault.Message(err))
	case u.ctx.Err() != nil:
	default:
		u.log.Warn("sender: a write of the unit failed", "write", what, "reason", fault.Message(err))
	}
	return false
}

// abandon ends the scan of a unit whose progress the store refused as
// malformed: the batch is dropped and the scan ended from the cursor last
// written, so its instances after it stay remaining (a retry sends them)
// and the unit does not resend the same batch for each of its attempts.
func (u *unitRun) abandon(rec *recorder) {
	rec.reset()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	_, err := rec.end(ctx)
	u.write("EndJobScan", err)
}

// failStart handles a unit that could not start: a lease lost writes
// nothing; a unit the store does not know as a job's spends its attempt;
// any other failure leaves the lease to run out.
func (u *unitRun) failStart(err error) {
	u.write("StartJobUnit", err)
	if fault.KindOf(err) == fault.Precondition {
		_, ferr := u.p.FailWork(context.WithoutCancel(u.ctx), u.fence, reasonInternal, u.sup.cfg.Now())
		u.write("FailWork", ferr)
	}
}

// list works a listing unit: each page lists up to tenant.JobListPage of
// the job's scans and queues their units, and the last completes it.
func (u *unitRun) list() {
	for range 1 << 20 {
		_, more, err := u.p.ListJobScans(u.ctx, u.fence, u.sup.cfg.Now())
		if !u.write("ListJobScans", err) || !more {
			return
		}
		u.sr.nudge()
	}
}

// finish ends the scan's unit as out says: writes what was answered, then
// ends the scan, or gives the unit back with or without an attempt spent.
func (u *unitRun) finish(rec *recorder, out outcome) {
	if u.lost.Load() || out.stopped {
		return
	}
	// The writes below are owed to the store even when the supervisor is
	// stopping: what the peer answered is recorded, and the unit given
	// back, while the database answers.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	now := u.sup.cfg.Now()
	switch {
	case out.temporary:
		if _, _, err := rec.flush(ctx); !u.write("RecordJobProgress", err) {
			if u.invalid.Load() {
				u.abandon(rec)
			}
			return
		}
		_, err := u.p.FailWork(ctx, u.fence, out.reason, now)
		u.write("FailWork", err)
		u.log.Info("sender: the unit failed for now; it is tried again later", "reason", out.reason)
	case out.release:
		if _, _, err := rec.flush(ctx); !u.write("RecordJobProgress", err) {
			if u.invalid.Load() {
				u.abandon(rec)
			}
			return
		}
		due := out.due
		if due.Before(now) {
			due = now
		}
		u.write("ReleaseJobUnit", u.p.ReleaseJobUnit(ctx, u.fence, due, out.reason, now))
	default:
		_, err := rec.end(ctx)
		if !u.write("EndJobScan", err) && u.invalid.Load() {
			u.abandon(rec)
		}
	}
}

// stopping returns the outcome of a unit that must end before its next
// instance (its sender disabled or removed, the pool shrunk, the server
// stopping), and whether it must.
func (u *unitRun) stopping() (outcome, bool) {
	select {
	case <-u.w.stop:
		reason, _ := u.w.why.Load().(string)
		if reason == "" {
			reason = reasonStopping
		}
		return outcome{release: true, reason: reason, due: u.sup.cfg.Now()}, true
	default:
	}
	if u.ctx.Err() != nil {
		return outcome{stopped: true}, true
	}
	return u.tenantStopped()
}

// checkpoint writes the batch when it is due, and reports whether the
// unit must stop: its job ended (a cancel, seen at this write), or the
// write failed.
func (u *unitRun) checkpoint(rec *recorder, force bool) (outcome, bool) {
	if !force && !rec.due() {
		return outcome{}, false
	}
	job, wrote, err := rec.flush(u.ctx)
	if !u.write("RecordJobProgress", err) {
		if u.invalid.Load() {
			u.abandon(rec)
		}
		return outcome{stopped: true}, true
	}
	if wrote && job.State.Ended() {
		u.log.Info("sender: the job ended while the unit sent; the unit ends", "state", string(job.State))
		// The scan is ended as its job's state allows; a write refused is
		// only logged.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
		defer cancel()
		_, err := rec.end(ctx)
		if err != nil && !errors.Is(err, tenant.ErrLeaseLost) {
			u.log.Info("sender: the scan of an ended job could not be ended; its lease runs out", "reason", fault.Message(err))
		}
		return outcome{stopped: true}, true
	}
	return outcome{}, false
}

// errSenderChanged is a sender whose row changed while its credential was
// read, three times in a row.
var errSenderChanged = fault.New(fault.Unavailable, "the sender changed while it was read; try again")

// storedSender reads a sender's row and, for a stow sender with
// authentication, its sealed credential, as one: the row, the credential,
// then the row again, taken only when it did not change between
// (Updated), so the credential is the one stored with that row's URL. A
// sender not there is NotFound.
func storedSender(ctx context.Context, p tenant.Pipeline, name string) (tenant.Sender, []byte, error) {
	for range 3 {
		row, err := p.Sender(ctx, name)
		if err != nil {
			return tenant.Sender{}, nil, err
		}
		if row.Type != tenant.SenderSTOW || row.STOW.Authentication == tenant.StowAuthNone || row.STOW.Authentication == "" {
			return row, nil, nil
		}
		sealed, err := p.SenderCredential(ctx, name)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			sealed = nil
		case err != nil:
			return tenant.Sender{}, nil, err
		}
		again, err := p.Sender(ctx, name)
		if err != nil {
			return tenant.Sender{}, nil, err
		}
		if again.Updated.Equal(row.Updated) {
			return row, sealed, nil
		}
	}
	return tenant.Sender{}, nil, errSenderChanged
}

// takeSlot takes one of the server's read slots for the sender.
func (u *unitRun) takeSlot() (func(), error) {
	return u.sup.takeRead("send:" + u.sr.t.name + "/" + u.sr.name)
}

// instances iterates a scan unit's stream after its cursor, in the
// catalog's order, a page at a time: the instances the catalog holds and,
// merged into their places, those a correction deleted since the job
// counted them (jobmeet.Stream).
type instances struct {
	u      *unitRun
	stream *jobmeet.Stream
}

// newInstances returns the iterator of a unit's stream after after.
func newInstances(u *unitRun, after string) *instances {
	return &instances{u: u, stream: jobmeet.NewStream(u.p, u.fence, after)}
}

// next returns the next instance of the stream, and whether a correction
// deleted it; false at its end.
func (it *instances) next() (tenant.ScanInstance, bool, bool, error) {
	ctx, cancel := context.WithTimeout(it.u.ctx, opTimeout)
	defer cancel()
	item, ok, err := it.stream.Next(ctx)
	return item.Instance, item.Deleted, ok, err
}

// readFailure returns the outcome of a read of the unit's stream or of a
// storage that failed.
func (u *unitRun) readFailure(err error) outcome {
	if u.ctx.Err() != nil {
		return outcome{stopped: true}
	}
	if fault.KindOf(err) == fault.Exhausted {
		return outcome{release: true, reason: reasonNoSlot, due: u.sup.cfg.Now().Add(SlotDelay)}
	}
	u.log.Warn("sender: a read for the unit failed", "reason", fault.Message(err))
	return outcome{temporary: true, reason: reasonStorage}
}

// dataLoss logs an instance whose stored bytes are not what was archived,
// as a read of it is reported.
func (u *unitRun) dataLoss(why string) {
	u.log.Error("sender: an instance's bytes on its storage are not what its manifest records; it is not sent", "detail", why)
}
