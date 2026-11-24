package fakeserver

import (
	"context"
	"fmt"
	"strings"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The fake's corrections: each tenant's, newest last, kept as answered. A
// delete's preview counts one instance per level of its target, and its
// token is the target written out, so a command's flow (preview, confirm,
// act) is what is exercised, not the server's rules, which the API's own
// tests hold.
type corrections struct {
	byTenant map[string][]*v1.Correction
	next     int
}

// correctionHandler is the fake's CorrectionService.
type correctionHandler struct{ s *Server }

var _ nopacsv1connect.CorrectionServiceHandler = correctionHandler{}

// registerCorrections adds the fake's CorrectionService.
func (s *Server) registerCorrections(srv *connect.Server) {
	nopacsv1connect.RegisterCorrectionServiceHandler(srv, correctionHandler{s})
}

// AddCorrection gives tenant a correction, as the server would have made it.
func (s *Server) AddCorrection(tenant string, c *v1.Correction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.corr.byTenant == nil {
		s.corr.byTenant = map[string][]*v1.Correction{}
	}
	s.corr.byTenant[tenant] = append(s.corr.byTenant[tenant], proto.Clone(c).(*v1.Correction))
}

// beginCorrection takes the lock and checks the tenant and the reason.
func (s *Server) beginCorrection(tenant, reason string, needReason bool) error {
	if err := s.begin(tenant); err != nil {
		return err
	}
	if s.corr.byTenant == nil {
		s.corr.byTenant = map[string][]*v1.Correction{}
	}
	if needReason && (strings.TrimSpace(reason) == "" || len(reason) > 1024) {
		return fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	return nil
}

// newCorrection records a correction made now and returns it.
func (s *Server) newCorrection(tenant string, c *v1.Correction) *v1.Correction {
	s.corr.next++
	c.Id = fmt.Sprintf("01jd%022d", s.corr.next)
	c.CreateTime = timestamppb.New(s.now())
	c.Origin = v1.CorrectionOrigin_CORRECTION_ORIGIN_PERSON
	if c.Counts == nil {
		c.Counts = &v1.CorrectionCounts{}
	}
	s.corr.byTenant[tenant] = append(s.corr.byTenant[tenant], c)
	return c
}

// findCorrection returns a tenant's correction of that id.
func (s *Server) findCorrection(tenant, id string) (*v1.Correction, error) {
	for _, c := range s.corr.byTenant[tenant] {
		if c.GetId() == id {
			return c, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no correction with this id")
}

// tokenOf is the fake's confirm token of a target.
func tokenOf(t *v1.CorrectionTarget) string {
	return "fake." + t.GetSubjectLabel() + "/" + t.GetSessionLabel() + "/" + t.GetScanLabel() + "/" + t.GetInstanceId()
}

// Delete answers a preview, or makes the delete its token was given for.
func (h correctionHandler) Delete(_ context.Context, req *v1.DeleteRequest) (*v1.DeleteResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	t := req.GetTarget()
	if t.GetSubjectLabel() == "" {
		return nil, fault.New(fault.Invalid, "target: names a subject")
	}
	counts := &v1.CorrectionCounts{Subjects: 1, Sessions: 1, Scans: 1, Instances: 1, Bytes: 1024}
	prev := &v1.DeletePreview{Counts: counts, Storages: []string{"main"}, Confirm: tokenOf(t), PurgeTime: timestamppb.New(s.now().AddDate(0, 0, 7)),
		StoragePurges:  []*v1.StoragePurge{{Storage: "main", Expected: v1.ObjectDisposition_OBJECT_DISPOSITION_REMOVED, Why: "directory"}},
		HeldAfterPurge: []string{t.GetSubjectLabel()}}
	if req.GetValidateOnly() {
		return &v1.DeleteResponse{Preview: prev}, nil
	}
	if req.GetConfirm() != tokenOf(t) {
		return nil, fault.New(fault.Precondition, "the confirm token does not match what the delete would now remove")
	}
	c := s.newCorrection(req.GetTenant(), &v1.Correction{Kind: v1.CorrectionKind_CORRECTION_KIND_DELETE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: t, Reason: req.GetReason(), Counts: counts, CommitTime: timestamppb.New(s.now()), PurgeTime: prev.GetPurgeTime(),
		Holds: []*v1.HeldValue{{Kind: v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT, Value: t.GetSubjectLabel()}}})
	prev.Confirm = ""
	return &v1.DeleteResponse{Preview: prev, Correction: c}, nil
}

// Relabel records a relabel, switched at once.
func (h correctionHandler) Relabel(_ context.Context, req *v1.RelabelRequest) (*v1.RelabelResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := s.newCorrection(req.GetTenant(), &v1.Correction{Kind: v1.CorrectionKind_CORRECTION_KIND_RELABEL, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: req.GetTarget(), ToLabel: req.GetToLabel(), Reason: req.GetReason(), CommitTime: timestamppb.New(s.now())})
	return &v1.RelabelResponse{Correction: c}, nil
}

// Replace records a replace of the quarantined file named.
func (h correctionHandler) Replace(_ context.Context, req *v1.ReplaceRequest) (*v1.ReplaceResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := s.arch.findFile(req.GetTenant(), req.GetQuarantinedFile())
	if err != nil {
		return nil, err
	}
	c := s.newCorrection(req.GetTenant(), &v1.Correction{Kind: v1.CorrectionKind_CORRECTION_KIND_REPLACE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		QuarantinedFile: f.GetId(), Reason: req.GetReason(), OldRevision: 1, NewRevision: 2, CommitTime: timestamppb.New(s.now()),
		Target: &v1.CorrectionTarget{SubjectLabel: "S1", SessionLabel: "E1", ScanLabel: "T1", InstanceId: f.GetSopInstanceUid()}})
	f.State = v1.QuarantineState_QUARANTINE_STATE_RELEASED
	return &v1.ReplaceResponse{Correction: c}, nil
}

// ListCorrections answers a tenant's corrections, newest first, in one page.
func (h correctionHandler) ListCorrections(_ context.Context, req *v1.ListCorrectionsRequest) (*v1.ListCorrectionsResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &v1.ListCorrectionsResponse{Corrections: []*v1.Correction{}}
	all := s.corr.byTenant[req.GetTenant()]
	for i := len(all) - 1; i >= 0; i-- {
		c := all[i]
		if len(req.GetKinds()) > 0 && !containsEnum(req.GetKinds(), c.GetKind()) || len(req.GetStates()) > 0 && !containsEnum(req.GetStates(), c.GetState()) {
			continue
		}
		listed := proto.Clone(c).(*v1.Correction)
		listed.Holds = nil
		out.Corrections = append(out.Corrections, listed)
	}
	return out, nil
}

// containsEnum reports whether v is among values.
func containsEnum[E comparable](values []E, v E) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// GetCorrection answers one correction.
func (h correctionHandler) GetCorrection(_ context.Context, req *v1.GetCorrectionRequest) (*v1.GetCorrectionResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, err := s.findCorrection(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.GetCorrectionResponse{Correction: c}, nil
}

// UndoCorrection undoes a committed delete.
func (h correctionHandler) UndoCorrection(_ context.Context, req *v1.UndoCorrectionRequest) (*v1.UndoCorrectionResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, err := s.findCorrection(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case c.GetState() == v1.CorrectionState_CORRECTION_STATE_UNDONE:
	case c.GetKind() == v1.CorrectionKind_CORRECTION_KIND_DELETE && c.GetState() == v1.CorrectionState_CORRECTION_STATE_COMMITTED:
		c.State, c.UndoReason, c.EndTime, c.PurgeTime, c.Holds = v1.CorrectionState_CORRECTION_STATE_UNDONE, req.GetReason(), timestamppb.New(s.now()), nil, nil
	default:
		return nil, fault.New(fault.Precondition, "the correction is neither a committed delete nor a prepared relabel")
	}
	return &v1.UndoCorrectionResponse{Correction: c}, nil
}

// PurgeCorrection purges a committed correction at once.
func (h correctionHandler) PurgeCorrection(_ context.Context, req *v1.PurgeCorrectionRequest) (*v1.PurgeCorrectionResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, err := s.findCorrection(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch c.GetState() {
	case v1.CorrectionState_CORRECTION_STATE_PURGED:
	case v1.CorrectionState_CORRECTION_STATE_COMMITTED:
		c.State, c.EndTime = v1.CorrectionState_CORRECTION_STATE_PURGED, timestamppb.New(s.now())
		c.Counts.ObjectsOwned, c.Counts.ObjectsRemoved, c.Counts.BytesRemoved = 3, 3, c.GetCounts().GetBytes()
		c.Dispositions = []*v1.StorageDisposition{{Storage: "main", ObjectsRemoved: 3}}
	default:
		return nil, fault.New(fault.Precondition, "the correction is not committed")
	}
	return &v1.PurgeCorrectionResponse{Correction: c}, nil
}

// AllowArrivals lets one value of a purged delete arrive again.
func (h correctionHandler) AllowArrivals(_ context.Context, req *v1.AllowArrivalsRequest) (*v1.AllowArrivalsResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c, err := s.findCorrection(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	if c.GetState() != v1.CorrectionState_CORRECTION_STATE_PURGED {
		return nil, fault.New(fault.Precondition, "a delete's arrivals are allowed once it is purged")
	}
	kept := c.Holds[:0]
	found := false
	for _, v := range c.GetHolds() {
		if proto.Equal(v, req.GetValue()) {
			found = true
			continue
		}
		kept = append(kept, v)
	}
	if !found && !c.GetArrivalsAllowed() {
		return nil, fault.New(fault.Precondition, "the delete does not hold this value")
	}
	c.Holds, c.ArrivalsAllowed = kept, len(kept) == 0
	return &v1.AllowArrivalsResponse{Correction: c}, nil
}

// ListScrubFindings answers none: the fake runs no scrub.
func (h correctionHandler) ListScrubFindings(_ context.Context, req *v1.ListScrubFindingsRequest) (*v1.ListScrubFindingsResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &v1.ListScrubFindingsResponse{Findings: []*v1.ScrubFinding{}}, nil
}

// ResolveInstances answers every pair unknown: the fake holds no instance.
func (h correctionHandler) ResolveInstances(_ context.Context, req *v1.ResolveInstancesRequest) (*v1.ResolveInstancesResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &v1.ResolveInstancesResponse{}
	for _, m := range req.GetMembers() {
		out.Instances = append(out.Instances, &v1.ResolvedInstance{Member: m, Disposition: v1.InstanceDisposition_INSTANCE_DISPOSITION_UNKNOWN})
	}
	return out, nil
}
