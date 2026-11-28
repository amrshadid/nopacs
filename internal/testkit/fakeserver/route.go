package fakeserver

import (
	"context"
	"strconv"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// routes is what the fake keeps for RouteService: each receiver's rule set
// versions as the calls made them, and the explanation, dry run and hits a
// test gave. Only the flows are exercised (a version made and activated, a
// page of versions, an answer printed), not the server's rules, which the
// API's own tests hold.
type routes struct {
	sets    map[string][]*v1.RuleSet
	explain *v1.ExplainRouteResponse
	dryRun  *v1.DryRunRoutesResponse
	hits    []*v1.RouteHit
	// asked is the last explanation's request, for a test to read.
	asked *v1.ExplainRouteRequest
}

// routeHandler is the fake's RouteService.
type routeHandler struct {
	nopacsv1connect.UnimplementedRouteServiceHandler
	s *Server
}

var _ nopacsv1connect.RouteServiceHandler = routeHandler{}

// registerRoutes adds the fake's RouteService.
func (s *Server) registerRoutes(srv *connect.Server) {
	nopacsv1connect.RegisterRouteServiceHandler(srv, routeHandler{s: s})
}

// SetRouteAnswers gives the explanation, the dry run and the hits the fake
// answers, each nil or empty for none.
func (s *Server) SetRouteAnswers(explain *v1.ExplainRouteResponse, dryRun *v1.DryRunRoutesResponse, hits []*v1.RouteHit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rts.explain, s.rts.dryRun, s.rts.hits = explain, dryRun, hits
}

// LastExplain returns the last ExplainRoute request the fake was given.
func (s *Server) LastExplain() *v1.ExplainRouteRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rts.asked
}

// receiverOf returns a tenant's receiver, or NotFound.
func (s *Server) receiverOf(tenant, name string) (*v1.Receiver, error) {
	r, ok := s.arch.receivers[key(tenant, name)]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no receiver with this name")
	}
	return r, nil
}

// CreateRuleSet stores the receiver's next version and activates it when
// asked, checking only that what it names exists.
func (h routeHandler) CreateRuleSet(_ context.Context, req *v1.CreateRuleSetRequest) (*v1.CreateRuleSetResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rcv, err := s.receiverOf(req.GetTenant(), req.GetReceiver())
	if err != nil {
		return nil, err
	}
	if req.GetRuleSet() == nil {
		return nil, fault.New(fault.Invalid, "rule_set: is required")
	}
	if req.GetActivate() {
		for _, a := range req.GetRuleSet().GetArchiveRules() {
			if _, ok := s.arch.storages[key(req.GetTenant(), a.GetStorage())]; !ok {
				return nil, fault.New(fault.Precondition, "archive rule "+a.GetName()+": the tenant has no storage named "+a.GetStorage())
			}
		}
		for _, f := range req.GetRuleSet().GetForwardRules() {
			if err := s.forwardable(req.GetTenant(), f.GetSenders()); err != nil {
				return nil, err
			}
		}
	}
	if s.rts.sets == nil {
		s.rts.sets = map[string][]*v1.RuleSet{}
	}
	k := key(req.GetTenant(), req.GetReceiver())
	rs := proto.Clone(req.GetRuleSet()).(*v1.RuleSet)
	rs.Receiver, rs.Version, rs.CreateTime = req.GetReceiver(), int32(len(s.rts.sets[k])+1), timestamppb.New(s.now()) //nolint:gosec // G115: a test's few versions.
	s.rts.sets[k] = append(s.rts.sets[k], rs)
	out := proto.Clone(rs).(*v1.RuleSet)
	if req.GetActivate() {
		rcv.RuleSetVersion = rs.Version
		out.Active = true
	}
	return &v1.CreateRuleSetResponse{RuleSet: out}, nil
}

