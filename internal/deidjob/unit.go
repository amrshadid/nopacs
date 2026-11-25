package deidjob

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons a unit is given back, the last error the store keeps on it.
const (
	reasonStopping = "the server is stopping"
	reasonNoSlot   = "no read slot free"
	reasonStorage  = "the storage did not answer"
	reasonDatabase = "the tenant's database did not answer"
	reasonInternal = "the unit could not be worked"
)

// outcome is how a scan's judging ended short of its end: given back with
// an attempt spent (temporary), without one (release), or with nothing
// more to write (stopped: the lease is lost, or the job ended).
type outcome struct {
	temporary, release, stopped bool
	reason                      string
	due                         time.Time
}

// unitRun is one claimed unit being worked.
type unitRun struct {
	t     *tenantRun
	sup   *Supervisor
	p     tenant.Pipeline
	fence tenant.WorkFence
	log   *slog.Logger
	// ctx ends when the lease is lost or the supervisor stops hard.
	ctx     context.Context
	lost    atomic.Bool
	invalid atomic.Bool
	stores  Storages
	bufs    [2][]byte
	// scan is the labels of the scan the unit judges, which a read that
	// turns to a replica looks up.
	scan label.Path
}

// work works one claimed unit to its end or until it is given back.
func (t *tenantRun) work(p tenant.Pipeline, st tenant.Store, w tenant.Work) {
	sup := t.sup
	ctx, cancel := context.WithCancel(sup.hard)
	defer cancel()
	u := &unitRun{t: t, sup: sup, p: p, fence: w.Fence(), ctx: ctx, log: t.log.With("unit", w.ID)}
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		u.renew(cancel)
	}()
	defer func() {
		cancel()
		<-renewed
	}()
	defer sup.cfg.Wake(t.name)
	start, err := p.StartJobUnit(ctx, u.fence, sup.cfg.Now())
	if err != nil {
		u.write("StartJobUnit", err)
		if fault.KindOf(err) == fault.Precondition {
			_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonInternal, sup.cfg.Now())
			u.write("FailWork", ferr)
		}
		return
	}
	u.log = u.log.With("job", start.Job.ID)
	kept, pixelLimit, headerLimit, err := u.settings(st)
	if err != nil {
		u.log.Warn("deid jobs: the settings a unit reads could not be read; it is tried again later", "reason", fault.Message(err))
		_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonDatabase, sup.cfg.Now())
		u.write("FailWork", ferr)
		return
	}
	if start.Job.State.Ended() {
		if start.Listing {
			u.write("CompleteWork", p.CompleteWork(ctx, u.fence, sup.cfg.Now(), nil))
		} else {
			_, err := p.EndJobScan(ctx, tenant.JobProgress{Fence: u.fence, From: start.Scan.Cursor, To: start.Scan.Cursor,
				FailuresKept: kept, Now: sup.cfg.Now()})
			u.write("EndJobScan", err)
		}
		return
	}
	if start.Listing {
		u.list()
		return
	}
	j := &judge{mode: start.Job.Mode, profile: start.Job.Profile, p: p, engines: map[tenant.ProfileRef]*engine.Engine{},
		rules: map[tenant.ProfileRef][]*v1.ExpandedRule{}, receptions: map[string]receptionFacts{},
		unusable: map[tenant.ProfileRef]bool{}, headerLimit: headerLimit, pixelLimit: min(pixelLimit, MaxPixelBytes),
		tried: sup.cfg.seams.tried, subject: start.Scan.Path.Subject,
		memory: sup.cfg.Memory}
	makeKeys := throwawayKeys
	if sup.cfg.seams.keys != nil {
		makeKeys = sup.cfg.seams.keys
	}
	if j.keys, err = makeKeys(); err != nil {
		u.log.Error("deid jobs: a unit's keys could not be made; it is tried again later", "reason", fault.Message(err))
		_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonInternal, sup.cfg.Now())
		u.write("FailWork", ferr)
		return
	}
	// The unit's keys and the tenant's UID key live only as long as it.
	defer func() {
		for _, k := range [][]byte{j.keys.UID, j.keys.Date, j.keys.Hash, j.uidKey} {
			clear(k)
		}
	}()
	if j.mode == tenant.DeidCheck {
		key, err := sup.cfg.UIDKey(ctx, t.name)
		j.uidKey = key
		if err != nil || len(key) < engine.MinKeyLength {
			u.log.Warn("deid jobs: the tenant's UID key could not be read; the unit is tried again later", "reason", fault.Message(err))
			_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonDatabase, sup.cfg.Now())
			u.write("FailWork", ferr)
			return
		}
	}
	u.stores = sup.cfg.OpenStorages(t.name, p)
	defer u.stores.Close()
	u.bufs = [2][]byte{make([]byte, readBuffer), make([]byte, readBuffer)}
	u.scan = start.Scan.Path
	u.log = u.log.With("scan", start.Scan.Path.Subject+"/"+start.Scan.Path.Session+"/"+start.Scan.Path.Scan)
	rec := newRecorder(p, u.fence, start.Scan.Cursor, kept, sup.cfg.Now, func() { sup.cfg.Wake(t.name) })
	u.finish(rec, u.judgeScan(j, start.Scan, rec))
}

