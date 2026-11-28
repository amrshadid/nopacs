package fakeserver

import (
	"context"
	"sort"
	"strconv"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// coverage is what the fake keeps of a tenant's coverage report: the report
// now, and the one a refresh makes of it.
type coverage struct {
	report   *v1.CoverageReport
	elements []*v1.CoverageElement
	next     *v1.CoverageReport
	nextEls  []*v1.CoverageElement
}

// SetCoverageRefresh sets the report the next RefreshCoverage of the tenant
// makes, with its elements: the refresh ends done at once, as a job of a
// small archive would, and the report is then answered by GetCoverage.
func (s *Server) SetCoverageRefresh(tenant string, report *v1.CoverageReport, elements []*v1.CoverageElement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deid.coverage == nil {
		s.deid.coverage = map[string]*coverage{}
	}
	c := s.deid.coverage[tenant]
	if c == nil {
		c = &coverage{}
		s.deid.coverage[tenant] = c
	}
	c.next, c.nextEls = report, elements
}

// GetCoverage answers a page of the tenant's report, as the elements are
// held, 100 to a page when the request names none.
func (h deidHandler) GetCoverage(_ context.Context, req *v1.GetCoverageRequest) (*v1.GetCoverageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &v1.GetCoverageResponse{}
	for _, k := range forTenant(s.arch.jobs, req.GetTenant()) {
		if j := s.arch.jobs[k]; j.GetDeidCoverage() != nil && !jobEnded(j.GetState()) {
			out.Refreshing = proto.Clone(j).(*v1.Job)
		}
	}
	c := s.deid.coverage[req.GetTenant()]
	if c == nil || c.report == nil {
		return out, nil
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = 100
	}
	start := 0
	if t := req.GetPageToken(); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > len(c.elements) {
			return nil, fault.New(fault.Invalid, "page_token: is not one this list returned for these arguments")
		}
		start = n
	}
	end := min(start+size, len(c.elements))
	out.Report = proto.Clone(c.report).(*v1.CoverageReport)
	for _, e := range c.elements[start:end] {
		out.Elements = append(out.Elements, proto.Clone(e).(*v1.CoverageElement))
	}
	if end < len(c.elements) {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}

// RefreshCoverage makes a coverage job, done at once, and the report the
// test set for it.
func (h deidHandler) RefreshCoverage(_ context.Context, req *v1.RefreshCoverageRequest) (*v1.RefreshCoverageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, k := range forTenant(s.arch.jobs, req.GetTenant()) {
		if j := s.arch.jobs[k]; j.GetDeidCoverage() != nil && !jobEnded(j.GetState()) {
			return nil, fault.New(fault.Precondition, "the tenant has a coverage job not ended; one runs at a time")
		}
	}
	id := s.newJobID(req.GetTenant())
	now := timestamppb.New(s.now())
	j := &v1.Job{Id: id, Kind: &v1.Job_DeidCoverage{DeidCoverage: &v1.DeidCoverageJob{}}, State: v1.JobState_JOB_STATE_DONE,
		Counts: &v1.JobCounts{Scans: 4, ScansDone: 4, Instances: 4, InstancesSent: 4, Listed: true}, CreateTime: now, StateTime: now, EndTime: now}
	s.arch.jobs[key(req.GetTenant(), id)] = j
	if s.deid.coverage == nil {
		s.deid.coverage = map[string]*coverage{}
	}
	c := s.deid.coverage[req.GetTenant()]
	if c == nil {
		c = &coverage{}
		s.deid.coverage[req.GetTenant()] = c
	}
	if c.next != nil {
		c.report, c.elements = proto.Clone(c.next).(*v1.CoverageReport), c.nextEls
		c.report.JobId, c.report.EndTime = id, now
		sort.SliceStable(c.elements, func(i, k int) bool { return c.elements[i].GetTag() < c.elements[k].GetTag() })
	}
	return &v1.RefreshCoverageResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}
