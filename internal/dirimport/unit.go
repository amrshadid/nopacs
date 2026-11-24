package dirimport

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/dirimport/mapping"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons a unit is given back, the last error the store keeps on it:
// fixed phrases, never a path.
const (
	reasonStopping   = "the server is stopping"
	reasonDatabase   = "the tenant's database did not answer"
	reasonInternal   = "the unit could not be worked"
	reasonNoReceiver = "the import receiver does not exist, or is not an import receiver"
	reasonDisabled   = "the import receiver is disabled"
	reasonNoPipeline = "the tenant's reception pipeline is not running"
	reasonNoMap      = "the import's map could not be used"
	reasonMapNotKept = "the job names a map file, and the map was not kept with the job when it was made; make the job again"
	reasonTooWide    = "a folder of the unit holds more names than import.max-dir-entries"
	reasonUnreadable = "the unit's folder could not be read"
	reasonStalled    = "the unit's walk made no progress for ten minutes (a file system that does not answer)"
)

// The causes a unit's context is canceled with, besides the supervisor
// stopping hard and the lease lost.
var (
	// errRootGone is the unit's root no longer named, allotted or
	// resolving to the directory it was opened at.
	errRootGone = errors.New("the unit's root is no longer available")
	// errStalled is the unit's walk making no progress for StallBound.
	errStalled = errors.New("the unit's walk made no progress")
)

// end is how a unit's walk ended.
type end int

// The ends.
const (
	// endWalked ends the walk (EndImportWalk): every entry judged, or the
	// job ended.
	endWalked end = iota
	// endRoot fails the unit with tenant.ImportReasonRootUnavailable.
	endRoot
	// endRelease gives the unit back without spending an attempt, its
	// cursor kept.
	endRelease
	// endTemporary gives the unit back with an attempt spent.
	endTemporary
	// endStopped writes nothing more: the lease is lost, or the supervisor
	// stops hard.
	endStopped
)

// outcome is how a unit ended, and why when it is given back.
type outcome struct {
	end    end
	reason string
	due    time.Time
}

// unitRun is one claimed unit being worked.
type unitRun struct {
	t     *tenantRun
	sup   *Supervisor
	p     tenant.Pipeline
	st    tenant.Store
	fence tenant.WorkFence
	log   *slog.Logger
	// ctx ends when the lease is lost, the supervisor stops hard, the root
	// is gone (errRootGone) or the walk stalls (errStalled).
	ctx    context.Context
	cancel context.CancelCauseFunc
	lost   atomic.Bool
	// progress is when the walk last moved (a batch of names read, an
	// entry judged, bytes of a file read), in Unix nanoseconds of the
	// supervisor's clock.
	progress atomic.Int64

	// mu guards what follows: the unit's folder, for a test's seam, and
	// its root, as checked again while it runs (rootHeld).
	mu          sync.Mutex
	folder      string
	rootName    string
	rootDir     string
	rootChecked time.Time
	rootGone    bool
}

// work works one claimed unit to its end or until it is given back.
func (t *tenantRun) work(p tenant.Pipeline, st tenant.Store, w tenant.Work) {
	sup := t.sup
	ctx, cancel := context.WithCancelCause(sup.hard)
	defer cancel(nil)
	u := &unitRun{t: t, sup: sup, p: p, st: st, fence: w.Fence(), ctx: ctx, cancel: cancel, log: t.log.With("unit", w.ID)}
	u.moved()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		u.renew()
	}()
	defer func() {
		cancel(nil)
		<-renewed
	}()
	defer sup.cfg.Wake(t.name)
	unit, job, err := p.StartImportUnit(ctx, u.fence, sup.cfg.Now())
	if err != nil {
		u.write("StartImportUnit", err)
		if fault.KindOf(err) == fault.Precondition {
			_, ferr := p.FailWork(context.WithoutCancel(ctx), u.fence, reasonInternal, sup.cfg.Now())
			u.write("FailWork", ferr)
		}
		return
	}
	u.mu.Lock()
	u.folder = unit.Folder
	u.mu.Unlock()
	u.log = u.log.With("job", job.ID, "import_unit", unit.ID)
	if job.State.Ended() {
		u.finish(outcome{end: endWalked})
		return
	}
	u.finish(u.walkUnit(unit, job))
}