// settings reads what the unit is bound by: job.failures-kept and
// deid.pixel-limit on the tenant, receiver.header-limit on the
// installation.
func (u *unitRun) settings(st tenant.Store) (int, int64, int64, error) {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	ts := u.sup.cfg.TenantSettings(st)
	kept, err := ts.FailuresKept(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	pixels, err := ts.PixelLimit(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	headers, err := u.sup.cfg.Installation.HeaderLimit(ctx)
	return kept, pixels, headers, err
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
			u.log.Info("deid jobs: the unit's lease was lost; it stops and writes nothing more")
			cancel()
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("deid jobs: the unit's lease could not be renewed", "reason", fault.Message(err))
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
		u.log.Info("deid jobs: the unit's lease was lost; it writes nothing more", "write", what)
	case fault.KindOf(err) == fault.Invalid:
		u.invalid.Store(true)
		u.log.Error("deid jobs: the store refused a write of the unit as malformed; the unit's scan is ended without it",
			"write", what, "reason", fault.Message(err))
	case u.ctx.Err() != nil:
	default:
		u.log.Warn("deid jobs: a write of the unit failed", "write", what, "reason", fault.Message(err))
	}
	return false
}

// list works a listing unit: each page lists up to tenant.JobListPage of
// the job's scans and queues their units, and the last completes it.
func (u *unitRun) list() {
	for range 1 << 20 {
		_, more, err := u.p.ListJobScans(u.ctx, u.fence, u.sup.cfg.Now())
		if !u.write("ListJobScans", err) || !more {
			return
		}
		u.t.wake()
	}
}

// stopping returns the outcome of a unit that must end before its next
// instance (the server stopping, its tenant let go, its lease lost), and
// whether it must.
func (u *unitRun) stopping() (outcome, bool) {
	if u.ctx.Err() != nil {
		return outcome{stopped: true}, true
	}
	if u.t.isRetired() {
		return outcome{release: true, reason: reasonStopping, due: u.sup.cfg.Now()}, true
	}
	// A tenant moved, fenced, restoring or failed is no longer served: the
	// unit is given back with the reason the registry states, its cursor
	// kept.
	if _, err := u.sup.cfg.Tenants.Store(u.t.name); err != nil {
		reason := fault.Message(err)
		if reason == "" {
			reason = "the tenant is not ready"
		}
		return outcome{release: true, reason: reason, due: u.sup.cfg.Now().Add(SlotDelay)}, true
	}
	return outcome{}, false
}

// meet checks an instance against the archive as it is now, just before it
// is read: one a correction deleted since the job was made is refused as
// deleted_in_archive and never read, and one replaced since the stream was
// read is judged at its current revision. ok is false when the unit must
// end before it, as out says.
func (u *unitRun) meet(item jobmeet.Item) (tenant.ScanInstance, *verdict, outcome, bool) {
	in := item.Instance
	deleted := item.Deleted
	if !deleted {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		defer cancel()
		m, err := jobmeet.Meet(ctx, u.p, u.fence, in)
		if err != nil {
			return in, nil, u.failure(err, reasonDatabase), false
		}
		in, deleted = m.Instance, m.Deleted
	}
	if deleted {
		u.log.Info("deid jobs: an instance was deleted after the job was made; it is not judged")
		return in, &verdict{kind: resultRefused, code: tenant.FailureDeletedInArchive, detail: detailDeleted,
			tallies: map[string]int64{tallyKey(tallyCheck, "deleted_in_archive"): 1}}, outcome{}, true
	}
	return in, nil, outcome{}, true
}

// judgeScan judges the scan's instances after its cursor, in the catalog's
// order, and returns how it ended; the zero outcome is the scan's end.
func (u *unitRun) judgeScan(j *judge, scan tenant.JobScan, rec *recorder) outcome {
	stream := jobmeet.NewStream(u.p, u.fence, scan.Cursor)
	for {
		if out, stop := u.stopping(); stop {
			return out
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		item, more, err := stream.Next(ctx)
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
		if !more {
			return outcome{}
		}
		cur, settled, out, ok := u.meet(item)
		if !ok {
			return out
		}
		var v verdict
		if settled != nil {
			v = *settled
		} else if v, out, ok = u.judgeOne(j, cur); !ok {
			return out
		}
		if !rec.fits(v) {
			if out, stop := u.checkpoint(rec, true); stop {
				return out
			}
		}
		rec.add(cur, v)
		if f := u.sup.cfg.seams.judged; f != nil && settled == nil {
			f(cur.SOPInstanceUID)
		}
		if out, stop := u.checkpoint(rec, false); stop {
			return out
		}
	}
}

// judgeOne reads and judges one instance. ok is false when the unit must
// end before it, as out says.
func (u *unitRun) judgeOne(j *judge, in tenant.ScanInstance) (verdict, outcome, bool) {
	file, err := u.read(in)
	switch {
	case errors.Is(err, errTooLarge):
		// Not judged, so a note: neither passed nor refused on its merits.
		return verdict{kind: resultNoted, code: tenant.FailureTooLarge, detail: detailTooLarge,
			tallies: map[string]int64{tallyKey(tallyCheck, "too_large"): 1}}, outcome{}, true
	case errors.Is(err, errMissing):
		u.dataLoss(detailMissing)
		return verdict{kind: resultRefused, code: tenant.FailureDamagedInStorage, detail: detailMissing,
			tallies: map[string]int64{tallyKey(tallyCheck, "damaged_in_storage"): 1}}, outcome{}, true
	case checkedread.Mismatched(err):
		u.dataLoss(detailDamaged)
		return verdict{kind: resultRefused, code: tenant.FailureDamagedInStorage, detail: detailDamaged,
			tallies: map[string]int64{tallyKey(tallyCheck, "damaged_in_storage"): 1}}, outcome{}, true
	case err != nil:
		return verdict{}, u.failure(err, reasonStorage), false
	}
	v, err := j.judgeFile(u.ctx, in, file)
	if err != nil {
		return verdict{}, u.failure(err, reasonDatabase), false
	}
	return v, outcome{}, true
}

// failure returns the outcome of a read or a database call that failed:
// stopped when the unit's context ended, given back without an attempt
// when no read slot was free, and with one otherwise.
func (u *unitRun) failure(err error, reason string) outcome {
	if u.ctx.Err() != nil {
		return outcome{stopped: true}
	}
	if fault.KindOf(err) == fault.Exhausted {
		return outcome{release: true, reason: reasonNoSlot, due: u.sup.cfg.Now().Add(SlotDelay)}
	}
	u.log.Warn("deid jobs: a read for the unit failed", "reason", fault.Message(err))
	return outcome{temporary: true, reason: reason}
}

// dataLoss logs an instance whose stored bytes are not what was archived,
// as a read of it is reported.
func (u *unitRun) dataLoss(why string) {
	u.log.Error("deid jobs: an instance's bytes on its storage are not what its manifest records; it is not judged", "detail", why)
}

// checkpoint writes the batch when it is due (or force), and reports
// whether the unit must stop: its job ended (a cancel, seen at this
// write), or the write failed.
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
		u.log.Info("deid jobs: the job ended while the unit judged; the unit ends", "state", string(job.State))
		ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
		defer cancel()
		if _, err := rec.end(ctx); err != nil && !errors.Is(err, tenant.ErrLeaseLost) {
			u.log.Info("deid jobs: the scan of an ended job could not be ended; its lease runs out", "reason", fault.Message(err))
		}
		return outcome{stopped: true}, true
	}
	return outcome{}, false
}

