package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's retention rules, holds and disposals (tenant.Retention,
// tenant.Holds, tenant.Disposal), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, run here in Postgres's transactions at read committed,
// each change under the tenant's configuration lock.

// AddRetentionRule implements tenant.Retention.
func (t *Tenant) AddRetentionRule(ctx context.Context, r tenant.RetentionRule, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	return t.sending().AddRetentionRule(ctx, r, ch, audit, guard)
}

// SetRetentionRule implements tenant.Retention.
func (t *Tenant) SetRetentionRule(ctx context.Context, r tenant.RetentionRule, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	return t.sending().SetRetentionRule(ctx, r, ch, audit, guard)
}

// RemoveRetentionRule implements tenant.Retention.
func (t *Tenant) RemoveRetentionRule(ctx context.Context, name string, ch tenant.RetentionChange, audit master.AuditEntry,
	guard tenant.Guard) (tenant.RetentionRule, error) {
	return t.sending().RemoveRetentionRule(ctx, name, ch, audit, guard)
}

// RetentionRule implements tenant.Retention.
func (t *Tenant) RetentionRule(ctx context.Context, name string, now time.Time) (tenant.RetentionRule, error) {
	return t.sending().RetentionRule(ctx, name, now)
}

// ListRetentionRules implements tenant.Retention.
func (t *Tenant) ListRetentionRules(ctx context.Context, now time.Time, page master.Page) ([]tenant.RetentionRule, string, error) {
	return t.sending().ListRetentionRules(ctx, now, page)
}

// DueSessions implements tenant.Retention.
func (t *Tenant) DueSessions(ctx context.Context, f tenant.DueFilter, page master.Page) ([]tenant.DueSession, string, error) {
	return t.sending().DueSessions(ctx, f, page)
}

// SessionRetention implements tenant.Retention.
func (t *Tenant) SessionRetention(ctx context.Context, studyUID string) (tenant.SessionRetention, error) {
	return t.sending().SessionRetention(ctx, studyUID)
}

// CheckRetentionClock implements tenant.Retention.
func (t *Tenant) CheckRetentionClock(ctx context.Context, c tenant.RetentionClockCheck) (tenant.RetentionClock, error) {
	return t.sending().CheckRetentionClock(ctx, c)
}

// ConfirmClock implements tenant.Retention.
func (t *Tenant) ConfirmClock(ctx context.Context, c tenant.ClockConfirm, audit master.AuditEntry, guard tenant.Guard) (tenant.RetentionClock,
	bool, error) {
	return t.sending().ConfirmClock(ctx, c, audit, guard)
}

// RetentionClock implements tenant.Retention.
func (t *Tenant) RetentionClock(ctx context.Context) (tenant.RetentionClock, error) {
	return t.sending().RetentionClock(ctx)
}

// QuarantineAging implements tenant.Retention.
func (t *Tenant) QuarantineAging(ctx context.Context) (tenant.QuarantineAging, error) {
	return t.sending().QuarantineAging(ctx)
}

// HoldStorages implements tenant.Holds.
func (t *Tenant) HoldStorages(ctx context.Context, target tenant.HoldTarget) ([]tenant.HoldStorage, error) {
	return t.sending().HoldStorages(ctx, target)
}

// PlaceHold implements tenant.Holds.
func (t *Tenant) PlaceHold(ctx context.Context, p tenant.HoldPlace, audit master.AuditEntry, guard tenant.Guard) (tenant.Hold, bool, error) {
	return t.sending().PlaceHold(ctx, p, audit, guard)
}

// MatchingHold implements tenant.Holds.
func (t *Tenant) MatchingHold(ctx context.Context, target tenant.HoldTarget, reason string) (tenant.Hold, bool, error) {
	return t.sending().MatchingHold(ctx, target, reason)
}

// ReleaseHold implements tenant.Holds.
func (t *Tenant) ReleaseHold(ctx context.Context, r tenant.HoldRelease, audit master.AuditEntry, guard tenant.Guard) (tenant.Hold, error) {
	return t.sending().ReleaseHold(ctx, r, audit, guard)
}

// Hold implements tenant.Holds.
func (t *Tenant) Hold(ctx context.Context, id string) (tenant.Hold, error) {
	return t.sending().Hold(ctx, id)
}

// ListHolds implements tenant.Holds.
func (t *Tenant) ListHolds(ctx context.Context, f tenant.HoldFilter, page master.Page) ([]tenant.Hold, string, error) {
	return t.sending().ListHolds(ctx, f, page)
}

// HoldRecordsOwed implements tenant.Holds.
func (t *Tenant) HoldRecordsOwed(ctx context.Context, limit int) ([]tenant.HoldRecordOwed, error) {
	return t.sending().HoldRecordsOwed(ctx, limit)
}

// MarkHoldRecord implements tenant.Holds.
func (t *Tenant) MarkHoldRecord(ctx context.Context, id, storage string, seq int64) error {
	return t.sending().MarkHoldRecord(ctx, id, storage, seq)
}

// AdoptHold implements tenant.Holds.
func (t *Tenant) AdoptHold(ctx context.Context, f tenant.HoldFound, audit master.AuditEntry) (tenant.Hold, bool, error) {
	return t.sending().AdoptHold(ctx, f, audit)
}

// CorrectionHeld implements tenant.Holds.
func (t *Tenant) CorrectionHeld(ctx context.Context, id string) (string, error) {
	return t.sending().CorrectionHeld(ctx, id)
}

// PreviewDisposal implements tenant.Disposal.
func (t *Tenant) PreviewDisposal(ctx context.Context, rule string, limit int, now time.Time) (tenant.DisposalPreview, error) {
	return t.sending().PreviewDisposal(ctx, rule, limit, now)
}

// QueueDispose implements tenant.Disposal.
func (t *Tenant) QueueDispose(ctx context.Context, d tenant.DisposeQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job,
	tenant.DisposalPreview, bool, error) {
	return t.sending().QueueDispose(ctx, d, audit, guard)
}

// DisposeJob implements tenant.Disposal.
func (t *Tenant) DisposeJob(ctx context.Context, id string) (tenant.DisposeJob, error) {
	return t.sending().DisposeJob(ctx, id)
}

// RecordDisposed implements tenant.Disposal.
func (t *Tenant) RecordDisposed(ctx context.Context, jobID, studyUID string, outcome tenant.DisposeOutcome, correction string) error {
	return t.sending().RecordDisposed(ctx, jobID, studyUID, outcome, correction)
}
