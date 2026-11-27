package correct

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// BatchRun is one pass of a batch delete job's runner (Engine.RunBatch).
type BatchRun struct {
	// Store is the tenant's batch deletes.
	Store tenant.DeleteBatches
	// JobID names the job.
	JobID string
	// Guard is the check each delete makes, in its own transaction, of the
	// key that made the job; nil for none. A refusal it answers as a
	// *KeyRefused stops the job (tenant.DeleteBatches.StopDeleteBatchJob).
	Guard tenant.Guard
	// Audit returns the audit entry of the delete of target at now.
	Audit func(target tenant.CorrectionTarget, now time.Time) master.AuditEntry
	// Met, when not nil, is told each target's outcome as it is recorded.
	Met func(t tenant.DeleteBatchTarget, outcome tenant.BatchOutcome)
}

// BatchResult is what one pass did.
type BatchResult struct {
	// Met counts the targets whose outcome the pass recorded, by outcome.
	Met map[tenant.BatchOutcome]int
	// Waiting counts the targets left for a later pass: a delete of one is
	// still prepared, or a database or storage did not answer.
	Waiting int
	// Stopped says the pass stopped at a pause or a cancel; Ended that the
	// job ended (or had).
	Stopped, Ended bool
	// Until is, for a pass that met a failure that may pass by itself or
	// that waited for one before (BackingOff), when the job's next pass may
	// try again; zero otherwise.
	Until time.Time
	// BackingOff says the pass tried nothing: the job's last failure is
	// more recent than its wait (batchBackoff).
	BackingOff bool
}

// KeyRefused is the refusal, in a batch's delete, of the key that made the
// job: revoked, expired, suspended, gone, or no longer an admin of the
// tenant. A Guard answers it; RunBatch stops the job with its reason.
type KeyRefused struct {
	// Why is the refusal's fixed reason (master's).
	Why string
	// Err is the refusal itself.
	Err error
}

// Error implements error.
func (k *KeyRefused) Error() string {
	return "the key that made the batch may no longer delete: " + k.Why
}

// Unwrap answers the refusal itself.
func (k *KeyRefused) Unwrap() error { return k.Err }

// batchPage is how many targets a pass reads at a time.
const batchPage = 500

// errBatchWaiting is a target whose delete is still prepared: the engine's
// start, or the request that made it, settles it, and a later pass meets
// the target again.
var errBatchWaiting = errors.New("correct: the target's delete is still prepared")

// The pace of a batch whose deletes fail: the wait after the first
// failure, doubled after each next one up to the most, and forgotten once a
// target is met.
const (
	batchFirstWait = 5 * time.Second
	batchMostWait  = 10 * time.Minute
)

// batchWait is when a batch's runner may try again, and the wait that set
// it.
type batchWait struct {
	until time.Time
	wait  time.Duration
}

// batchBackoff answers when the job's runner may next try a delete, zero
// when it may now.
func (e *Engine) batchBackoff(job string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.batchWaits[job]
	if !ok || !e.now().Before(w.until) {
		return time.Time{}
	}
	return w.until
}

// batchFailed doubles the job's wait (batchFirstWait at the first failure,
// at most batchMostWait) from now, and answers when it ends: a storage that
// keeps refusing would otherwise make a new abandoned delete every pass,
// without bound.
func (e *Engine) batchFailed(job string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.batchWaits == nil {
		e.batchWaits = map[string]batchWait{}
	}
	w := e.batchWaits[job]
	w.wait = min(max(2*w.wait, batchFirstWait), batchMostWait)
	w.until = e.now().Add(w.wait)
	e.batchWaits[job] = w
	return w.until
}

// batchMet forgets the job's wait: a target was met.
func (e *Engine) batchMet(job string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.batchWaits, job)
}

// RunBatch works the targets of a batch delete job not yet met, in order,
// each through the ordinary delete (Delete) with the job's reason, and
// records what became of each; once every target has its outcome it ends
// the job. The job is read again before each target, and the pass stops at
// a pause or a cancel; a delete's prepare also refuses a job paused or
// ended since, in its own transaction. A target is never forced: a refusal
// that does not pass by itself is a skip with its fixed reason. A target
// whose delete was made but whose outcome was not recorded (a stop between
// the two) is recorded deleted by that delete, never deleted twice. A
// temporary failure leaves the target for the next pass, which the caller
// runs again later; until the job's wait after it has passed, doubling with
// each failure in a row, a pass tries nothing (BatchResult.BackingOff). A
// target another runner of the job prepared a delete of, or met, in the
// meantime is left for the next pass, never recorded as a skip.
func (e *Engine) RunBatch(ctx context.Context, r BatchRun) (BatchResult, error) {
	res := BatchResult{Met: map[tenant.BatchOutcome]int{}}
	if until := e.batchBackoff(r.JobID); !until.IsZero() {
		res.BackingOff, res.Until = true, until
		return res, nil
	}
	after := 0
	for {
		page, err := r.Store.DeleteBatchTargets(ctx, r.JobID, after, batchPage)
		if err != nil {
			return res, err
		}
		for _, t := range page {
			after = t.Ord
			if t.Outcome != tenant.BatchPending {
				continue
			}
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			bj, err := r.Store.DeleteBatchJob(ctx, r.JobID)
			if err != nil {
				return res, err
			}
			if bj.Job.State.Ended() {
				res.Ended = true
				return res, nil
			}
			if bj.Paused {
				res.Stopped = true
				return res, nil
			}
			outcome, id, err := e.meetBatchTarget(ctx, r, bj, t)
			var refused *KeyRefused
			switch {
			case errors.As(err, &refused):
				_, serr := r.Store.StopDeleteBatchJob(ctx, r.JobID, "stopped: the key that made the batch may no longer delete ("+refused.Why+")", e.now())
				e.cfg.Log.Warn("corrections: a batch delete stopped: the key that made it may no longer delete", "job", r.JobID,
					"key", bj.Job.CreatedByKeyID, "reason", refused.Why)
				res.Stopped = true
				return res, serr
			case errors.Is(err, tenant.ErrBatchStopped):
				res.Stopped = true
				return res, nil
			case errors.Is(err, errBatchWaiting), errors.Is(err, tenant.ErrBatchTargetTaken):
				res.Waiting++
				continue
			case err != nil:
				// A database or a storage that does not answer would most
				// likely refuse the next target too: the pass ends, and the
				// next one goes on from this target once the job's wait has
				// passed.
				res.Waiting++
				res.Until = e.batchFailed(r.JobID)
				return res, err
			}
			_, err = r.Store.RecordBatchTarget(ctx, tenant.BatchRecord{JobID: r.JobID, Ord: t.Ord, Outcome: outcome, Correction: id, Now: e.now()})
			switch {
			case errors.Is(err, tenant.ErrBatchStopped):
				res.Stopped = true
				return res, nil
			case errors.Is(err, tenant.ErrBatchTargetTaken):
				res.Waiting++
				continue
			case err != nil:
				res.Waiting++
				res.Until = e.batchFailed(r.JobID)
				return res, err
			}
			e.batchMet(r.JobID)
			res.Met[outcome]++
			if r.Met != nil {
				r.Met(t, outcome)
			}
		}
		if len(page) < batchPage {
			break
		}
	}
	if res.Waiting > 0 {
		return res, nil
	}
	if _, err := r.Store.EndDeleteBatchJob(ctx, r.JobID, e.now()); err != nil {
		return res, err
	}
	res.Ended = true
	return res, nil
}