// walkUnit reads what the unit needs (its job, its receiver, its root, its
// layout and map) and walks its folder.
func (u *unitRun) walkUnit(unit tenant.ImportUnit, job tenant.Job) outcome {
	cfg := u.sup.cfg
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	full, err := u.p.ImportJob(ctx, job.ID)
	if err != nil || full.Import == nil {
		return u.failure(err, reasonDatabase)
	}
	spec := *full.Import
	rcv, err := u.p.Receiver(ctx, spec.Receiver)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return outcome{end: endTemporary, reason: reasonNoReceiver}
	case err != nil:
		return u.failure(err, reasonDatabase)
	case rcv.Type != tenant.ReceiverImport:
		return outcome{end: endTemporary, reason: reasonNoReceiver}
	case !rcv.Enabled:
		// As a send job's unit waits for its sender: the job waits while
		// its receiver is disabled, spending no attempt.
		return outcome{end: endRelease, reason: reasonDisabled, due: cfg.Now().Add(WaitDelay)}
	}
	sink := cfg.Sink(u.t.name)
	if sink == nil {
		return outcome{end: endRelease, reason: reasonNoPipeline, due: cfg.Now().Add(RefusedDelay)}
	}
	maxEntries, err := cfg.Installation.ImportMaxDirEntries(ctx)
	if err != nil {
		return u.failure(err, reasonDatabase)
	}
	dir, err := cfg.Roots.Root(ctx, u.t.name, u.st, rcv.Import.Root)
	if err != nil {
		if fault.KindOf(err) != fault.Precondition {
			return u.failure(err, reasonDatabase)
		}
		u.log.Warn("imports: the unit's root is not available; the unit fails", "root", rcv.Import.Root, "reason", fault.Message(err))
		return outcome{end: endRoot}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		u.log.Warn("imports: the unit's root cannot be opened; the unit fails", "root", rcv.Import.Root)
		return outcome{end: endRoot}
	}
	defer func() { _ = root.Close() }()
	u.mu.Lock()
	u.rootName, u.rootDir, u.rootChecked = rcv.Import.Root, dir, cfg.Now()
	u.mu.Unlock()
	layout, err := rcv.Import.ParsedLayout()
	if err != nil {
		u.log.Error("imports: the receiver's layout does not parse", "reason", fault.Message(err))
		return outcome{end: endTemporary, reason: reasonInternal}
	}
	resolver, err := storedResolver(rcv, spec, layout)
	switch {
	case errors.Is(err, errMapNotKept):
		u.log.Warn("imports: the job names a map file whose map was not kept with it; the unit fails")
		return outcome{end: endTemporary, reason: reasonMapNotKept}
	case err != nil:
		u.log.Warn("imports: the import's map could not be used; the unit is tried again later", "reason", fault.Message(err))
		return outcome{end: endTemporary, reason: reasonNoMap}
	}
	w := &walker{u: u, sink: sink, rcv: rcv, resolver: resolver, root: root, folder: unit.Folder, cursor: unit.Cursor,
		listing: unit.Listing, unitDepth: layout.UnitDepth(), maxEntries: maxEntries, keep: cfg.seams.keep}
	w.lists = w.listing && depthOf(unit.Folder) < w.unitDepth
	return w.walk()
}

// errMapNotKept is a job that names a map file, its own or its receiver's,
// with no map kept: every job made reads the file once and keeps the map
// (CheckJob), so its units never read a file that may have changed or been
// swapped since; one that kept none was not made so, and is refused.
var errMapNotKept = errors.New("the job names a map file and keeps no map")

