package api

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/transfer"
)

// The audit details of a transfer.
const (
	detailTargetTenant = "target_tenant"
	detailMove         = "move"
)

// transferService is TransferService: it makes the transfer jobs of a
// tenant, which the server's runner (package transfer) works.
type transferService struct{ c *core }

var _ nopacsv1connect.TransferServiceHandler = transferService{}

// transferDetails returns a transfer request's audit details: where it goes,
// whether it moves and whether it is a preview, never the labels.
func transferDetails(r *v1.CreateTransferRequest) map[string]string {
	return map[string]string{detailTargetTenant: r.GetTargetTenant(), detailMove: strconv.FormatBool(r.GetMove()),
		detailValidateOnly: strconv.FormatBool(r.GetValidateOnly())}
}

// transfersOf returns the tenant's transfers of st.
func transfersOf(st tenant.Store) (tenant.Transfers, error) {
	db, ok := st.(tenant.Transfers)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no transfers")
	}
	return db, nil
}

// checkTransfer returns the Invalid error of a transfer request, or what it
// transfers.
func checkTransfer(req *v1.CreateTransferRequest) (tenant.JobTarget, error) {
	t := tenant.JobTarget{SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel()}
	if req.GetSubjectLabel() == "" {
		return t, invalid("subject_label", "is required")
	}
	if err := tenant.ValidateLabel("subject", t.SubjectLabel); err != nil {
		return t, fieldOf("subject_label", err)
	}
	if t.SessionLabel != "" {
		if err := tenant.ValidateLabel("session", t.SessionLabel); err != nil {
			return t, fieldOf("session_label", err)
		}
	}
	if err := itemName("target_tenant", req.GetTargetTenant()); err != nil {
		return t, err
	}
	if req.GetTargetTenant() == req.GetTenant() {
		return t, invalid("target_tenant", "is another tenant than the source")
	}
	if req.GetReceiver() != "" {
		if err := itemName("receiver", req.GetReceiver()); err != nil {
			return t, err
		}
	}
	if err := tenant.ValidateUndoReason(req.GetReason()); err != nil {
		return t, fieldOf("reason", err)
	}
	return t, nil
}

