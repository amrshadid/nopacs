package rebuild

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/interrupt"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons a unit fails an attempt, the last error the store keeps on
// it.
const (
	reasonStopping = "the server is stopping"
	reasonStorage  = "a storage did not answer"
	reasonRecord   = "a correction record cannot be read"
	reasonDatabase = "the tenant's database did not answer"
	reasonDrain    = "the tenant's receptions did not stop in time"
)

// errDrain marks a replace's wait for the tenant's writers that ended
// before they stopped.
var errDrain = errors.New("the tenant's writers did not stop")

// errStopped is a unit that must write nothing more: its lease is lost,
// the server stops, or its tenant was let go.
var errStopped = errors.New("the unit stops")

// unitRun is one claimed unit being worked.
type unitRun struct {
	t     *tenantRun
	sup   *Supervisor
	p     tenant.Pipeline
	fence tenant.WorkFence
	log   *slog.Logger
	// ctx ends when the lease is lost or the supervisor stops hard.
	ctx  context.Context
	lost atomic.Bool
	job  tenant.Job
}

// work works one claimed unit to its end, or until it must stop or an
// attempt fails.
func (t *tenantRun) work(p tenant.Pipeline, w tenant.Work) {
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
	// Last of all, once the unit has written its end (or its attempt's
	// failure, or the job's): receivers a replace held back see at once
	// whether it still holds them.
	defer sup.cfg.Resume(t.name)
	err := u.run()
	switch {
	case err == nil, u.lost.Load(), errors.Is(err, errStopped):
		return
	case errors.Is(err, tenant.ErrLeaseLost):
		return
	}
	reason := reasonDatabase
	switch {
	case errors.Is(err, errUnreadableRecord):
		reason = reasonRecord
	case errors.Is(err, errDrain):
		reason = reasonDrain
	case errors.Is(err, errStorage):
		reason = reasonStorage
	case fault.KindOf(err) == fault.Precondition:
		// A refusal of the store is a fixed phrase of its own, which says
		// what stopped the attempt better than a database that did not
		// answer, and holds no value read from a file.
		reason = attemptReason(fault.Message(err))
	}
	u.log.Warn("rebuilds: the unit's attempt failed; it is tried again later", "reason", fault.Message(err))
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer fcancel()
	failed, ferr := p.FailWork(fctx, u.fence, reason, sup.cfg.Now())
	switch {
	case ferr != nil && !errors.Is(ferr, tenant.ErrLeaseLost):
		u.log.Warn("rebuilds: the unit's failure could not be recorded", "reason", fault.Message(ferr))
	case ferr == nil && failed.State == tenant.WorkFailed:
		// Its last attempt: the store ended the job failed, and its
		// shadow goes now.
		t.dropLeft(p)
	}
}

// attemptReason returns message cut to the longest reason a store keeps of
// a failed attempt or a job, never empty.
func attemptReason(message string) string {
	if message == "" {
		return reasonDatabase
	}
	if len(message) > tenant.MaxWorkTextLength {
		message = message[:tenant.MaxWorkTextLength]
	}
	return message
}

// errStorage marks a failure of a storage, as opposed to the database.
var errStorage = errors.New("a storage did not answer")

// storageErr marks err as a storage's.
func storageErr(err error) error {
	if err == nil || errors.Is(err, errUnreadableRecord) {
		return err
	}
	return errors.Join(errStorage, err)
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
			u.log.Info("rebuilds: the unit's lease was lost; it stops and writes nothing more")
			cancel()
			return
		}
		if err != nil && u.ctx.Err() == nil {
			u.log.Warn("rebuilds: the unit's lease could not be renewed", "reason", fault.Message(err))
		}
	}
}

// stopping returns errStopped when the unit must end before its next scan.
func (u *unitRun) stopping() error {
	if u.ctx.Err() != nil || u.t.isRetired() {
		return errStopped
	}
	return nil
}

