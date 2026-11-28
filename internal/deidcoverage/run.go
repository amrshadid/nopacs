package deidcoverage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Store is what a coverage job does with the tenant's database.
type Store interface {
	tenant.DeidCoverages
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
	// Page is how many scans one write holds, 1 to tenant.MaxDeidCoveragePage;
	// zero is DefaultPage.
	Page int
	// Now is the clock the job's times come from.
	Now func() time.Time
	// Log receives a pass's notes; nil discards them.
	Log *slog.Logger
}

// DefaultPage is the scans one write holds when Config.Page is zero: a page
// is one transaction, and the elements of its scans together.
const DefaultPage = 100

// Runner runs coverage jobs.
type Runner struct {
	cfg Config
	// seams is what a test reads of a pass.
	seams struct {
		// largestScan is the most distinct elements one scan held in memory.
		largestScan int
	}
}

// New returns a Runner, or an Invalid error for a Config that lacks a part.
func New(cfg Config) (*Runner, error) {
	if cfg.Store == nil || cfg.Storages == nil || cfg.Now == nil {
		return nil, fault.New(fault.Invalid, "deidcoverage: a runner needs a store, storages and a clock")
	}
	if cfg.Page == 0 {
		cfg.Page = DefaultPage
	}
	if cfg.Page < 1 || cfg.Page > tenant.MaxDeidCoveragePage {
		return nil, fault.New(fault.Invalid, "deidcoverage: a page holds 1 to the most a write takes")
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
	// Written counts the scans this pass counted or skipped.
	Written int
}

// Run takes the job as far as it goes: page after page from the place it
// stands at, until the last scan, then its end. It returns when ctx ends or
// on the first error, the job left where the last page put it.
func (r *Runner) Run(ctx context.Context, jobID string) (Result, error) {
	var res Result
	job, err := r.cfg.Store.DeidCoverageJob(ctx, jobID)
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
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		scans, err := r.cfg.Store.DeidCoverageScans(ctx, after, r.cfg.Page)
		if err != nil {
			return res, err
		}
		if len(scans) == 0 {
			ended, err := r.cfg.Store.EndDeidCoverage(ctx, jobID, r.cfg.Now())
			if err != nil {
				return res, err
			}
			res.Ended = ended.State.Ended()
			return res, nil
		}
		page := newPage()
		rows := make([]tenant.DeidCoverageScanRow, len(scans))
		for i, s := range scans {
			row, err := r.read(ctx, sts, s, page)
			if err != nil {
				return res, err
			}
			rows[i] = row
		}
		got, err := r.cfg.Store.ApplyDeidCoverage(ctx, tenant.DeidCoverageApply{JobID: jobID, After: after, Scans: rows, Elements: page.list(), Now: r.cfg.Now()})
		if err != nil {
			if fault.KindOf(err) == fault.Precondition {
				// The job ended (canceled) or another runner wrote the page:
				// nothing is left for this pass to write.
				cur, jerr := r.cfg.Store.DeidCoverageJob(ctx, jobID)
				res.Ended = jerr == nil && cur.Job.State.Ended()
				if res.Ended {
					return res, nil
				}
			}
			return res, err
		}
		res.Written += len(rows)
		after = got.After
	}
}

// page is the elements of the scans of one write added up.
type page struct {
	elements map[tenant.CoverageKey]*tenant.CoverageElement
}

// newPage returns an empty page.
func newPage() *page { return &page{elements: map[tenant.CoverageKey]*tenant.CoverageElement{}} }

// list returns the page's elements in order of tag, creator, withheld and
// VR, so the same scans write the same page.
func (p *page) list() []tenant.CoverageElement {
	keys := slices.SortedFunc(maps.Keys(p.elements), func(a, b tenant.CoverageKey) int {
		switch {
		case a.Tag != b.Tag:
			return cmpUint(a.Tag, b.Tag)
		case a.Creator != b.Creator:
			return cmpString(a.Creator, b.Creator)
		case a.Withheld != b.Withheld:
			return cmpBool(a.Withheld, b.Withheld)
		}
		return cmpString(a.VR, b.VR)
	})
	out := make([]tenant.CoverageElement, len(keys))
	for i, k := range keys {
		out[i] = *p.elements[k]
	}
	return out
}

// cmpUint, cmpString and cmpBool order two values, false before true.
func cmpUint(a, b uint32) int {
	if a < b {
		return -1
	}
	return 1
}

func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	return 1
}

func cmpBool(a, b bool) int {
	if !a && b {
		return -1
	}
	return 1
}

// read counts the unnamed elements of every line of the scan's metadata file
// into the page, and returns the scan's row. A scan whose metadata is missing
// or not in its form is skipped, and counts nothing: what was read of it
// before the fault is dropped, so a scan is counted whole or not at all. A
// storage that does not answer, or a metadata file that cannot be read, is an
// error: the pass stops and the next goes on from the same place.
func (r *Runner) read(ctx context.Context, sts Storages, s tenant.DeidCoverageScan, p *page) (tenant.DeidCoverageScanRow, error) {
	skipped := tenant.DeidCoverageScanRow{ID: s.ID, Skipped: true}
	key, err := layout.MetadataKey(s.Path)
	if err != nil {
		return skipped, nil
	}
	st, err := sts.Get(ctx, s.Storage)
	if err != nil {
		return skipped, err
	}
	rc, err := st.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return skipped, nil
	}
	if err != nil {
		return skipped, err
	}
	defer func() { _ = rc.Close() }()
	row := tenant.DeidCoverageScanRow{ID: s.ID}
	local := map[tenant.CoverageKey]*tenant.CoverageElement{}
	// fresh counts the elements of local the page does not hold yet.
	fresh := 0
	rd := sidecar.NewReader(rc)
	for {
		if err := ctx.Err(); err != nil {
			return skipped, err
		}
		line, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, sidecar.ErrFormat) {
			return skipped, nil
		}
		if err != nil {
			return skipped, err
		}
		row.Instances++
		if line.Unnamed == nil {
			continue
		}
		row.Recorded++
		if line.UnnamedOver > 0 {
			row.Capped++
			row.NotListed += int64(line.UnnamedOver)
		}
		for _, u := range line.Unnamed {
			k := tenant.CoverageKey{Tag: u.Tag, Creator: u.Creator, Withheld: u.Withheld, VR: u.VR}
			e := local[k]
			if e == nil {
				// A scan's elements are held in memory until it is read
				// whole, so they are bounded as a report is: what the page
				// holds already, and the new elements of this scan, are at
				// most MaxCoverageElements. Occurrences past it are counted
				// as not listed, as the page's are.
				if _, held := p.elements[k]; !held {
					if fresh >= tenant.MaxCoverageElements-len(p.elements) {
						row.NotListed += int64(u.Count)
						continue
					}
					fresh++
				}
				e = &tenant.CoverageElement{CoverageKey: k}
				local[k] = e
			}
			e.Instances++
			e.Occurrences += int64(u.Count)
		}
	}
	r.seams.largestScan = max(r.seams.largestScan, len(local))
	// The scan is read whole: its elements join the page's. A page holds at
	// most the bound of distinct elements; the occurrences of one past it are
	// counted as not listed, never dropped without a trace.
	for k, e := range local {
		held := p.elements[k]
		switch {
		case held != nil:
			held.Instances += e.Instances
			held.Occurrences += e.Occurrences
		case len(p.elements) < tenant.MaxCoverageElements:
			p.elements[k] = e
		default:
			row.NotListed += e.Occurrences
		}
	}
	return row, nil
}
