package api

import (
	"context"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// createRebuildJob makes a rebuild job (job.proto, RebuildJob): its mode
// and, on a Postgres tenant, the scratch schema its shadow is built in,
// which the store checks. One that replaces the catalog needs admin on the
// tenant, checked after the operator's requirement, so an operator is
// answered permission denied; an admin is answered that it is not served
// yet. A rebuild from one storage is refused before this as not served.
// Its audit entry names its mode.
func (s jobService) createRebuildJob(ctx context.Context, d access.Decision, p tenant.Pipeline, st tenant.Store,
	req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	c := s.c
	rj := req.GetRebuild()
	var mode tenant.RebuildMode
	switch rj.GetMode() {
	case v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE:
		mode = tenant.RebuildCompare
	case v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE:
		mode = tenant.RebuildReplace
	default:
		return nil, c.failIn(d, st, invalid("rebuild.mode", "is compare or replace"))
	}
	if sc := rj.GetScratchSchema(); sc != "" {
		if err := tenant.ValidateScratch(sc); err != nil {
			return nil, c.failIn(d, st, fieldOf("rebuild.scratch_schema", err))
		}
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	nj := tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: mode, Scratch: rj.GetScratchSchema()}, Created: c.now()}
	if err := nj.Validate(); err != nil {
		return nil, c.failIn(d, st, err)
	}
	if mode == tenant.RebuildReplace {
		// An operator is told the role it lacks, as a served replace would
		// tell it; an admin, that a replace is not served: nothing yet
		// pauses the receivers or holds corrections back while its shadow
		// is built, so a file archived meanwhile would leave the catalog.
		if err := c.roleGuard(d, master.RoleAdmin)(ctx); err != nil {
			return nil, c.failIn(d, st, err)
		}
		return nil, c.refuseNotServed(d, st, "rebuild.mode: a replace rebuild is not served yet")
	}
	facts := access.Facts{Object: id, Details: map[string]string{detailKind: "rebuild", detailMode: string(mode)}}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}

// rebuildMessage returns a rebuild job's kind as the contract writes it.
func rebuildMessage(r *tenant.RebuildJob) *v1.RebuildJob {
	if r == nil {
		return &v1.RebuildJob{}
	}
	mode := v1.RebuildJobMode(v1.RebuildJobMode_value["REBUILD_JOB_MODE_"+strings.ToUpper(string(r.Mode))])
	return &v1.RebuildJob{Mode: mode, ScratchSchema: r.Scratch}
}
