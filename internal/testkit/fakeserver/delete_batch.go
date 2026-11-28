package fakeserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The fake's batch deletes: a preview counts one instance per target and
// its token is a digest of the targets and the reason, so a command's flow
// (preview, confirm, follow) is what is exercised, not the server's rules,
// which the API's own tests hold. A confirmed batch is a job of kind
// delete_batch, queued, whose targets are each pending.

// batchToken is the fake's token of a batch preview.
func batchToken(req *v1.DeleteBatchRequest) string {
	var b strings.Builder
	b.WriteString(req.GetReason())
	for _, t := range req.GetTargets() {
		b.WriteString("\x00" + t.GetSubjectLabel() + "/" + t.GetSessionLabel() + "/" + t.GetScanLabel())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "fake-batch." + hex.EncodeToString(sum[:8])
}

// DeleteBatch answers a batch's preview, or makes its job.
func (h correctionHandler) DeleteBatch(_ context.Context, req *v1.DeleteBatchRequest) (*v1.DeleteBatchResponse, error) {
	s := h.s
	err := s.beginCorrection(req.GetTenant(), req.GetReason(), true)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.refuseMasked(messageMaskedBulk); err != nil {
		return nil, err
	}
	if len(req.GetTargets()) == 0 {
		return nil, fault.New(fault.Invalid, "targets: name at least one")
	}
	p := &v1.DeleteBatchPreview{Counts: &v1.CorrectionCounts{}, PurgeTime: timestamppb.New(s.now().Add(7 * 24 * 3600e9))}
	for _, t := range req.GetTargets() {
		if t.GetSubjectLabel() == "" || t.GetInstanceId() != "" {
			return nil, fault.New(fault.Invalid, "targets: each names a subject, a session or a scan")
		}
		item := &v1.DeleteBatchItem{Target: proto.Clone(t).(*v1.CorrectionTarget), Counts: &v1.CorrectionCounts{Instances: 1, Bytes: 1000},
			Storages: []string{"main"}}
		if s.corr.batchItem != nil {
			s.corr.batchItem(item)
		}
		p.Items = append(p.Items, item)
		p.Counts.Instances++
		p.Counts.Bytes += 1000
	}
	token := batchToken(req)
	if req.GetValidateOnly() {
		p.Confirm = token
		return &v1.DeleteBatchResponse{Preview: p}, nil
	}
	if req.GetConfirm() != token {
		return nil, fault.New(fault.Precondition, "the confirm token does not match the targets or the reason")
	}
	for _, k := range forTenant(s.arch.jobs, req.GetTenant()) {
		if j := s.arch.jobs[k]; j.GetDeleteBatch() != nil && !jobEnded(j.GetState()) {
			return nil, fault.New(fault.Precondition, "the tenant has a batch delete job not ended; one runs at a time")
		}
	}
	j := &v1.Job{Id: s.newJobID(req.GetTenant()), Kind: &v1.Job_DeleteBatch{DeleteBatch: &v1.DeleteBatchJob{Reason: req.GetReason(),
		Targets: int64(len(req.GetTargets()))}}, State: v1.JobState_JOB_STATE_QUEUED,
		Counts:     &v1.JobCounts{Scans: int64(len(req.GetTargets())), Instances: p.GetCounts().GetInstances(), Bytes: p.GetCounts().GetBytes(), Listed: true},
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	if s.corr.batches == nil {
		s.corr.batches = map[string][]*v1.DeleteBatchTarget{}
	}
	var targets []*v1.DeleteBatchTarget
	for i, t := range req.GetTargets() {
		targets = append(targets, &v1.DeleteBatchTarget{Position: int64(i + 1), Target: proto.Clone(t).(*v1.CorrectionTarget),
			Outcome: v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_PENDING, Instances: 1, Bytes: 1000})
	}
	s.corr.batches[key(req.GetTenant(), j.GetId())] = targets
	return &v1.DeleteBatchResponse{Preview: &v1.DeleteBatchPreview{Counts: p.GetCounts()}, Job: proto.Clone(j).(*v1.Job)}, nil
}

// SetBatchItem gives fn each item of a batch's preview, to add what keeps the
// target from being deleted: its holds, its replicas, a retention minimum.
func (s *Server) SetBatchItem(fn func(*v1.DeleteBatchItem)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corr.batchItem = fn
}

// SetBatchOutcome records what became of one target of a batch job, as the
// server's runner would.
func (s *Server) SetBatchOutcome(tenant, job string, position int, outcome v1.DeleteBatchOutcome, correction string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.corr.batches[key(tenant, job)][position-1]
	t.Outcome, t.CorrectionId = outcome, correction
}

// ListDeleteBatchTargets answers a page of a batch job's targets.
func (h correctionHandler) ListDeleteBatchTargets(_ context.Context, req *v1.ListDeleteBatchTargetsRequest) (*v1.ListDeleteBatchTargetsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	all, ok := s.corr.batches[key(req.GetTenant(), req.GetJobId())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no batch delete job with this id")
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := make([]*v1.DeleteBatchTarget, 0, to-from)
	for _, t := range all[from:to] {
		out = append(out, proto.Clone(t).(*v1.DeleteBatchTarget))
	}
	return &v1.ListDeleteBatchTargetsResponse{Targets: out, NextPageToken: next}, nil
}

// pauseJob sets a batch job's paused flag; another kind does not pause.
func (s *Server) pauseJob(tenant, id string, paused bool) (*v1.Job, error) {
	err := s.begin(tenant)
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	j, err := s.job(tenant, id)
	if err != nil {
		return nil, err
	}
	switch {
	case j.GetDeleteBatch() == nil:
		return nil, fault.New(fault.Precondition, "only a batch delete job pauses")
	case jobEnded(j.GetState()):
		return nil, fault.New(fault.Precondition, "the job has ended")
	case !paused:
		if err := s.refuseMasked(messageMaskedBulk); err != nil {
			return nil, err
		}
	}
	j.Paused = paused
	return proto.Clone(j).(*v1.Job), nil
}

// PauseJob pauses a batch job.
func (h jobHandler) PauseJob(_ context.Context, req *v1.PauseJobRequest) (*v1.PauseJobResponse, error) {
	j, err := h.s.pauseJob(req.GetTenant(), req.GetId(), true)
	if err != nil {
		return nil, err
	}
	return &v1.PauseJobResponse{Job: j}, nil
}

// ResumeJob resumes a paused batch job.
func (h jobHandler) ResumeJob(_ context.Context, req *v1.ResumeJobRequest) (*v1.ResumeJobResponse, error) {
	j, err := h.s.pauseJob(req.GetTenant(), req.GetId(), false)
	if err != nil {
		return nil, err
	}
	return &v1.ResumeJobResponse{Job: j}, nil
}