// reached marks the durable step name of the unit's job done: a server
// built for a harness that kills it there ends at this call
// (internal/interrupt, named rebuild:<mode>:<name>).
func (u *unitRun) reached(name string) {
	if u.job.Rebuild != nil {
		interrupt.At("rebuild", string(u.job.Rebuild.Mode), name)
	}
}

// op returns a context for one call to the database.
func (u *unitRun) op() (context.Context, context.CancelFunc) {
	return context.WithTimeout(u.ctx, opTimeout)
}

// run does the unit's whole work: the build from where the shadow stands,
// then the compare or the activation, then the shadow dropped and the unit
// completed. A job that has ended (canceled, or ended by an earlier attempt
// that stopped before its shadow was dropped) only has its shadow dropped.
func (u *unitRun) run() error {
	ctx, cancel := u.op()
	job, err := u.p.StartRebuildUnit(ctx, u.fence, u.sup.cfg.Now())
	cancel()
	if err != nil {
		return err
	}
	u.job = job
	u.log = u.log.With("job", job.ID)
	if !job.State.Ended() {
		err := u.drain()
		if err == nil {
			err = u.build()
		}
		if err == nil {
			u.reached("built")
			err = u.end()
		}
		if errors.Is(err, tenant.ErrRebuildRefused) {
			// No attempt gets past this refusal: the job ends failed with
			// it as its reason, and its shadow goes, rather than the unit
			// being tried again until its attempts run out.
			u.log.Warn("rebuilds: the rebuild cannot go on; its job ends failed", "reason", fault.Message(err))
			ctx, cancel := u.op()
			_, err = u.p.FailRebuild(ctx, u.fence, attemptReason(fault.Message(err)), u.sup.cfg.Now())
			cancel()
		}
		if err != nil {
			return err
		}
	}
	if u.sup.cfg.seams.beforeDrop != nil {
		u.sup.cfg.seams.beforeDrop(job.ID)
	}
	ctx, cancel = u.op()
	defer cancel()
	if err := u.p.DropShadow(ctx, job.ID); err != nil {
		return err
	}
	u.reached("dropped")
	return u.p.CompleteWork(ctx, u.fence, u.sup.cfg.Now(), nil)
}

// drain waits, for a replace, until nothing in the process writes the
// tenant's catalog in use (Config.Drain), before the build reads a storage
// and so before the activation: a file archived after the build passed its
// scan would leave the catalog at the activation. A compare changes no
// catalog and waits for nothing. It runs at every start of the unit, a
// build resumed after a restart included, since what it waits for may have
// started again in between.
func (u *unitRun) drain() error {
	if u.sup.cfg.Drain == nil || u.job.Rebuild == nil || u.job.Rebuild.Mode != tenant.RebuildReplace {
		return nil
	}
	if err := u.sup.cfg.Drain(u.ctx, u.t.name); err != nil {
		if u.ctx.Err() != nil {
			return errStopped
		}
		return errors.Join(errDrain, err)
	}
	u.log.Info("rebuilds: the tenant's receptions are stopped; the shadow is built")
	return nil
}

// end compares the shadow and ends the job, or makes it the catalog in use.
func (u *unitRun) end() error {
	if u.job.Rebuild != nil && u.job.Rebuild.Mode == tenant.RebuildReplace {
		ctx, cancel := context.WithTimeout(u.ctx, activationTimeout)
		defer cancel()
		j, err := u.p.ActivateCatalog(ctx, u.fence, u.sup.cfg.Now())
		if err == nil {
			u.reached("activated")
			u.log.Info("rebuilds: the shadow is the catalog in use", "instances", j.Counts.InstancesSent, "left_out", j.Counts.InstancesRefused)
		}
		return err
	}
	ctx, cancel := context.WithTimeout(u.ctx, activationTimeout)
	defer cancel()
	cmp, err := u.p.CompareCatalog(ctx, u.fence)
	if err != nil {
		return err
	}
	j, err := u.p.EndRebuild(ctx, u.fence, cmp, u.sup.cfg.Now())
	if err == nil {
		u.reached("compared")
		u.log.Info("rebuilds: compared", "equal", cmp.Equal(), "tables_differing", len(cmp.Tables), "instances", j.Counts.InstancesSent)
	}
	return err
}

