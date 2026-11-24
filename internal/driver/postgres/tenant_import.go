package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's import jobs (tenant.Importing), on the tables of
// migrations/tenant/0018_import.sql: package sqlkit's one implementation,
// run here in Postgres's transactions at read committed, each change
// through Tenant.pipe, as the senders' and jobs' are. The rows are locked
// in the package's order (doc.go): a unit of work's row (its fence), a
// reception's, an import unit's (several in order of unit id), the import
// job's, then the job's. What
// the pipeline decides of an import's files is written by the pipeline's
// own methods, in their transactions, through importSending; their events
// are written last with the method's own.

// importSending returns the import implementation a pipeline method runs in
// its own transaction.
func importSending() sqlkit.Sending { return sqlkit.Sending{D: sendingDialect} }

// CreateImportJob implements tenant.Imports.
func (t *Tenant) CreateImportJob(ctx context.Context, j tenant.NewImportJob, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().CreateImportJob(ctx, j, audit, guard)
}

// ImportJob implements tenant.Imports.
func (t *Tenant) ImportJob(ctx context.Context, id string) (tenant.Job, error) {
	return t.sending().ImportJob(ctx, id)
}

// ListImportUnits implements tenant.Imports.
func (t *Tenant) ListImportUnits(ctx context.Context, jobID string, filter tenant.ImportUnitFilter, page master.Page) ([]tenant.ImportUnit, string, error) {
	return t.sending().ListImportUnits(ctx, jobID, filter, page)
}

// ListImportFiles implements tenant.Imports.
func (t *Tenant) ListImportFiles(ctx context.Context, jobID string, filter tenant.ImportFileFilter, page master.Page) ([]tenant.ImportFile, string, error) {
	return t.sending().ListImportFiles(ctx, jobID, filter, page)
}

// StartImportUnit implements tenant.ImportWork.
func (t *Tenant) StartImportUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.ImportUnit, tenant.Job, error) {
	return t.sending().StartImportUnit(ctx, fence, now)
}

// AddImportUnits implements tenant.ImportWork.
func (t *Tenant) AddImportUnits(ctx context.Context, fence tenant.WorkFence, folders []string, now time.Time) (tenant.Job, error) {
	return t.sending().AddImportUnits(ctx, fence, folders, now)
}

// RecordImportSkip implements tenant.ImportWork.
func (t *Tenant) RecordImportSkip(ctx context.Context, s tenant.ImportSkip) (tenant.Job, error) {
	return t.sending().RecordImportSkip(ctx, s)
}

// EndImportWalk implements tenant.ImportWork.
func (t *Tenant) EndImportWalk(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	return t.sending().EndImportWalk(ctx, fence, now)
}

// FailImportUnit implements tenant.ImportWork.
func (t *Tenant) FailImportUnit(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Job, error) {
	return t.sending().FailImportUnit(ctx, fence, reason, now)
}

// ReleaseImportUnit implements tenant.ImportWork.
func (t *Tenant) ReleaseImportUnit(ctx context.Context, fence tenant.WorkFence, due time.Time, reason string, now time.Time) error {
	return t.sending().ReleaseImportUnit(ctx, fence, due, reason, now)
}

// TimeOutImportSettling implements tenant.ImportWork.
func (t *Tenant) TimeOutImportSettling(ctx context.Context, jobID string, now time.Time) (tenant.Job, error) {
	return t.sending().TimeOutImportSettling(ctx, jobID, now)
}

// ArrivalParts implements tenant.ImportWork.
func (t *Tenant) ArrivalParts(ctx context.Context, receptionID, sopInstanceUID string, sum tenant.SHA256) (map[string]string, bool, error) {
	return t.sending().ArrivalParts(ctx, receptionID, sopInstanceUID, sum)
}
