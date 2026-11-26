package fakeserver

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// monitorHandler is MonitorService over the fake's tables: a status made of
// what they hold, and a stream that hands out the events queued with
// QueueEvent, one per stream, and ends when there are none, so a test that
// runs each command to its end never waits on a stream.
type monitorHandler struct{ s *Server }

var _ nopacsv1connect.MonitorServiceHandler = monitorHandler{}

// QueueEvent queues an event of tenant for the next stream of it. A job_step
// event moves the job it names to its state and counts when it is sent.
func (s *Server) QueueEvent(tenant string, e *v1.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if s.arch.events == nil {
		s.arch.events = map[string][]*v1.Event{}
	}
	e.Tenant = tenant
	s.arch.events[tenant] = append(s.arch.events[tenant], e)
}

// SetStatusCounts sets the counts a tenant's status answers.
func (s *Server) SetStatusCounts(tenant string, c *v1.TenantCounts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if s.arch.counts == nil {
		s.arch.counts = map[string]*v1.TenantCounts{}
	}
	s.arch.counts[tenant] = c
}

// GetStatus answers one tenant's status from the fake's tables: its counts
// as set, its storages and its receivers with their health; every tenant's
// is not answered by the fake.
func (h monitorHandler) GetStatus(_ context.Context, req *v1.GetStatusRequest) (*v1.GetStatusResponse, error) {
	s := h.s
	name := req.GetScope().GetTenant()
	if name == "" {
		return nil, fault.New(fault.Invalid, "scope: the fake answers one tenant's status")
	}
	err := s.begin(name)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st := &v1.TenantStatus{Tenant: name, DatabaseState: v1.TenantState_TENANT_STATE_READY, EventCursor: "c0",
		Counts: s.arch.counts[name], Throughput: &v1.Throughput{}}
	if st.Counts == nil {
		st.Counts = &v1.TenantCounts{}
	}
	for _, k := range forTenant(s.arch.storages, name) {
		sto := s.arch.storages[k]
		ss := &v1.StorageStatus{Storage: sto.GetName(), CapacityBytes: sto.GetCapacityBytes(), Tier: sto.GetTier(), ReplicaOf: sto.GetReplicaOf(),
			SwitchGeneration: sto.GetSwitchGeneration()}
		if fn := s.repl.status[k]; fn != nil {
			fn(ss)
		}
		st.Storages = append(st.Storages, ss)
		if sto.GetReplicaOf() != "" {
			st.Replicas = append(st.Replicas, s.replicaStatusOf(name, sto))
		}
	}
	st.Retention = s.ret.status[name]
	if as := s.ret.statusAs[name]; as != "" {
		st.Tenant = as
	}
	for _, k := range forTenant(s.arch.receivers, name) {
		r := s.arch.receivers[k]
		st.Receivers = append(st.Receivers, &v1.ReceiverStatus{Receiver: r.GetName(), Health: s.arch.health[k], AnyCaller: r.GetDimse().GetAnyCaller()})
	}
	for _, k := range forTenant(s.arch.senders, name) {
		st.Senders = append(st.Senders, &v1.SenderStatus{Sender: s.arch.senders[k].GetName(), Health: s.senderHealthOf(name, s.arch.senders[k])})
	}
	return &v1.GetStatusResponse{Tenants: []*v1.TenantStatus{st}}, nil
}

// Watch sends a keep-alive, then the first event queued for the tenant, if
// any, and ends.
func (h monitorHandler) Watch(_ context.Context, req *v1.WatchRequest, stream nopacsv1connect.MonitorServiceWatchServerStream) error {
	s := h.s
	name := req.GetScope().GetTenant()
	if name == "" {
		return fault.New(fault.Invalid, "scope: the fake streams one tenant's events")
	}
	err := s.begin(name)
	var next *v1.Event
	if err == nil && len(s.arch.events[name]) > 0 {
		next, s.arch.events[name] = s.arch.events[name][0], s.arch.events[name][1:]
		// An event that moves a job is the job moving: the job a later call
		// reads is as the event left it.
		if js := next.GetJobStep(); js != nil {
			if j := s.arch.jobs[key(name, js.GetJob())]; j != nil {
				j.State, j.Counts, j.Reason = js.GetState(), js.GetCounts(), js.GetReason()
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := stream.Send(&v1.WatchResponse{Kind: &v1.WatchResponse_KeepAlive{KeepAlive: &v1.WatchKeepAlive{Cursor: req.GetCursor()}}}); err != nil {
		return err
	}
	if next != nil {
		return stream.Send(&v1.WatchResponse{Kind: &v1.WatchResponse_Event{Event: next}})
	}
	return nil
}
