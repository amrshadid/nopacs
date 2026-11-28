package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pace of batch deletes.
const (
	// batchInterval is how often a tenant's batch delete jobs not ended are
	// looked for and worked: a job confirmed starts within it, and one
	// resumed goes on within it.
	batchInterval = 5 * time.Second
	// batchMethod is the method of the audit entry of each delete a batch
	// makes, written as the server's in the name of the key that confirmed
	// the batch (master.AuditEntry.Method).
	batchMethod = "correction.delete_batch"
)

// tenantBatches runs one tenant's batch delete jobs: every batchInterval,
// each job not ended and not paused has its targets not yet met deleted,
// one at a time, through the ordinary delete of the corrections engine
// (correct.Engine.RunBatch), never a second path. The state is the
// database's, so a restart goes on where the last pass stopped.
type tenantBatches struct {
	w    *tenantWork
	name string
	// st is the tenant's database as the background work writes it, each
	// write fenced; base is the database as the registry answered it, by
	// which the corrections engines know the tenant's engine. Nil base is
	// st.
	st   tenantDB
	base tenantDB
	log  *slog.Logger
}

// run works the batches every batchInterval until ctx ends.
func (b *tenantBatches) run(ctx context.Context) {
	t := time.NewTicker(batchInterval)
	defer t.Stop()
	for {
		if err := b.once(ctx); err != nil && ctx.Err() == nil {
			b.log.Warn("batch deletes: a pass could not finish; it goes on at the next", "kind", fault.KindOf(err).String(),
				"reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// once works every batch delete job not ended.
func (b *tenantBatches) once(ctx context.Context) error {
	es := b.w.engines.Load()
	if es == nil {
		return nil
	}
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		jobs, next, err := b.st.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Kind == tenant.JobDeleteBatch {
				errs = append(errs, b.work(ctx, es, j))
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return errors.Join(errs...)
}

// work runs one pass of a batch job on the tenant's engine.
func (b *tenantBatches) work(ctx context.Context, es *correct.Engines, j tenant.Job) error {
	guard, err := b.w.keyGuard(ctx, b.name, j.CreatedByKeyID)
	if err != nil {
		return err
	}
	base := b.base
	if base == nil {
		base = b.st
	}
	e, release, err := es.Use(ctx, b.name, base)
	if err != nil {
		return err
	}
	defer release()
	res, err := e.RunBatch(ctx, correct.BatchRun{Store: b.st, JobID: j.ID, Guard: guard,
		Audit: func(t tenant.CorrectionTarget, now time.Time) master.AuditEntry {
			// The server's entry: the key that confirmed the batch is named
			// in its details and on the delete itself (Correction.KeyID),
			// never as the entry's caller, which the server is.
			a := serverAudit(b.name, batchMethod, j.ID, now)
			a.Details = map[string]string{"job": j.ID, "target": strings.Join(nonEmpty(t.Subject, t.Session, t.Scan), "/")}
			if j.CreatedByKeyID != "" {
				a.Details["key"] = j.CreatedByKeyID
			}
			return a
		}})
	if res.Ended {
		b.log.Info("batch deletes: a batch ended", "job", j.ID)
	}
	return err
}

// nonEmpty returns the strings given that are not empty, in order.
func nonEmpty(s ...string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// keyGuard returns the check a change the server makes in the name of a
// key makes, in its own transaction: that the key still exists, is not
// revoked, expired or suspended, and is admin on the tenant
// (master.Store.CheckActor). A refusal is answered as a
// *correct.KeyRefused with its fixed reason. A job's deletes run for as long
// as it lists targets, and must not go on in the name of a key revoked or
// demoted since. The local operator's (no key) needs none: nil.
func (w *tenantWork) keyGuard(ctx context.Context, name, keyID string) (tenant.Guard, error) {
	if keyID == "" {
		return nil, nil
	}
	tenantID, err := w.tenantID(ctx, name)
	if err != nil {
		return nil, err
	}
	return w.roleGuard(tenantID, keyID, master.RoleAdmin), nil
}
