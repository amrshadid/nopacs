package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoJob answers a job id the tenant does not have.
const messageNoJob = "the tenant has no job with this id"

// jobService is JobService: a tenant's jobs, which the store keeps and the
// server's sending carries out; a person makes one, reads it, cancels it
// and retries what it did not send.
type jobService struct{ c *core }

var _ nopacsv1connect.JobServiceHandler = jobService{}

// lookupJob reads a job, answering one the tenant does not have as NotFound
// with the contract's message.
func lookupJob(ctx context.Context, p tenant.Pipeline, id string) (tenant.Job, error) {
	j, err := p.Job(ctx, id)
	if fault.KindOf(err) == fault.NotFound {
		return j, fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	return j, err
}

// jobViews turns jobs into the contract's messages, adding to a job in
// progress why it is not moving when its sender is disabled or down: the
// store does not know the server's view of a sender, so the phrase is
// added here, read once for each sender the jobs name. A sender that is
// gone, or cannot be read, adds nothing.
type jobViews struct {
	c          *core
	p          tenant.Pipeline
	tenantName string
	held       map[string]string
	// quota is the phrase of an import held back by the tenant's storage
	// quota, read once (quotaRead) for the jobs of one answer.
	quota     string
	quotaRead bool
}

// newJobViews returns the views of the jobs of the tenant named tenantName,
// whose pipeline is p.
func (c *core) newJobViews(p tenant.Pipeline, tenantName string) *jobViews {
	return &jobViews{c: c, p: p, tenantName: tenantName, held: map[string]string{}}
}

// message returns j as the contract writes it.
func (v *jobViews) message(ctx context.Context, j tenant.Job) *v1.Job {
	if j.Kind == tenant.JobDispose {
		m := jobMessage(j, "")
		if dp, ok := v.p.(tenant.Disposal); ok {
			if dj, err := dp.DisposeJob(ctx, j.ID); err == nil {
				m.Kind = &v1.Job_Dispose{Dispose: disposeJobMessage(dj)}
			}
		}
		return m
	}
	if j.Kind == tenant.JobDeleteBatch {
		m := jobMessage(j, "")
		if db, ok := v.p.(tenant.DeleteBatches); ok {
			if bj, err := db.DeleteBatchJob(ctx, j.ID); err == nil {
				m.Kind = &v1.Job_DeleteBatch{DeleteBatch: &v1.DeleteBatchJob{Reason: bj.Reason, Targets: bj.Targets}}
				m.Paused = bj.Paused && !j.State.Ended()
			}
		}
		return m
	}
	if j.Kind == tenant.JobSearchBackfill {
		m := jobMessage(j, "")
		if sb, ok := v.p.(tenant.SearchBackfills); ok {
			if bj, err := sb.SearchBackfillJob(ctx, j.ID); err == nil {
				m.Kind = &v1.Job_SearchBackfill{SearchBackfill: &v1.SearchBackfillJob{Fields: bj.Fields, ScansSkipped: bj.Skipped}}
			}
		}
		return m
	}
	if j.Kind == tenant.JobReplicate {
		return jobMessage(j, replicaHeld(ctx, v.p, j))
	}
	if j.Kind == tenant.JobImport && !j.State.Ended() && j.Reason == "" {
		// An import waits while its tenant is at its storage quota: its
		// files are refused for now and its units look again later. Read
		// once for the jobs of one answer.
		if !v.quotaRead {
			v.quota, v.quotaRead = v.c.quotaHeld(ctx, v.p), true
		}
		return jobMessage(j, v.quota)
	}
	if j.State.Ended() || j.Reason != "" || j.Sender == "" {
		return jobMessage(j, "")
	}
	phrase, seen := v.held[j.Sender]
	if !seen {
		if s, err := v.p.Sender(ctx, j.Sender); err == nil {
			switch {
			case !s.Enabled:
				phrase = reasonSenderDisabled
			case v.c.senders.SenderHealth(v.tenantName, s).State == tenant.SenderDown:
				phrase = reasonSenderDown
			}
		}
		v.held[j.Sender] = phrase
	}
	return jobMessage(j, phrase)
}

// CreateJob makes a send job of a subject, a session or a scan, by labels,
// to a sender the tenant has; or a deid job of a subject, a session, a scan
// or the whole tenant. The server makes its id.
func (s jobService) CreateJob(ctx context.Context, req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.JobServiceCreateJobProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), createJobDetails(req))
	if err != nil {
		return nil, err
	}
	if ij := req.GetImportJob(); ij != nil && ij.GetRetentionStart() != nil {
		return nil, c.refuseNotServed(d, st, "import_job.retention_start: retention is not served yet")
	}
	if rj := req.GetRebuild(); rj != nil && rj.GetFromStorage() != "" {
		return nil, c.refuseNotServed(d, st, "rebuild.from_storage: a rebuild from one storage is not served yet")
	}
	if req.GetImportJob() != nil {
		return s.createImportJob(ctx, d, p, st, req)
	}
	if req.GetRebuild() != nil {
		return s.createRebuildJob(ctx, d, p, st, req)
	}
	if req.GetDeid() != nil {
		return s.createDeidJob(ctx, d, p, st, req)
	}
	if req.GetScrub() != nil {
		return s.createScrubJob(ctx, d, p, st, req)
	}
	send := req.GetSend()
	if send == nil {
		return nil, c.failIn(d, st, invalid("send", "is required: the kind of the job, send or deid"))
	}
	if send.GetReception() != "" || send.GetReceiver() != "" {
		return nil, c.failIn(d, st, invalid("send.reception", "and send.receiver are set by the server for a forward, and are empty here"))
	}
	if err := senderName("send.sender", send.GetSender()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	nj := tenant.NewJob{
		ID: id, Sender: send.GetSender(), Created: c.now(),
		Target: tenant.JobTarget{SubjectLabel: send.GetSubjectLabel(), SessionLabel: send.GetSessionLabel(), ScanLabel: send.GetScanLabel()},
	}
	if err := nj.Target.Validate(); err != nil {
		return nil, c.failIn(d, st, fieldOf("send", err))
	}
	if err := nj.Validate(); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, access.Facts{Object: id}), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}

