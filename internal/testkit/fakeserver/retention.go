package fakeserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// retention is what the fake keeps for RetentionService: each tenant's rules,
// due sessions and holds as a test gave them or the calls made them. Only the
// flows are exercised (a rule's mask, a disposal's preview and token, a
// clock's confirmation), not the server's rules, which the API's own tests
// hold.
type retention struct {
	rules  map[string][]*v1.RetentionRule
	due    map[string][]*v1.DueSession
	holds  map[string][]*v1.Hold
	status map[string]*v1.RetentionStatus
	// holdsServed makes PlaceHold and ReleaseHold answer; the server does not
	// serve them yet.
	holdsServed bool
	next        int
	// maxPerPass is retention.max-per-pass, which bounds a preview's
	// max_sessions; 0 is the server's 1000.
	maxPerPass int
	// statusAs makes a tenant's status answer name another tenant, as a
	// server that answered for the wrong one would.
	statusAs map[string]string
}

// retentionHandler is the fake's RetentionService.
type retentionHandler struct {
	nopacsv1connect.UnimplementedRetentionServiceHandler
	s *Server
}

var _ nopacsv1connect.RetentionServiceHandler = retentionHandler{}

// registerRetention adds the fake's RetentionService.
func (s *Server) registerRetention(srv *connect.Server) {
	nopacsv1connect.RegisterRetentionServiceHandler(srv, retentionHandler{s: s})
}

// AddDueSession gives a tenant a session that is due, as the daily pass would
// have found it.
func (s *Server) AddDueSession(tenant string, d *v1.DueSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ret.due == nil {
		s.ret.due = map[string][]*v1.DueSession{}
	}
	s.ret.due[tenant] = append(s.ret.due[tenant], proto.Clone(d).(*v1.DueSession))
}

// AddHold gives a tenant a hold.
func (s *Server) AddHold(tenant string, h *v1.Hold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ret.holds == nil {
		s.ret.holds = map[string][]*v1.Hold{}
	}
	s.ret.holds[tenant] = append(s.ret.holds[tenant], proto.Clone(h).(*v1.Hold))
}

// SetRetentionStatus sets the retention state a tenant's status answers.
func (s *Server) SetRetentionStatus(tenant string, rs *v1.RetentionStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ret.status == nil {
		s.ret.status = map[string]*v1.RetentionStatus{}
	}
	s.ret.status[tenant] = proto.Clone(rs).(*v1.RetentionStatus)
}

// ServeHolds makes PlaceHold and ReleaseHold answer, as a server that serves
// them.
func (s *Server) ServeHolds() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ret.holdsServed = true
}