// storageNames returns the names of the tenant's storages a catalog is made
// from, in order, and the storage each is read from (by name, the storage
// itself unless named otherwise): every storage but a replica, which holds
// its primary's keys, so a scan read from both would be one series on two
// storages, and one read from the replica alone would name the replica as
// where the catalog's files are.
//
// A compare that names the one storage it reads from
// (tenant.RebuildJob.FromStorage) reads that storage alone: a replica is
// read for its primary, the scans it holds written under the primary's
// name, so the compare says whether the replica alone holds the catalog's
// scans of its primary as the catalog places them (what a switch to it
// needs); another storage is read for itself.
func (u *unitRun) storageNames() ([]string, map[string]string, error) {
	var names []string
	readFrom := map[string]string{}
	from := ""
	if u.job.Rebuild != nil {
		from = u.job.Rebuild.FromStorage
	}
	cursor := ""
	for {
		ctx, cancel := u.op()
		page, next, err := u.p.ListStorages(ctx, master.Page{Limit: 1000, Cursor: cursor})
		cancel()
		if err != nil {
			return nil, nil, err
		}
		for _, s := range page {
			switch {
			case from == "":
				if s.ReplicaOf == "" {
					names = append(names, s.Name)
				}
			case s.Name == from && s.ReplicaOf != "":
				names, readFrom[s.ReplicaOf] = append(names, s.ReplicaOf), s.Name
			case s.Name == from:
				names = append(names, s.Name)
			}
		}
		if cursor = next; cursor == "" {
			if from != "" && len(names) == 0 {
				return nil, nil, fault.New(fault.Precondition, "rebuild: the tenant has no storage named "+from+" to read from")
			}
			slices.Sort(names)
			return names, readFrom, nil
		}
	}
}

// build writes every scan of every storage into the shadow, from where the
// shadow's cursor stands, and finishes it.
func (u *unitRun) build() error {
	ctx, cancel := u.op()
	prog, err := u.p.PrepareShadow(ctx, u.fence)
	cancel()
	if err != nil {
		return err
	}
	if prog.Complete {
		return nil
	}
	names, readFrom, err := u.storageNames()
	if err != nil {
		return err
	}
	ctx, cancel = u.op()
	tenantID, err := u.sup.cfg.TenantID(ctx, u.t.name)
	cancel()
	if err != nil {
		return err
	}
	auth := recordAuth{tenantID: tenantID, keys: u.sup.cfg.Keys}
	stores := u.sup.cfg.OpenStorages(u.t.name, u.p)
	defer stores.Close()
	for _, name := range names {
		if name < prog.Cursor.Storage {
			continue
		}
		if err := u.stopping(); err != nil {
			return err
		}
		if name != prog.Cursor.Storage {
			ctx, cancel := u.op()
			prog, err = u.p.FinishRebuildStorage(ctx, u.fence, prog.Cursor.Storage, name, nil)
			cancel()
			if err != nil {
				return err
			}
		}
		source := name
		if r, ok := readFrom[name]; ok {
			source = r
		}
		bs, err := stores.Get(u.ctx, source)
		if err != nil {
			return storageErr(err)
		}
		var st Storage = bs
		var decide decider
		if d := u.sup.cfg.Decide; d != nil {
			decide = func(c record.Correction) (record.Correction, bool, error) { return d(u.ctx, u.p, c) }
		}
		h, err := readHides(u.ctx, st, auth, decide)
		if err != nil {
			return storageErr(err)
		}
		if h.unauthenticated > 0 {
			u.log.Warn("rebuilds: correction records on a storage do not verify under the installation key; they hide nothing",
				"storage", name, "records", h.unauthenticated)
		}
		if err := listScans(u.ctx, st, prog.Cursor.After, func(k scanKeys) error {
			if err := u.stopping(); err != nil {
				return err
			}
			var err error
			prog, err = u.buildScan(st, name, k, h, prog)
			if err == nil {
				u.reached("scan")
			}
			if err == nil && u.sup.cfg.seams.afterScan != nil {
				u.sup.cfg.seams.afterScan(k.key)
			}
			return err
		}); err != nil {
			return storageErr(err)
		}
		// The storage's own tallies ride on the move past it, the one
		// write made once per storage however often the unit is resumed.
		tallies := map[string]int64{}
		if h.unresolved > 0 {
			tallies[tenant.TallyUnresolvedCorrection] = h.unresolved
		}
		if h.unauthenticated > 0 {
			tallies[tenant.TallyUnauthenticatedCorrection] = h.unauthenticated
		}
		if len(tallies) == 0 {
			tallies = nil
		}
		next := ""
		if i := slices.Index(names, name); i+1 < len(names) {
			next = names[i+1]
		}
		ctx, cancel := u.op()
		prog, err = u.p.FinishRebuildStorage(ctx, u.fence, name, next, tallies)
		cancel()
		if err != nil {
			return err
		}
	}
	if !prog.Complete {
		ctx, cancel := u.op()
		prog, err = u.p.FinishRebuildStorage(ctx, u.fence, prog.Cursor.Storage, "", nil)
		cancel()
	}
	return err
}

