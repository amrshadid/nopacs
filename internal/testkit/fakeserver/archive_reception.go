package fakeserver

import (
	"context"
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// receptionHandler implements ReceptionService.
type receptionHandler struct{ s *Server }

// ended says whether a step is one of the three that nothing leaves.
func ended(step v1.ReceptionStep) bool {
	return step == v1.ReceptionStep_RECEPTION_STEP_DONE || step == v1.ReceptionStep_RECEPTION_STEP_QUARANTINED || step == v1.ReceptionStep_RECEPTION_STEP_FAILED
}

// ListReceptions answers a page of receptions, newest first, with the
// contract's filters; the Study Instance UID filter is the fake's to allow
// since its caller is the local operator.
func (h receptionHandler) ListReceptions(_ context.Context, req *v1.ListReceptionsRequest) (*v1.ListReceptionsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, st := range req.GetSteps() {
		if st == v1.ReceptionStep_RECEPTION_STEP_UNSPECIFIED {
			return nil, fault.New(fault.Invalid, "steps: none may be unspecified")
		}
	}
	var all []*v1.Reception
	for _, k := range forTenant(s.arch.receptions, req.GetTenant()) {
		r := s.arch.receptions[k]
		switch {
		case req.GetReceiver() != "" && r.GetReceiver() != req.GetReceiver(),
			req.GetStudyInstanceUid() != "" && r.GetStudyInstanceUid() != req.GetStudyInstanceUid(),
			len(req.GetSteps()) > 0 && !slices.Contains(req.GetSteps(), r.GetStep()),
			req.GetInProgress() && ended(r.GetStep()),
			req.GetStartTime() != nil && r.GetStartTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !r.GetStartTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, r)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetStartTime().AsTime().After(all[j].GetStartTime().AsTime()) })
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListReceptionsResponse{Receptions: all[from:to], NextPageToken: next}, nil
}

// GetReception answers one reception with its arrivals; the scans are the
// ones a test gave through the reception's id, none in the fake.
func (h receptionHandler) GetReception(_ context.Context, req *v1.GetReceptionRequest) (*v1.GetReceptionResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, ok := s.arch.receptions[key(req.GetTenant(), req.GetId())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no reception with that id")
	}
	out := &v1.GetReceptionResponse{Reception: r}
	if r.GetStep() == v1.ReceptionStep_RECEPTION_STEP_DONE {
		out.Scans = []*v1.ReceptionScan{{Label: "scan-aaaa", SeriesInstanceUid: "1.2.3.4.1", Files: r.GetCounts().GetFilesArchived(), Bytes: r.GetCounts().GetBytesArchived()}}
	}
	for _, k := range forTenant(s.arch.receptions, req.GetTenant()) {
		o := s.arch.receptions[k]
		if o.GetStudyInstanceUid() == r.GetStudyInstanceUid() {
			out.Arrivals = append(out.Arrivals, &v1.ReceptionArrival{Id: o.GetId(), Arrival: o.GetArrival(), Step: o.GetStep(), StartTime: o.GetStartTime()})
		}
	}
	sort.SliceStable(out.Arrivals, func(i, j int) bool { return out.Arrivals[i].GetArrival() < out.Arrivals[j].GetArrival() })
	return out, nil
}

// RetryReception makes a new reception from a failed one, once.
func (h receptionHandler) RetryReception(_ context.Context, req *v1.RetryReceptionRequest) (*v1.RetryReceptionResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, ok := s.arch.receptions[key(req.GetTenant(), req.GetId())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no reception with that id")
	}
	if r.GetStep() != v1.ReceptionStep_RECEPTION_STEP_FAILED || r.GetDiscardTime() != nil {
		return nil, fault.New(fault.Precondition, "the reception has not failed, or its files were discarded")
	}
	if r.GetRetryReception() != "" {
		return &v1.RetryReceptionResponse{Reception: r, Retry: s.arch.receptions[key(req.GetTenant(), r.GetRetryReception())]}, nil
	}
	receiver := r.GetReceiver()
	if req.GetReceiver() != "" {
		if _, ok := s.arch.receivers[key(req.GetTenant(), req.GetReceiver())]; !ok {
			return nil, fault.New(fault.Precondition, "the receiver named does not exist")
		}
		receiver = req.GetReceiver()
	}
	n := &v1.Reception{
		Id: r.GetId() + "-retry", Receiver: receiver, StudyInstanceUid: r.GetStudyInstanceUid(), Arrival: r.GetArrival() + 1,
		Step: v1.ReceptionStep_RECEPTION_STEP_COMPLETE, Counts: &v1.ReceptionCounts{FilesReceived: r.GetCounts().GetFilesReceived() - r.GetCounts().GetFilesArchived()},
		StartTime: timestamppb.New(s.now()), StepTime: timestamppb.New(s.now()), RetryOf: r.GetId(),
	}
	r.RetryReception = n.GetId()
	s.arch.receptions[key(req.GetTenant(), n.GetId())] = n
	return &v1.RetryReceptionResponse{Reception: r, Retry: n}, nil
}

// DiscardReception records a failed reception's files as discarded, once.
func (h receptionHandler) DiscardReception(_ context.Context, req *v1.DiscardReceptionRequest) (*v1.DiscardReceptionResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetReason()) == "" || len(req.GetReason()) > 1024 {
		return nil, fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	r, ok := s.arch.receptions[key(req.GetTenant(), req.GetId())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no reception with that id")
	}
	if r.GetStep() != v1.ReceptionStep_RECEPTION_STEP_FAILED || r.GetRetryReception() != "" {
		return nil, fault.New(fault.Precondition, "the reception has not failed, or it was retried")
	}
	if r.GetDiscardTime() == nil {
		r.DiscardReason, r.DiscardTime = req.GetReason(), timestamppb.New(s.now())
	}
	return &v1.DiscardReceptionResponse{Reception: r}, nil
}