// abandon ends the scan of a unit whose progress the store refused as
// malformed, from the cursor last written, so its instances after it stay
// remaining and the unit does not judge the same batch for each attempt.
func (u *unitRun) abandon(rec *recorder) {
	rec.reset()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	_, err := rec.end(ctx)
	u.write("EndJobScan", err)
}

// finish ends the scan's unit as out says: writes what was judged, then
// ends the scan, or gives the unit back with or without an attempt spent.
func (u *unitRun) finish(rec *recorder, out outcome) {
	if u.lost.Load() || out.stopped {
		return
	}
	// Owed to the store even when the supervisor is stopping: what was
	// judged is recorded, and the unit given back, while the database
	// answers.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	now := u.sup.cfg.Now()
	if !out.temporary && !out.release {
		if _, err := rec.end(ctx); !u.write("EndJobScan", err) && u.invalid.Load() {
			u.abandon(rec)
		}
		return
	}
	if _, _, err := rec.flush(ctx); !u.write("RecordJobProgress", err) {
		if u.invalid.Load() {
			u.abandon(rec)
		}
		return
	}
	if out.temporary {
		_, err := u.p.FailWork(ctx, u.fence, out.reason, now)
		u.write("FailWork", err)
		u.log.Info("deid jobs: the unit failed for now; it is tried again later", "reason", out.reason)
		return
	}
	due := out.due
	if due.Before(now) {
		due = now
	}
	u.write("ReleaseJobUnit", u.p.ReleaseJobUnit(ctx, u.fence, due, out.reason, now))
}
