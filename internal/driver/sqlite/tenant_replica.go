package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's replicas, recalls, repairs and switches (tenant.Replication,
// tenant.Recalls, tenant.Repairs, tenant.Switching), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one implementation,
// run here in SQLite's transactions, as the senders' and jobs' are.

// AttachReplica implements tenant.Replication.
func (t *Tenant) AttachReplica(ctx context.Context, replica, primary string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Storage, int64, bool, error) {
	return t.sending().AttachReplica(ctx, replica, primary, now, audit, guard)
}

// DetachReplica implements tenant.Replication.
func (t *Tenant) DetachReplica(ctx context.Context, d tenant.ReplicaDetach, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, []string, error) {
	return t.sending().DetachReplica(ctx, d, audit, guard)
}

// BackfillReplica implements tenant.Replication.
func (t *Tenant) BackfillReplica(ctx context.Context, replica string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (int64, error) {
	return t.sending().BackfillReplica(ctx, replica, now, audit, guard)
}

// ListReplicas implements tenant.Replication.
func (t *Tenant) ListReplicas(ctx context.Context, primary string, page master.Page) ([]tenant.ReplicaStatus, string, error) {
	return t.sending().ListReplicas(ctx, primary, page)
}

// ReplicaScan implements tenant.Replication.
func (t *Tenant) ReplicaScan(ctx context.Context, replica, seriesUID string) (tenant.ReplicaScan, error) {
	return t.sending().ReplicaScan(ctx, replica, seriesUID)
}

// InStepReplicas implements tenant.Replication.
func (t *Tenant) InStepReplicas(ctx context.Context, seriesUIDs []string) (map[string][]string, error) {
	return t.sending().InStepReplicas(ctx, seriesUIDs)
}

// ClaimReplicaScans implements tenant.Replication.
func (t *Tenant) ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error) {
	return t.sending().ClaimReplicaScans(ctx, owner, now, lease, limit)
}

// RenewReplicaScan implements tenant.Replication.
func (t *Tenant) RenewReplicaScan(ctx context.Context, fence tenant.ReplicaFence, now time.Time, lease time.Duration) error {
	return t.sending().RenewReplicaScan(ctx, fence, now, lease)
}

// EndReplicaScan implements tenant.Replication.
func (t *Tenant) EndReplicaScan(ctx context.Context, fence tenant.ReplicaFence, manifest tenant.SHA256, now time.Time) (tenant.ReplicaScanState, error) {
	return t.sending().EndReplicaScan(ctx, fence, manifest, now)
}

// FailReplicaScan implements tenant.Replication.
func (t *Tenant) FailReplicaScan(ctx context.Context, f tenant.ReplicaFailure) error {
	return t.sending().FailReplicaScan(ctx, f)
}

// RecordReplicaDamage implements tenant.Replication.
func (t *Tenant) RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error) {
	return t.sending().RecordReplicaDamage(ctx, d)
}

// ClearReplicaDamage implements tenant.Replication.
func (t *Tenant) ClearReplicaDamage(ctx context.Context, fence *tenant.WorkFence, replica, seriesUID, sopUID string, now time.Time) (bool, error) {
	return t.sending().ClearReplicaDamage(ctx, fence, replica, seriesUID, sopUID, now)
}

// ReplicaDamaged implements tenant.Replication.
func (t *Tenant) ReplicaDamaged(ctx context.Context, replica, seriesUID string) (map[string]string, error) {
	return t.sending().ReplicaDamaged(ctx, replica, seriesUID)
}

// RequestRecalls implements tenant.Recalls.
func (t *Tenant) RequestRecalls(ctx context.Context, a tenant.RecallAsk) (tenant.RecallAsked, error) {
	return t.sending().RequestRecalls(ctx, a)
}

// Recall implements tenant.Recalls.
func (t *Tenant) Recall(ctx context.Context, storage, key string) (tenant.Recall, bool, error) {
	return t.sending().Recall(ctx, storage, key)
}

// DueRecalls implements tenant.Recalls.
func (t *Tenant) DueRecalls(ctx context.Context, now time.Time, limit int) ([]tenant.Recall, error) {
	return t.sending().DueRecalls(ctx, now, limit)
}

// RecordRecall implements tenant.Recalls.
func (t *Tenant) RecordRecall(ctx context.Context, a tenant.RecallAnswer) (tenant.Recall, error) {
	return t.sending().RecordRecall(ctx, a)
}

