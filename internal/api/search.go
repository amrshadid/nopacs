package api

import (
	"context"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// searchService is SearchService: a tenant's subjects, sessions and scans
// found by a condition on the values their files hold, read from its
// database alone.
type searchService struct{ c *core }

var _ nopacsv1connect.SearchServiceHandler = searchService{}

// maxSearchQuery bounds a search's condition text.
const maxSearchQuery = 4096

// searchLevel returns the level a request asks for.
func searchLevel(l v1.SearchLevel) (query.Level, error) {
	switch l {
	case v1.SearchLevel_SEARCH_LEVEL_UNSPECIFIED, v1.SearchLevel_SEARCH_LEVEL_SCAN:
		return query.LevelScan, nil
	case v1.SearchLevel_SEARCH_LEVEL_SESSION:
		return query.LevelSession, nil
	case v1.SearchLevel_SEARCH_LEVEL_SUBJECT:
		return query.LevelSubject, nil
	}
	return "", invalid("level", "is a level the contract defines")
}

// Search answers a page of a search.
func (s searchService) Search(ctx context.Context, req *v1.SearchRequest) (*v1.SearchResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SearchServiceSearchProcedure)
	if err != nil {
		return nil, err
	}
	// The condition is read first, so a condition that does not parse is
	// refused once the caller is known.
	node, bad := searchCondition(req)
	details := searchDetails(req)
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	if bad != nil {
		return nil, c.failIn(d, st, bad)
	}
	q := tenant.SearchQuery{Condition: node, Count: req.GetCount(), Hide: originalsFor(d) == tenant.OriginalsHidden, Now: c.now()}
	if q.Level, err = searchLevel(req.GetLevel()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if q.Page, err = pageOf(req.GetPageSize(), req.GetPageToken()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetFacet() != "" {
		f, ferr := query.ParseField(req.GetFacet())
		if ferr != nil {
			return nil, c.failIn(d, st, fault.Wrap(fault.Invalid, ferr, "facet: "+ferr.Error()))
		}
		q.Facet = &f
	}
	if req.GetCount() || q.Facet != nil {
		if q.CountRows, err = c.values().OnTenant(st).SearchFacetRows(ctx); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	got, err := p.Search(ctx, q)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, q.Page.Cursor))
	}
	c.done(d, st)
	out := &v1.SearchResponse{NextPageToken: got.Next, TotalIsLowerBound: got.AtLeast}
	if got.Counted {
		n := got.Total
		out.TotalCount = &n
	}
	for _, it := range got.Subjects {
		out.Subjects = append(out.Subjects, subjectMessage(it))
	}
	for _, it := range got.Sessions {
		out.Sessions = append(out.Sessions, sessionMessage(it))
	}
	for _, it := range got.Scans {
		out.Scans = append(out.Scans, scanMessage(it))
	}
	for _, f := range got.Facet {
		out.Facet = append(out.Facet, &v1.FacetValue{Value: f.Value, Count: f.Count})
	}
	return out, nil
}

// ListSearchAttributes answers the attributes a tenant's search holds.
func (s searchService) ListSearchAttributes(ctx context.Context, req *v1.ListSearchAttributesRequest) (*v1.ListSearchAttributesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.SearchServiceListSearchAttributesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	got, err := p.SearchAttributes(ctx)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	out := &v1.ListSearchAttributesResponse{}
	for _, a := range got {
		out.Attributes = append(out.Attributes, &v1.SearchAttribute{Field: a.Field, Ready: a.Ready, ScansSkipped: a.ScansSkipped})
	}
	return out, nil
}

// searchCondition reads a search's condition: nil for none, and the error
// of one that is too long or does not parse.
func searchCondition(req *v1.SearchRequest) (query.Node, error) {
	switch {
	case len(req.GetQuery()) > maxSearchQuery:
		return nil, invalid("query", "is at most 4096 bytes")
	case strings.TrimSpace(req.GetQuery()) == "":
		return nil, nil
	}
	n, err := query.Parse(req.GetQuery())
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "query: "+err.Error())
	}
	return n, nil
}

// searchDetails are what the log keeps of a search: its level and the
// fields its condition compares, never a value, and the facet's field.
func searchDetails(req *v1.SearchRequest) map[string]string {
	details := map[string]string{"level": strings.ToLower(strings.TrimPrefix(req.GetLevel().String(), "SEARCH_LEVEL_"))}
	if node, err := searchCondition(req); err != nil {
		details["query"] = "invalid"
	} else if fields := query.Fields(node); len(fields) > 0 {
		details["fields"] = clip(strings.Join(fields, ","), 512)
	}
	if req.GetFacet() != "" {
		details["facet"] = clip(req.GetFacet(), 128)
	}
	return details
}
