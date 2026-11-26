package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/repair"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pace of a tenant's repair worker.
const (
	// repairIdle is how long the worker waits after a claim that found no
	// repair job due, or a WakeRepairs, whichever comes first.
	repairIdle = 30 * time.Second
	// repairRetryWait is how long it waits after a job whose objects could
	// not all be repaired now, before it claims again: the unit is due again
	// only after its own retry delay, so claiming at once finds nothing.
	repairRetryWait = 10 * time.Second
	// autoRepairInterval is how often, with replica.repair = auto, the
	// findings of the tenant's primaries are looked at for damage a scrub
	// found since the last automatic repair: a scrub of a storage runs for
	// hours, so five minutes adds little to the wait.
	autoRepairInterval = 5 * time.Minute
	// repairPutFloor is the speed, in bytes a second, the check of
	// replica.repair-lease assumes a put to a primary reaches when no speed
	// was measured: 100 MiB a second, a local disk's or a bucket's in the
	// same network. A lease too short to put the largest file a receiver of
	// the tenant accepts at that speed is refused before any repair runs,
	// since a put cut short at its lease's due is tried again at the next
	// attempt and cut short again, and the object is never repaired.
	repairPutFloor = 100 << 20
)

// tenantRepairs runs one tenant's repair jobs (package repair) and, with
// replica.repair = auto, makes an automatic one after a scrub found damage.
type tenantRepairs struct {
	w    *tenantWork
	name string
	st   tenantDB
	open func(ctx context.Context, storage string) (blob.Store, error)
	gate repair.WritePause
	wake chan struct{}
	log  *slog.Logger

	// refused is the lease last refused, so the refusal is logged once per
	// value, not once a claim.
	refused time.Duration
	// largest and largestAt are the largest instance the catalog holds and
	// when it was read (largestHeld).
	largest   int64
	largestAt time.Time
}

// run claims and runs the tenant's repair jobs until ctx ends. The lease is
// read before each claim, so a change of replica.repair-lease applies to
// the next job; a lease too short is refused (checkRepairLease) and nothing
// is claimed until it is lengthened.
func (r *tenantRepairs) run(ctx context.Context) {
	for ctx.Err() == nil {
		runner, err := r.runner(ctx)
		if err != nil {
			r.wait(ctx, repairIdle)
			continue
		}
		res, err := runner.Run(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, repair.ErrNotDue):
			r.wait(ctx, repairIdle)
		case errors.Is(err, repair.ErrTryAgain), errors.Is(err, repair.ErrGaveUp):
			r.log.Warn("repairs: a repair job left objects to repair later", "job", res.Job, "repaired", res.Counts[repair.Repaired],
				"retry", res.Counts[repair.Retry], "gave_up", errors.Is(err, repair.ErrGaveUp))
			r.wait(ctx, repairRetryWait)
		case err != nil:
			r.log.Warn("repairs: a repair job's run failed; tried again later", "job", res.Job, "kind", fault.KindOf(err).String())
			r.wait(ctx, repairIdle)
		case res.Done:
			r.log.Info("repairs: a repair job ended", "job", res.Job, "repaired", res.Counts[repair.Repaired],
				"already_good", res.Counts[repair.AlreadyGood], "no_good_copy", res.Counts[repair.NoGoodCopy])
		}
	}
}

// wait waits d, a WakeRepairs, or ctx's end.
func (r *tenantRepairs) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-r.wake:
	case <-t.C:
	}
}

