package settingvalue

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// SearchFacetRows returns search.facet-rows as it applies on the tenant: the
// most rows a search counts to answer a facet or a count.
func (t Tenant) SearchFacetRows(ctx context.Context) (int64, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.SearchFacetRows)
	return typed(settings.SearchFacetRows, v, err, settings.ParseSearchFacetRows)
}
