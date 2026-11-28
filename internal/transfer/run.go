package transfer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons a job ends with, fixed phrases the contract's Job.reason may
// hold, none naming a value of a file.
const (
	reasonTargetGone    = "the target tenant is gone"
	reasonNoReceiver    = "the target has no enabled receiver of type job with that name"
	reasonSomeFailed    = "some instances were not copied; the source is kept"
	reasonSourceChanged = "the source changed while it was copied; it is kept"
	reasonHeld          = "a hold stops the delete; the copy is kept"
	reasonKept          = "a retention rule keeps the source; the copy is kept"
	reasonDeleteFailed  = "the delete of the source was refused; the copy is kept"
	reasonNothing       = "the target holds nothing to transfer"
	reasonWaited        = "the transfer waited on refusals that pass by themselves for longer than transfer.wait"
	reasonTargetChanged = "the target no longer holds what was copied; the source is kept"
	reasonPreviewDiffer = "the source's delete would cover other instances than were copied; the source is kept"
	reasonSourceGone    = "the source was deleted by something other than this job; the copy is confirmed"
)

// Runner runs the transfer jobs of one source tenant.
type Runner struct {
	cfg   Config
	log   *slog.Logger
	waits *Waiting
}

// New returns a runner over cfg.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Source == nil || cfg.Storages == nil || cfg.Target == nil || cfg.SourceID == "":
		return nil, fault.New(fault.Internal, "transfer: a runner needs its source, its storages and its targets")
	case cfg.Workers < 0:
		return nil, fault.New(fault.Internal, "transfer: workers are not negative")
	}
	if cfg.Workers == 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.ConfirmWait == 0 {
		cfg.ConfirmWait = DefaultConfirmWait
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	waits := cfg.Waiting
	if waits == nil {
		waits = NewWaiting()
	}
	return &Runner{cfg: cfg, log: log, waits: waits}, nil
}

// Result is what one pass did.
type Result struct {
	// Step is the step the job was at when the pass ended.
	Step tenant.TransferStep
	// Ended says the job ended (or had).
	Ended bool
	// Waiting says the job goes on at a later pass: the target did not take
	// a file now, a copy is not yet archived, a database or storage did not
	// answer.
	Waiting bool
	// Until is when a later pass may try again, zero for now.
	Until time.Time
}

func (r *Runner) now() time.Time { return r.cfg.Now().UTC().Truncate(time.Microsecond) }

// Run takes the job as far as it goes now, step by step, and returns when it
// ended, when what it waits for has not come, or when ctx ends. It may run
// again at any time and after any stop: every step reads the job's saved
// state first and repeats nothing a record shows done.
func (r *Runner) Run(ctx context.Context, jobID string) (Result, error) {
	tj, err := r.cfg.Source.TransferJob(ctx, jobID)
	if err != nil {
		return Result{}, err
	}
	for {
		res := Result{Step: tj.Step, Ended: tj.Job.State.Ended()}
		if res.Ended {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		stop, err := r.admit(ctx, tj)
		if err != nil || stop {
			res.Ended = stop
			return res, err
		}
		tgt, err := r.cfg.Target(ctx, tj.TargetTenant)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return r.end(ctx, tj, tenant.JobFailed, reasonTargetGone)
		case err != nil:
			return r.holdUp(ctx, tj, Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}, nil)
		}
		rcv, err := tgt.Receiver(ctx, tj.Receiver)
		switch {
		case fault.KindOf(err) == fault.NotFound, err == nil && (rcv.Type != tenant.ReceiverJob || !rcv.Enabled):
			return r.end(ctx, tj, tenant.JobFailed, reasonNoReceiver)
		case err != nil:
			return r.holdUp(ctx, tj, Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}, nil)
		}
		next, err := r.step(ctx, tj, tgt, rcv)
		if err != nil || next.Waiting {
			return r.holdUp(ctx, tj, next, err)
		}
		if next.Ended {
			r.waits.clear(jobID)
			return next, nil
		}
		r.waits.clear(jobID)
		if tj, err = r.cfg.Source.TransferJob(ctx, jobID); err != nil {
			return next, err
		}
	}
}

