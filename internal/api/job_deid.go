package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deidModeOf returns the mode a request names, and false for UNSPECIFIED
// or a number the contract does not define.
func deidModeOf(m v1.DeidJobMode) (tenant.DeidMode, bool) {
	switch m {
	case v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN:
		return tenant.DeidDryRun, true
	case v1.DeidJobMode_DEID_JOB_MODE_CHECK:
		return tenant.DeidCheck, true
	}
	return "", false
}

// createDeidJob makes a deid job (job.proto, DeidJob): a dry run of a
// version, the latest when the request names version 0, fixed when the
// job is made; or a check, which names none. Its role, audit and errors
// are CreateJob's.
func (s jobService) createDeidJob(ctx context.Context, d access.Decision, p tenant.Pipeline, st tenant.Store, req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	c := s.c
	dj := req.GetDeid()
	mode, ok := deidModeOf(dj.GetMode())
	if !ok {
		return nil, c.failIn(d, st, invalid("deid.mode", "is required: DEID_JOB_MODE_DRY_RUN or DEID_JOB_MODE_CHECK"))
	}
	var ref tenant.ProfileRef
	switch {
	case mode == tenant.DeidCheck && (dj.GetProfile() != "" || dj.GetProfileVersion() != 0):
		return nil, c.failIn(d, st, invalid("deid.profile", "is empty for a check, which verifies against the version each instance records"))
	case dj.GetProfile() == "" && dj.GetProfileVersion() != 0:
		return nil, c.failIn(d, st, invalid("deid.profile_version", "is 0 when no profile is named"))
	case dj.GetProfile() != "":
		if err := profileName("deid.profile", dj.GetProfile()); err != nil {
			return nil, c.failIn(d, st, err)
		}
		if dj.GetProfileVersion() < 0 {
			return nil, c.failIn(d, st, invalid("deid.profile_version", "is 0 (the latest) or more"))
		}
		profiles, err := profilesOf(st)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		v, err := readVersion(ctx, profiles, dj.GetProfile(), dj.GetProfileVersion())
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		ref = v.Ref()
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	nj := tenant.NewJob{ID: id, Kind: tenant.JobDeid, Mode: mode, Profile: ref, Created: c.now(),
		Target: tenant.JobTarget{SubjectLabel: dj.GetSubjectLabel(), SessionLabel: dj.GetSessionLabel(), ScanLabel: dj.GetScanLabel()}}
	if !nj.Target.Whole() {
		if err := nj.Target.Validate(); err != nil {
			return nil, c.failIn(d, st, fieldOf("deid", err))
		}
	}
	if err := nj.Validate(); err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Object: id}
	if !ref.IsZero() {
		// The version the job fixed, in place of the name the request gave.
		facts.Details = createJobDetails(req)
		facts.Details[detailProfile] = ref.String()
	}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}