// runner returns the repair runner of the tenant with the lease
// replica.repair-lease gives now, once checkRepairLease takes it.
func (r *tenantRepairs) runner(ctx context.Context) (*repair.Runner, error) {
	lease, err := r.w.values.OnTenant(r.st).RepairLease(ctx)
	if err != nil {
		return nil, err
	}
	held, err := r.largestHeld(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkRepairLease(ctx, r.st, lease, held); err != nil {
		if lease != r.refused {
			r.refused = lease
			r.log.Error("repairs: replica.repair-lease is refused; no repair runs until it is lengthened", "lease", lease.String(),
				"reason", fault.Message(err))
		}
		return nil, err
	}
	r.refused = 0
	return repair.New(repair.Config{DB: r.st, Tenant: r.name, Open: r.open, Now: r.w.now, Owner: r.w.owner, Lease: lease,
		Pause: r.gate, Log: r.log})
}

// largestHeld returns the largest instance the tenant's catalog holds
// (tenant.InstanceSizes), read again at most every largestHeldInterval:
// the read goes through every instance row, too costly before each claim,
// and an instance larger than every receiver's bound arrives only through
// a bound since lowered or an import, both rare.
func (r *tenantRepairs) largestHeld(ctx context.Context) (int64, error) {
	now := r.w.now()
	if !r.largestAt.IsZero() && now.Sub(r.largestAt) < largestHeldInterval && !now.Before(r.largestAt) {
		return r.largest, nil
	}
	n, err := r.st.LargestInstance(ctx)
	if err != nil {
		return 0, err
	}
	r.largest, r.largestAt = n, now
	return n, nil
}

// largestHeldInterval is how long the largest instance read is kept.
const largestHeldInterval = time.Hour

// checkRepairLease refuses a lease too short to put the largest object the
// tenant may hold at repairPutFloor: each put of a repair has the lease
// less repair.DefaultPutMargin, and the largest object is the larger of
// held, the largest instance the catalog holds (one archived under a
// bound since lowered counts), and the largest file a receiver of the
// tenant accepts, of every type (DIMSE, STOW-RS and import receivers alike
// are listed, each with its own MaxFileBytes), or tenant.DefaultMaxFileBytes
// when it has none. No speed of the storage is measured yet, so the floor
// is a fixed one rather than the storage's own.
func checkRepairLease(ctx context.Context, st tenant.Pipeline, lease time.Duration, held int64) error {
	largest := int64(0)
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rs, next, err := st.ListReceivers(ctx, page)
		if err != nil {
			return err
		}
		for _, r := range rs {
			largest = max(largest, r.MaxFileBytes)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if largest == 0 {
		largest = tenant.DefaultMaxFileBytes
	}
	what := "the largest file a receiver of the tenant accepts"
	if held > largest {
		largest, what = held, "the largest instance the tenant holds"
	}
	window := lease - repair.DefaultPutMargin
	need := time.Duration(largest/repairPutFloor+1) * time.Second
	if window < need {
		return fault.New(fault.Invalid, "a repair's put has "+window.String()+" within a lease of "+lease.String()+
			", and "+what+" ("+strconv.FormatInt(largest, 10)+" bytes) needs "+need.String()+
			" at 100 MiB a second; set replica.repair-lease to at least "+(need+repair.DefaultPutMargin).String())
	}
	return nil
}

// autoLoop makes an automatic repair job, with replica.repair = auto, for
// each primary that has a replica and holds missing or damaged findings
// found since its last automatic repair job, and none running.
func (r *tenantRepairs) autoLoop(ctx context.Context) {
	t := time.NewTicker(autoRepairInterval)
	defer t.Stop()
	for {
		if err := r.autoOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("repairs: the automatic repairs could not be looked at; tried again later", "kind", fault.KindOf(err).String())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// autoOnce is one look of autoLoop.
func (r *tenantRepairs) autoOnce(ctx context.Context) error {
	mode, err := r.w.values.OnTenant(r.st).ReplicaRepair(ctx)
	if err != nil || mode != string(repair.ModeAuto) {
		return err
	}
	primaries, err := r.primariesWithReplicas(ctx)
	if err != nil {
		return err
	}
	jobs, err := r.repairJobs(ctx)
	if err != nil {
		return err
	}
	made := false
	for _, p := range primaries {
		s := jobs[p]
		if s.running {
			continue
		}
		newer, err := r.foundSince(ctx, p, s)
		if err != nil {
			return err
		}
		if !newer {
			continue
		}
		now := r.w.now().UTC().Truncate(time.Microsecond)
		j := repair.Job{ID: server.NewID(), Storage: p, Mode: repair.ModeAuto}
		if _, err := r.st.CreateJob(ctx, j.NewJob(now), serverAudit(r.name, "repair.automatic", p, now), nil); err != nil {
			return err
		}
		r.log.Info("repairs: an automatic repair job was made after a scrub found damage", "storage", p, "job", j.ID)
		made = true
	}
	if made {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// foundSince reports whether the primary named holds a missing or damaged
// finding found since its newest automatic repair job (any, when it has
// none), reading the findings page after page until one is found: the
// findings do not come newest first, so the one that matters may be on any
// page, and a storage of many objects holds more than one page of them.
func (r *tenantRepairs) foundSince(ctx context.Context, primary string, s repairState) (bool, error) {
	filter := tenant.ScrubFindingFilter{Storage: primary, Kinds: []tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged}}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		fs, next, err := r.st.ListScrubFindings(ctx, filter, page)
		if err != nil {
			return false, err
		}
		for _, f := range fs {
			if !s.ok || f.Found.After(s.created) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		page.Cursor = next
	}
}

// primariesWithReplicas returns the names of the tenant's primaries that
// have a replica: a repair reads only from a replica.
func (r *tenantRepairs) primariesWithReplicas(ctx context.Context) ([]string, error) {
	var rows []tenant.Storage
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		batch, next, err := r.st.ListStorages(ctx, page)
		if err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	has := map[string]bool{}
	for _, s := range rows {
		if s.ReplicaOf != "" {
			has[s.ReplicaOf] = true
		}
	}
	var out []string
	for _, s := range rows {
		if s.ReplicaOf == "" && has[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

// repairState is what the tenant's repair jobs say of one storage: when its
// newest automatic job was made, and whether a repair job of it has not
// ended.
type repairState struct {
	created time.Time
	ok      bool
	running bool
}

// repairJobs returns, per storage, its repair jobs' state, read from the
// newest jobs first, at most maxRepairJobPages pages.
func (r *tenantRepairs) repairJobs(ctx context.Context) (map[string]repairState, error) {
	out := map[string]repairState{}
	page := master.Page{Limit: master.MaxPageLimit}
	for pages := 0; pages < maxRepairJobPages; pages++ {
		jobs, next, err := r.st.ListJobs(ctx, tenant.JobFilter{}, page)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Kind != tenant.JobRepair || j.Repair == nil {
				continue
			}
			s := out[j.Repair.Storage]
			if !j.State.Ended() {
				s.running = true
			}
			if j.Repair.Automatic && (!s.ok || j.Created.After(s.created)) {
				s.created, s.ok = j.Created, true
			}
			out[j.Repair.Storage] = s
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return out, nil
}

// maxRepairJobPages bounds the pages of jobs one look reads: the newest
// jobs come first, and an automatic repair is made at most every
// autoRepairInterval per storage, so its last one is near the top.
const maxRepairJobPages = 10
