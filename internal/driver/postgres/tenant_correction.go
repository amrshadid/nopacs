package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's corrections (tenant.Corrections), on the tables of
// migrations/tenant/0022_corrections.sql: package sqlkit's one
// implementation, run here in Postgres's transactions, each change through
// Tenant.pipe, as the scrubs' are. Every change locks the tenant's
// configuration first, so two corrections never move catalog rows at once.

// PreviewDelete implements tenant.Corrections.
func (t *Tenant) PreviewDelete(ctx context.Context, target tenant.CorrectionTarget, now time.Time) (tenant.DeletePreview, error) {
	return t.sending().PreviewDelete(ctx, target, now)
}

// PrepareCorrection implements tenant.Corrections.
func (t *Tenant) PrepareCorrection(ctx context.Context, p tenant.CorrectionPrepare, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, bool, error) {
	return t.sending().PrepareCorrection(ctx, p, audit, guard)
}

// CorrectionScans implements tenant.Corrections.
func (t *Tenant) CorrectionScans(ctx context.Context, id string) ([]tenant.CorrectionScan, error) {
	return t.sending().CorrectionScans(ctx, id)
}

// CommitCorrection implements tenant.Corrections.
func (t *Tenant) CommitCorrection(ctx context.Context, c tenant.CorrectionCommit) (tenant.Correction, error) {
	return t.sending().CommitCorrection(ctx, c)
}

// AbandonCorrection implements tenant.Corrections.
func (t *Tenant) AbandonCorrection(ctx context.Context, id, why string, now time.Time) (tenant.Correction, error) {
	return t.sending().AbandonCorrection(ctx, id, why, now)
}

// UndoCorrection implements tenant.Corrections.
func (t *Tenant) UndoCorrection(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, error) {
	return t.sending().UndoCorrection(ctx, id, reason, now, audit, guard)
}

// StartPurge implements tenant.Corrections.
func (t *Tenant) StartPurge(ctx context.Context, id string, now time.Time, audit *master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, error) {
	return t.sending().StartPurge(ctx, id, now, audit, guard)
}

// RecordPurgedObject implements tenant.Corrections.
func (t *Tenant) RecordPurgedObject(ctx context.Context, o tenant.PurgedObject) error {
	return t.sending().RecordPurgedObject(ctx, o)
}

// FinishPurge implements tenant.Corrections.
func (t *Tenant) FinishPurge(ctx context.Context, f tenant.PurgeFinish) (tenant.Correction, error) {
	return t.sending().FinishPurge(ctx, f)
}

// RecordCopied implements tenant.Corrections.
func (t *Tenant) RecordCopied(ctx context.Context, id string, copied int64) error {
	return t.sending().RecordCopied(ctx, id, copied)
}

// MarkRecords implements tenant.Corrections.
func (t *Tenant) MarkRecords(ctx context.Context, id string, state record.State) error {
	return t.sending().MarkRecords(ctx, id, state)
}

// Correction implements tenant.Corrections.
func (t *Tenant) Correction(ctx context.Context, id string) (tenant.Correction, error) {
	return t.sending().Correction(ctx, id)
}

// ListCorrections implements tenant.Corrections.
func (t *Tenant) ListCorrections(ctx context.Context, filter tenant.CorrectionFilter, page master.Page) ([]tenant.Correction, string, error) {
	return t.sending().ListCorrections(ctx, filter, page)
}

// PendingCorrections implements tenant.Corrections.
func (t *Tenant) PendingCorrections(ctx context.Context, after string, limit int) ([]tenant.Correction, error) {
	return t.sending().PendingCorrections(ctx, after, limit)
}

// DueCorrections implements tenant.Corrections.
func (t *Tenant) DueCorrections(ctx context.Context, now time.Time, limit int) ([]tenant.Correction, error) {
	return t.sending().DueCorrections(ctx, now, limit)
}

// AllowArrivals implements tenant.Corrections.
func (t *Tenant) AllowArrivals(ctx context.Context, id string, value tenant.HeldValue, reason string, now time.Time,
	audit master.AuditEntry, guard tenant.Guard) (tenant.Correction, error) {
	return t.sending().AllowArrivals(ctx, id, value, reason, now, audit, guard)
}

// CheckPurgeClock implements tenant.Corrections.
func (t *Tenant) CheckPurgeClock(ctx context.Context, now time.Time) error {
	return t.sending().CheckPurgeClock(ctx, now)
}

// PurgeTargets implements tenant.Corrections.
func (t *Tenant) PurgeTargets(ctx context.Context, id string, limit int) ([]tenant.PurgeTarget, error) {
	return t.sending().PurgeTargets(ctx, id, limit)
}

// AddPurgeTargets implements tenant.Corrections.
func (t *Tenant) AddPurgeTargets(ctx context.Context, id string, targets []tenant.PurgeTarget) error {
	return t.sending().AddPurgeTargets(ctx, id, targets)
}

// SettlePurgeTargets implements tenant.Corrections.
func (t *Tenant) SettlePurgeTargets(ctx context.Context, id string, settled []tenant.PurgeSettled) error {
	return t.sending().SettlePurgeTargets(ctx, id, settled)
}

// RetainedDue implements tenant.Corrections.
func (t *Tenant) RetainedDue(ctx context.Context, now time.Time, limit int) ([]tenant.PurgedObject, error) {
	return t.sending().RetainedDue(ctx, now, limit)
}

// ClearWithdrawn implements tenant.Corrections.
func (t *Tenant) ClearWithdrawn(ctx context.Context, id string) ([]string, error) {
	return t.sending().ClearWithdrawn(ctx, id)
}

// MarkSwept implements tenant.Corrections.
func (t *Tenant) MarkSwept(ctx context.Context, id string) error {
	return t.sending().MarkSwept(ctx, id)
}

// SweepableScans implements tenant.Corrections.
func (t *Tenant) SweepableScans(ctx context.Context, id string) (map[string]bool, error) {
	return t.sending().SweepableScans(ctx, id)
}

// HeldByCorrection implements tenant.Corrections.
func (t *Tenant) HeldByCorrection(ctx context.Context, p label.Path, studyUID, seriesUID string) (tenant.CorrectionHold, error) {
	return t.sending().HeldByCorrection(ctx, p, studyUID, seriesUID)
}

// DeletedMatches implements tenant.Corrections.
func (t *Tenant) DeletedMatches(ctx context.Context, p tenant.DeletedProbe) (tenant.DeletedMatch, error) {
	return t.sending().DeletedMatches(ctx, p)
}

// ResolveInstances implements tenant.Corrections.
func (t *Tenant) ResolveInstances(ctx context.Context, refs []tenant.InstanceRef) ([]tenant.ResolvedInstance, error) {
	return t.sending().ResolveInstances(ctx, refs)
}
