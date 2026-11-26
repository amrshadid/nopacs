package fakeserver

import (
	"context"
	"fmt"
	"sort"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// replicas is what the fake keeps for ReplicaService: how far each replica
// is, the corrections that count one, and what stands in the way of a
// switch. Only the flows are exercised (attach, detach, repair, a switch's
// preview and confirmation), not the server's rules, which the API's own
// tests hold.
type replicas struct {
	lag      map[string]*v1.ReplicaStatus
	counted  map[string]int
	blockers map[string][]string
	status   map[string]func(*v1.StorageStatus)
	// BackfillServed makes BackfillReplica answer; the server does not serve it yet.
	backfillServed bool
	// used is the confirm tokens a switch already took, so a token works once.
	used map[string]bool
	// drain is how long the copy of a primary takes to drain, by tenant and
	// primary; a switch whose drain timeout is shorter is refused.
	drain map[string]time.Duration
}

// replicaHandler is the fake's ReplicaService; the methods it does not
// implement answer UNIMPLEMENTED, as a server that does not serve them yet.
type replicaHandler struct {
	nopacsv1connect.UnimplementedReplicaServiceHandler
	s *Server
}

var _ nopacsv1connect.ReplicaServiceHandler = replicaHandler{}

// registerReplicas adds the fake's ReplicaService.
func (s *Server) registerReplicas(srv *connect.Server) {
	nopacsv1connect.RegisterReplicaServiceHandler(srv, replicaHandler{s: s})
}

// SetReplicaLag sets how far a replica is from its primary, as ListReplicas
// and the tenant's status answer it.
func (s *Server) SetReplicaLag(tenant string, r *v1.ReplicaStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repl.lag == nil {
		s.repl.lag = map[string]*v1.ReplicaStatus{}
	}
	s.repl.lag[key(tenant, r.GetReplica())] = proto.Clone(r).(*v1.ReplicaStatus)
}

// CountReplicaInCorrections makes n committed corrections count a replica
// among their copies, so a detach is refused without abandon_purges.
func (s *Server) CountReplicaInCorrections(tenant, replica string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repl.counted == nil {
		s.repl.counted = map[string]int{}
	}
	s.repl.counted[key(tenant, replica)] = n
}

// SetSwitchBlockers makes a switch's preview of a primary name what stands
// in its way.
func (s *Server) SetSwitchBlockers(tenant, primary string, blockers ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repl.blockers == nil {
		s.repl.blockers = map[string][]string{}
	}
	s.repl.blockers[key(tenant, primary)] = blockers
}

// SetSwitchDrain makes the copy of a primary take d to drain: a switch whose
// drain_timeout (ten minutes when absent) is shorter is refused as unavailable
// with nothing changed, as the server refuses when the writes in flight do not
// end in time.
func (s *Server) SetSwitchDrain(tenant, primary string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repl.drain == nil {
		s.repl.drain = map[string]time.Duration{}
	}
	s.repl.drain[key(tenant, primary)] = d
}

// SetStorageStatus makes the tenant's status change a storage's entry by fn
// before it is answered, for the fields only the server's own state has
// (encryption, coverage, a switch in progress).
func (s *Server) SetStorageStatus(tenant, storage string, fn func(*v1.StorageStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repl.status == nil {
		s.repl.status = map[string]func(*v1.StorageStatus){}
	}
	s.repl.status[key(tenant, storage)] = fn
}

// ServeBackfill makes BackfillReplica answer, as a server that serves it.
func (s *Server) ServeBackfill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repl.backfillServed = true
}

// FailWithReason is FailWith for an answer that also carries the contract's
// reason (STORAGE_SWITCHING, NOT_ONLINE, HELD, RETENTION_MINIMUM, ...) and a
// retry delay as error details, as the real server writes them.
func (s *Server) FailWithReason(procedure string, kind fault.Kind, message, reason string, retry time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[procedure] = codes.ToConnect(codes.WithReason(fault.New(kind, message), reason, nil, retry))
}

// replicaStatusOf is the answer for one replica: the lag a test set, or an
// empty one.
func (s *Server) replicaStatusOf(tenant string, st *v1.Storage) *v1.ReplicaStatus {
	if r, ok := s.repl.lag[key(tenant, st.GetName())]; ok {
		out := proto.Clone(r).(*v1.ReplicaStatus)
		out.Replica, out.Primary, out.Tier = st.GetName(), st.GetReplicaOf(), st.GetTier()
		return out
	}
	return &v1.ReplicaStatus{Replica: st.GetName(), Primary: st.GetReplicaOf(), Tier: st.GetTier()}
}

// ListReplicas answers the storages that have a primary, by primary then name.
func (h replicaHandler) ListReplicas(_ context.Context, req *v1.ListReplicasRequest) (*v1.ListReplicasResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*v1.ReplicaStatus
	for _, k := range forTenant(s.arch.storages, req.GetTenant()) {
		st := s.arch.storages[k]
		if st.GetReplicaOf() != "" && (req.GetPrimary() == "" || req.GetPrimary() == st.GetReplicaOf()) {
			all = append(all, s.replicaStatusOf(req.GetTenant(), st))
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetPrimary() < all[j].GetPrimary() })
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListReplicasResponse{Replicas: all[from:to], NextPageToken: next}, nil
}

// newReplicateJob queues a replicate job of a replica.
func (s *Server) newReplicateJob(tenant, replica, primary string) *v1.Job {
	j := &v1.Job{Id: s.newJobID(tenant), State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{},
		Kind:       &v1.Job_Replicate{Replicate: &v1.ReplicateJob{Replica: replica, Primary: primary, Mode: v1.ReplicateJobMode_REPLICATE_JOB_MODE_BACKFILL}},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(tenant, j.GetId())] = j
	return proto.Clone(j).(*v1.Job)
}

// AttachReplica makes a storage a replica of a primary and queues its copy.
func (h replicaHandler) AttachReplica(_ context.Context, req *v1.AttachReplicaRequest) (*v1.AttachReplicaResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetReplica())
	if err != nil {
		return nil, err
	}
	primary, err := s.arch.findStorage(req.GetTenant(), req.GetPrimary())
	if err != nil {
		return nil, fault.New(fault.Precondition, "the tenant has no primary with that name")
	}
	switch {
	case req.GetReplica() == req.GetPrimary():
		return nil, fault.New(fault.Invalid, "replica: a storage is not a replica of itself")
	case primary.GetReplicaOf() != "":
		return nil, fault.New(fault.Precondition, "the primary is itself a replica")
	case primary.GetTier() == v1.StorageTier_STORAGE_TIER_ARCHIVE:
		return nil, fault.New(fault.Precondition, "the primary is of the archive tier")
	case st.GetReplicaOf() != "" && st.GetReplicaOf() != req.GetPrimary():
		return nil, fault.New(fault.Precondition, "the storage is a replica of another primary")
	case s.arch.holdsFiles[key(req.GetTenant(), req.GetReplica())]:
		return nil, fault.New(fault.Precondition, "the storage holds archived files")
	}
	if st.GetReplicaOf() == req.GetPrimary() {
		return &v1.AttachReplicaResponse{Storage: withoutSecret(st, false)}, nil
	}
	st.ReplicaOf = req.GetPrimary()
	_, held := s.arch.secrets[key(req.GetTenant(), req.GetReplica())]
	return &v1.AttachReplicaResponse{Storage: withoutSecret(st, held), Backfill: s.newReplicateJob(req.GetTenant(), req.GetReplica(), req.GetPrimary())}, nil
}