// RecallCounts implements tenant.Recalls.
func (t *Tenant) RecallCounts(ctx context.Context, now time.Time) (tenant.RecallCounts, error) {
	return t.sending().RecallCounts(ctx, now)
}

// RecordRepair implements tenant.Repairs.
func (t *Tenant) RecordRepair(ctx context.Context, r tenant.RepairRecord) (tenant.Repair, bool, error) {
	return t.sending().RecordRepair(ctx, r)
}

// ReleaseRepair implements tenant.Repairs.
func (t *Tenant) ReleaseRepair(ctx context.Context, fence tenant.WorkFence, id string) error {
	return t.sending().ReleaseRepair(ctx, fence, id)
}

// ScanRepairHeld implements tenant.Repairs.
func (t *Tenant) ScanRepairHeld(ctx context.Context, seriesUID string, now time.Time) (bool, error) {
	return t.sending().ScanRepairHeld(ctx, seriesUID, now)
}

// CompleteRepair implements tenant.Repairs.
func (t *Tenant) CompleteRepair(ctx context.Context, fence *tenant.WorkFence, id string, now time.Time, audit master.AuditEntry) (tenant.Repair, error) {
	return t.sending().CompleteRepair(ctx, fence, id, now, audit)
}

// Repair implements tenant.Repairs.
func (t *Tenant) Repair(ctx context.Context, id string) (tenant.Repair, error) {
	return t.sending().Repair(ctx, id)
}

// FindingRepairs implements tenant.Repairs.
func (t *Tenant) FindingRepairs(ctx context.Context, findingID string) ([]tenant.Repair, error) {
	return t.sending().FindingRepairs(ctx, findingID)
}

// RepairFindings implements tenant.Repairs.
func (t *Tenant) RepairFindings(ctx context.Context, ids []string) ([]tenant.ScrubFinding, error) {
	return t.sending().RepairFindings(ctx, ids)
}

// RecordRepairOutcome implements tenant.Repairs.
func (t *Tenant) RecordRepairOutcome(ctx context.Context, o tenant.RepairOutcome) (tenant.Job, error) {
	return t.sending().RecordRepairOutcome(ctx, o)
}

// EndRepairJob implements tenant.Repairs.
func (t *Tenant) EndRepairJob(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	return t.sending().EndRepairJob(ctx, fence, now)
}

// SwitchFacts implements tenant.Switching.
func (t *Tenant) SwitchFacts(ctx context.Context, primary, replica string) (tenant.SwitchFacts, error) {
	return t.sending().SwitchFacts(ctx, primary, replica)
}

// BeginSwitch implements tenant.Switching.
func (t *Tenant) BeginSwitch(ctx context.Context, s tenant.SwitchStart, audit master.AuditEntry, guard tenant.Guard) (tenant.StorageSwitch, bool, error) {
	return t.sending().BeginSwitch(ctx, s, audit, guard)
}

// SwitchStep implements tenant.Switching.
func (t *Tenant) SwitchStep(ctx context.Context, id string, step tenant.SwitchStepName, now time.Time) (tenant.StorageSwitch, error) {
	return t.sending().SwitchStep(ctx, id, step, now)
}

// SwapStorages implements tenant.Switching.
func (t *Tenant) SwapStorages(ctx context.Context, s tenant.SwitchSwap, audit master.AuditEntry) (tenant.StorageSwitch, error) {
	return t.sending().SwapStorages(ctx, s, audit)
}

// RefuseSwitch implements tenant.Switching.
func (t *Tenant) RefuseSwitch(ctx context.Context, id, refusal string, now time.Time, audit master.AuditEntry) (tenant.StorageSwitch, error) {
	return t.sending().RefuseSwitch(ctx, id, refusal, now, audit)
}

// FinishSwitch implements tenant.Switching.
func (t *Tenant) FinishSwitch(ctx context.Context, id string, now time.Time) (tenant.StorageSwitch, error) {
	return t.sending().FinishSwitch(ctx, id, now)
}

// Switch implements tenant.Switching.
func (t *Tenant) Switch(ctx context.Context, id string) (tenant.StorageSwitch, error) {
	return t.sending().Switch(ctx, id)
}

// UnfinishedSwitches implements tenant.Switching.
func (t *Tenant) UnfinishedSwitches(ctx context.Context) ([]tenant.StorageSwitch, error) {
	return t.sending().UnfinishedSwitches(ctx)
}

// LastSwitch implements tenant.Switching.
func (t *Tenant) LastSwitch(ctx context.Context, storage string) (tenant.StorageSwitch, bool, error) {
	return t.sending().LastSwitch(ctx, storage)
}
