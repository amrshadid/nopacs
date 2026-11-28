package searchfill

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Store is what a backfill does with the tenant's database.
type Store interface {
	tenant.SearchBackfills
}

// Storages opens the storages a pass reads, each once, and closes them at
// its end: package checkedread's Stores is one.
type Storages interface {
	// Get returns the storage named name, opening it on first use.
	Get(ctx context.Context, name string) (blob.Store, error)
	// Close closes every storage opened.
	Close()
}

// Config is what a Runner needs.
type Config struct {
	// Store is the tenant's database, each write fenced as the caller's
	// background work fences it.
	Store Store
	// Storages opens the storages the scans' metadata is read from.
	Storages func(ctx context.Context) (Storages, error)
	// Page is how many scans one write holds, 1 to tenant.MaxSearchBackfillPage;
	// zero is DefaultPage.
	Page int
	// Now is the clock the job's times come from.
	Now func() time.Time
	// Log receives a pass's notes; nil discards them.
	Log *slog.Logger
}

// MaxStalePages is how many times a pass reads a page again because a scan
// of it changed meanwhile, before it stops with the error and leaves the
// page to the next pass.
const MaxStalePages = 3

// DefaultPage is the scans one write holds when Config.Page is zero: a page
// is one transaction of a few hundred rows, each a row and its values.
const DefaultPage = 200

// Runner runs search backfill jobs.
type Runner struct {
	cfg Config
}

// New returns a Runner, or an Invalid error for a Config that lacks a part.
func New(cfg Config) (*Runner, error) {
	if cfg.Store == nil || cfg.Storages == nil || cfg.Now == nil {
		return nil, fault.New(fault.Invalid, "searchfill: a runner needs a store, storages and a clock")
	}
	if cfg.Page == 0 {
		cfg.Page = DefaultPage
	}
	if cfg.Page < 1 || cfg.Page > tenant.MaxSearchBackfillPage {
		return nil, fault.New(fault.Invalid, "searchfill: a page holds 1 to the most a write takes")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Runner{cfg: cfg}, nil
}

// Result is what a pass did.
type Result struct {
	// Ended says the job has ended, by this pass or before it.
	Ended bool
	// Written counts the scans this pass wrote or skipped.
	Written int
}

// Run takes the job as far as it goes: page after page from the place it
// stands at, until the last scan, then its end. It returns when ctx ends or
// on the first error, the job left where the last page put it.
func (r *Runner) Run(ctx context.Context, jobID string) (Result, error) {
	var res Result
	job, err := r.cfg.Store.SearchBackfillJob(ctx, jobID)
	if err != nil {
		return res, err
	}
	if job.Job.State.Ended() {
		res.Ended = true
		return res, nil
	}
	sts, err := r.cfg.Storages(ctx)
	if err != nil {
		return res, err
	}
	defer sts.Close()
	after := job.After
	stale := 0
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		scans, err := r.cfg.Store.SearchBackfillScans(ctx, after, r.cfg.Page)
		if err != nil {
			return res, err
		}
		if len(scans) == 0 {
			ended, err := r.cfg.Store.EndSearchBackfill(ctx, jobID, r.cfg.Now())
			if err != nil {
				return res, err
			}
			res.Ended = ended.State.Ended()
			return res, nil
		}
		rows := make([]tenant.SearchBackfillRow, len(scans))
		for i, s := range scans {
			row, skipped, err := r.read(ctx, sts, s)
			if err != nil {
				return res, err
			}
			rows[i] = tenant.SearchBackfillRow{ID: s.ID, Correction: s.Correction, Manifest: s.Manifest, Row: row, Skipped: skipped}
		}
		got, err := r.cfg.Store.ApplySearchBackfill(ctx, tenant.SearchBackfillApply{JobID: jobID, After: after, Rows: rows, Now: r.cfg.Now()})
		if errors.Is(err, tenant.ErrSearchBackfillStale) && stale < MaxStalePages {
			// A scan of the page changed after its metadata was read: the
			// page is listed and read again, which finds the change.
			stale++
			continue
		}
		if err != nil {
			if fault.KindOf(err) == fault.Precondition {
				// The job ended (canceled) or another runner wrote the page:
				// nothing is left for this pass to write.
				cur, jerr := r.cfg.Store.SearchBackfillJob(ctx, jobID)
				res.Ended = jerr == nil && cur.Job.State.Ended()
				if res.Ended {
					return res, nil
				}
			}
			return res, err
		}
		res.Written += len(rows)
		stale = 0
		after = got.After
	}
}

// read returns the row of the scan from the first line of its metadata
// file, and true for a scan whose metadata is missing or not in its form,
// which is skipped. A storage that does not answer, or a metadata file that
// cannot be read to its first line, is an error: the pass stops.
func (r *Runner) read(ctx context.Context, sts Storages, s tenant.SearchBackfillScan) (search.Row, bool, error) {
	key, err := layout.MetadataKey(s.Path)
	if err != nil {
		return search.Row{}, true, nil
	}
	st, err := sts.Get(ctx, s.Storage)
	if err != nil {
		return search.Row{}, false, err
	}
	rc, err := st.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return search.Row{}, true, nil
	}
	if err != nil {
		return search.Row{}, false, err
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	line, _, err := rd.Next()
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, sidecar.ErrFormat):
		return search.Row{}, true, nil
	case err != nil:
		return search.Row{}, false, err
	case rd.Version() != sidecar.Version || rd.Scan() == nil:
		return search.Row{}, true, nil
	}
	// The lines are in order of SOP Instance UID: the first is the file the
	// scan's search values are read from, as the archive step reads them.
	return search.ExtractJSON(line.Dataset, line.SOPInstanceUID), false, nil
}