// storedResolver returns the resolver of an import job's paths from what
// the job kept when it was made: the receiver's layout, with the job's map
// configuration or else the receiver's, and the map the job keeps (given
// inline, or read from the map file the job or the receiver named). The
// map file is never read here.
func storedResolver(rcv tenant.Receiver, spec tenant.ImportJob, layout *importpath.Layout) (*importpath.Resolver, error) {
	use := spec.Map
	if use.IsZero() {
		use = rcv.Import.Map
	}
	data := spec.MapCSV
	named := spec.MapFile != "" || rcv.Import.MapFile != ""
	switch {
	case len(data) == 0 && named:
		return nil, errMapNotKept
	case use.IsZero() && len(data) == 0:
		return layout.Resolver(nil, importpath.MapUse{})
	case use.IsZero():
		return nil, fault.New(fault.Precondition, "a map is given with no map configuration")
	case len(data) == 0:
		return nil, fault.New(fault.Precondition, "a map configuration is given with no map")
	}
	// A kept map was bounded when the job was made (tenant.MaxImportMapCSV),
	// whether it was given inline or read from a file (CheckJob).
	m, err := mapping.Parse(data, mapping.MaxInlineBytes, mapping.Columns{Key: use.KeyColumns, Value: use.ValueColumn})
	if err != nil {
		// A mapping error names a line by its number, never a cell.
		return nil, fault.Wrap(fault.Precondition, err, err.Error())
	}
	return layout.Resolver(m, importpath.MapUse{Key: use.KeyParts, Gives: use.Gives})
}

// renew renews the unit's lease every renew interval until its context
// ends. At each tick it first ends a walk that has stalled (stall) and a
// walk whose root is gone (rootHeld), and cancels the unit when the lease
// is lost.
func (u *unitRun) renew() {
	t := time.NewTicker(u.sup.cfg.seams.renew)
	defer t.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-t.C:
		}
		if u.stalled() {
			u.stall()
			return
		}
		if !u.rootHeld(false) {
			u.cancel(errRootGone)
			return
		}
		ctx, c := context.WithTimeout(u.ctx, opTimeout)
		_, err := u.p.RenewWork(ctx, u.fence, u.sup.cfg.Now(), Lease)
		c()
		if errors.Is(err, tenant.ErrLeaseLost) {
			u.lost.Store(true)
			u.log.Info("imports: the unit's lease was lost; it stops and writes nothing more")
			u.cancel(nil)
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("imports: the unit's lease could not be renewed", "reason", fault.Message(err))
		}
	}
}

// moved notes that the walk made progress now.
func (u *unitRun) moved() { u.progress.Store(u.sup.cfg.Now().UnixNano()) }

// stalled reports whether the walk made no progress for the stall bound.
func (u *unitRun) stalled() bool {
	return u.sup.cfg.Now().UnixNano()-u.progress.Load() > int64(u.sup.cfg.seams.stall)
}

// stall ends a unit whose walk made no progress for the stall bound: a
// read of the file system that does not return (a server gone, a stale
// mount) would otherwise hold the unit forever while its lease is renewed.
// The lease is no longer renewed, the unit is marked lost so its walker,
// if it ever returns, writes nothing more, and its unit of work fails with
// an attempt spent, so another claim takes it up and a cause that stays
// fails the job. The walker's goroutine keeps its place until the read
// returns.
func (u *unitRun) stall() {
	u.lost.Store(true)
	u.cancel(errStalled)
	u.log.Warn("imports: the unit's walk made no progress; it fails for now and is tried again later")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	_, err := u.p.FailWork(ctx, u.fence, reasonStalled, u.sup.cfg.Now())
	if err != nil && !errors.Is(err, tenant.ErrLeaseLost) {
		u.log.Warn("imports: a write of the unit failed", "write", "FailWork", "reason", fault.Message(err))
	}
	u.ended(endTemporary, reasonStalled)
}

// rootHeld reports whether the unit's root is still named, allotted to the
// tenant and resolving to the directory the unit opened, read again when
// force is set or when it was last read RootCheckInterval ago: a root an
// admin takes away stops a unit in flight within that bound, not only the
// next one. A root that cannot be read for now (the database not
// answering) is taken as held until it can.
func (u *unitRun) rootHeld(force bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.rootGone:
		return false
	case u.rootName == "":
		return true
	}
	now := u.sup.cfg.Now()
	if !force && now.Sub(u.rootChecked) < u.sup.cfg.seams.rootCheck {
		return true
	}
	u.rootChecked = now
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	dir, err := u.sup.cfg.Roots.Root(ctx, u.t.name, u.st, u.rootName)
	switch {
	case err == nil && dir == u.rootDir:
		return true
	case err != nil && fault.KindOf(err) != fault.Precondition:
		return true
	case err != nil:
		u.log.Warn("imports: the unit's root is no longer available; the unit fails", "root", u.rootName, "reason", fault.Message(err))
	default:
		u.log.Warn("imports: the unit's root resolves to another directory now; the unit fails", "root", u.rootName)
	}
	u.rootGone = true
	return false
}