// DetachReplica makes a replica an ordinary storage again.
func (h replicaHandler) DetachReplica(_ context.Context, req *v1.DetachReplicaRequest) (*v1.DetachReplicaResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetReplica())
	if err != nil {
		return nil, err
	}
	if req.GetAbandonPurges() != (req.GetAbandonReason() != "") {
		return nil, fault.New(fault.Invalid, "abandon_purges and abandon_reason go together")
	}
	if st.GetReplicaOf() == "" {
		return nil, fault.New(fault.Precondition, "the storage is not a replica")
	}
	if n := s.repl.counted[key(req.GetTenant(), req.GetReplica())]; n > 0 && !req.GetAbandonPurges() {
		return nil, fault.New(fault.Precondition, fmt.Sprintf("%d corrections count this replica among their copies; abandon_purges goes ahead", n))
	}
	st.ReplicaOf = ""
	_, held := s.arch.secrets[key(req.GetTenant(), req.GetReplica())]
	return &v1.DetachReplicaResponse{Storage: withoutSecret(st, held)}, nil
}

// BackfillReplica queues a copy job when the fake serves it, and answers
// UNIMPLEMENTED as the real server does until it does.
func (h replicaHandler) BackfillReplica(_ context.Context, req *v1.BackfillReplicaRequest) (*v1.BackfillReplicaResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !s.repl.backfillServed {
		return nil, errNotServed
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetReplica())
	if err != nil {
		return nil, err
	}
	if st.GetReplicaOf() == "" {
		return nil, fault.New(fault.Precondition, "the storage is not a replica")
	}
	return &v1.BackfillReplicaResponse{Job: s.newReplicateJob(req.GetTenant(), req.GetReplica(), st.GetReplicaOf())}, nil
}