// admit checks again that the key that made the job may still run it, and
// ends the job failed when it may not: its work stops rather than go on in
// the name of a key revoked or demoted since. It reports whether the job
// ended.
func (r *Runner) admit(ctx context.Context, tj tenant.TransferJob) (bool, error) {
	if r.cfg.Authorize == nil || tj.Job.CreatedByKeyID == "" {
		return false, nil
	}
	err := r.cfg.Authorize(ctx, tj.Job.CreatedByKeyID, tj.TargetTenantID)
	var refused *correct.KeyRefused
	if !errors.As(err, &refused) {
		return false, err
	}
	reason := "stopped: the key that made the transfer may no longer run it (" + refused.Why + ")"
	if len(reason) > tenant.MaxFailureLength {
		reason = reason[:tenant.MaxFailureLength]
	}
	r.log.Warn("transfer: a job stopped: the key that made it may no longer run it", "job", tj.Job.ID, "key", tj.Job.CreatedByKeyID,
		"reason", refused.Why)
	_, err = r.cfg.Source.EndTransfer(ctx, tj.Job.ID, tenant.JobFailed, reason, r.now())
	return true, err
}

// end ends the job with the state and reason, and answers the pass.
func (r *Runner) end(ctx context.Context, tj tenant.TransferJob, state tenant.JobState, reason string) (Result, error) {
	_, err := r.cfg.Source.EndTransfer(ctx, tj.Job.ID, state, reason, r.now())
	return Result{Step: tenant.TransferEnded, Ended: err == nil}, err
}

// step runs the step the job is at.
func (r *Runner) step(ctx context.Context, tj tenant.TransferJob, tgt Target, rcv tenant.Receiver) (Result, error) {
	switch tj.Step {
	case tenant.TransferListing:
		if err := r.list(ctx, tj); err != nil {
			if fault.KindOf(err) == fault.NotFound {
				return r.end(ctx, tj, tenant.JobFailed, "the source no longer holds what the job names")
			}
			return Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}, err
		}
		if _, err := r.cfg.Source.StartTransferStep(ctx, tj.Job.ID, tenant.TransferCopying, r.now()); err != nil {
			return Result{Step: tj.Step}, err
		}
		return Result{Step: tenant.TransferCopying}, nil
	case tenant.TransferCopying:
		waiting, err := r.copyAll(ctx, tj, tgt, rcv)
		if err != nil || waiting {
			return Result{Step: tj.Step, Waiting: true, Until: r.now().Add(retryWait)}, err
		}
		if _, err := r.cfg.Source.StartTransferStep(ctx, tj.Job.ID, tenant.TransferConfirming, r.now()); err != nil {
			return Result{Step: tj.Step}, err
		}
		return Result{Step: tenant.TransferConfirming}, nil
	case tenant.TransferConfirming:
		return r.confirmAll(ctx, tj, tgt)
	case tenant.TransferDeleting:
		return r.deleteSource(ctx, tj, tgt)
	}
	return Result{Step: tj.Step, Ended: true}, nil
}

// afterConfirm decides what follows a confirming step that settled every
// copied item: the job ends done for a copy and for a move with nothing to
// delete, goes to the delete for a move whose every item is confirmed, and
// ends partial, source kept, when any instance failed.
func (r *Runner) afterConfirm(ctx context.Context, tj tenant.TransferJob) (Result, error) {
	cur, err := r.cfg.Source.TransferJob(ctx, tj.Job.ID)
	if err != nil {
		return Result{Step: tj.Step}, err
	}
	switch {
	case cur.Items.Failed > 0:
		return r.end(ctx, cur, tenant.JobPartial, reasonSomeFailed)
	case cur.Items.Total() == 0:
		return r.end(ctx, cur, tenant.JobDone, reasonNothing)
	case !cur.Move:
		return r.end(ctx, cur, tenant.JobDone, "")
	}
	if _, err := r.cfg.Source.StartTransferStep(ctx, tj.Job.ID, tenant.TransferDeleting, r.now()); err != nil {
		return Result{Step: tj.Step}, err
	}
	return Result{Step: tenant.TransferDeleting}, nil
}

