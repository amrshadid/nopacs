package deidjob

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// resultKind is what a unit made of one instance.
type resultKind int

// The kinds of result, as a job counts them: sent is passed, warned is
// passed with a note, refused is did not pass.
const (
	resultPassed resultKind = iota + 1
	resultNoted
	resultRefused
)

// verdict is what became of one instance: its result, the failure's code
// and detail for a note or a refusal, its class and syntax when known, and
// what it adds to the job's tallies.
type verdict struct {
	kind             resultKind
	code             tenant.JobFailureCode
	detail           string
	sopClass, syntax string
	tallies          map[string]int64
}

// recorder holds a scan unit's verdicts since its cursor and writes them,
// fenced by the unit's lease, in batches of at most MaxBatch instances or
// BatchInterval, or sooner when the batch's tallies would pass
// tenant.MaxJobTallyBatch keys.
type recorder struct {
	p     tenant.Pipeline
	fence tenant.WorkFence
	now   func() time.Time
	kept  int
	// cursor is the unit's cursor as last written; to is the last
	// instance added since.
	cursor, to string
	batch      tenant.JobProgress
	count      int
	first      time.Time
	wrote      func()
}

// newRecorder returns the recorder of a unit whose cursor is cursor.
func newRecorder(p tenant.Pipeline, fence tenant.WorkFence, cursor string, kept int, now func() time.Time, wrote func()) *recorder {
	r := &recorder{p: p, fence: fence, now: now, kept: kept, cursor: cursor, to: cursor, wrote: wrote}
	r.reset()
	return r
}

// reset empties the batch, back to the cursor last written.
func (r *recorder) reset() {
	r.batch = tenant.JobProgress{Fence: r.fence, FailuresKept: r.kept, Tallies: map[string]int64{}}
	r.count = 0
	r.to = r.cursor
}

// fits reports whether v's tallies fit in the batch beside those it holds.
func (r *recorder) fits(v verdict) bool {
	n := len(r.batch.Tallies)
	for k := range v.tallies {
		if _, ok := r.batch.Tallies[k]; !ok {
			n++
		}
	}
	return n <= tenant.MaxJobTallyBatch
}

// add records v of in, the next instance of the unit's stream.
func (r *recorder) add(in tenant.ScanInstance, v verdict) {
	b := &r.batch
	switch v.kind {
	case resultPassed:
		b.Sent, b.SentBytes = b.Sent+1, b.SentBytes+in.Size
	case resultNoted:
		b.Warned, b.WarnedBytes = b.Warned+1, b.WarnedBytes+in.Size
	case resultRefused:
		b.Refused, b.RefusedBytes = b.Refused+1, b.RefusedBytes+in.Size
	}
	if v.kind != resultPassed {
		b.Failures = append(b.Failures, tenant.JobFailure{SOPInstanceUID: in.SOPInstanceUID, Code: v.code,
			SOPClassUID: v.sopClass, TransferSyntaxUID: v.syntax, Detail: v.detail, Time: r.now()})
	}
	for k, n := range v.tallies {
		b.Tallies[k] += n
	}
	if r.count == 0 {
		r.first = time.Now()
	}
	r.count++
	r.to = in.SOPInstanceUID
}

// due reports whether the batch is to be written now.
func (r *recorder) due() bool {
	return r.count >= MaxBatch || (r.count > 0 && time.Since(r.first) >= BatchInterval)
}

// flush writes the batch, when it holds any, and returns the job as the
// write left it; ok is false when nothing was written.
func (r *recorder) flush(ctx context.Context) (job tenant.Job, ok bool, err error) {
	if r.count == 0 {
		return tenant.Job{}, false, nil
	}
	job, err = r.p.RecordJobProgress(ctx, r.progress())
	if err != nil {
		return job, false, err
	}
	r.cursor = r.to
	r.reset()
	r.wrote()
	return job, true, nil
}

// end writes what the batch holds and ends the scan (EndJobScan).
func (r *recorder) end(ctx context.Context) (tenant.Job, error) {
	job, err := r.p.EndJobScan(ctx, r.progress())
	if err == nil {
		r.cursor = r.to
		r.reset()
		r.wrote()
	}
	return job, err
}

// progress returns the batch as a write records it.
func (r *recorder) progress() tenant.JobProgress {
	p := r.batch
	p.From, p.To, p.Now = r.cursor, r.to, r.now()
	if len(p.Tallies) == 0 {
		p.Tallies = nil
	}
	return p
}
