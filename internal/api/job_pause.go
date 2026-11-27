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

// PauseJob pauses a batch delete job: it stays in its state and its runner
// deletes no next target until it is resumed.
func (s jobService) PauseJob(ctx context.Context, req *v1.PauseJobRequest) (*v1.PauseJobResponse, error) {
	j, err := s.pause(ctx, nopacsv1connect.JobServicePauseJobProcedure, req.GetTenant(), req.GetId(), true, master.RoleOperator)
	if err != nil {
		return nil, err
	}
	return &v1.PauseJobResponse{Job: j}, nil
}

// ResumeJob lets a paused batch delete job go on.
func (s jobService) ResumeJob(ctx context.Context, req *v1.ResumeJobRequest) (*v1.ResumeJobResponse, error) {
	j, err := s.pause(ctx, nopacsv1connect.JobServiceResumeJobProcedure, req.GetTenant(), req.GetId(), false, master.RoleAdmin)
	if err != nil {
		return nil, err
	}
	return &v1.ResumeJobResponse{Job: j}, nil
}

// pause pauses (paused true) or resumes the job of id, checking in the
// change's transaction that the caller still holds role on the tenant.
// Only a batch delete job pauses: another kind is a precondition failure.
func (s jobService) pause(ctx context.Context, procedure, tenantName, id string, paused bool, role master.Role) (*v1.Job, error) {
	c := s.c
	d, err := c.changing(ctx, procedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, tenantName, nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", id); err != nil {
		return nil, c.failIn(d, st, err)
	}
	j, err := p.Job(ctx, id)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil, c.failIn(d, st, fault.Wrap(fault.NotFound, err, messageNoJob))
	case err != nil:
		return nil, c.failIn(d, st, err)
	case j.Kind != tenant.JobDeleteBatch:
		return nil, c.failIn(d, st, precondition("only a batch delete job pauses in this version; this job is of kind "+string(j.Kind)))
	}
	db, err := deleteBatches(st)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	bj, err := db.PauseDeleteBatch(ctx, id, paused, c.now(), changeEntry(d, access.Facts{}), c.roleGuard(d, role))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	return c.newJobViews(p, tenantName).message(ctx, bj.Job), nil
}