// list reads the catalog's instances of the job's target into its items,
// scan by scan; it may run again over a job part listed, which adds only
// what is missing.
func (r *Runner) list(ctx context.Context, tj tenant.TransferJob) error {
	return EachScan(ctx, r.cfg.Source, tj.Job.Target, func(path label.Path, _ tenant.CatalogScan) error {
		return r.listScan(ctx, tj.Job.ID, path)
	})
}

// ScanLister is the catalog's reads of sessions and scans.
type ScanLister interface {
	tenant.SessionReads
	tenant.ScanReads
}

// EachScan calls fn with the labels and row of each scan of the subject or
// session t names, in the catalog's order. A subject or session the catalog
// does not hold is NotFound.
func EachScan(ctx context.Context, src ScanLister, t tenant.JobTarget, fn func(label.Path, tenant.CatalogScan) error) error {
	sessions := []string{t.SessionLabel}
	if t.SessionLabel == "" {
		sessions = sessions[:0]
		page := master.Page{Limit: master.MaxPageLimit}
		for {
			out, err := src.ListSessions(ctx, tenant.SessionFilter{SubjectLabel: t.SubjectLabel, Order: tenant.OrderLabel}, page)
			if err != nil {
				return err
			}
			for _, s := range out.Items {
				sessions = append(sessions, s.Label)
			}
			if out.Next == "" {
				break
			}
			page.Cursor = out.Next
		}
	}
	for _, session := range sessions {
		scans := master.Page{Limit: master.MaxPageLimit}
		for {
			out, err := src.ListScans(ctx, tenant.ScanFilter{SubjectLabel: t.SubjectLabel, SessionLabel: session, Order: tenant.OrderLabel}, scans)
			if err != nil {
				return err
			}
			for _, sc := range out.Items {
				if err := fn(label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}, sc); err != nil {
					return err
				}
			}
			if out.Next == "" {
				break
			}
			scans.Cursor = out.Next
		}
	}
	return nil
}

// listScan adds the instances of one scan to the job's items.
func (r *Runner) listScan(ctx context.Context, jobID string, path label.Path) error {
	page := master.Page{Limit: tenant.MaxTransferItemBatch}
	for {
		held, next, err := r.cfg.Source.ListHeldInstances(ctx, path, page)
		if err != nil {
			return err
		}
		items := make([]tenant.TransferNewItem, len(held))
		for i, in := range held {
			items[i] = tenant.TransferNewItem{Path: path, SOPInstanceUID: in.SOPInstanceUID, Size: in.Size, SHA256: in.ArchivedSHA256,
				AsReceived: in.Profile.IsZero()}
		}
		if _, err := r.cfg.Source.AddTransferItems(ctx, jobID, items); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// holdUp answers a pass that did not go on, for refusals that pass by
// themselves or an error: the job keeps waiting, until it has waited longer
// than transfer.wait, when it ends failed so its subject is free again. The
// end of ctx is not a wait.
func (r *Runner) holdUp(ctx context.Context, tj tenant.TransferJob, res Result, err error) (Result, error) {
	if ctx.Err() != nil {
		return res, err
	}
	wait := DefaultWait
	if r.cfg.Wait != nil {
		if w, werr := r.cfg.Wait(ctx); werr == nil && w > 0 {
			wait = w
		}
	}
	since := r.waits.mark(tj.Job.ID, r.now())
	if r.now().Sub(since) > wait {
		r.waits.clear(tj.Job.ID)
		return r.end(ctx, tj, tenant.JobFailed, reasonWaited)
	}
	return res, err
}
