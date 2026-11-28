package settings

import (
	"errors"
	"strconv"
)

// SearchFacetRows is the most rows a search may count to give a facet or a
// count of the rows it matches: a facet or count of a search that matches
// more is refused, with the number it counted to, since its cost grows with
// the rows matched (a search table read for each, 14 ms at 200,000 scans
// on Postgres and 45 ms on SQLite). Ordinary: the installation's value is
// every tenant's default, and a tenant's admin may set a bound of its own,
// since the cost is the tenant's own database's.
const SearchFacetRows = "search.facet-rows"

// The bounds of search.facet-rows: a thousand rows, which is a facet of a
// small tenant, to a hundred million, past any tenant the archive is built
// for.
const (
	MinSearchFacetRows = 1_000
	MaxSearchFacetRows = 100_000_000
)

// ParseSearchFacetRows reads search.facet-rows: a whole number of rows from
// 1000 to 100000000.
func ParseSearchFacetRows(v string) (int64, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < MinSearchFacetRows || n > MaxSearchFacetRows {
		return 0, errors.New("is a whole number of rows from 1000 to 100000000")
	}
	return n, nil
}

// searchDefinitions are the settings of search.
func searchDefinitions() []Definition {
	return []Definition{{
		// A million rows: a facet of that many scans reads for under a
		// second on the databases measured, and a tenant whose scans
		// match more than that is asked to narrow its search.
		Name:           SearchFacetRows,
		Default:        "1000000",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseSearchFacetRows(v); return err },
	}}
}