// ruleOf finds a tenant's rule by name.
func (s *Server) ruleOf(tenant, name string) (*v1.RetentionRule, error) {
	for _, r := range s.ret.rules[tenant] {
		if r.GetName() == name {
			return r, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no rule with that name")
}

// checkRule applies the rules of a rule the contract states most plainly.
func checkRule(r *v1.RetentionRule) error {
	if err := itemOK("rule.name", r.GetName()); err != nil {
		return err
	}
	k, d := r.GetKeepAtLeast(), r.GetDisposeAfter()
	if k == nil && d == nil {
		return fault.New(fault.Invalid, "rule: needs keep_at_least or dispose_after")
	}
	if k != nil && d != nil && (d.GetYears() < k.GetYears() || (d.GetYears() == k.GetYears() && d.GetDays() < k.GetDays())) {
		return fault.New(fault.Invalid, "rule.dispose_after: is shorter than keep_at_least")
	}
	return nil
}

// AddRetentionRule adds a rule.
func (h retentionHandler) AddRetentionRule(_ context.Context, req *v1.AddRetentionRuleRequest) (*v1.AddRetentionRuleResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req.GetRule()).(*v1.RetentionRule)
	if err := checkRule(r); err != nil {
		return nil, err
	}
	if _, err := s.ruleOf(req.GetTenant(), r.GetName()); err == nil {
		return nil, fault.New(fault.Conflict, "the tenant has a rule with that name")
	}
	if r.GetAction() == v1.RetentionAction_RETENTION_ACTION_UNSPECIFIED {
		r.Action = v1.RetentionAction_RETENTION_ACTION_REVIEW
	}
	r.CreateTime, r.UpdateTime = timestamppb.New(s.now()), timestamppb.New(s.now())
	if s.ret.rules == nil {
		s.ret.rules = map[string][]*v1.RetentionRule{}
	}
	s.ret.rules[req.GetTenant()] = append(s.ret.rules[req.GetTenant()], r)
	slices.SortFunc(s.ret.rules[req.GetTenant()], func(a, b *v1.RetentionRule) int { return strings.Compare(a.GetName(), b.GetName()) })
	return &v1.AddRetentionRuleResponse{Rule: proto.Clone(r).(*v1.RetentionRule)}, nil
}

// GetRetentionRule answers one rule.
func (h retentionHandler) GetRetentionRule(_ context.Context, req *v1.GetRetentionRuleRequest) (*v1.GetRetentionRuleResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, err := s.ruleOf(req.GetTenant(), req.GetName())
	if err != nil {
		return nil, err
	}
	return &v1.GetRetentionRuleResponse{Rule: proto.Clone(r).(*v1.RetentionRule)}, nil
}

// ListRetentionRules answers a page of a tenant's rules by name.
func (h retentionHandler) ListRetentionRules(_ context.Context, req *v1.ListRetentionRulesRequest) (*v1.ListRetentionRulesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	all := s.ret.rules[req.GetTenant()]
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListRetentionRulesResponse{Rules: all[from:to], NextPageToken: next}, nil
}

// SetRetentionRule changes the fields of a rule the mask names.
func (h retentionHandler) SetRetentionRule(_ context.Context, req *v1.SetRetentionRuleRequest) (*v1.SetRetentionRuleResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	cur, err := s.ruleOf(req.GetTenant(), req.GetRule().GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, fault.New(fault.Invalid, "update_mask: is required and not empty")
	}
	next := proto.Clone(cur).(*v1.RetentionRule)
	in := req.GetRule()
	for _, p := range paths {
		switch p {
		case "selector":
			next.Selector = in.GetSelector()
		case "keep_at_least":
			next.KeepAtLeast = in.GetKeepAtLeast()
		case "dispose_after":
			next.DisposeAfter = in.GetDisposeAfter()
		case "action":
			next.Action = in.GetAction()
		default:
			return nil, fault.New(fault.Invalid, "update_mask: "+p+" is not a field a change may name")
		}
	}
	if err := checkRule(next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	proto.Reset(cur)
	proto.Merge(cur, next)
	return &v1.SetRetentionRuleResponse{Rule: proto.Clone(cur).(*v1.RetentionRule)}, nil
}

// RemoveRetentionRule removes a rule.
func (h retentionHandler) RemoveRetentionRule(_ context.Context, req *v1.RemoveRetentionRuleRequest) (*v1.RemoveRetentionRuleResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, err := s.ruleOf(req.GetTenant(), req.GetName())
	if err != nil {
		return nil, err
	}
	rules := s.ret.rules[req.GetTenant()]
	s.ret.rules[req.GetTenant()] = slices.DeleteFunc(rules, func(x *v1.RetentionRule) bool { return x == r })
	return &v1.RemoveRetentionRuleResponse{Rule: r}, nil
}

// ListDueSessions answers the due sessions a test gave, by rule, with the
// held ones only when asked for.
func (h retentionHandler) ListDueSessions(_ context.Context, req *v1.ListDueSessionsRequest) (*v1.ListDueSessionsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetRule() != "" {
		if _, err := s.ruleOf(req.GetTenant(), req.GetRule()); err != nil {
			return nil, err
		}
	}
	var all []*v1.DueSession
	for _, d := range s.ret.due[req.GetTenant()] {
		if (req.GetRule() == "" || d.GetRule() == req.GetRule()) && (req.GetIncludeHeld() || !d.GetHeld()) {
			all = append(all, d)
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListDueSessionsResponse{Sessions: all[from:to], NextPageToken: next}, nil
}

// disposeToken is the fake's confirm token of a disposal: the rule and how
// many sessions are listed, so a session that fell due since is refused.
func disposeToken(rule string, n int) string { return fmt.Sprintf("dp.%s.%d", rule, n) }

// AnswerStatusAs makes the status asked for a tenant come back holding another
// tenant's name, for a client that must not take an answer for the one asked.
func (s *Server) AnswerStatusAs(tenant, other string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ret.statusAs == nil {
		s.ret.statusAs = map[string]string{}
	}
	s.ret.statusAs[tenant] = other
}

// SetMaxPerPass sets the fake's retention.max-per-pass, the most sessions a
// preview may ask to list.
func (s *Server) SetMaxPerPass(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ret.maxPerPass = n
}

// Dispose answers a preview of what a rule would delete, or queues the job
// the preview's token was given for.
func (h retentionHandler) Dispose(_ context.Context, req *v1.DisposeRequest) (*v1.DisposeResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := s.ruleOf(req.GetTenant(), req.GetRule()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return nil, fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	if req.GetValidateOnly() {
		bound := int32(1000)
		if h.s.ret.maxPerPass > 0 && h.s.ret.maxPerPass < 1000 {
			bound = int32(h.s.ret.maxPerPass) // #nosec G115 -- below 1000
		}
		if n := req.GetMaxSessions(); n < 0 || n > 1000 || n > bound {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("max_sessions: is 1 to 1000 and no more than retention.max-per-pass (%d); 0 means 100", bound))
		}
	}
	var listed []*v1.DueSession
	var held int64
	for _, d := range s.ret.due[req.GetTenant()] {
		switch {
		case d.GetRule() != req.GetRule():
		case d.GetHeld():
			held++
		default:
			listed = append(listed, d)
		}
	}
	if len(listed) == 0 {
		return nil, fault.New(fault.Precondition, "nothing is due under the rule")
	}
	token := disposeToken(req.GetRule(), len(listed))
	if req.GetValidateOnly() {
		var counts v1.CorrectionCounts
		for _, d := range listed {
			counts.Sessions++
			counts.Instances += d.GetInstances()
			counts.Bytes += d.GetBytes()
		}
		return &v1.DisposeResponse{Preview: &v1.DisposalPreview{Rule: req.GetRule(), Sessions: listed, SessionsHeld: held, Counts: &counts, Confirm: token}}, nil
	}
	if req.GetConfirm() != token {
		return nil, fault.New(fault.Precondition, "the confirm token does not match what the disposal would now delete")
	}
	j := &v1.Job{Id: s.newJobID(req.GetTenant()), State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{},
		Kind:       &v1.Job_Dispose{Dispose: &v1.DisposeJob{Rule: req.GetRule(), Reason: req.GetReason(), Sessions: int64(len(listed))}},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	return &v1.DisposeResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// ConfirmClock ends a pause when the time typed is within five minutes of the
// fake's clock, as the contract says.
func (h retentionHandler) ConfirmClock(_ context.Context, req *v1.ConfirmClockRequest) (*v1.ConfirmClockResponse, error) {
	s := h.s
	tenant, _, err := scopeOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if tenant == "" {
		return nil, fault.New(fault.Invalid, "scope: the fake confirms one tenant's clock")
	}
	if err := s.checkTenant(tenant); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetReason()) == "" || req.GetObservedTime() == nil {
		return nil, fault.New(fault.Invalid, "a reason and an observed_time are required")
	}
	if d := req.GetObservedTime().AsTime().Sub(s.now()); d > 5*time.Minute || d < -5*time.Minute {
		return nil, fault.New(fault.Precondition, "observed_time differs from the server's clock by more than 5 minutes")
	}
	rs := s.ret.status[tenant]
	if rs == nil {
		rs = &v1.RetentionStatus{}
	}
	rs = proto.Clone(rs).(*v1.RetentionStatus)
	rs.Paused, rs.PauseReason = false, ""
	if s.ret.status == nil {
		s.ret.status = map[string]*v1.RetentionStatus{}
	}
	s.ret.status[tenant] = rs
	return &v1.ConfirmClockResponse{Tenants: []string{tenant}, Status: rs}, nil
}

// PlaceHold places a hold when the fake serves holds, and answers
// UNIMPLEMENTED as the real server does until it does.
func (h retentionHandler) PlaceHold(_ context.Context, req *v1.PlaceHoldRequest) (*v1.PlaceHoldResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !s.ret.holdsServed {
		return nil, errNotServed
	}
	if strings.TrimSpace(req.GetReason()) == "" {
		return nil, fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	s.ret.next++
	hold := &v1.Hold{Id: fmt.Sprintf("hold_%d", s.ret.next), Target: proto.Clone(req.GetTarget()).(*v1.HoldTarget), Reason: req.GetReason(),
		State: v1.HoldState_HOLD_STATE_ACTIVE, PlacedByKeyId: "k00000000001", PlaceTime: timestamppb.New(s.now()), Storages: []string{"main"}}
	if s.ret.holds == nil {
		s.ret.holds = map[string][]*v1.Hold{}
	}
	s.ret.holds[req.GetTenant()] = append([]*v1.Hold{hold}, s.ret.holds[req.GetTenant()]...)
	return &v1.PlaceHoldResponse{Hold: proto.Clone(hold).(*v1.Hold)}, nil
}

// holdOf finds a tenant's hold by id.
func (s *Server) holdOf(tenant, id string) (*v1.Hold, error) {
	for _, h := range s.ret.holds[tenant] {
		if h.GetId() == id {
			return h, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no hold with that id")
}

// ReleaseHold releases a hold when the fake serves holds.
func (h retentionHandler) ReleaseHold(_ context.Context, req *v1.ReleaseHoldRequest) (*v1.ReleaseHoldResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !s.ret.holdsServed {
		return nil, errNotServed
	}
	hold, err := s.holdOf(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	hold.State, hold.ReleaseReason, hold.ReleaseTime, hold.ReleasedByKeyId = v1.HoldState_HOLD_STATE_RELEASED, req.GetReason(), timestamppb.New(s.now()), "k00000000001"
	return &v1.ReleaseHoldResponse{Hold: proto.Clone(hold).(*v1.Hold)}, nil
}

// GetHold answers one hold.
func (h retentionHandler) GetHold(_ context.Context, req *v1.GetHoldRequest) (*v1.GetHoldResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	hold, err := s.holdOf(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.GetHoldResponse{Hold: proto.Clone(hold).(*v1.Hold)}, nil
}

// ListHolds answers a page of a tenant's holds, newest first as given, by the
// states and the subject asked for.
func (h retentionHandler) ListHolds(_ context.Context, req *v1.ListHoldsRequest) (*v1.ListHoldsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*v1.Hold
	for _, hold := range s.ret.holds[req.GetTenant()] {
		if (len(req.GetStates()) == 0 || slices.Contains(req.GetStates(), hold.GetState())) &&
			(req.GetSubjectLabel() == "" || req.GetSubjectLabel() == hold.GetTarget().GetSubjectLabel()) {
			all = append(all, hold)
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListHoldsResponse{Holds: all[from:to], NextPageToken: next}, nil
}