// transferReceiver returns the receiver of type job of the target a transfer
// goes through: the one named, or the only enabled one the target has.
func transferReceiver(ctx context.Context, tp tenant.Pipeline, name string) (tenant.Receiver, error) {
	if name != "" {
		r, err := tp.Receiver(ctx, name)
		if fault.KindOf(err) == fault.NotFound || err == nil && (r.Type != tenant.ReceiverJob || !r.Enabled) {
			return tenant.Receiver{}, precondition("the target has no enabled receiver of type job named " + name)
		}
		return r, err
	}
	var found []tenant.Receiver
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rs, next, err := tp.ListReceivers(ctx, page)
		if err != nil {
			return tenant.Receiver{}, err
		}
		for _, r := range rs {
			if r.Type == tenant.ReceiverJob && r.Enabled {
				found = append(found, r)
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	switch len(found) {
	case 0:
		return tenant.Receiver{}, precondition("the target has no enabled receiver of type job")
	case 1:
		return found[0], nil
	}
	return tenant.Receiver{}, precondition("the target has several enabled receivers of type job; name one in receiver")
}

// CreateTransfer previews, or makes, a transfer.
func (s transferService) CreateTransfer(ctx context.Context, req *v1.CreateTransferRequest) (*v1.CreateTransferResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.TransferServiceCreateTransferProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: transferDetails(req)}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	target, err := checkTransfer(req)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	facts.Details[detailReason] = req.GetReason()
	db, err := transfersOf(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	// The target: found by its name, and the caller's key must be writer on
	// it. One that does not exist is told apart from one without a grant by
	// nobody who may not list every tenant.
	trow, err := c.master.TenantByName(ctx, req.GetTargetTenant())
	if fault.KindOf(err) == fault.NotFound {
		return nil, c.failWith(d, st, facts, noTenant(d))
	}
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if err := c.master.CheckActor(ctx, d.Actor(master.TenantScope(trow.ID), master.RoleWriter)); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	tst, err := c.reg.Store(req.GetTargetTenant())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	tp, err := c.pipeline(tst)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rcv, err := transferReceiver(ctx, tp, req.GetReceiver())
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetValidateOnly() {
		pv, err := transfer.PreviewOf(ctx, p, target, rcv.Deidentifies())
		if err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		c.record(d, st, master.OutcomeDone, "", facts)
		c.record(d, tst, master.OutcomeDone, "", facts)
		return &v1.CreateTransferResponse{Preview: transferPreviewMessage(pv, rcv)}, nil
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	facts.Details[detailJob] = id
	job, err := db.QueueTransfer(ctx, tenant.TransferQueue{ID: id, Target: target, TargetTenant: trow.Name, TargetTenantID: trow.ID, Receiver: rcv.Name,
		SourceTenant: req.GetTenant(), Move: req.GetMove(), Reason: req.GetReason(), KeyID: callerKeyID(d), Now: c.now()}, changeEntry(d, facts),
		c.roleGuard(d, master.RoleAdmin))
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	// The target's own entry: its log says that a transfer into it was made,
	// by whom and from where, as the source's does.
	c.record(d, tst, master.OutcomeDone, "", facts)
	tj, err := db.TransferJob(ctx, job.ID)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	return &v1.CreateTransferResponse{Transfer: transferMessage(tj)}, nil
}

// transferPreviewMessage returns a preview as the contract writes it.
func transferPreviewMessage(p transfer.Preview, rcv tenant.Receiver) *v1.TransferPreview {
	return &v1.TransferPreview{Sessions: p.Sessions, Scans: p.Scans, Instances: p.Instances, Bytes: p.Bytes, Receiver: rcv.Name,
		ReceiverDeidentifies: rcv.Deidentifies(), RefusedInstances: p.Refused, RefusalsCounted: p.RefusalsCounted}
}

// transferMessage returns a transfer as the contract writes it.
func transferMessage(tj tenant.TransferJob) *v1.Transfer {
	return &v1.Transfer{Job: jobMessage(tj.Job, ""), SubjectLabel: tj.Job.Target.SubjectLabel, SessionLabel: tj.Job.Target.SessionLabel,
		TargetTenant: tj.TargetTenant, Receiver: tj.Receiver, Move: tj.Move, Reason: tj.Reason,
		Step:               enumOf[v1.TransferStep](v1.TransferStep_value, "TRANSFER_STEP_", string(tj.Step)),
		Items:              &v1.TransferItemCounts{Listed: tj.Items.Listed, Copied: tj.Items.Copied, Confirmed: tj.Items.Confirmed, Failed: tj.Items.Failed},
		DeleteCorrectionId: tj.DeleteCorrection}
}

// GetTransfer answers one transfer.
func (s transferService) GetTransfer(ctx context.Context, req *v1.GetTransferRequest) (*v1.GetTransferResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.TransferServiceGetTransferProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	db, err := transfersOf(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	tj, err := db.TransferJob(ctx, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetTransferResponse{Transfer: transferMessage(tj)}, nil
}

// ListTransfers answers a page of transfers.
func (s transferService) ListTransfers(ctx context.Context, req *v1.ListTransfersRequest) (*v1.ListTransfersResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.TransferServiceListTransfersProcedure)
	if err != nil {
		return nil, err
	}
	_, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	db, err := transfersOf(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	tjs, next, err := db.ListTransfers(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, req.GetPageToken()))
	}
	out := &v1.ListTransfersResponse{Transfers: make([]*v1.Transfer, len(tjs)), NextPageToken: next}
	for i, tj := range tjs {
		out.Transfers[i] = transferMessage(tj)
	}
	c.done(d, st)
	return out, nil
}