// quarantineHandler implements QuarantineService.
type quarantineHandler struct{ s *Server }

// ListQuarantinedFiles answers a page of records, newest first, with the
// contract's filters.
func (h quarantineHandler) ListQuarantinedFiles(_ context.Context, req *v1.ListQuarantinedFilesRequest) (*v1.ListQuarantinedFilesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*v1.QuarantinedFile
	for _, k := range forTenant(s.arch.files, req.GetTenant()) {
		f := s.arch.files[k]
		switch {
		case req.GetState() != v1.QuarantineState_QUARANTINE_STATE_UNSPECIFIED && f.GetState() != req.GetState(),
			req.GetReason() != v1.QuarantineReason_QUARANTINE_REASON_UNSPECIFIED && f.GetReason() != req.GetReason(),
			req.GetReceiver() != "" && f.GetReceiver() != req.GetReceiver(),
			req.GetReception() != "" && f.GetReception() != req.GetReception(),
			req.GetStudyInstanceUid() != "" && f.GetStudyInstanceUid() != req.GetStudyInstanceUid(),
			req.GetSopInstanceUid() != "" && f.GetSopInstanceUid() != req.GetSopInstanceUid(),
			req.GetStartTime() != nil && f.GetQuarantineTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !f.GetQuarantineTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, f)
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].GetQuarantineTime().AsTime().After(all[j].GetQuarantineTime().AsTime())
	})
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListQuarantinedFilesResponse{Files: all[from:to], NextPageToken: next}, nil
}

// findFile returns a tenant's quarantined file or the contract's NOT_FOUND.
func (a *archive) findFile(tenant, id string) (*v1.QuarantinedFile, error) {
	f, ok := a.files[key(tenant, id)]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no quarantined file with that id")
	}
	return f, nil
}

// GetQuarantinedFile answers one record, and its header when asked for.
func (h quarantineHandler) GetQuarantinedFile(_ context.Context, req *v1.GetQuarantinedFileRequest) (*v1.GetQuarantinedFileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := s.arch.findFile(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	out := &v1.GetQuarantinedFileResponse{File: f}
	if req.GetIncludeHeader() && f.GetContentKept() {
		out.HeaderJson = s.arch.headers[key(req.GetTenant(), req.GetId())]
	}
	return out, nil
}

// ReleaseQuarantinedFile releases a held file into a reception of the
// receiver it came to, or the one named.
func (h quarantineHandler) ReleaseQuarantinedFile(_ context.Context, req *v1.ReleaseQuarantinedFileRequest) (*v1.ReleaseQuarantinedFileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if req.GetMode() != v1.ReleaseMode_RELEASE_MODE_AS_IS {
		return nil, fault.New(fault.Invalid, "mode: only as-is is accepted")
	}
	if req.GetReason() != "" && (strings.TrimSpace(req.GetReason()) == "" || len(req.GetReason()) > 1024) {
		return nil, fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	f, err := s.arch.findFile(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case f.GetState() == v1.QuarantineState_QUARANTINE_STATE_RELEASED:
		return &v1.ReleaseQuarantinedFileResponse{File: f}, nil
	case f.GetState() == v1.QuarantineState_QUARANTINE_STATE_REJECTED || !f.GetContentKept():
		return nil, fault.New(fault.Precondition, "the file is rejected, or its content was never kept")
	}
	receiver := f.GetReceiver()
	if req.GetReceiver() != "" {
		if _, ok := s.arch.receivers[key(req.GetTenant(), req.GetReceiver())]; !ok {
			return nil, fault.New(fault.Precondition, "the receiver named does not exist")
		}
		receiver = req.GetReceiver()
	}
	f.State, f.ReleaseMode, f.ReleaseReceiver = v1.QuarantineState_QUARANTINE_STATE_RELEASED, req.GetMode(), receiver
	f.ReleaseReception, f.ReleaseReason, f.DecideTime, f.ContentKept = "rec-released", req.GetReason(), timestamppb.New(s.now()), true
	f.ReleaseStorage, f.ReleaseProfile = req.GetStorage(), req.GetProfile()
	return &v1.ReleaseQuarantinedFileResponse{File: f}, nil
}

// RejectQuarantinedFile removes a held file's bytes and keeps its record.
func (h quarantineHandler) RejectQuarantinedFile(_ context.Context, req *v1.RejectQuarantinedFileRequest) (*v1.RejectQuarantinedFileResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetReason()) == "" || len(req.GetReason()) > 1024 {
		return nil, fault.New(fault.Invalid, "reason: is 1 to 1024 bytes, not only spaces")
	}
	f, err := s.arch.findFile(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch f.GetState() {
	case v1.QuarantineState_QUARANTINE_STATE_REJECTED:
		return &v1.RejectQuarantinedFileResponse{File: f}, nil
	case v1.QuarantineState_QUARANTINE_STATE_RELEASED:
		return nil, fault.New(fault.Precondition, "the file is released")
	}
	f.State, f.RejectReason, f.DecideTime, f.ContentKept = v1.QuarantineState_QUARANTINE_STATE_REJECTED, req.GetReason(), timestamppb.New(s.now()), false
	return &v1.RejectQuarantinedFileResponse{File: f}, nil
}
