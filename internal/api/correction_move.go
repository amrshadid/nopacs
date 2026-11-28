package api

import (
	"context"
	"strconv"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// detailRewriteIdentity says whether a move asks its identity rewrite.
const detailRewriteIdentity = "rewrite_identity"

// moveDetails returns a move's audit details: the session's path of
// labels, the destination, whether it is a preview and whether it asks the
// identity rewrite.
func moveDetails(r *v1.MoveRequest) map[string]string {
	return map[string]string{detailTarget: labelPath(r.GetSubjectLabel(), r.GetSessionLabel()), detailToLabel: labelPath(r.GetToSubjectLabel()),
		detailValidateOnly: strconv.FormatBool(r.GetValidateOnly()), detailRewriteIdentity: strconv.FormatBool(r.GetRewriteIdentity())}
}

// mergeDetails returns a merge's audit details, as moveDetails.
func mergeDetails(r *v1.MergeRequest) map[string]string {
	return map[string]string{detailTarget: labelPath(r.GetSubjectLabel()), detailToLabel: labelPath(r.GetIntoSubjectLabel()),
		detailValidateOnly: strconv.FormatBool(r.GetValidateOnly()), detailRewriteIdentity: strconv.FormatBool(r.GetRewriteIdentity())}
}

// moveAsk is what a Move or a Merge asks, read from its request.
type moveAsk struct {
	procedure, tenant string
	target            tenant.CorrectionTarget
	to, reason        string
	validateOnly      bool
	confirm           string
	rewrite           bool
	details           map[string]string
}

// Move answers a move's preview, or makes the move its token was given
// for and leaves its copy and its publish to the background.
func (s correctionService) Move(ctx context.Context, req *v1.MoveRequest) (*v1.MoveResponse, error) {
	p, c, err := s.move(ctx, moveAsk{procedure: nopacsv1connect.CorrectionServiceMoveProcedure, tenant: req.GetTenant(),
		target: tenant.CorrectionTarget{Subject: req.GetSubjectLabel(), Session: req.GetSessionLabel()}, to: req.GetToSubjectLabel(),
		reason: req.GetReason(), validateOnly: req.GetValidateOnly(), confirm: req.GetConfirm(), rewrite: req.GetRewriteIdentity(),
		details: moveDetails(req)})
	if err != nil {
		return nil, err
	}
	return &v1.MoveResponse{Preview: p, Correction: c}, nil
}

// Merge answers a merge's preview, or makes the merge its token was given
// for and leaves its copy and its publish to the background.
func (s correctionService) Merge(ctx context.Context, req *v1.MergeRequest) (*v1.MergeResponse, error) {
	p, c, err := s.move(ctx, moveAsk{procedure: nopacsv1connect.CorrectionServiceMergeProcedure, tenant: req.GetTenant(),
		target: tenant.CorrectionTarget{Subject: req.GetSubjectLabel()}, to: req.GetIntoSubjectLabel(), reason: req.GetReason(),
		validateOnly: req.GetValidateOnly(), confirm: req.GetConfirm(), rewrite: req.GetRewriteIdentity(), details: mergeDetails(req)})
	if err != nil {
		return nil, err
	}
	return &v1.MergeResponse{Preview: p, Correction: c}, nil
}

// checkMove returns the Invalid error of a move or a merge, or nil.
func checkMove(a moveAsk) error {
	if err := tenant.ValidateUndoReason(a.reason); err != nil {
		return fieldOf("reason", err)
	}
	if err := tenant.ValidateMove(a.target, a.to); err != nil {
		return fieldOf("to_subject_label", err)
	}
	switch {
	case a.validateOnly && a.confirm != "":
		return invalid("confirm", "is empty with validate_only")
	case !a.validateOnly && a.confirm == "":
		return invalid("confirm", "is the confirm token of a preview (validate_only) of the same move")
	case len(a.confirm) > tenant.MaxConfirmLength:
		return invalid("confirm", "is at most 128 bytes")
	}
	return nil
}

// move runs a Move or a Merge: the preview, or the move recorded and its
// copy, publish and identity rewrite started in the background.
func (s correctionService) move(ctx context.Context, a moveAsk) (*v1.MovePreview, *v1.Correction, error) {
	c := s.c
	d, err := c.changing(ctx, a.procedure)
	if err != nil {
		return nil, nil, err
	}
	facts := access.Facts{Details: a.details}
	_, st, err := c.inTenant(ctx, d, a.tenant, facts.Details)
	if err != nil {
		return nil, nil, err
	}
	if err := checkMove(a); err != nil {
		return nil, nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = a.reason
	e, release, err := c.engine(ctx, a.tenant, st)
	if err != nil {
		return nil, nil, c.failWith(d, st, facts, err)
	}
	defer release()
	if a.validateOnly {
		p, err := e.PreviewMove(ctx, a.target, a.to)
		if err != nil {
			return nil, nil, c.failWith(d, st, facts, err)
		}
		c.record(d, st, master.OutcomeDone, "", facts)
		return movePreviewMessage(p), nil, nil
	}
	made, err := e.Move(ctx, correct.MoveRequest{Target: a.target, To: a.to, Reason: a.reason, Confirm: a.confirm, KeyID: callerKeyID(d),
		RewriteIdentity: a.rewrite, Audit: changeEntry(d, facts), Guard: c.roleGuard(d, master.RoleAdmin)})
	if err != nil {
		if made.ID != "" {
			// Recorded, then abandoned: its entry was written with it.
			return nil, nil, c.toAPI(d, err)
		}
		return nil, nil, c.failWith(d, st, facts, err)
	}
	if made.State == record.StatePrepared {
		id := made.ID
		c.background(a.tenant, st, "move", func(ctx context.Context, e *correct.Engine) error {
			pc, err := e.ContinueRelabel(ctx, id)
			if err != nil || pc.State != record.StateCommitted || !pc.RewriteIdentity {
				return err
			}
			// Edits it refuses are abandoned with their reasons, each its own
			// correction; the move stands.
			if _, err := e.RewriteIdentity(ctx, id); err != nil && fault.KindOf(err) == fault.Unavailable {
				return err
			}
			return nil
		})
	}
	return nil, correctionMessage(made, true), nil
}

// movePreviewMessage returns a move's preview as the contract writes it.
func movePreviewMessage(p correct.MovePreview) *v1.MovePreview {
	m := &v1.MovePreview{Counts: correctionCounts(p.Counts), Sessions: p.Sessions, Storages: p.Storages, Holds: p.Holds,
		Clashes: p.Clashes, IssueTime: timestamppb.New(p.Issued), PurgeTime: timestamp(p.PurgeAfter), Confirm: p.Confirm}
	if m.Sessions == nil {
		m.Sessions = []string{}
	}
	if m.Storages == nil {
		m.Storages = []string{}
	}
	return m
}
