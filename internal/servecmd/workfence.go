package servecmd

import (
	"context"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant another installation took over (fenced), or an export handed
// off (moved), must have nothing more written for it here: two
// installations writing one storage, or a write after an export's snapshot,
// leaves a copy no catalog describes. The registry refuses such a tenant's
// store to every new writer, but the background work holds the store it was
// started over, and its workers run for minutes (a copy, a repair). So each
// write they make, to the tenant's database or to a storage, asks the
// registry's fence first (registry.Registry.CheckFence), as a correction's
// step does, and is refused with the fence's error; the work itself is
// stopped as soon as the fence is seen (tenantWork.Fenced), and an export's
// drain waits for it (tenantWork.Quiesce).

// fencedDB is a tenant's database as its background work writes it: every
// write the replicator, the recall worker, the repairs and retention make
// asks fence first and is refused, writing nothing, when it errs. Reads
// pass through unasked.
type fencedDB struct {
	tenantDB
	fence func() error
}

// fenceDB returns st with its writes fenced by fence; a nil fence (no
// registry) returns st as it is.
func fenceDB(st tenantDB, fence func() error) tenantDB {
	if fence == nil {
		return st
	}
	return fencedDB{tenantDB: st, fence: fence}
}

// ClaimReplicaScans asks the fence, then claims.
func (d fencedDB) ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error) {
	if err := d.fence(); err != nil {
		return nil, err
	}
	return d.tenantDB.ClaimReplicaScans(ctx, owner, now, lease, limit)
}

// RenewReplicaScan asks the fence, then renews.
func (d fencedDB) RenewReplicaScan(ctx context.Context, fence tenant.ReplicaFence, now time.Time, lease time.Duration) error {
	if err := d.fence(); err != nil {
		return err
	}
	return d.tenantDB.RenewReplicaScan(ctx, fence, now, lease)
}

// EndReplicaScan asks the fence, then ends the scan.
func (d fencedDB) EndReplicaScan(ctx context.Context, fence tenant.ReplicaFence, manifest tenant.SHA256, now time.Time) (tenant.ReplicaScanState, error) {
	if err := d.fence(); err != nil {
		return "", err
	}
	return d.tenantDB.EndReplicaScan(ctx, fence, manifest, now)
}

// FailReplicaScan asks the fence, then records the failure.
func (d fencedDB) FailReplicaScan(ctx context.Context, f tenant.ReplicaFailure) error {
	if err := d.fence(); err != nil {
		return err
	}
	return d.tenantDB.FailReplicaScan(ctx, f)
}

// RecordReplicaDamage asks the fence, then records the damage.
func (d fencedDB) RecordReplicaDamage(ctx context.Context, r tenant.ReplicaDamageRecord) (bool, error) {
	if err := d.fence(); err != nil {
		return false, err
	}
	return d.tenantDB.RecordReplicaDamage(ctx, r)
}

// RecordRecall asks the fence, then records the store's answer.
func (d fencedDB) RecordRecall(ctx context.Context, a tenant.RecallAnswer) (tenant.Recall, error) {
	if err := d.fence(); err != nil {
		return tenant.Recall{}, err
	}
	return d.tenantDB.RecordRecall(ctx, a)
}

// RecordRepair asks the fence, then records the repair.
func (d fencedDB) RecordRepair(ctx context.Context, r tenant.RepairRecord) (tenant.Repair, bool, error) {
	if err := d.fence(); err != nil {
		return tenant.Repair{}, false, err
	}
	return d.tenantDB.RecordRepair(ctx, r)
}

// ReleaseRepair asks the fence, then releases.
func (d fencedDB) ReleaseRepair(ctx context.Context, fence tenant.WorkFence, id string) error {
	if err := d.fence(); err != nil {
		return err
	}
	return d.tenantDB.ReleaseRepair(ctx, fence, id)
}

// CompleteRepair asks the fence, then completes.
func (d fencedDB) CompleteRepair(ctx context.Context, fence *tenant.WorkFence, id string, now time.Time, audit master.AuditEntry) (tenant.Repair, error) {
	if err := d.fence(); err != nil {
		return tenant.Repair{}, err
	}
	return d.tenantDB.CompleteRepair(ctx, fence, id, now, audit)
}

// RecordRepairOutcome asks the fence, then records the outcome.
func (d fencedDB) RecordRepairOutcome(ctx context.Context, o tenant.RepairOutcome) (tenant.Job, error) {
	if err := d.fence(); err != nil {
		return tenant.Job{}, err
	}
	return d.tenantDB.RecordRepairOutcome(ctx, o)
}

// EndRepairJob asks the fence, then ends the job.
func (d fencedDB) EndRepairJob(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	if err := d.fence(); err != nil {
		return tenant.Job{}, err
	}
	return d.tenantDB.EndRepairJob(ctx, fence, now)
}

// ClaimWork asks the fence, then claims.
func (d fencedDB) ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.Work, error) {
	if err := d.fence(); err != nil {
		return tenant.Work{}, err
	}
	return d.tenantDB.ClaimWork(ctx, kind, owner, now, lease)
}