// GetJob answers one job, with its counts now.
func (s jobService) GetJob(ctx context.Context, req *v1.GetJobRequest) (*v1.GetJobResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.JobServiceGetJobProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := lookupJob(ctx, p, req.GetId())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	var tallies []tenant.JobTally
	if out.Kind == tenant.JobDeid || out.Kind == tenant.JobScrub || out.Kind == tenant.JobRebuild || out.Kind == tenant.JobRepair ||
		out.Kind == tenant.JobDeleteBatch {
		if tallies, err = p.JobTallies(ctx, out.ID); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	m := hideImportPaths(d, c.newJobViews(p, req.GetTenant()).message(ctx, out))
	if err := s.withImportMap(ctx, d, p, out, m); err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	for _, t := range tallies {
		m.Tallies = append(m.Tallies, &v1.JobTally{Key: t.Key, Count: t.Count})
	}
	return &v1.GetJobResponse{Job: m}, nil
}

// jobFilter returns the filter a listing asks for, checked.
func jobFilter(r *v1.ListJobsRequest) (tenant.JobFilter, error) {
	f := tenant.JobFilter{Sender: r.GetSender(), InProgress: r.GetInProgress()}
	if f.Sender != "" {
		if err := senderName("sender", f.Sender); err != nil {
			return f, err
		}
	}
	if len(r.GetStates()) > maxStateFilter {
		return f, invalid("states", "names at most 16 states")
	}
	for _, s := range r.GetStates() {
		js, ok := jobStateOf(s)
		if !ok {
			return f, invalid("states", "names only states the contract defines, never JOB_STATE_UNSPECIFIED")
		}
		f.States = append(f.States, js)
	}
	var err error
	if f.Since, err = filterTime("start_time", r.GetStartTime()); err != nil {
		return f, err
	}
	if f.Until, err = filterTime("end_time", r.GetEndTime()); err != nil {
		return f, err
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return f, invalid("end_time", "is after start_time")
	}
	return f, f.Validate()
}

// ListJobs answers a page of a tenant's jobs, newest first, filtered by
// state, sender and time.
func (s jobService) ListJobs(ctx context.Context, req *v1.ListJobsRequest) (*v1.ListJobsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.JobServiceListJobsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), jobFilterDetails(req))
	if err != nil {
		return nil, err
	}
	filter, err := jobFilter(req)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListJobs(ctx, filter, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	views := c.newJobViews(p, req.GetTenant())
	out := &v1.ListJobsResponse{Jobs: make([]*v1.Job, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Jobs = append(out.Jobs, hideImportPaths(d, views.message(ctx, it)))
	}
	return out, nil
}

// CancelJob ends a queued or running job as canceled; a job canceled
// already is answered as it stands.
func (s jobService) CancelJob(ctx context.Context, req *v1.CancelJobRequest) (*v1.CancelJobResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.JobServiceCancelJobProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := p.CancelJob(ctx, req.GetId(), c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleOperator))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	if out.Kind == tenant.JobRebuild {
		// A replace canceled no longer holds the tenant's receivers: they
		// are told at once, not at the supervisor's next round.
		c.receiversChanged(req.GetTenant())
	}
	return &v1.CancelJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}

// RetryJob makes a new job of what an ended job did not send; a job
// retried already is answered as it stands, with the job made then.
func (s jobService) RetryJob(ctx context.Context, req *v1.RetryJobRequest) (*v1.RetryJobResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.JobServiceRetryJobProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	// The retry of an import job is in the contract and not served yet: it
	// is answered as a part not served, never as a refusal of this job.
	if j, err := p.Job(ctx, req.GetId()); err == nil && j.Kind == tenant.JobImport {
		return nil, c.refuseNotServed(d, st, "a retry of an import job is not served yet; a new import job over the same path counts what is held as already held")
	}
	newID := c.newID()
	if master.ValidateID("job id", newID) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	ended, retry, err := p.RetryJob(ctx, req.GetId(), newID, c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, master.RoleOperator))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	views := c.newJobViews(p, req.GetTenant())
	return &v1.RetryJobResponse{Job: views.message(ctx, ended), Retry: views.message(ctx, retry)}, nil
}

// ListJobFailures answers a page of the instances a job refused and, with
// include_warnings, those sent with a warning.
func (s jobService) ListJobFailures(ctx context.Context, req *v1.ListJobFailuresRequest) (*v1.ListJobFailuresResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.JobServiceListJobFailuresProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListJobFailures(ctx, req.GetId(), req.GetIncludeWarnings(), page)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListJobFailuresResponse{Failures: make([]*v1.JobFailure, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Failures = append(out.Failures, jobFailureMessage(it))
	}
	return out, nil
}
