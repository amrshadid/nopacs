package servecmd

import (
	"context"
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/deidjob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// rerunMethod is the method of the audit entry of each new revision a
// profile re-run publishes, written as the server's in the name of the key
// that made the job.
const rerunMethod = "correction.profile_rerun"

// deidRevisions returns what publishes the new revisions of a profile
// re-run on the tenant named name: the corrections engine's own edit path
// (correct.Engine.Revise), never a second one. st is the database as the
// registry answered it, by which the engines know the tenant's engine.
func (w *tenantWork) deidRevisions(ctx context.Context, name string, st tenant.Store) (deidjob.Reviser, func(), error) {
	es := w.engines.Load()
	if es == nil {
		return nil, nil, fault.New(fault.Unavailable, "the corrections engines are not ready")
	}
	e, release, err := es.Use(ctx, name, st)
	if err != nil {
		return nil, nil, err
	}
	return &profileReviser{w: w, e: e, tenant: name}, release, nil
}

// profileReviser publishes a re-run's revisions through a tenant's
// corrections engine, each in the name of the key that made the job and
// guarded by that key's role as a batch delete's are.
type profileReviser struct {
	w      *tenantWork
	e      *correct.Engine
	tenant string
}

// Revise implements deidjob.Reviser.
func (r *profileReviser) Revise(ctx context.Context, rv deidjob.Revision) error {
	guard, err := r.w.keyGuard(ctx, r.tenant, rv.KeyID)
	if err != nil {
		return err
	}
	now := r.w.now()
	audit := serverAudit(r.tenant, rerunMethod, rv.JobID, now)
	audit.Details = map[string]string{"job": rv.JobID, "profile": rv.Profile.String(),
		"target": strings.Join(nonEmpty(rv.Target.Subject, rv.Target.Session, rv.Target.Scan), "/")}
	if rv.KeyID != "" {
		audit.Details["key"] = rv.KeyID
	}
	values := make([]correct.EditChange, len(rv.Catalog))
	for i, c := range rv.Catalog {
		values[i] = correct.EditChange{Tag: c.Tag, Value: c.Value}
	}
	_, err = r.e.Revise(ctx, correct.ReviseRequest{Target: rv.Target, OldSHA256: rv.OldSHA256, File: rv.File, Profile: rv.Profile, Values: values,
		ProfileSHA256: rv.ProfileSHA256, Base: rv.Base, Unnamed: rv.Unnamed, UnnamedOver: rv.UnnamedOver,
		Reason: "profile re-run " + rv.Profile.String() + " (job " + rv.JobID + ")", KeyID: rv.KeyID, Audit: audit, Guard: guard})
	var refused *correct.KeyRefused
	if errors.As(err, &refused) {
		return deidjob.ErrKeyRefused
	}
	return err
}
