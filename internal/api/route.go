package api

import (
	"context"
	"errors"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// routeService is RouteService: each receiver's routing rule set versions,
// kept by the tenant's store (tenant.Routing), what they decide for a file,
// a dry run over a receiver's latest sessions, and each rule's hits.
type routeService struct{ c *core }

var _ nopacsv1connect.RouteServiceHandler = routeService{}

// detailActivate is the audit detail of whether a new version was
// activated.
const detailActivate = "activate"

// CreateRuleSet stores a receiver's next rule set version and, when asked,
// points the receiver at it, under the checks of a receiver's change.
func (s routeService) CreateRuleSet(ctx context.Context, req *v1.CreateRuleSetRequest) (*v1.CreateRuleSetResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RouteServiceCreateRuleSetProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), map[string]string{detailActivate: strconv.FormatBool(req.GetActivate())})
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	rs, err := ruleSetValues("rule_set", req.GetReceiver(), req.GetRuleSet())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Details: map[string]string{detailActivate: strconv.FormatBool(req.GetActivate())}}
	if req.GetActivate() {
		// Under the lock a change of a receiver's de-identification takes,
		// so the profile the destination profiles are judged against is
		// the one the receiver has when the version runs.
		c.places.Lock()
		defer c.places.Unlock()
		rcv, err := lookupReceiver(ctx, p, req.GetReceiver())
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		if err := c.checkRuleSetProfiles(ctx, p, rcv, rs); err != nil {
			return nil, c.layerFail(d, st, facts, err)
		}
	}
	out, err := p.CreateRuleSet(ctx, rs, req.GetActivate(), c.now(), changeEntry(d, facts), c.adminGuard(d))
	if err != nil {
		return nil, c.failWith(d, st, facts, fieldOf("rule_set", err))
	}
	if req.GetActivate() {
		c.receiversChanged(req.GetTenant())
		return &v1.CreateRuleSetResponse{RuleSet: ruleSetMessage(out, out.Version)}, nil
	}
	return &v1.CreateRuleSetResponse{RuleSet: ruleSetMessage(out, 0)}, nil
}

// layerFail is failWith, the refusal of a destination profile that loosens
// a layer below it carrying its problems (problemsError).
func (c *core) layerFail(d access.Decision, st tenant.Store, facts access.Facts, err error) error {
	out := c.failWith(d, st, facts, err)
	var lr *layerRefusal
	if errors.As(err, &lr) {
		return problemsError(out, lr.problems)
	}
	return out
}

// activeVersion returns the version the receiver runs, 0 for none.
func activeVersion(ctx context.Context, p tenant.Pipeline, receiver string) (int32, error) {
	r, err := lookupReceiver(ctx, p, receiver)
	if err != nil {
		return 0, err
	}
	return r.Routing.Version, nil
}

// GetRuleSet answers one version of a receiver's rule set, or the one it
// runs.
func (s routeService) GetRuleSet(ctx context.Context, req *v1.GetRuleSetRequest) (*v1.GetRuleSetResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RouteServiceGetRuleSetProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := ruleVersion("version", req.GetVersion()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	active, err := activeVersion(ctx, p, req.GetReceiver())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	rs, err := p.RuleSet(ctx, req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetRuleSetResponse{RuleSet: ruleSetMessage(rs, active)}, nil
}

// ListRuleSets answers a page of a receiver's versions, the newest first.
func (s routeService) ListRuleSets(ctx context.Context, req *v1.ListRuleSetsRequest) (*v1.ListRuleSetsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RouteServiceListRuleSetsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	// A receiver removed keeps its versions, which still read: the active
	// one is then none.
	active, err := activeVersion(ctx, p, req.GetReceiver())
	if err != nil && fault.KindOf(err) != fault.NotFound {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListRuleSets(ctx, req.GetReceiver(), page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListRuleSetsResponse{NextPageToken: next}
	for _, rs := range items {
		out.RuleSets = append(out.RuleSets, ruleSetMessage(rs, active))
	}
	return out, nil
}

// ActivateRuleSet points a receiver at one of its versions, or at none.
func (s routeService) ActivateRuleSet(ctx context.Context, req *v1.ActivateRuleSetRequest) (*v1.ActivateRuleSetResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.RouteServiceActivateRuleSetProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := ruleVersion("version", req.GetVersion()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.places.Lock()
	defer c.places.Unlock()
	if req.GetVersion() != 0 {
		rcv, err := lookupReceiver(ctx, p, req.GetReceiver())
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		rs, err := p.RuleSet(ctx, req.GetReceiver(), req.GetVersion())
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		if err := c.checkRuleSetProfiles(ctx, p, rcv, rs); err != nil {
			return nil, c.layerFail(d, st, access.Facts{}, err)
		}
	}
	out, err := p.ActivateRuleSet(ctx, req.GetReceiver(), req.GetVersion(), c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.receiversChanged(req.GetTenant())
	resp := &v1.ActivateRuleSetResponse{}
	if out.Version != 0 {
		resp.RuleSet = ruleSetMessage(out, out.Version)
	}
	return resp, nil
}

// GetRouteHits answers how many receptions each rule of a receiver routed.
func (s routeService) GetRouteHits(ctx context.Context, req *v1.GetRouteHitsRequest) (*v1.GetRouteHitsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.RouteServiceGetRouteHitsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("receiver", req.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := ruleVersion("version", req.GetVersion()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	hits, err := p.RouteHits(ctx, req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	out := &v1.GetRouteHitsResponse{}
	for _, h := range hits {
		out.Hits = append(out.Hits, hitMessage(h))
	}
	return out, nil
}
