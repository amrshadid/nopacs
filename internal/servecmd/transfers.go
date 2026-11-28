package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/transfer"
)

// The pace of transfers.
const (
	// transferInterval is how often a tenant's transfer jobs not ended are
	// looked for and taken a step: a job made starts within it, and one
	// waiting for the target goes on within it.
	transferInterval = 5 * time.Second
	// transferDeleteMethod is the method of the audit entry of the delete a
	// move makes of its source, written as the server's in the name of the
	// key that made the transfer.
	transferDeleteMethod = "transfer.delete_source"
)

// transferDeps are what the server's transfers share, set once the reception
// and the reads they use exist.
type transferDeps struct {
	// pipes finds a tenant's running reception pipeline, which a transfer
	// hands its files to.
	pipes *runningPipelines
	// reads bounds the reads of archived bytes open at once, with the API's
	// and the senders'; counts counts those a replica served.
	reads  *checkedread.Slots
	counts *checkedread.Counts
	values settingvalue.Reader
	d      Deps
}

// setTransferDeps gives the work what the transfer jobs need; until then
// they wait.
func (w *tenantWork) setTransferDeps(pipes *runningPipelines, reads *checkedread.Slots, counts *checkedread.Counts) {
	w.transfers.Store(&transferDeps{pipes: pipes, reads: reads, counts: counts, values: w.values, d: w.d})
}

// tenantTransfers runs one tenant's transfer jobs: every transferInterval,
// each job not ended is taken as far as it goes (transfer.Runner.Run). The
// state is the database's, so a restart goes on where the last pass stopped.
type tenantTransfers struct {
	w    *tenantWork
	name string
	// st is the tenant's database as the background work writes it, each
	// write fenced; base is the database as the registry answered it, by
	// which the corrections engines know the tenant's engine.
	st   tenantDB
	base tenantDB
	log  *slog.Logger
	// waits remembers, across passes, since when each job has waited.
	waits *transfer.Waiting
}

