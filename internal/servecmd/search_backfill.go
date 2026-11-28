package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/searchfill"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// searchFillInterval is how often a tenant's search backfill jobs not ended
// are looked for and taken as far as they go: a job made starts within it,
// and one a restart left goes on within it.
const searchFillInterval = 5 * time.Second

// tenantSearchFills runs one tenant's search backfill jobs: every
// searchFillInterval, each job not ended is taken from the place its row
// keeps (searchfill.Runner.Run), so a restart goes on after the last page
// written.
type tenantSearchFills struct {
	w    *tenantWork
	name string
	// st is the tenant's database as the background work writes it, each
	// write fenced.
	st  tenantDB
	log *slog.Logger
}

// run works the search backfills every searchFillInterval until ctx ends.
func (t *tenantSearchFills) run(ctx context.Context) {
	tick := time.NewTicker(searchFillInterval)
	defer tick.Stop()
	for {
		if err := t.once(ctx); err != nil && ctx.Err() == nil {
			t.log.Warn("search backfill: a pass could not finish; it goes on at the next", "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once takes every search backfill job not ended as far as it goes.
func (t *tenantSearchFills) once(ctx context.Context) error {
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		jobs, next, err := t.st.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Kind == tenant.JobSearchBackfill {
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

// work runs one pass of a search backfill job.
func (t *tenantSearchFills) work(ctx context.Context, j tenant.Job) error {
	w := t.w
	// The server's opener of storages, which opens beside what another
	// open cleaned up, and cleans up no unfinished upload of its own.
	opener := w.opener
	opener.StaleUploadAge = -1
	tenantID := func(ctx context.Context, name string) (string, error) { return w.tenantID(ctx, name) }
	runner, err := searchfill.New(searchfill.Config{
		Store: t.st,
		// Storages are opened for reading only: the job writes the tenant's
		// database, never a storage.
		Storages: func(context.Context) (searchfill.Storages, error) {
			return checkedread.NewStores(opener, tenantID, t.st, t.name), nil
		},
		Now: w.now, Log: t.log,
	})
	if err != nil {
		return err
	}
	res, err := runner.Run(ctx, j.ID)
	if res.Ended {
		t.log.Info("search backfill: a backfill ended", "job", j.ID)
	}
	return err
}