// buildScan reads one scan and writes it into the shadow, or what of it was
// left out and why.
func (u *unitRun) buildScan(st Storage, storage string, k scanKeys, h *hides, prog tenant.RebuildProgress) (tenant.RebuildProgress, error) {
	r, err := readScan(u.ctx, st, storage, k.path)
	if err != nil {
		return prog, err
	}
	leaveOut := func(tally string, files, bytes int64) (tenant.RebuildProgress, error) {
		ctx, cancel := u.op()
		defer cancel()
		return u.p.RebuildScan(ctx, tenant.RebuildScanWrite{Fence: u.fence, From: prog.Cursor, Key: k.key, Done: true, LeftOut: files,
			LeftOutBytes: bytes, Tallies: map[string]int64{tally: 1}})
	}
	if r.unreadable != "" {
		u.log.Warn("rebuilds: a scan is left out", "subject", k.path.Subject, "session", k.path.Session, "scan", k.path.Scan,
			"why", r.unreadable)
		return leaveOut(tenant.TallyLeftOutUnreadable, r.files, r.bytes)
	}
	key := hideKey(k.path, r.scan.SeriesUID)
	if h.scans[key] {
		return leaveOut(tenant.TallyLeftOutDeleted, r.files, r.bytes)
	}
	var hidden, hiddenBytes int64
	if gone := h.instances[key]; len(gone) > 0 {
		kept := r.instances[:0]
		for _, in := range r.instances {
			if gone[in.SOPInstanceUID] {
				hidden++
				hiddenBytes += in.Length
				continue
			}
			kept = append(kept, in)
		}
		r.instances = kept
		if len(kept) == 0 {
			return leaveOut(tenant.TallyLeftOutDeleted, hidden, hiddenBytes)
		}
	}
	if known, err := u.profilesKnown(r.instances); err != nil || !known {
		if err != nil {
			return prog, err
		}
		return leaveOut(tenant.TallyLeftOutProfileUnknown, r.files, r.bytes)
	}
	first := prog.Cursor.Partial != k.key
	if first {
		// Every instance of the scan is asked of the shadow before its
		// first write, as its profiles are: an instance another scan holds
		// met in a later batch, after the scan was written in part, could
		// no longer leave the scan out whole.
		holder, err := u.instancesHeld(r.instances)
		if err != nil {
			return prog, err
		}
		if holder != "" {
			tally := tenant.TallyLeftOutHeldElsewhere
			if holder != storage {
				tally = tenant.TallySeriesOnTwoStorages
			}
			u.log.Warn("rebuilds: a scan is left out", "subject", k.path.Subject, "session", k.path.Session, "scan", k.path.Scan,
				"why", tally)
			return leaveOut(tally, r.files, r.bytes)
		}
	}
	ins := r.instances
	if !first {
		for len(ins) > 0 && ins[0].SOPInstanceUID <= prog.Cursor.PartialUID {
			ins = ins[1:]
		}
	}
	for {
		n := min(u.sup.cfg.seams.batch, len(ins))
		w := tenant.RebuildScanWrite{Fence: u.fence, From: prog.Cursor, Key: k.key, Scan: &r.scan, Instances: ins[:n], Done: n == len(ins)}
		if w.Done && hidden > 0 {
			w.LeftOut, w.LeftOutBytes, w.Tallies = hidden, hiddenBytes, map[string]int64{tenant.TallyLeftOutDeleted: 1}
		}
		ctx, cancel := u.op()
		next, err := u.p.RebuildScan(ctx, w)
		cancel()
		if err != nil {
			if tally := refusedTally(err); tally != "" && first {
				u.log.Warn("rebuilds: a scan is left out", "subject", k.path.Subject, "session", k.path.Session, "scan", k.path.Scan,
					"why", tally)
				return leaveOut(tally, r.files, r.bytes)
			}
			return prog, err
		}
		prog, first = next, false
		if ins = ins[n:]; len(ins) == 0 {
			return prog, nil
		}
	}
}