// run works the transfers every transferInterval until ctx ends.
func (t *tenantTransfers) run(ctx context.Context) {
	tick := time.NewTicker(transferInterval)
	defer tick.Stop()
	for {
		if err := t.once(ctx); err != nil && ctx.Err() == nil {
			t.log.Warn("transfers: a pass could not finish; it goes on at the next", "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once takes every transfer job not ended a step.
func (t *tenantTransfers) once(ctx context.Context) error {
	deps := t.w.transfers.Load()
	if deps == nil || t.w.engines.Load() == nil {
		return nil
	}
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		jobs, next, err := t.st.ListTransfers(ctx, page)
		if err != nil {
			return err
		}
		for _, tj := range jobs {
			if !tj.Job.State.Ended() {
				errs = append(errs, t.work(ctx, deps, tj))
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return errors.Join(errs...)
}

// work runs one pass of a transfer job.
func (t *tenantTransfers) work(ctx context.Context, deps *transferDeps, tj tenant.TransferJob) error {
	w := t.w
	id, err := w.tenantID(ctx, t.name)
	if err != nil {
		return err
	}
	tenantID := func(ctx context.Context, name string) (string, error) {
		row, err := deps.d.Master.TenantByName(ctx, name)
		return row.ID, err
	}
	opener := place.Opener{Values: deps.values, Keys: deps.d.Keys, StaleUploadAge: -1}
	if t.waits == nil {
		t.waits = transfer.NewWaiting()
	}
	runner, err := transfer.New(transfer.Config{
		Wait: deps.values.OnTenant(t.st).TransferWait, Waiting: t.waits,
		Source: t.st, SourceID: id,
		Storages: func(context.Context) (transfer.Storages, func(), error) {
			s := checkedread.NewStores(opener, tenantID, t.st, t.name).WithCounts(deps.counts)
			return s, s.Close, nil
		},
		Target: func(_ context.Context, name string) (transfer.Target, error) { return t.target(deps, name) },
		Deleter: func(ctx context.Context) (transfer.Deleter, func(), error) {
			es := w.engines.Load()
			if es == nil {
				return nil, nil, fault.New(fault.Unavailable, "the server runs no corrections yet")
			}
			base := t.base
			if base == nil {
				base = t.st
			}
			return es.Use(ctx, t.name, base)
		},
		Authorize: func(ctx context.Context, keyID, targetTenantID string) error {
			for _, c := range []struct {
				tenantID string
				role     master.Role
			}{{id, master.RoleAdmin}, {targetTenantID, master.RoleWriter}} {
				if err := w.roleGuard(c.tenantID, keyID, c.role)(ctx); err != nil {
					return err
				}
			}
			return nil
		},
		DeleteGuard: func(ctx context.Context, keyID string) (tenant.Guard, error) { return w.keyGuard(ctx, t.name, keyID) },
		DeleteAudit: func(j tenant.TransferJob, now time.Time) master.AuditEntry {
			// The server's entry: the key that made the transfer is named in
			// its details and on the delete itself, never as the entry's
			// caller, which the server is.
			a := serverAudit(t.name, transferDeleteMethod, j.Job.ID, now)
			a.Details = map[string]string{"job": j.Job.ID, "target_tenant": j.TargetTenant}
			if j.Job.CreatedByKeyID != "" {
				a.Details["key"] = j.Job.CreatedByKeyID
			}
			return a
		},
		Reads: deps.reads, Now: clock.Now, Log: t.log,
	})
	if err != nil {
		return err
	}
	res, err := runner.Run(ctx, tj.Job.ID)
	if res.Ended {
		t.log.Info("transfers: a transfer ended", "job", tj.Job.ID)
	}
	return err
}

// transferTarget is the tenant a transfer copies into: its database, as the
// background work writes it, and its running reception pipeline.
type transferTarget struct {
	tenantDB
	p *receive.Pipeline
}

// AcceptTransfer hands the file to the target's receiver of type job.
func (t transferTarget) AcceptTransfer(ctx context.Context, a receive.Arrival) receive.Outcome {
	return t.p.AcceptTransfer(ctx, a)
}

// TransferRow answers the row the target's catalog will hold for the file.
func (t transferTarget) TransferRow(ctx context.Context, receiver, uid string, sum tenant.SHA256) (receive.TransferRow, error) {
	return t.p.TransferRow(ctx, receiver, uid, sum)
}

// target returns the tenant named name as a transfer copies into it: its
// store as the registry answers a writer (a moved, fenced or unready tenant
// is Unavailable or a precondition failure, and the job waits) and its
// running pipeline.
func (t *tenantTransfers) target(deps *transferDeps, name string) (transfer.Target, error) {
	raw, err := t.w.tenants.Store(name)
	if err != nil {
		return nil, err
	}
	db, err := dbOf(raw)
	if err != nil {
		return nil, err
	}
	p, ok := deps.pipes.get(name)
	if !ok {
		return nil, fault.New(fault.Unavailable, "the target tenant's reception pipeline is not running")
	}
	return transferTarget{tenantDB: fenceDB(db, t.w.fenceOf(name)), p: p}, nil
}

// roleGuard returns the check, in the name of the key, that it still exists,
// is not revoked, expired or suspended, and holds role on the tenant of that
// id (master.Store.CheckActor); a refusal is answered as a
// *correct.KeyRefused with its fixed reason, which a job's runner ends the
// job with. The local operator's (no key) needs none: it passes.
func (w *tenantWork) roleGuard(tenantID, keyID string, role master.Role) tenant.Guard {
	if keyID == "" {
		return func(context.Context) error { return nil }
	}
	by := master.ActingKey(keyID, master.TenantScope(tenantID), role, w.now)
	return func(ctx context.Context) error {
		err := w.d.Master.CheckActor(ctx, by)
		if ref, ok := master.RefusalOf(err); ok {
			return &correct.KeyRefused{Why: ref.Reason, Err: err}
		}
		if err != nil {
			return err
		}
		// A suspended key is refused by the access check, not by
		// CheckActor: a restore suspends every key it brings back until the
		// local operator reinstates them.
		k, err := w.d.Master.Key(ctx, keyID)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return &correct.KeyRefused{Why: master.RefusedUnknownKey, Err: err}
		case err != nil:
			return err
		case !k.Suspended.IsZero():
			return &correct.KeyRefused{Why: "key suspended", Err: fault.New(fault.Unauthenticated, "the acting key is suspended")}
		}
		return nil
	}
}
