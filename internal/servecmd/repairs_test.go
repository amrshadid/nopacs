package servecmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pagedFindings is a tenant's database whose primary main holds two
// findings, one a page: the first found before its last automatic repair
// job, the second after; its jobs list that job, ended; the jobs it is
// asked to make are counted, not written.
type pagedFindings struct {
	tenantDB
	last time.Time
	made atomic.Int64
}

// ListScrubFindings answers one finding a page.
func (p *pagedFindings) ListScrubFindings(_ context.Context, filter tenant.ScrubFindingFilter, page master.Page) ([]tenant.ScrubFinding,
	string, error) {
	if filter.Storage != "main" {
		return nil, "", nil
	}
	if page.Cursor == "" {
		return []tenant.ScrubFinding{{Storage: "main", Kind: tenant.FindingDamaged, Found: p.last.Add(-time.Hour)}}, "next", nil
	}
	return []tenant.ScrubFinding{{Storage: "main", Kind: tenant.FindingMissing, Found: p.last.Add(time.Hour)}}, "", nil
}

// ListJobs answers the last automatic repair job of main, ended.
func (p *pagedFindings) ListJobs(context.Context, tenant.JobFilter, master.Page) ([]tenant.Job, string, error) {
	return []tenant.Job{{ID: "j-1", Kind: tenant.JobRepair, State: tenant.JobDone, Created: p.last,
		Repair: &tenant.RepairJob{Storage: "main", Automatic: true}}}, "", nil
}

// CreateJob counts the job.
func (p *pagedFindings) CreateJob(context.Context, tenant.NewJob, master.AuditEntry, tenant.Guard) (tenant.Job, error) {
	p.made.Add(1)
	return tenant.Job{}, nil
}

// TestAnAutomaticRepairSeesAFindingOnALaterPage guards autoOnce's reading
// of the findings: a finding found since the primary's last automatic
// repair makes a new automatic job even when it is not on the first page
// of findings.
func TestAnAutomaticRepairSeesAFindingOnALaterPage(t *testing.T) {
	r := newWorkRig(t)
	if err := r.db.SetSetting(r.ctx, "replica.repair", "auto", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	db := &pagedFindings{tenantDB: r.db, last: time.Now().Add(-24 * time.Hour)}
	rep := &tenantRepairs{w: r.w, name: "lab", st: db, log: r.w.log, wake: make(chan struct{}, 1),
		open: func(context.Context, string) (blob.Store, error) { return nil, errors.New("not opened") }}
	if err := rep.autoOnce(r.ctx); err != nil {
		t.Fatal(err)
	}
	if n := db.made.Load(); n != 1 {
		t.Fatalf("a finding on the second page made %d automatic jobs, want 1", n)
	}
}
