package backup

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// queueRebuild makes a rebuild job of the tenant, queued, as a server makes
// one before its worker claims it, building in scratch on Postgres.
func (r *rig) queueRebuild(id, scratch string) {
	r.t.Helper()
	rj := &tenant.RebuildJob{Mode: tenant.RebuildCompare}
	if r.postgres {
		rj.Scratch = scratch
	}
	if _, err := r.p.CreateJob(r.ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: rj, Created: time.Now().UTC()}, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// wantEnded fails unless the job id ended canceled, with the reason a
// restore gives it.
func (r *rig) wantEnded(id string) {
	r.t.Helper()
	j, err := r.p.Job(r.ctx, id)
	if err != nil || j.State != tenant.JobCanceled || j.Reason != tenant.JobReasonEndedByRestore {
		r.t.Fatalf("the rebuild left not ended is %s, %q (%v); want canceled, %q", j.State, j.Reason, err, tenant.JobReasonEndedByRestore)
	}
}

// TestReconcileEndsARebuildLeftNotEnded guards a copy that holds a rebuild
// job a server made and had not ended when the copy was taken (queued, its
// mark on the scratch schema on Postgres): the reconcile of the copy in
// place ends it canceled, with its reason, releases its scratch schema, and
// rebuilds the catalog in the same schema, rather than being refused for
// ever by the rule that no two rebuilds run; a restore of a backup that
// holds it does the same, on each database type.
func TestReconcileEndsARebuildLeftNotEnded(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles[:2]...)
		r.queueRebuild("leftrebuild0001", r.reconcileConfig().Scratch)
		path, _ := r.backup()
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore of a backup holding a rebuild not ended failed: %v", err)
		}
		if !rep.Tenants[0].Rebuild.Rebuild.Activated {
			t.Fatalf("the restore's rebuild is %+v", rep.Tenants[0].Rebuild)
		}
		r.wantEnded("leftrebuild0001")

		cfg := r.reconcileConfig()
		r.queueRebuild("leftrebuild0002", cfg.Scratch)
		_ = r.st.Close()
		_ = r.m.Close()
		rep, err = ReconcileOnly(r.ctx, cfg)
		if err != nil {
			t.Fatalf("the reconcile of a copy holding a rebuild not ended failed: %v", err)
		}
		if !rep.Tenants[0].Rebuild.Rebuild.Activated || ReconcileInterrupted(r.dataDir) {
			t.Fatalf("the reconcile's rebuild is %+v, marker left %v", rep.Tenants[0].Rebuild, ReconcileInterrupted(r.dataDir))
		}
		r.openMaster()
		r.openTenant()
		r.wantEnded("leftrebuild0002")
		if left, err := r.p.ShadowsLeft(r.ctx); err != nil || len(left) != 0 {
			t.Fatalf("shadows left after the reconcile: %v (%v)", left, err)
		}
	})
}
