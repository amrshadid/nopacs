package fakeserver

import (
	"context"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/search"
)

// searchHandler is the fake's SearchService: only the backfill of an
// attribute, which makes a job, since a command's flow is what is
// exercised, not the search tables, which the API's own tests hold. The
// attribute named EchoTime is the one being filled; any other the search
// tables hold is ready.
type searchHandler struct {
	nopacsv1connect.UnimplementedSearchServiceHandler
	s *Server
}

// registerSearch adds the SearchService.
func (s *Server) registerSearch(srv *connect.Server) {
	nopacsv1connect.RegisterSearchServiceHandler(srv, searchHandler{s: s})
}

// BackfillAttribute makes a search backfill job of the attribute being
// filled, queued.
func (h searchHandler) BackfillAttribute(_ context.Context, req *v1.BackfillAttributeRequest) (*v1.BackfillAttributeResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	switch a := req.GetAttribute(); {
	case !search.Known(a):
		return nil, fault.New(fault.Invalid, "the tenant's search tables hold no attribute named \""+a+"\"")
	case a != "EchoTime":
		return nil, fault.New(fault.Precondition, a+" is filled for every scan already; there is nothing to backfill")
	}
	for _, k := range forTenant(s.arch.jobs, req.GetTenant()) {
		if j := s.arch.jobs[k]; j.GetSearchBackfill() != nil && !jobEnded(j.GetState()) {
			return nil, fault.New(fault.Precondition, "the tenant has a search backfill job not ended; one runs at a time")
		}
	}
	j := &v1.Job{Id: s.newJobID(req.GetTenant()), Kind: &v1.Job_SearchBackfill{SearchBackfill: &v1.SearchBackfillJob{Fields: []string{"EchoTime"}}},
		State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{Scans: 4, Instances: 4, Listed: true},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	return &v1.BackfillAttributeResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}
