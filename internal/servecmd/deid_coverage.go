package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/deidcoverage"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// deidCoverageInterval is how often a tenant's coverage jobs not ended are
// looked for and taken as far as they go: a job made starts within it, and
// one a restart left goes on within it.
const deidCoverageInterval = 5 * time.Second

// tenantDeidCoverages runs one tenant's coverage jobs: every
// deidCoverageInterval, each job not ended is taken from the place its row
// keeps (deidcoverage.Runner.Run), so a restart goes on after the last page
// written.
type tenantDeidCoverages struct {
	w    *tenantWork
	name string
	// st is the tenant's database as the background work writes it, each
	// write fenced.
	st  tenantDB
	log *slog.Logger
}

// run works the coverage jobs every deidCoverageInterval until ctx ends.
func (t *tenantDeidCoverages) run(ctx context.Context) {
	tick := time.NewTicker(deidCoverageInterval)
	defer tick.Stop()
	for {
		if err := t.once(ctx); err != nil && ctx.Err() == nil {
			t.log.Warn("deid coverage: a pass could not finish; it goes on at the next", "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once takes every coverage job not ended as far as it goes.
func (t *tenantDeidCoverages) once(ctx context.Context) error {
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		jobs, next, err := t.st.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Kind == tenant.JobDeidCoverage {
				errs = append(errs, t.work(ctx, j))
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return errors.Join(errs...)
}

// work runs one pass of a coverage job.
func (t *tenantDeidCoverages) work(ctx context.Context, j tenant.Job) error {
	w := t.w
	// The server's opener of storages, which opens beside what another open
	// cleaned up, and cleans up no unfinished upload of its own.
	opener := w.opener
	opener.StaleUploadAge = -1
	tenantID := func(ctx context.Context, name string) (string, error) { return w.tenantID(ctx, name) }
	runner, err := deidcoverage.New(deidcoverage.Config{
		Store: t.st,
		// Storages are opened for reading only: the job writes the tenant's
		// database, never a storage.
		Storages: func(context.Context) (deidcoverage.Storages, error) {
			return checkedread.NewStores(opener, tenantID, t.st, t.name), nil
		},
		Now: w.now, Log: t.log,
	})
	if err != nil {
		return err
	}
	res, err := runner.Run(ctx, j.ID)
	if res.Ended {
		t.log.Info("deid coverage: a job ended", "job", j.ID)
	}
	return err
}
