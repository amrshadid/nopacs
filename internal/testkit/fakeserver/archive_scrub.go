package fakeserver

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// createScrubJob makes a scrub job of a storage the tenant has, or of every
// storage, queued with no counts yet. Like the server, it refuses a scrub of
// a storage another scrub of which is queued or running. The caller holds
// the lock.
func (s *Server) createScrubJob(tenant string, in *v1.ScrubJob) (*v1.CreateJobResponse, error) {
	if in.GetScheduled() {
		return nil, fault.New(fault.Invalid, "scrub.scheduled: is the server's to set")
	}
	if in.GetStorage() != "" {
		if _, ok := s.arch.storages[key(tenant, in.GetStorage())]; !ok {
			return nil, fault.New(fault.Precondition, "scrub.storage: the tenant has no storage of that name")
		}
	}
	for _, j := range s.arch.jobs {
		if sj := j.GetScrub(); sj != nil && !jobEnded(j.GetState()) && sj.GetStorage() == in.GetStorage() {
			return nil, fault.New(fault.Precondition, "another scrub of it is queued or running")
		}
	}
	j := &v1.Job{Id: s.newJobID(tenant), Kind: &v1.Job_Scrub{Scrub: proto.Clone(in).(*v1.ScrubJob)}, State: v1.JobState_JOB_STATE_QUEUED,
		Counts: &v1.JobCounts{}, CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(tenant, j.GetId())] = j
	return &v1.CreateJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// createRebuildJob makes a rebuild job, queued with no counts yet; it holds
// the server's rules of the request: a mode, a from_storage only for a
// compare and of a storage the tenant has, and one rebuild at a time. The
// caller holds the lock.
func (s *Server) createRebuildJob(tenant string, in *v1.RebuildJob) (*v1.CreateJobResponse, error) {
	switch in.GetMode() {
	case v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE:
	case v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE:
		if in.GetFromStorage() != "" {
			return nil, fault.New(fault.Invalid, "rebuild.from_storage: is for a compare")
		}
	default:
		return nil, fault.New(fault.Invalid, "rebuild.mode: is required")
	}
	if in.GetFromStorage() != "" {
		if _, ok := s.arch.storages[key(tenant, in.GetFromStorage())]; !ok {
			return nil, fault.New(fault.Precondition, "rebuild.from_storage: the tenant has no storage of that name")
		}
	}
	for _, j := range s.arch.jobs {
		if j.GetRebuild() != nil && !jobEnded(j.GetState()) {
			return nil, fault.New(fault.Precondition, "another rebuild is queued or running")
		}
	}
	j := &v1.Job{Id: s.newJobID(tenant), Kind: &v1.Job_Rebuild{Rebuild: proto.Clone(in).(*v1.RebuildJob)}, State: v1.JobState_JOB_STATE_QUEUED,
		Counts: &v1.JobCounts{}, CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(tenant, j.GetId())] = j
	return &v1.CreateJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// AddScrubFinding gives tenant a scrub finding, as a scrub would have
// recorded it.
func (s *Server) AddScrubFinding(tenant string, f *v1.ScrubFinding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.corr.findings == nil {
		s.corr.findings = map[string][]*v1.ScrubFinding{}
	}
	s.corr.findings[tenant] = append(s.corr.findings[tenant], proto.Clone(f).(*v1.ScrubFinding))
}

// ListScrubFindings answers a page of the findings AddScrubFinding gave, by
// the kinds and the storage asked for, those cleared only with
// include_cleared.
func (h correctionHandler) ListScrubFindings(_ context.Context, req *v1.ListScrubFindingsRequest) (*v1.ListScrubFindingsResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), "", false)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	all := []*v1.ScrubFinding{}
	for _, f := range s.corr.findings[req.GetTenant()] {
		kept := len(req.GetKinds()) == 0
		for _, k := range req.GetKinds() {
			kept = kept || k == f.GetKind()
		}
		if kept && (req.GetStorage() == "" || req.GetStorage() == f.GetStorage()) && (req.GetIncludeCleared() || f.GetClearTime() == nil) {
			all = append(all, f)
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListScrubFindingsResponse{Findings: all[from:to], NextPageToken: next}, nil
}
