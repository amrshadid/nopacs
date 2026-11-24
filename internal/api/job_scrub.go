package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// createScrubJob makes a scrub job (job.proto, ScrubJob) of one storage of
// the tenant, or of every one. Only the server makes a scheduled one. Its
// role, audit and errors are CreateJob's; the audit entry names its
// storage.
func (s jobService) createScrubJob(ctx context.Context, d access.Decision, p tenant.Pipeline, st tenant.Store, req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	c := s.c
	sj := req.GetScrub()
	if sj.GetScheduled() {
		return nil, c.failIn(d, st, invalid("scrub.scheduled", "is set by the server for a scrub it makes by itself, and is false here"))
	}
	if sj.GetStorage() != "" {
		if err := tenant.ValidateName("storage", sj.GetStorage()); err != nil {
			return nil, c.failIn(d, st, fieldOf("scrub.storage", err))
		}
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	nj := tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: sj.GetStorage(), Created: c.now()}
	if err := nj.Validate(); err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Object: id, Details: createJobDetails(req)}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}
