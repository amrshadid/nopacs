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
	case v1.DeidJobMode_DEID_JOB_MODE_RERUN:
		return tenant.DeidRerun, true
	case v1.DeidJobMode_DEID_JOB_MODE_RERUN_PREVIEW:
		return tenant.DeidRerunPreview, true
	}
	return "", false
}

// createDeidJob makes a deid job (job.proto, DeidJob): a dry run of a
// version, the latest when the request names version 0, fixed when the
// job is made; or a check, which names none; or a re-run of a newer version
// over the archive (DEID_JOB_MODE_RERUN) or its preview, which name one
// and need the admin role, never a masked key. Its role, audit and errors
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
	case mode.Rerun() && dj.GetProfile() == "":
		return nil, c.failIn(d, st, invalid("deid.profile", "is required: the profile whose newer version the re-run runs"))
	case mode == tenant.DeidRerun && tenant.ValidateUndoReason(dj.GetReason()) != nil:
		return nil, c.failIn(d, st, invalid("deid.reason", "is required for a re-run: why, 1 to 1,000 characters"))
	case !mode.Rerun() && dj.GetReason() != "":
		return nil, c.failIn(d, st, invalid("deid.reason", "is empty unless the job re-runs a profile"))
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
		// A re-run's version, like a receiver's, is a layer over the tenant's
		// base: one that would let through more than the base is refused now,
		// not file by file.
		if mode.Rerun() {
			if err := c.checkReceiverBase(ctx, p, "deid.profile", ref); err != nil {
				return nil, c.layerFail(d, st, access.Facts{}, err)
			}
		}
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
	if mode == tenant.DeidRerun {
		facts.Details["reason"] = clip(dj.GetReason(), 200)
	}
	out, err := p.CreateJob(ctx, nj, changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}
