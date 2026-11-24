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
// answered permission denied. A rebuild from one storage is refused before
// this as not served. Its audit entry names its mode.
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
	// A replace empties the catalog and fills it again, so it needs admin:
	// checked first, so an operator is told the role it lacks whatever the
	// store would say of the job, and again in the transaction that makes
	// the job, as every role is.
	need := master.RoleOperator
	if mode == tenant.RebuildReplace {
		need = master.RoleAdmin
		if err := c.roleGuard(d, need)(ctx); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	facts := access.Facts{Object: id, Details: map[string]string{detailKind: "rebuild", detailMode: string(mode)}}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, need))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	if mode == tenant.RebuildReplace {
		// The replace job, queued, is the pause itself: the receivers'
		// supervisor reads it at once rather than at its next round, stops
		// the tenant's pipeline and refuses new files as a temporary
		// failure until the job ends, and the rebuild builds its shadow
		// once that stop is over.
		c.receiversChanged(req.GetTenant())
	}
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