// write logs a write of the unit that failed, and marks the unit lost when
// its lease was. It reports whether the write succeeded.
func (u *unitRun) write(what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, tenant.ErrLeaseLost):
		u.lost.Store(true)
		u.log.Info("imports: the unit's lease was lost; it writes nothing more", "write", what)
	case u.ctx.Err() != nil:
	default:
		u.log.Warn("imports: a write of the unit failed", "write", what, "reason", fault.Message(err))
	}
	return false
}

// failure returns the outcome of a read or write that failed: stopped when
// the unit's context ended or its lease was lost, and given back with an
// attempt spent otherwise.
func (u *unitRun) failure(err error, reason string) outcome {
	if errors.Is(err, tenant.ErrLeaseLost) {
		u.lost.Store(true)
		return outcome{end: endStopped}
	}
	if u.ctx.Err() != nil {
		return u.canceled()
	}
	if err != nil {
		u.log.Warn("imports: a read or write of the unit failed; it is tried again later", "reason", fault.Message(err))
	}
	return outcome{end: endTemporary, reason: reason}
}

// canceled returns the outcome of a unit whose context ended: failed with
// the root's reason when its root is gone, and stopped otherwise.
func (u *unitRun) canceled() outcome {
	if errors.Is(context.Cause(u.ctx), errRootGone) {
		return outcome{end: endRoot}
	}
	return outcome{end: endStopped}
}

// stopping returns the outcome of a unit that must end before its next
// entry or hand-in (the server stopping hard, its tenant let go, its lease
// lost, its root gone), and whether it must. The root is read again here
// once RootCheckInterval has passed since it was last read, so a walk
// that judges entries quickly needs no tick to see it gone.
func (u *unitRun) stopping() (outcome, bool) {
	if u.ctx.Err() != nil {
		return u.canceled(), true
	}
	if u.t.isRetired() {
		return outcome{end: endRelease, reason: reasonStopping, due: u.sup.cfg.Now()}, true
	}
	if !u.rootHeld(false) {
		return outcome{end: endRoot}, true
	}
	return outcome{}, false
}

// finish ends the unit as out says. Owed to the store even when the
// supervisor is stopping, while the database answers.
func (u *unitRun) finish(out outcome) {
	if u.lost.Load() || out.end == endStopped {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
	defer cancel()
	now := u.sup.cfg.Now()
	reason := out.reason
	switch out.end {
	case endWalked:
		_, err := u.p.EndImportWalk(ctx, u.fence, now)
		u.write("EndImportWalk", err)
	case endRoot:
		reason = tenant.ImportReasonRootUnavailable
		_, err := u.p.FailImportUnit(ctx, u.fence, tenant.ImportReasonRootUnavailable, now)
		u.write("FailImportUnit", err)
	case endTemporary:
		_, err := u.p.FailWork(ctx, u.fence, out.reason, now)
		u.write("FailWork", err)
		u.log.Info("imports: the unit failed for now; it is tried again later", "reason", out.reason)
	case endRelease:
		due := out.due
		if due.Before(now) {
			due = now
		}
		u.write("ReleaseImportUnit", u.p.ReleaseImportUnit(ctx, u.fence, due, out.reason, now))
	}
	u.ended(out.end, reason)
}

// ended tells a test's seam how the unit ended: its folder, the end, and
// the reason it was given back or failed with, "" when its walk ended.
func (u *unitRun) ended(how end, reason string) {
	f := u.sup.cfg.seams.ended
	if f == nil {
		return
	}
	u.mu.Lock()
	folder := u.folder
	u.mu.Unlock()
	f(folder, how, reason)
}
