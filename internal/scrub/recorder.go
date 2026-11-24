package scrub

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The details of an instance a scrub refused, from a fixed set.
const (
	detailMissing   = "its object is not on its storage"
	detailDamaged   = "its object is not what was archived"
	detailNotOnline = "its object is held in a tier the storage cannot read at once"
)

// recorder gathers what a scan's unit read, in the stream's order, and
// writes it as the job's progress in batches: at most MaxBatch instances,
// or BatchInterval after the batch's first. A finding is recorded before
// the write that moves the cursor past its instance, so a crash between
// the two reads the instance again and records the same finding, which
// counts nothing twice.
type recorder struct {
	u *unitRun
	// cursor is the last instance written.
	cursor string
	p      tenant.JobProgress
	n      int
	since  time.Time
	// tallies are the scan's own tallies, which ride on the next write that
	// counts an instance (a progress write that counts none adds to no
	// tally), so they are counted once, with the cursor's move.
	tallies map[string]int64
}

// tally adds the scan's own tallies to the next write.
func (r *recorder) tally(t map[string]int64) {
	for k, v := range t {
		if r.tallies == nil {
			r.tallies = map[string]int64{}
		}
		r.tallies[k] += v
	}
}

// add counts one instance: read good, or refused with the code and
// detail, with the tallies its write adds to.
func (r *recorder) add(in tenant.ScanInstance, code tenant.JobFailureCode, detail string, tallies map[string]int64) {
	if r.n == 0 {
		r.p = tenant.JobProgress{From: r.cursor}
		r.since = r.u.sup.cfg.Now()
	}
	r.n++
	r.p.To = in.SOPInstanceUID
	if code == "" {
		r.p.Sent, r.p.SentBytes = r.p.Sent+1, r.p.SentBytes+in.Size
	} else {
		r.p.Refused, r.p.RefusedBytes = r.p.Refused+1, r.p.RefusedBytes+in.Size
		r.p.Failures = append(r.p.Failures, tenant.JobFailure{SOPInstanceUID: in.SOPInstanceUID, Code: code, Detail: detail,
			Time: r.u.sup.cfg.Now()})
	}
	for k, v := range tallies {
		if r.p.Tallies == nil {
			r.p.Tallies = map[string]int64{}
		}
		r.p.Tallies[k] += v
	}
}

// due reports whether the batch should be written now.
func (r *recorder) due() bool {
	return r.n >= MaxBatch || (r.n > 0 && r.u.sup.cfg.Now().Sub(r.since) >= BatchInterval)
}

// progress returns the batch as the progress write of the unit.
func (r *recorder) progress() tenant.JobProgress {
	p := r.p
	if r.n == 0 {
		p = tenant.JobProgress{From: r.cursor, To: r.cursor}
	} else if len(r.tallies) > 0 {
		all := make(map[string]int64, len(p.Tallies)+len(r.tallies))
		for k, v := range p.Tallies {
			all[k] += v
		}
		for k, v := range r.tallies {
			all[k] += v
		}
		p.Tallies = all
	}
	p.Fence, p.FailuresKept, p.Now = r.u.fence, r.u.kept, r.u.sup.cfg.Now()
	return p
}

// flush writes the batch, if any, and returns the job as the write left it
// and whether it wrote.
func (r *recorder) flush(ctx context.Context) (tenant.Job, bool, error) {
	if r.n == 0 {
		return tenant.Job{}, false, nil
	}
	job, err := r.u.p.RecordJobProgress(ctx, r.progress())
	if err != nil {
		return tenant.Job{}, false, err
	}
	r.cursor, r.n, r.tallies = r.p.To, 0, nil
	r.u.sup.cfg.Wake(r.u.t.name)
	return job, true, nil
}

// end writes the batch and ends the scan.
func (r *recorder) end(ctx context.Context) (tenant.Job, error) {
	job, err := r.u.p.EndJobScan(ctx, r.progress())
	if err == nil {
		r.cursor, r.n, r.tallies = r.p.To, 0, nil
	}
	return job, err
}
