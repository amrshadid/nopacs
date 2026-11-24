package send

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// resultKind is what the peer's answer, or the read, made of one instance.
type resultKind int

// The kinds of result.
const (
	// resultSent is an instance the peer answered success for.
	resultSent resultKind = iota + 1
	// resultWarned is one it kept with a warning.
	resultWarned
	// resultRefused is one refused, by the peer or before it reached it.
	resultRefused
	// resultOwed is one a stow peer left unaccounted, the first time.
	resultOwed
)

// result is what became of one instance.
type result struct {
	kind resultKind
	// code, status and detail are a failure's or a warning's
	// (tenant.JobFailure).
	code   tenant.JobFailureCode
	status int32
	detail string
	// sopClass and syntax are the instance's, when known.
	sopClass, syntax string
	// superseded says the instance's file was replaced while it was sent:
	// it is counted as sent all the same, and tallied so a person can send
	// again.
	superseded bool
}

// The fixed phrases of an instance's failure (JobFailure.detail).
const (
	detailRefused       = "refused by the peer"
	detailWarning       = "kept by the peer with a warning"
	detailClass         = "not accepted: SOP class"
	detailSyntax        = "not accepted: transfer syntax"
	detailUnaccounted   = "left unaccounted by the peer twice"
	detailDamaged       = "its bytes on the storage are not what was archived"
	detailMissing       = "its file is missing from the storage"
	detailMetaDisagrees = "not sent: its file meta information and its data set name different SOP classes or instances"
	detailNoMeta        = "not sent: its file meta information cannot be read, so what its data set is and where it starts are not known"
	detailTooManyKinds  = "not sent: the scan holds more kinds of instance than one association proposes"
	detailDeleted       = "not sent: a correction deleted it after the job was made"
	detailWholeRequest  = tenant.FailureDetailWholeRequest
	detailWithoutStatus = "refused by the peer without a status"
)

// sent, warned, refused and owed make the results of each kind.
func sent() result { return result{kind: resultSent} }

func warned(status int32) result {
	return result{kind: resultWarned, code: tenant.FailurePeerWarning, status: status, detail: detailWarning}
}

func refused(code tenant.JobFailureCode, status int32, detail string) result {
	return result{kind: resultRefused, code: code, status: status, detail: detail}
}

func owed() result { return result{kind: resultOwed} }

// recorder holds a scan unit's answers since its cursor and writes them,
// fenced by the unit's lease, in batches of at most MaxBatch instances or
// BatchInterval.
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
	// wrote is called after each write that succeeded.
	wrote func()
}

// newRecorder returns the recorder of a unit whose cursor is cursor.
func newRecorder(p tenant.Pipeline, fence tenant.WorkFence, cursor string, kept int, now func() time.Time, wrote func()) *recorder {
	r := &recorder{p: p, fence: fence, now: now, kept: kept, cursor: cursor, to: cursor, wrote: wrote}
	r.reset()
	return r
}

// reset empties the batch, back to the cursor last written.
func (r *recorder) reset() {
	r.batch = tenant.JobProgress{Fence: r.fence, FailuresKept: r.kept}
	r.count = 0
	r.to = r.cursor
}

// count adds res of in to the batch's counts and failures.
func (r *recorder) tally(in tenant.ScanInstance, res result) {
	b := &r.batch
	switch res.kind {
	case resultSent:
		b.Sent, b.SentBytes = b.Sent+1, b.SentBytes+in.Size
	case resultWarned:
		b.Warned, b.WarnedBytes = b.Warned+1, b.WarnedBytes+in.Size
	case resultRefused:
		b.Refused, b.RefusedBytes = b.Refused+1, b.RefusedBytes+in.Size
	}
	if res.superseded && (res.kind == resultSent || res.kind == resultWarned) {
		if b.Tallies == nil {
			b.Tallies = map[string]int64{}
		}
		b.Tallies[jobmeet.TallySupersededDuringSend]++
	}
	if res.kind == resultWarned || res.kind == resultRefused {
		b.Failures = append(b.Failures, tenant.JobFailure{SOPInstanceUID: in.SOPInstanceUID, Code: res.code, PeerStatus: res.status,
			SOPClassUID: res.sopClass, TransferSyntaxUID: res.syntax, Detail: res.detail, Time: r.now()})
	}
	if r.count == 0 {
		r.first = time.Now()
	}
	r.count++
}

// add records res of in, the next instance of the unit's stream after
// those added before it.
func (r *recorder) add(in tenant.ScanInstance, res result) {
	if res.kind == resultOwed {
		r.batch.Owed = append(r.batch.Owed, in.SOPInstanceUID)
		if r.count == 0 {
			r.first = time.Now()
		}
		r.count++
	} else {
		r.tally(in, res)
	}
	r.to = in.SOPInstanceUID
}

// settle records res of in, an instance owed to the unit tried again by
// name: never owed again.
func (r *recorder) settle(in tenant.ScanInstance, res result) {
	if res.kind == resultOwed {
		res = refused(tenant.FailurePeerUnaccounted, 0, detailUnaccounted)
	}
	r.batch.Settled = append(r.batch.Settled, in.SOPInstanceUID)
	r.tally(in, res)
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
	return p
}