// ruleSet returns version v of the receiver's rule sets, 0 for the one it
// runs, marked active when it is.
func (s *Server) ruleSet(tenant, receiver string, v int32) (*v1.RuleSet, error) {
	rcv, err := s.receiverOf(tenant, receiver)
	if err != nil {
		return nil, err
	}
	if v == 0 {
		v = rcv.GetRuleSetVersion()
	}
	sets := s.rts.sets[key(tenant, receiver)]
	if v < 1 || int(v) > len(sets) {
		return nil, fault.New(fault.NotFound, "the receiver has no rule set version "+strconv.Itoa(int(v)))
	}
	out := proto.Clone(sets[v-1]).(*v1.RuleSet)
	out.Active = v == rcv.GetRuleSetVersion()
	return out, nil
}

// GetRuleSet answers one version.
func (h routeHandler) GetRuleSet(_ context.Context, req *v1.GetRuleSetRequest) (*v1.GetRuleSetResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rs, err := s.ruleSet(req.GetTenant(), req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, err
	}
	return &v1.GetRuleSetResponse{RuleSet: rs}, nil
}

// ListRuleSets answers a page of a receiver's versions, the newest first.
func (h routeHandler) ListRuleSets(_ context.Context, req *v1.ListRuleSetsRequest) (*v1.ListRuleSetsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rcv, err := s.receiverOf(req.GetTenant(), req.GetReceiver())
	if err != nil {
		return nil, err
	}
	sets := s.rts.sets[key(req.GetTenant(), req.GetReceiver())]
	from, to, next, err := pageOf(len(sets), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListRuleSetsResponse{NextPageToken: next}
	for i := from; i < to; i++ {
		rs := proto.Clone(sets[len(sets)-1-i]).(*v1.RuleSet)
		rs.Active = rs.GetVersion() == rcv.GetRuleSetVersion()
		out.RuleSets = append(out.RuleSets, rs)
	}
	return out, nil
}

// ActivateRuleSet points the receiver at a version, or at none.
func (h routeHandler) ActivateRuleSet(_ context.Context, req *v1.ActivateRuleSetRequest) (*v1.ActivateRuleSetResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rcv, err := s.receiverOf(req.GetTenant(), req.GetReceiver())
	if err != nil {
		return nil, err
	}
	if req.GetVersion() == 0 {
		rcv.RuleSetVersion = 0
		return &v1.ActivateRuleSetResponse{}, nil
	}
	rs, err := s.ruleSet(req.GetTenant(), req.GetReceiver(), req.GetVersion())
	if err != nil {
		return nil, err
	}
	rcv.RuleSetVersion, rs.Active = rs.GetVersion(), true
	return &v1.ActivateRuleSetResponse{RuleSet: rs}, nil
}

// ExplainRoute answers the explanation a test gave, keeping the request.
func (h routeHandler) ExplainRoute(_ context.Context, req *v1.ExplainRouteRequest) (*v1.ExplainRouteResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := s.receiverOf(req.GetTenant(), req.GetReceiver()); err != nil {
		return nil, err
	}
	if req.GetShowValues() {
		if err := s.refuseMasked(messageMaskedOriginals); err != nil {
			return nil, err
		}
	}
	s.rts.asked = proto.Clone(req).(*v1.ExplainRouteRequest)
	if s.rts.explain == nil {
		return nil, fault.New(fault.Unavailable, "the fake was given no explanation")
	}
	return proto.Clone(s.rts.explain).(*v1.ExplainRouteResponse), nil
}

// DryRunRoutes answers the dry run a test gave.
func (h routeHandler) DryRunRoutes(_ context.Context, req *v1.DryRunRoutesRequest) (*v1.DryRunRoutesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if s.rts.dryRun == nil {
		return nil, fault.New(fault.Unavailable, "the fake was given no dry run")
	}
	return proto.Clone(s.rts.dryRun).(*v1.DryRunRoutesResponse), nil
}

// GetRouteHits answers the hits a test gave.
func (h routeHandler) GetRouteHits(_ context.Context, req *v1.GetRouteHitsRequest) (*v1.GetRouteHitsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &v1.GetRouteHitsResponse{}
	for _, hit := range s.rts.hits {
		out.Hits = append(out.Hits, proto.Clone(hit).(*v1.RouteHit))
	}
	return out, nil
}
