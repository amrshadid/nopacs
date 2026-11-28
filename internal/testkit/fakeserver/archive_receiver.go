package fakeserver

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// receiverHandler implements ReceiverService.
type receiverHandler struct{ s *Server }

// health is the health the fake answers: down while disabled, else up, or
// idle for an import receiver, which holds no listener.
func health(r *v1.Receiver, now *timestamppb.Timestamp) *v1.ReceiverHealth {
	if !r.GetEnabled() {
		return &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_DOWN, DownReason: v1.ReceiverDownReason_RECEIVER_DOWN_REASON_DISABLED, Detail: "the receiver is disabled", Since: now}
	}
	if r.GetImporter() != nil {
		return &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_IDLE, Since: now}
	}
	return &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_UP, Since: now}
}

// answered returns a receiver as an answer carries it, with its health now.
func (s *Server) answered(tenant string, r *v1.Receiver) *v1.Receiver {
	out := proto.Clone(r).(*v1.Receiver)
	out.Health = health(out, out.GetUpdateTime())
	if h, ok := s.arch.health[key(tenant, r.GetName())]; ok {
		out.Health = h
	}
	return out
}

// AddReceiver adds a receiver, filling the defaults the contract names.
func (h receiverHandler) AddReceiver(_ context.Context, req *v1.AddReceiverRequest) (*v1.AddReceiverResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	in := req.GetReceiver()
	if err := itemOK("receiver.name", in.GetName()); err != nil {
		return nil, err
	}
	d := in.GetDimse()
	if im := in.GetImporter(); im != nil {
		if err := checkImporter(im); err != nil {
			return nil, err
		}
	} else if in.GetStow() == nil && (d == nil || d.GetPort() < 1 || d.GetPort() > 65535 || d.GetAeTitle() == "") {
		return nil, fault.New(fault.Invalid, "receiver.dimse: a port and an AE title are required")
	}
	if _, ok := s.arch.storages[key(req.GetTenant(), in.GetStorage())]; !ok {
		return nil, fault.New(fault.Precondition, "the tenant has no storage with that name")
	}
	if err := s.forwardable(req.GetTenant(), in.GetForwardTo()); err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), in.GetName())
	if _, ok := s.arch.receivers[k]; ok {
		return nil, fault.New(fault.Conflict, "the tenant has a receiver with that name")
	}
	r := proto.Clone(in).(*v1.Receiver)
	if r.GetQuietPeriod() == nil {
		r.QuietPeriod = durationpb.New(60_000_000_000)
	}
	if r.GetMaxOpenTime() == nil {
		r.MaxOpenTime = durationpb.New(3600_000_000_000)
	}
	if r.GetMaxFileBytes() == 0 {
		r.MaxFileBytes = 1 << 32
	}
	if st := r.GetStow(); st != nil && st.GetMaxRequestBytes() == 0 {
		st.MaxRequestBytes = 1 << 34
	}
	r.CreateTime, r.UpdateTime = timestamppb.New(s.now()), timestamppb.New(s.now())
	s.arch.receivers[k] = r
	return &v1.AddReceiverResponse{Receiver: s.answered(req.GetTenant(), r)}, nil
}

// GetReceiver answers one receiver with its health.
func (h receiverHandler) GetReceiver(_ context.Context, req *v1.GetReceiverRequest) (*v1.GetReceiverResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, ok := s.arch.receivers[key(req.GetTenant(), req.GetName())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no receiver with that name")
	}
	return &v1.GetReceiverResponse{Receiver: s.answered(req.GetTenant(), r)}, nil
}

