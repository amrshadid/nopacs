package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The catalog's reads (tenant.CatalogReads): the statements and every
// decision of the reads are sqlkit's, shared with every SQL driver; this
// file gives them SQLite's parameters, times and lists of ids, and runs
// them on the read pool.

// catalogSQL writes the catalog's reads with SQLite's parameters and
// times; a list of ids is one JSON array (json_each), each id a lookup of
// a primary key.
var catalogSQL = sqlkit.CatalogSQL{
	Param: func(int) string { return "?" },
	Time:  func(t time.Time) any { return micros(t) },
	InIDs: func(column, p string) string { return column + " IN (SELECT value FROM json_each(" + p + "))" },
	IDs:   func(ids []int64) (any, error) { return sqlkit.JSONText(ids) },
}

// catalogRunner runs the catalog's statements on a tenant's read pool.
type catalogRunner struct{ t *Tenant }

// Rows implements sqlkit.CatalogRunner.
func (r catalogRunner) Rows(ctx context.Context, message string, q sqlkit.CatalogRead, scan func(sqlkit.Row) error) error {
	_, err := readRows(ctx, r.t, message, q.SQL, q.Args, func(row interface{ Scan(...any) error }) (struct{}, error) { return struct{}{}, scan(row) })
	return err
}

// One implements sqlkit.CatalogRunner.
func (r catalogRunner) One(ctx context.Context, what, message string, q sqlkit.CatalogRead, scan func(sqlkit.Row) error) error {
	_, err := readOne(ctx, r.t, what, message, q.SQL, q.Args, func(row interface{ Scan(...any) error }) (struct{}, error) { return struct{}{}, scan(row) })
	return err
}

// Totals implements sqlkit.CatalogRunner.
func (r catalogRunner) Totals(ctx context.Context) (tenant.CatalogCounts, error) {
	return r.t.CatalogTotals(ctx)
}

// Binding implements sqlkit.CatalogRunner.
func (r catalogRunner) Binding(parts ...string) string { return r.t.binding(parts...) }

// catalog returns the catalog's reads on this tenant: a time column holds
// microseconds since 1970.
func (t *Tenant) catalog() sqlkit.CatalogReader[int64] {
	return sqlkit.CatalogReader[int64]{SQL: catalogSQL, Run: catalogRunner{t}, Time: fromMicros}
}

// CatalogSubject implements tenant.SubjectReads.
func (t *Tenant) CatalogSubject(ctx context.Context, subjectLabel string) (tenant.CatalogSubject, error) {
	return t.catalog().CatalogSubject(ctx, subjectLabel)
}

// ListSubjects implements tenant.SubjectReads.
func (t *Tenant) ListSubjects(ctx context.Context, f tenant.SubjectFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
	return t.catalog().ListSubjects(ctx, f, page)
}

// CatalogSession implements tenant.SessionReads.
func (t *Tenant) CatalogSession(ctx context.Context, subjectLabel, sessionLabel string) (tenant.CatalogSession, error) {
	return t.catalog().CatalogSession(ctx, subjectLabel, sessionLabel)
}

// ListSessions implements tenant.SessionReads.
func (t *Tenant) ListSessions(ctx context.Context, f tenant.SessionFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
	return t.catalog().ListSessions(ctx, f, page)
}

// CatalogScan implements tenant.ScanReads.
func (t *Tenant) CatalogScan(ctx context.Context, p label.Path) (tenant.CatalogScan, error) {
	return t.catalog().CatalogScan(ctx, p)
}

// ListScans implements tenant.ScanReads.
func (t *Tenant) ListScans(ctx context.Context, f tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
	return t.catalog().ListScans(ctx, f, page)
}

// ListHeldInstances implements tenant.InstanceReads.
func (t *Tenant) ListHeldInstances(ctx context.Context, p label.Path, page master.Page) ([]tenant.ScanInstance, string, error) {
	return t.catalog().ListHeldInstances(ctx, p, page)
}

// HeldInstance implements tenant.InstanceReads.
func (t *Tenant) HeldInstance(ctx context.Context, p label.Path, uid string) (tenant.ScanInstance, error) {
	return t.catalog().HeldInstance(ctx, p, uid)
}
