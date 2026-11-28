package sqlite

import (
	"context"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's pixel templates (tenant.PixelTemplates), on the
// pixel_templates table of migrations/tenant/0001_baseline.sql: package
// sqlkit's one implementation, run here in sqlite's transactions.

var _ tenant.PixelTemplates = (*Tenant)(nil)

// AddPixelTemplates implements tenant.PixelTemplates.
func (t *Tenant) AddPixelTemplates(ctx context.Context, templates []tenant.PixelTemplate, audit master.AuditEntry, guard tenant.Guard) error {
	return t.deidentification().AddPixelTemplates(ctx, templates, audit, guard)
}

// RemovePixelTemplate implements tenant.PixelTemplates.
func (t *Tenant) RemovePixelTemplate(ctx context.Context, key tenant.PixelTemplateKey, audit master.AuditEntry, guard tenant.Guard) error {
	return t.deidentification().RemovePixelTemplate(ctx, key, audit, guard)
}

// ListPixelTemplates implements tenant.PixelTemplates.
func (t *Tenant) ListPixelTemplates(ctx context.Context, page master.Page) ([]tenant.PixelTemplate, string, error) {
	return t.deidentification().ListPixelTemplates(ctx, page)
}

// FindPixelTemplate implements tenant.PixelTemplates.
func (t *Tenant) FindPixelTemplate(ctx context.Context, q tenant.PixelTemplateQuery) (tenant.PixelTemplate, bool, error) {
	return t.deidentification().FindPixelTemplate(ctx, q)
}