// instancesHeld returns the storage of a scan of the shadow that holds one
// of the instances, "" when none does, asking the shadow a batch at a
// time.
func (u *unitRun) instancesHeld(ins []tenant.RebuiltInstance) (string, error) {
	for len(ins) > 0 {
		n := min(tenant.MaxRebuildBatch, len(ins))
		uids := make([]string, n)
		for i, in := range ins[:n] {
			uids[i] = in.SOPInstanceUID
		}
		ctx, cancel := u.op()
		holder, err := u.p.ShadowHolder(ctx, u.fence, uids)
		cancel()
		if err != nil || holder != "" {
			return holder, err
		}
		ins = ins[n:]
	}
	return "", nil
}

// refusedTally returns the tally of a scan the shadow refused whole, or ""
// for any other failure: a series or an instance held by a scan of another
// storage is two storages' series, one held by a scan of the same storage
// is that storage naming it twice.
func refusedTally(err error) string {
	var lc *tenant.LabelConflict
	switch {
	case errors.As(err, &lc):
		return tenant.TallyLeftOutLabelConflict
	case errors.Is(err, tenant.ErrOtherStorage):
		return tenant.TallySeriesOnTwoStorages
	case errors.Is(err, tenant.ErrSeriesElsewhere), errors.Is(err, tenant.ErrInstanceElsewhere):
		return tenant.TallyLeftOutHeldElsewhere
	case errors.Is(err, tenant.ErrProfileUnknown):
		return tenant.TallyLeftOutProfileUnknown
	}
	return ""
}

// profilesKnown reports whether the tenant has every profile version the
// instances name, so a scan is never written in part and then refused.
func (u *unitRun) profilesKnown(ins []tenant.RebuiltInstance) (bool, error) {
	seen := map[tenant.ProfileRef]bool{}
	for _, in := range ins {
		if in.Profile.IsZero() || seen[in.Profile] {
			continue
		}
		seen[in.Profile] = true
		ctx, cancel := u.op()
		_, err := u.p.ProfileVersion(ctx, in.Profile.Name, in.Profile.Version)
		cancel()
		if fault.KindOf(err) == fault.NotFound {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}