// RenewWork asks the fence, then renews.
func (d fencedDB) RenewWork(ctx context.Context, fence tenant.WorkFence, now time.Time, lease time.Duration) (tenant.Work, error) {
	if err := d.fence(); err != nil {
		return tenant.Work{}, err
	}
	return d.tenantDB.RenewWork(ctx, fence, now, lease)
}

// FailWork asks the fence, then fails the unit.
func (d fencedDB) FailWork(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Work, error) {
	if err := d.fence(); err != nil {
		return tenant.Work{}, err
	}
	return d.tenantDB.FailWork(ctx, fence, reason, now)
}

// StartJobUnit asks the fence, then starts the unit.
func (d fencedDB) StartJobUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.JobUnit, error) {
	if err := d.fence(); err != nil {
		return tenant.JobUnit{}, err
	}
	return d.tenantDB.StartJobUnit(ctx, fence, now)
}

// CreateJob asks the fence, then makes the job.
func (d fencedDB) CreateJob(ctx context.Context, j tenant.NewJob, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := d.fence(); err != nil {
		return tenant.Job{}, err
	}
	return d.tenantDB.CreateJob(ctx, j, audit, guard)
}

// CheckRetentionClock asks the fence, then checks and records the clock.
func (d fencedDB) CheckRetentionClock(ctx context.Context, c tenant.RetentionClockCheck) (tenant.RetentionClock, error) {
	if err := d.fence(); err != nil {
		return tenant.RetentionClock{}, err
	}
	return d.tenantDB.CheckRetentionClock(ctx, c)
}

// QueueDispose asks the fence, then queues.
func (d fencedDB) QueueDispose(ctx context.Context, q tenant.DisposeQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job,
	tenant.DisposalPreview, bool, error) {
	if err := d.fence(); err != nil {
		return tenant.Job{}, tenant.DisposalPreview{}, false, err
	}
	return d.tenantDB.QueueDispose(ctx, q, audit, guard)
}

// RecordDisposed asks the fence, then records the outcome.
func (d fencedDB) RecordDisposed(ctx context.Context, jobID, studyUID string, outcome tenant.DisposeOutcome, correction string) error {
	if err := d.fence(); err != nil {
		return err
	}
	return d.tenantDB.RecordDisposed(ctx, jobID, studyUID, outcome, correction)
}

// fencedOpen returns open whose stores ask fence before each write of
// bytes or of state (Put, Delete, Purge, a recall); a nil fence returns
// open as it is.
func fencedOpen(open func(ctx context.Context, storage string) (blob.Store, error), fence func() error) func(ctx context.Context,
	storage string) (blob.Store, error) {
	if fence == nil {
		return open
	}
	return func(ctx context.Context, storage string) (blob.Store, error) {
		bs, err := open(ctx, storage)
		if err != nil {
			return nil, err
		}
		fs := fencedStore{Store: bs, fence: fence}
		// A store that takes recalls keeps that part, so the recall worker
		// still finds it (blob.Recaller).
		if rc, ok := bs.(blob.Recaller); ok {
			return fencedRecaller{fencedStore: fs, rc: rc}, nil
		}
		return fs, nil
	}
}

// fencedStore is a storage whose writes ask fence first.
type fencedStore struct {
	blob.Store
	fence func() error
}

// Put asks the fence, then puts.
func (s fencedStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if err := s.fence(); err != nil {
		return blob.Written{}, err
	}
	return s.Store.Put(ctx, key, r)
}

// Delete asks the fence, then deletes.
func (s fencedStore) Delete(ctx context.Context, key string) error {
	if err := s.fence(); err != nil {
		return err
	}
	return s.Store.Delete(ctx, key)
}

// Purge asks the fence, then purges.
func (s fencedStore) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	if err := s.fence(); err != nil {
		return nil, err
	}
	return s.Store.Purge(ctx, key)
}

// fencedRecaller is a fencedStore of a store that takes recalls.
type fencedRecaller struct {
	fencedStore
	rc blob.Recaller
}

// Recall asks the fence, then asks the store to restore the object.
func (s fencedRecaller) Recall(ctx context.Context, key string, req blob.RecallRequest) (blob.Recall, error) {
	if err := s.fence(); err != nil {
		return blob.Recall{}, err
	}
	return s.rc.Recall(ctx, key, req)
}

// fenceNotice returns check, as the registry calls it (registry.Fence),
// telling fenced (when not nil) the name of each tenant check fences, so
// its background work stops at once rather than at the next reading of the
// tenants. The registry holds the tenant's entry locked while it calls a
// Fence, so fenced must not block or call the registry.
func fenceNotice(check registry.Fence, fenced func(name string)) registry.Fence {
	return func(ctx context.Context, id, name string, st tenant.Store) (string, error) {
		reason, err := check(ctx, id, name, st)
		if err == nil && reason != "" && fenced != nil {
			fenced(name)
		}
		return reason, err
	}
}
