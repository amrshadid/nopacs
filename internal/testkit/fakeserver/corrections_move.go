package fakeserver

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Move answers a move's preview, one session of one scan, or records the
// move committed; its token is the session's labels written out, as a
// delete's is the target's.
func (h correctionHandler) Move(_ context.Context, req *v1.MoveRequest) (*v1.MoveResponse, error) {
	t := &v1.CorrectionTarget{SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel()}
	p, c, err := h.move(req.GetTenant(), t, req.GetToSubjectLabel(), req.GetReason(), req.GetConfirm(), req.GetValidateOnly())
	if err != nil {
		return nil, err
	}
	return &v1.MoveResponse{Preview: p, Correction: c}, nil
}

// Merge answers a merge's preview, or records the merge committed, as
// Move.
func (h correctionHandler) Merge(_ context.Context, req *v1.MergeRequest) (*v1.MergeResponse, error) {
	t := &v1.CorrectionTarget{SubjectLabel: req.GetSubjectLabel()}
	p, c, err := h.move(req.GetTenant(), t, req.GetIntoSubjectLabel(), req.GetReason(), req.GetConfirm(), req.GetValidateOnly())
	if err != nil {
		return nil, err
	}
	return &v1.MergeResponse{Preview: p, Correction: c}, nil
}

// move is Move's and Merge's.
func (h correctionHandler) move(tenant string, t *v1.CorrectionTarget, to, reason, confirm string, validateOnly bool) (*v1.MovePreview,
	*v1.Correction, error) {
	s := h.s
	err := s.beginCorrection(tenant, reason, true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if err := s.refuseMasked(messageMaskedDestination); err != nil {
		return nil, nil, err
	}
	if t.GetSubjectLabel() == "" || to == "" || to == t.GetSubjectLabel() {
		return nil, nil, fault.New(fault.Invalid, "to_subject_label: names another subject")
	}
	if validateOnly {
		sessions := []string{t.GetSessionLabel()}
		if t.GetSessionLabel() == "" {
			sessions = []string{"S1"}
		}
		return &v1.MovePreview{Counts: &v1.CorrectionCounts{Sessions: 1, Scans: 1, Instances: 1}, Sessions: sessions, Storages: []string{"main"},
			IssueTime: timestamppb.New(s.now()), PurgeTime: timestamppb.New(s.now().AddDate(0, 0, 7)), Confirm: tokenOf(t)}, nil, nil
	}
	if confirm != tokenOf(t) {
		return nil, nil, fault.New(fault.Precondition, "the confirm token is not this server's preview of the same move")
	}
	c := s.newCorrection(tenant, &v1.Correction{Kind: v1.CorrectionKind_CORRECTION_KIND_MOVE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: t, ToLabel: to, Reason: reason, Counts: &v1.CorrectionCounts{Sessions: 1, Scans: 1, Instances: 1},
		CommitTime: timestamppb.New(s.now()), PurgeTime: timestamppb.New(s.now().AddDate(0, 0, 7))})
	return nil, c, nil
}
