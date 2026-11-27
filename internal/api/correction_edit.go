package api

import (
	"context"
	"fmt"
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

// detailInstanceCount is how many instances an edit's preview lists.
const detailInstanceCount = "instances"

// checkEdit returns the Invalid error of an edit request, or nil.
func checkEdit(req *v1.EditRequest, target tenant.CorrectionTarget) error {
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return fieldOf("reason", err)
	}
	if err := target.Validate(); err != nil {
		return fieldOf("target", err)
	}
	switch {
	case req.GetValidateOnly() && req.GetConfirm() != "":
		return invalid("confirm", "is empty with validate_only")
	case !req.GetValidateOnly() && req.GetConfirm() == "":
		return invalid("confirm", "is the confirm token of a preview (validate_only) of the same edit")
	case len(req.GetConfirm()) > tenant.MaxConfirmLength:
		return invalid("confirm", "is at most 128 bytes")
	case req.GetShowValues() && !req.GetValidateOnly():
		return invalid("show_values", "is asked only with validate_only")
	}
	return nil
}

// editChanges reads an edit's changes, each attribute by its keyword or
// as gggg,eeee; what an edit may change is the engine's to judge.
func editChanges(in []*v1.EditChange) ([]correct.EditChange, error) {
	if len(in) == 0 || len(in) > correct.MaxEditChanges {
		return nil, invalid("changes", "names 1 to "+strconv.Itoa(correct.MaxEditChanges)+" attributes")
	}
	out := make([]correct.EditChange, 0, len(in))
	for i, ch := range in {
		t, err := correct.ParseEditTag(ch.GetAttribute())
		if err != nil {
			return nil, invalid(fmt.Sprintf("changes[%d].attribute", i), fault.Message(err))
		}
		out = append(out, correct.EditChange{Tag: t, Value: ch.GetValue(), Remove: ch.GetRemove()})
	}
	return out, nil
}

// Edit answers an edit's preview, or makes the edit its confirm token was
// given for.
func (s correctionService) Edit(ctx context.Context, req *v1.EditRequest) (*v1.EditResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.CorrectionServiceEditProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: editDetails(req)}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	target := targetOf(req.GetTarget())
	if err := checkEdit(req, target); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	changes, err := editChanges(req.GetChanges())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	e, release, err := c.engine(ctx, req.GetTenant(), st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	defer release()
	if req.GetValidateOnly() {
		p, err := e.PreviewEdit(ctx, correct.EditPreviewRequest{Target: target, Changes: changes, ShowValues: req.GetShowValues()})
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		facts.Details[detailInstanceCount] = strconv.Itoa(len(p.Instances))
		c.record(d, st, master.OutcomeDone, "", facts)
		return &v1.EditResponse{Preview: editPreviewMessage(p)}, nil
	}
	made, err := e.Edit(ctx, correct.EditRequest{Target: target, Changes: changes, Reason: req.GetReason(), Confirm: req.GetConfirm(),
		KeyID: callerKeyID(d), Audit: changeEntry(d, facts), Guard: c.roleGuard(d, master.RoleAdmin)})
	switch {
	case err != nil && made.State == record.StateAbandoned:
		// Recorded, then abandoned with its reason: the contract answers
		// the correction so, its entry written with it.
		c.log.Warn("api: an edit was abandoned", "correction", made.ID, "reason", made.StateReason, "error", fault.Message(err))
	case err != nil && made.ID != "":
		return nil, c.toAPI(d, err)
	case err != nil:
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.EditResponse{Correction: correctionMessage(made, true)}, nil
}

// editPreviewMessage returns an edit's preview as the contract writes it.
func editPreviewMessage(p correct.EditPreview) *v1.EditPreview {
	out := &v1.EditPreview{Storages: p.Storages, IssueTime: timestamppb.New(p.Issued), Confirm: p.Confirm}
	if out.Storages == nil {
		out.Storages = []string{}
	}
	for _, in := range p.Instances {
		m := &v1.EditPreviewInstance{SubjectLabel: in.Path.Subject, SessionLabel: in.Path.Session, ScanLabel: in.Path.Scan,
			InstanceId: in.UID, Revision: int32(in.Revision)} //nolint:gosec // G115: a revision is at most layout.MaxRevision.
		for _, a := range in.Attributes {
			m.Attributes = append(m.Attributes, &v1.EditPreviewAttribute{Attribute: tagWord(a.Tag), Present: a.Present, Known: a.Known,
				OldValue: a.Old, NewValue: a.New, Remove: a.Remove})
		}
		out.Instances = append(out.Instances, m)
	}
	return out
}
