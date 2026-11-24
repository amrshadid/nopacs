package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's scrubs (tenant.Scrubbing), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one implementation,
// run here in SQLite's transactions, as the senders' and jobs' are.

// RecordScrubFinding implements tenant.Scrubbing.
func (t *Tenant) RecordScrubFinding(ctx context.Context, f tenant.ScrubFindingRecord) (tenant.ScrubFinding, bool, error) {
	return t.sending().RecordScrubFinding(ctx, f)
}

// ClearScrubFinding implements tenant.Scrubbing.
func (t *Tenant) ClearScrubFinding(ctx context.Context, fence tenant.WorkFence, id string, now time.Time) (tenant.ScrubFinding, bool, error) {
	return t.sending().ClearScrubFinding(ctx, fence, id, now)
}

// ClearObjectFindings implements tenant.Scrubbing.
func (t *Tenant) ClearObjectFindings(ctx context.Context, fence tenant.WorkFence, storage, key string, kinds []tenant.ScrubFindingKind,
	now time.Time) (int, error) {
	return t.sending().ClearObjectFindings(ctx, fence, storage, key, kinds, now)
}

// ScanScrubFindings implements tenant.Scrubbing.
func (t *Tenant) ScanScrubFindings(ctx context.Context, storage string, path label.Path, afterKey string,
	afterKind tenant.ScrubFindingKind) ([]tenant.ScrubFinding, error) {
	return t.sending().ScanScrubFindings(ctx, storage, path, afterKey, afterKind)
}

// AddScrubTallies implements tenant.Scrubbing.
func (t *Tenant) AddScrubTallies(ctx context.Context, fence tenant.WorkFence, tallies map[string]int64) error {
	return t.sending().AddScrubTallies(ctx, fence, tallies)
}

// LastScrub implements tenant.Scrubbing.
func (t *Tenant) LastScrub(ctx context.Context, storage string) (tenant.Job, bool, error) {
	return t.sending().LastScrub(ctx, storage)
}

// ListScrubFindings implements tenant.Scrubbing.
func (t *Tenant) ListScrubFindings(ctx context.Context, filter tenant.ScrubFindingFilter, page master.Page) ([]tenant.ScrubFinding, string, error) {
	return t.sending().ListScrubFindings(ctx, filter, page)
}

// OpenScrubFindings implements tenant.Scrubbing.
func (t *Tenant) OpenScrubFindings(ctx context.Context) (map[tenant.ScrubFindingKind]int64, error) {
	return t.sending().OpenScrubFindings(ctx)
}

// DamagedInstances implements tenant.Scrubbing.
func (t *Tenant) DamagedInstances(ctx context.Context, uids []string) (map[string]string, error) {
	return t.sending().DamagedInstances(ctx, uids)
}
