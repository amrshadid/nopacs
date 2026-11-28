package sqlite

import (
	"context"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's search (tenant.Searching): package sqlkit's one
// implementation, over the search tables the catalog's own transactions
// keep, compiled for this database's dialect.

// search returns the search on this tenant.
func (t *Tenant) search() sqlkit.SearchReader[int64] {
	return sqlkit.SearchReader[int64]{CatalogReader: t.catalog(), Dialect: query.SQLite}
}

// Search implements tenant.Searching.
func (t *Tenant) Search(ctx context.Context, q tenant.SearchQuery) (tenant.SearchResult, error) {
	return t.search().Search(ctx, q)
}

// SearchAttributes implements tenant.Searching.
func (t *Tenant) SearchAttributes(ctx context.Context) ([]tenant.SearchAttributeState, error) {
	return t.search().SearchAttributes(ctx)
}