// meetBatchTarget deletes one target of a batch, or says why not: a delete
// its prepare already recorded on it is the outcome when it was published
// (whatever became of it since, an undo included); one still prepared
// waits; one abandoned is made again. Otherwise the target is previewed:
// a hold or a rule's keep-at-least skips it with that reason, a preview
// that answers otherwise than when the job was made skips it as changed,
// and the delete carries the preview's token, so its prepare checks the
// same again in its own transaction.
func (e *Engine) meetBatchTarget(ctx context.Context, r BatchRun, bj tenant.DeleteBatchJob, t tenant.DeleteBatchTarget) (tenant.BatchOutcome,
	string, error) {
	if t.Correction != "" {
		c, err := e.cfg.Store.Correction(ctx, t.Correction)
		if err != nil {
			return "", "", err
		}
		switch c.State {
		case record.StatePrepared:
			return "", "", errBatchWaiting
		case record.StateAbandoned:
		default:
			return tenant.BatchDeleted, c.ID, nil
		}
	}
	p, err := e.Preview(ctx, t.Target)
	if err != nil {
		return e.batchSkipOf(ctx, r.JobID, err)
	}
	switch {
	case len(p.Holds) > 0:
		return tenant.BatchSkippedHeld, "", nil
	case p.SessionsUnderMinimum > 0:
		return tenant.BatchSkippedKept, "", nil
	case tenant.DeleteFingerprint(p.DeletePreview) != t.Fingerprint:
		return tenant.BatchSkippedChanged, "", nil
	}
	now := e.now()
	c, err := e.Delete(ctx, DeleteRequest{Target: t.Target, Reason: bj.Reason, Confirm: p.Confirm, KeyID: bj.Job.CreatedByKeyID,
		Batch: &tenant.BatchOrigin{JobID: r.JobID, Ord: t.Ord}, Audit: r.Audit(t.Target, now), Guard: r.Guard})
	var refused *KeyRefused
	switch {
	case errors.As(err, &refused):
		return "", "", err
	case err != nil && c.ID != "":
		// Prepared, then abandoned (a storage that did not take its record):
		// the target is met again at the next pass.
		return "", "", err
	case err != nil:
		return e.batchSkipOf(ctx, r.JobID, err)
	}
	return tenant.BatchDeleted, c.ID, nil
}

// batchSkipOf answers what a refusal of a target's preview or delete makes
// of the target: the skip for its reason, or the error itself when it may
// pass by itself (a database, storage or engine not answering, the call's
// own end) and when the job was stopped (the caller stops). A refusal of no
// known reason is logged with its message, which the skip's fixed reason
// does not keep.
func (e *Engine) batchSkipOf(ctx context.Context, job string, err error) (tenant.BatchOutcome, string, error) {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, tenant.ErrBatchStopped),
		errors.Is(err, tenant.ErrBatchTargetTaken):
		return "", "", err
	case errors.Is(err, tenant.ErrHeld):
		return tenant.BatchSkippedHeld, "", nil
	case errors.Is(err, tenant.ErrRetentionMinimum):
		return tenant.BatchSkippedKept, "", nil
	case errors.Is(err, tenant.ErrOverlap):
		return tenant.BatchSkippedOverlap, "", nil
	case errors.Is(err, errTokenMismatch):
		return tenant.BatchSkippedChanged, "", nil
	}
	switch fault.KindOf(err) {
	case fault.NotFound:
		return tenant.BatchSkippedGone, "", nil
	case fault.Unavailable, fault.Exhausted, fault.Internal:
		return "", "", err
	}
	e.cfg.Log.Warn("corrections: a batch target's delete was refused; the batch skips it", "job", job, "kind", fault.KindOf(err).String(),
		"reason", fault.Message(err))
	return tenant.BatchSkippedRefused, "", nil
}
