package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
)

// MaxFacetValues is the most distinct values a facet answers; a facet of an
// attribute with more gives the most frequent of them.
const MaxFacetValues = 1000

// Searching is a tenant's search over its catalog: the condition language
// that routing shares (package query) compiled to the search tables (package
// search), which the same transactions that write the catalog keep right.
type Searching interface {
	// Search answers a search. It is refused as Invalid when the condition
	// names a field the search tables do not hold, compares it with a
	// value of another type, or is served by no index (an "and" is served
	// when one child is, an "or" when every child is, and a negation alone
	// never); as a precondition failure when it names an attribute still
	// being filled for the scans already archived (SearchAttributeState).
	// The page is of the rows of the condition's level in order of their
	// ids, a keyset cursor resuming after the last, so a row archived or
	// removed meanwhile neither shifts a page nor repeats a row.
	Search(ctx context.Context, q SearchQuery) (SearchResult, error)
	// SearchAttributes lists each attribute the search tables hold and
	// whether its column is filled for every scan.
	SearchAttributes(ctx context.Context) ([]SearchAttributeState, error)
}

// SearchQuery is a search.
type SearchQuery struct {
	// Level is the level whose rows are listed: subject, session or scan.
	Level query.Level
	// Condition is the checked condition (query.Parse), nil for every row.
	Condition query.Node
	// StudyUID, when not empty, keeps only the scans of the session with
	// this Study Instance UID, as an addition to Condition that needs no
	// index (the session is found by its unique UID). Only a search of
	// scans takes it. It is how a search of the series of one study (PS3.18
	// 10.6) reads the same rows as the search service.
	StudyUID string
	// Page bounds the answer.
	Page master.Page
	// Count asks how many rows match, counted up to CountRows.
	Count bool
	// Facet, when not nil, asks for the distinct values of this field of
	// the rows that match with how many rows have each; the field must be
	// held at Level.
	Facet *query.Field
	// CountRows bounds the rows a count or a facet reads (the setting
	// search.facet-rows): a count that reaches it says so and stops, and a
	// facet of more rows is refused with the bound. It is at least one.
	CountRows int64
	// Hide keeps from the search every scan with a file archived as
	// received, and every session with such a scan, so what a condition
	// matches says nothing of a value read from those files: for a caller
	// that may not learn the original values.
	Hide bool
	// Now is when the search runs: relative dates count back from its day.
	Now time.Time
}

// SearchResult is the answer to a search: the rows of the page, of the
// level asked.
type SearchResult struct {
	// Subjects, Sessions or Scans hold the page, whichever Level names.
	Subjects []CatalogSubject
	Sessions []CatalogSession
	Scans    []CatalogScan
	// Next is the cursor of the next page, empty after the last.
	Next string
	// Total is how many rows match, when counted. AtLeast says the count
	// stopped at SearchQuery.CountRows: more rows match.
	Total   int64
	Counted bool
	AtLeast bool
	// Facet holds, when asked, the distinct values and their counts, the
	// most frequent first.
	Facet []FacetCount
}

// FacetCount is one value of a facet and how many rows have it.
type FacetCount struct {
	// Value is the value as written in the search table: a number or a
	// date in the form of its column.
	Value string
	// Count is how many rows have it.
	Count int64
}

// SearchAttributeState says whether the search tables' column of an
// attribute is filled for every scan.
type SearchAttributeState struct {
	// Field is the attribute's keyword.
	Field string
	// Ready is false while the scans archived before the attribute was
	// added are still being filled.
	Ready bool
	// ScansSkipped is how many scans the last backfill that covered the
	// attribute could not read (their metadata was missing or not in its
	// form), which keep no value for it: a fill that ended partial is
	// visible here. Zero for an attribute no backfill covered.
	ScansSkipped int64
}