// ListReceivers answers a page of a tenant's receivers by name.
func (h receiverHandler) ListReceivers(_ context.Context, req *v1.ListReceiversRequest) (*v1.ListReceiversResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	page, next, err := pageOfKeys(forTenant(s.arch.receivers, req.GetTenant()), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListReceiversResponse{NextPageToken: next}
	for _, k := range page {
		out.Receivers = append(out.Receivers, s.answered(req.GetTenant(), s.arch.receivers[k]))
	}
	return out, nil
}

// SetReceiver changes the fields the mask names, as the contract's paths.
func (h receiverHandler) SetReceiver(_ context.Context, req *v1.SetReceiverRequest) (*v1.SetReceiverResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, fault.New(fault.Invalid, "update_mask: is required and not empty")
	}
	k := key(req.GetTenant(), req.GetReceiver().GetName())
	cur, ok := s.arch.receivers[k]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no receiver with that name")
	}
	if t := req.GetExpectedUpdateTime(); t != nil && !t.AsTime().Equal(cur.GetUpdateTime().AsTime()) {
		return nil, fault.New(fault.Precondition, "the receiver changed after it was read; read it again and make the change on what it holds now")
	}
	in := req.GetReceiver()
	next := proto.Clone(cur).(*v1.Receiver)
	for _, p := range paths {
		switch p {
		case "enabled":
			next.Enabled = in.GetEnabled()
		case "storage":
			if _, ok := s.arch.storages[key(req.GetTenant(), in.GetStorage())]; !ok {
				return nil, fault.New(fault.Precondition, "the tenant has no storage with that name")
			}
			next.Storage = in.GetStorage()
		case "naming":
			next.Naming = in.GetNaming()
		case "forward_to":
			if err := s.forwardable(req.GetTenant(), in.GetForwardTo()); err != nil {
				return nil, err
			}
			next.ForwardTo = in.GetForwardTo()
		case "quiet_period":
			if in.GetQuietPeriod() == nil {
				return nil, fault.New(fault.Invalid, "quiet_period: is required when named")
			}
			next.QuietPeriod = in.GetQuietPeriod()
		case "max_open_time":
			if in.GetMaxOpenTime() == nil {
				return nil, fault.New(fault.Invalid, "max_open_time: is required when named")
			}
			next.MaxOpenTime = in.GetMaxOpenTime()
		case "max_file_bytes":
			next.MaxFileBytes = in.GetMaxFileBytes()
		case "deidentification":
			next.Deidentification = in.GetDeidentification()
		case "dimse.port":
			next.GetDimse().Port = in.GetDimse().GetPort()
		case "dimse.bind_address":
			next.GetDimse().BindAddress = in.GetDimse().GetBindAddress()
		case "dimse.ae_title":
			next.GetDimse().AeTitle = in.GetDimse().GetAeTitle()
		case "dimse.allowed_callers":
			next.GetDimse().Callers = &v1.DimseReceiver_AllowedCallers{AllowedCallers: in.GetDimse().GetAllowedCallers()}
		case "dimse.any_caller":
			next.GetDimse().Callers = &v1.DimseReceiver_AnyCaller{AnyCaller: in.GetDimse().GetAnyCaller()}
		case "dimse.tls":
			next.GetDimse().Tls = in.GetDimse().GetTls()
		case "dimse.sop_classes":
			next.GetDimse().SopClasses = in.GetDimse().GetSopClasses()
		case "dimse.transfer_syntaxes":
			next.GetDimse().TransferSyntaxes = in.GetDimse().GetTransferSyntaxes()
		case "stow.key_ids", "stow.max_request_bytes":
			if next.GetStow() == nil {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" names a field of another type")
			}
			if p == "stow.key_ids" {
				next.GetStow().KeyIds = in.GetStow().GetKeyIds()
			} else {
				next.GetStow().MaxRequestBytes = in.GetStow().GetMaxRequestBytes()
			}
		default:
			if !setImportField(next, in.GetImporter(), p) {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" names no field that can change")
			}
		}
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.arch.receivers[k] = next
	return &v1.SetReceiverResponse{Receiver: s.answered(req.GetTenant(), next)}, nil
}

// RemoveReceiver removes a receiver's configuration.
func (h receiverHandler) RemoveReceiver(_ context.Context, req *v1.RemoveReceiverRequest) (*v1.RemoveReceiverResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), req.GetName())
	r, ok := s.arch.receivers[k]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no receiver with that name")
	}
	delete(s.arch.receivers, k)
	out := s.answered(req.GetTenant(), r)
	out.Health = &v1.ReceiverHealth{State: v1.ReceiverState_RECEIVER_STATE_DOWN, DownReason: v1.ReceiverDownReason_RECEIVER_DOWN_REASON_DISABLED}
	return &v1.RemoveReceiverResponse{Receiver: out}, nil
}

// forwardable refuses, as a precondition, a list of senders to forward to that
// names one the tenant does not have.
func (s *Server) forwardable(tenant string, senders []string) error {
	for _, name := range senders {
		if _, ok := s.arch.senders[key(tenant, name)]; !ok {
			return fault.New(fault.Precondition, "forward_to: the tenant has no sender named "+name)
		}
	}
	return nil
}

// TryReceiverProfile is not served by the fake server: it keeps no files to
// try a version over.
func (receiverHandler) TryReceiverProfile(context.Context, *v1.TryReceiverProfileRequest) (*v1.TryReceiverProfileResponse, error) {
	return nil, errNotServed
}