// Repair queues a repair job of a primary's findings.
func (h replicaHandler) Repair(_ context.Context, req *v1.RepairRequest) (*v1.RepairResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetStorage())
	if err != nil {
		return nil, err
	}
	if (len(req.GetFindings()) > 0) == req.GetAll() {
		return nil, fault.New(fault.Invalid, "name findings, or all, and not both")
	}
	if st.GetReplicaOf() != "" {
		return nil, fault.New(fault.Precondition, "the storage is a replica, whose objects the server copies again by itself")
	}
	for _, id := range req.GetFindings() {
		found := false
		for _, f := range s.corr.findings[req.GetTenant()] {
			found = found || (f.GetId() == id && f.GetStorage() == req.GetStorage())
		}
		if !found {
			return nil, fault.New(fault.NotFound, "the tenant has no finding on this storage with the id "+id)
		}
	}
	j := &v1.Job{Id: s.newJobID(req.GetTenant()), State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{},
		Kind: &v1.Job_Repair{Repair: &v1.RepairJob{Storage: req.GetStorage(), From: req.GetFrom(),
			Findings: append([]string(nil), req.GetFindings()...), All: req.GetAll()}},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	return &v1.RepairResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// switchToken is the fake's confirm token of a switch: the two names and the
// primary's generation, as the real token's digest holds both places and it.
func switchToken(primary, replica string, generation int64) string {
	return fmt.Sprintf("sw.%s.%s.%d", primary, replica, generation)
}

// SwitchStorage answers a preview, or swaps the two storages' places (each
// name keeps its role, and both generations rise) when the token is the one a
// preview gave and nothing stands in the way.
func (h replicaHandler) SwitchStorage(_ context.Context, req *v1.SwitchStorageRequest) (*v1.SwitchStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	primary, err := s.arch.findStorage(req.GetTenant(), req.GetPrimary())
	if err != nil {
		return nil, err
	}
	replica, err := s.arch.findStorage(req.GetTenant(), req.GetReplica())
	if err != nil {
		return nil, err
	}
	if req.GetSkipVerification() != (req.GetSkipReason() != "") {
		return nil, fault.New(fault.Invalid, "skip_verification and skip_reason go together")
	}
	if replica.GetReplicaOf() != primary.GetName() {
		return nil, fault.New(fault.Precondition, "the replica is not a replica of the primary")
	}
	blockers := s.repl.blockers[key(req.GetTenant(), req.GetPrimary())]
	token := switchToken(req.GetPrimary(), req.GetReplica(), primary.GetSwitchGeneration())
	if req.GetValidateOnly() {
		if req.GetConfirm() != "" {
			return nil, fault.New(fault.Invalid, "confirm: must be empty with validate_only")
		}
		return &v1.SwitchStorageResponse{Ready: len(blockers) == 0, Blockers: blockers, ScansPending: int64(len(blockers)) * 3,
			Receivers: []string{"scanner1"}, Confirm: token, SwitchGeneration: primary.GetSwitchGeneration()}, nil
	}
	if req.GetConfirm() == "" {
		return nil, fault.New(fault.Invalid, "confirm: is required unless validate_only")
	}
	if req.GetConfirm() != token || s.repl.used[req.GetConfirm()] {
		return nil, fault.New(fault.Precondition, "the confirm token does not match the places and generation now, or was used")
	}
	if len(blockers) > 0 && !req.GetSkipVerification() {
		return nil, fault.New(fault.Precondition, "the switch is refused: "+blockers[0])
	}
	drainBound := 10 * time.Minute
	if req.GetDrainTimeout() != nil {
		drainBound = req.GetDrainTimeout().AsDuration()
		if drainBound < time.Second || drainBound > time.Hour {
			return nil, fault.New(fault.Invalid, "drain_timeout: is 1 second to 1 hour")
		}
	}
	if s.repl.drain[key(req.GetTenant(), req.GetPrimary())] > drainBound {
		return nil, fault.New(fault.Unavailable, "the writes in flight did not end within the drain timeout")
	}
	if s.repl.used == nil {
		s.repl.used = map[string]bool{}
	}
	s.repl.used[token] = true
	primary.SwitchGeneration++
	replica.SwitchGeneration++
	return &v1.SwitchStorageResponse{Ready: true, ScansPending: 0, Receivers: []string{"scanner1"},
		Primary: withoutSecret(primary, false), Replica: withoutSecret(replica, false), Pause: durationpb.New(1500 * time.Millisecond),
		AssociationsRefused: 4, SwitchTime: timestamppb.New(s.now()), SwitchGeneration: primary.GetSwitchGeneration()}, nil
}

// FailWithDetailOnly is FailWithReason for a server whose message does not
// start with the reason: the message is exactly the one given, and the reason
// and the delay travel only in the error's details, as a client that reads
// details must be able to show them.
func (s *Server) FailWithDetailOnly(procedure string, kind fault.Kind, message, reason string, retry time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	marked := codes.ToConnect(codes.WithReason(fault.New(kind, message), reason, nil, retry))
	plain := connect.NewError(marked.Code(), message)
	for _, d := range marked.Details() {
		plain = plain.WithDetail(d)
	}
	s.fail[procedure] = plain
}
