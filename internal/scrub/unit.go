package scrub

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
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

// outcome is how a unit's work ended short of its end: given back with an
// attempt spent (temporary), without one (release), or with nothing more to
// write (stopped: the lease is lost, or the job ended).
type outcome struct {
	temporary, release, stopped bool
	reason                      string
	due                         time.Time
}

// unitRun is one claimed unit being worked.
type unitRun struct {
	t   *tenantRun
	sup *Supervisor
	p   tenant.Pipeline
	// st is the tenant's database as a store, which its settings and its
	// verifications are read through.
	st    tenant.Store
	fence tenant.WorkFence
	log   *slog.Logger
	// ctx ends when the lease is lost or the supervisor stops hard.
	ctx  context.Context
	lost atomic.Bool
	// stores hands out storages to read and list only.
	stores readStores
	buf    []byte
	// rate is scrub.rate on the tenant; kept job.failures-kept.
	rate int64
	kept int
	// sample is scrub.archive-sample on the tenant, as a share from 0 to 1.
	sample float64
	// job is the unit's job as it started.
	job tenant.Job
	// reads counts the reads of each key, for the test's seam.
	reads map[string]int
	// tenantID is the tenant's id, which its correction records are
	// authenticated for, read once a unit (recordTenant).
	tenantID string
	// replicas are the replicas of each primary, and rows the storages by
	// name, each read once a unit (replicasOf, storageRow).
	replicas map[string][]tenant.ReplicaStatus
	rows     map[string]tenant.Storage
}

// work works one claimed unit to its end or until it is given back.
func (t *tenantRun) work(p tenant.Pipeline, st tenant.Store, w tenant.Work) {
	sup := t.sup
	ctx, cancel := context.WithCancel(sup.hard)
	defer cancel()
	u := &unitRun{t: t, sup: sup, p: p, st: st, fence: w.Fence(), ctx: ctx, log: t.log.With("unit", w.ID), reads: map[string]int{}}
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
	u.job = start.Job
	u.log = u.log.With("job", start.Job.ID)
	if err := u.settings(st); err != nil {
		u.log.Warn("scrubs: the settings a unit reads could not be read; it is tried again later", "reason", fault.Message(err))
		_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonDatabase, sup.cfg.Now())
		u.write("FailWork", ferr)
		return
	}
	if start.Job.State.Ended() {
		if start.Listing {
			u.write("CompleteWork", p.CompleteWork(ctx, u.fence, sup.cfg.Now(), nil))
		} else {
			_, err := p.EndJobScan(ctx, tenant.JobProgress{Fence: u.fence, From: start.Scan.Cursor, To: start.Scan.Cursor,
				FailuresKept: u.kept, Now: sup.cfg.Now()})
			u.write("EndJobScan", err)
		}
		return
	}
	u.stores = readStores{s: sup.cfg.OpenStorages(t.name, p)}
	defer u.stores.Close()
	u.buf = make([]byte, readBuffer)
	if out := u.requested(); out != (outcome{}) {
		u.giveBack(out) // nothing of the unit's own work is read yet
		return
	}
	if start.Listing {
		if out := u.walk(); out != (outcome{}) {
			u.giveBack(out)
			return
		}
		u.list()
		return
	}
	u.log = u.log.With("scan", start.Scan.Path.Subject+"/"+start.Scan.Path.Session+"/"+start.Scan.Path.Scan)
	rec := &recorder{u: u, cursor: start.Scan.Cursor, since: sup.cfg.Now()}
	u.finish(rec, u.scrubScan(start.Scan, rec))
}

// settings reads what the unit is bound by: job.failures-kept, scrub.rate
// and scrub.archive-sample on the tenant, each at the unit's start, so a
// change applies from the next unit without a restart.
func (u *unitRun) settings(st tenant.Store) error {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	ts := u.sup.cfg.TenantSettings(st)
	kept, err := ts.FailuresKept(ctx)
	if err != nil {
		return err
	}
	rate, err := ts.ScrubRate(ctx)
	if err != nil {
		return err
	}
	sample, err := ts.ArchiveSample(ctx)
	if err != nil {
		return err
	}
	if sample < 0 || sample > 1 || sample != sample {
		return fault.New(fault.Internal, "scrub: the share of archive-tier objects read is not from 0 to 1")
	}
	u.kept, u.rate, u.sample = kept, rate, sample
	return nil
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
			u.log.Info("scrubs: the unit's lease was lost; it stops and writes nothing more")
			cancel()
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("scrubs: the unit's lease could not be renewed", "reason", fault.Message(err))
		}
	}
}

// write logs a write of the unit that failed, and marks the unit lost when
// its lease was. It reports whether the write succeeded.
func (u *unitRun) write(what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, tenant.ErrLeaseLost):
		u.lost.Store(true)
		u.log.Info("scrubs: the unit's lease was lost; it writes nothing more", "write", what)
	case u.ctx.Err() != nil:
	default:
		u.log.Warn("scrubs: a write of the unit failed", "write", what, "reason", fault.Message(err))
	}
	return false
}

// list works a listing unit's listing: each page lists up to
// tenant.JobListPage of the job's scans and queues their units, and the
// last completes it.
func (u *unitRun) list() {
	for range maxListPages {
		_, more, err := u.p.ListJobScans(u.ctx, u.fence, u.sup.cfg.Now())
		if !u.write("ListJobScans", err) || !more {
			return
		}
		u.t.wake()
	}
}

// stopping returns the outcome of a unit that must end before its next
// object (the server stopping, its tenant let go, its lease lost), and
// whether it must.
func (u *unitRun) stopping() (outcome, bool) {
	if u.ctx.Err() != nil {
		return outcome{stopped: true}, true
	}
	if u.t.isRetired() {
		return outcome{release: true, reason: reasonStopping, due: u.sup.cfg.Now()}, true
	}
	return outcome{}, false
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
	if errors.Is(err, tenant.ErrLeaseLost) {
		u.lost.Store(true)
		return outcome{stopped: true}
	}
	u.log.Warn("scrubs: a read for the unit failed", "reason", fault.Message(err))
	return outcome{temporary: true, reason: reason}
}

// giveBack gives the unit back as out says, writing nothing else: for a
// listing unit, whose walk records its findings as it goes.
func (u *unitRun) giveBack(out outcome) {
	if u.lost.Load() || out.stopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	now := u.sup.cfg.Now()
	if out.temporary {
		_, err := u.p.FailWork(ctx, u.fence, out.reason, now)
		u.write("FailWork", err)
		return
	}
	due := out.due
	if due.Before(now) {
		due = now
	}
	u.write("ReleaseJobUnit", u.p.ReleaseJobUnit(ctx, u.fence, due, out.reason, now))
}

// finish ends the scan's unit as out says: writes what was read, then ends
// the scan, or gives the unit back with or without an attempt spent.
func (u *unitRun) finish(rec *recorder, out outcome) {
	if u.lost.Load() || out.stopped {
		return
	}
	// Owed to the store even when the supervisor is stopping: what was
	// read is recorded, and the unit given back, while the database
	// answers.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	if !out.temporary && !out.release {
		job, err := rec.end(ctx)
		if u.write("EndJobScan", err) && job.State.Ended() {
			// The job's last scan: its storages' coverage changed, and the
			// metrics show it from now.
			u.recordCoverage(u.st, job)
		}
		return
	}
	if _, _, err := rec.flush(ctx); !u.write("RecordJobProgress", err) {
		return
	}
	u.giveBack(out)
}
