package fakeserver

import (
	"context"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// transfers is what the fake keeps for TransferService: the transfers made,
// by tenant and id. Only the flows are exercised (a preview, a confirmed
// move, a list and a show), not the server's rules, which the API's own tests
// hold.
type transfers struct {
	made map[string]*v1.Transfer
}

// transferHandler is the fake's TransferService.
type transferHandler struct {
	nopacsv1connect.UnimplementedTransferServiceHandler
	s *Server
}

var _ nopacsv1connect.TransferServiceHandler = transferHandler{}

// registerTransfers adds the fake's TransferService.
func (s *Server) registerTransfers(srv *connect.Server) {
	nopacsv1connect.RegisterTransferServiceHandler(srv, transferHandler{s: s})
}

// CreateTransfer answers a transfer's preview, or makes the job: one
// instance per session named, the receiver the request names or "xfer".
func (h transferHandler) CreateTransfer(_ context.Context, req *v1.CreateTransferRequest) (*v1.CreateTransferResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetSubjectLabel() == "" || req.GetTargetTenant() == "" || req.GetTargetTenant() == req.GetTenant() {
		return nil, fault.New(fault.Invalid, "subject_label and target_tenant: name a subject and another tenant")
	}
	if err := s.checkTenant(req.GetTargetTenant()); err != nil {
		return nil, err
	}
	receiver := req.GetReceiver()
	if receiver == "" {
		receiver = "xfer"
	}
	p := &v1.TransferPreview{Sessions: 1, Scans: 1, Instances: 3, Bytes: 3000, Receiver: receiver, RefusalsCounted: true}
	if req.GetValidateOnly() {
		return &v1.CreateTransferResponse{Preview: p}, nil
	}
	if s.xfers.made == nil {
		s.xfers.made = map[string]*v1.Transfer{}
	}
	for _, t := range s.xfers.made {
		if t.GetSubjectLabel() == req.GetSubjectLabel() && !jobEnded(t.GetJob().GetState()) {
			return nil, fault.New(fault.Precondition, "a transfer of this subject has not ended; one runs at a time over a subject")
		}
	}
	j := &v1.Job{Id: s.newJobID(req.GetTenant()), State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{Instances: p.GetInstances(), Bytes: p.GetBytes()},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	t := &v1.Transfer{Job: j, SubjectLabel: req.GetSubjectLabel(), SessionLabel: req.GetSessionLabel(), TargetTenant: req.GetTargetTenant(), Receiver: receiver,
		Move: req.GetMove(), Reason: req.GetReason(), Step: v1.TransferStep_TRANSFER_STEP_LISTING, Items: &v1.TransferItemCounts{}}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	s.xfers.made[key(req.GetTenant(), j.GetId())] = t
	return &v1.CreateTransferResponse{Preview: p, Transfer: t}, nil
}

// GetTransfer answers one transfer.
func (h transferHandler) GetTransfer(_ context.Context, req *v1.GetTransferRequest) (*v1.GetTransferResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	t, ok := s.xfers.made[key(req.GetTenant(), req.GetId())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no transfer with this id")
	}
	return &v1.GetTransferResponse{Transfer: t}, nil
}

// ListTransfers answers a page of the tenant's transfers, newest first.
func (h transferHandler) ListTransfers(_ context.Context, req *v1.ListTransfersRequest) (*v1.ListTransfersResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, k := range forTenant(s.xfers.made, req.GetTenant()) {
		keys = append([]string{k}, keys...)
	}
	page, next, err := pageOfKeys(keys, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListTransfersResponse{NextPageToken: next}
	for _, k := range page {
		out.Transfers = append(out.Transfers, s.xfers.made[k])
	}
	return out, nil
}
