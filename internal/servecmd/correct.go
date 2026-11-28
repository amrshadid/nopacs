package servecmd

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CorrectionPassInterval is how often the corrections' pass runs
// (correct.Engines): each tenant's Recover after a start, then the purges
// whose grace period has ended. A minute: a grace ends at most a minute
// late, at the cost of a few reads of each tenant's database a minute.
const CorrectionPassInterval = correct.DefaultInterval

// tenantScanLocks holds each tenant's scan locks (receive.ScanLocks), made
// when first asked for and kept for the server's life: every pipeline of
// the tenant, one after another, and its corrections engine take the same
// lock of a scan, so a correction rewriting a scan's manifest across a
// pipeline's restart still keeps the next pipeline from it.
type tenantScanLocks struct {
	mu    sync.Mutex
	locks map[string]*receive.ScanLocks
}

// of returns the scan locks of the tenant named name.
func (t *tenantScanLocks) of(name string) *receive.ScanLocks {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = map[string]*receive.ScanLocks{}
	}
	l := t.locks[name]
	if l == nil {
		l = &receive.ScanLocks{}
		t.locks[name] = l
	}
	return l
}

// runningPipelines holds each tenant's running reception pipeline, which a
// replace makes its file through: published once it has started and
// withdrawn before it stops.
type runningPipelines struct {
	mu    sync.Mutex
	pipes map[string]*receive.Pipeline
}

// set publishes p as the running pipeline of the tenant named name.
func (r *runningPipelines) set(name string, p *receive.Pipeline) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pipes == nil {
		r.pipes = map[string]*receive.Pipeline{}
	}
	r.pipes[name] = p
}

// drop withdraws p, when it is still the tenant's published pipeline.
func (r *runningPipelines) drop(name string, p *receive.Pipeline) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pipes[name] == p {
		delete(r.pipes, name)
	}
}

// get returns the tenant's running pipeline, and whether one runs.
func (r *runningPipelines) get(name string) (*receive.Pipeline, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pipes[name]
	return p, ok
}

// ReplaceSource makes a replace's file through the tenant's running
// pipeline (api.ReplaceSources); with none running it is unavailable, as a
// release is.
func (r *runningPipelines) ReplaceSource(ctx context.Context, tenantName string, q tenant.QuarantinedFile) (receive.ReplaceFile, error) {
	p, ok := r.get(tenantName)
	if !ok {
		return receive.ReplaceFile{}, errNoPipeline
	}
	return p.ReplaceSource(ctx, q)
}

var _ api.ReplaceSources = (*runningPipelines)(nil)

// newCorrections returns every tenant's corrections engine, each made when
// first asked for: its storages opened for writing by the same rules as
// the pipeline's (a delete's records, a relabel's copies, a replace's new
// revision, a purge), its delete.grace read live from the tenant's
// settings, its preview tokens keyed with a key derived from the
// installation key for that use alone (secret.ConfirmToken), so only this
// installation makes a token a delete takes, its correction records
// authenticated for the tenant's id with the installation keys
// (secret.CorrectionRecord), so the readers of a storage tell them from a
// record anyone else wrote there, its scans locked with the
// tenant's pipelines' locks, and the incoming bytes a purge clears handed
// to the tenant's pipeline, which removes them as it removes a rejected
// file's (and keeps the removal while none runs). Its storages are opened
// to write with cleanups, the server's one, shared with the pipelines.
//
// While a switch of any of the tenant's storages runs, a correction's next
// step that writes to a storage is refused as the switch gate refuses a
// write (switchingFence), and a purge already running asks the gate before
// each removal (correct.Config.Pause). Each new revision an edit writes is
// judged first by the tenant's profile (editJudge), within memory, the
// server's one bound on de-identification's memory.
func newCorrections(d Deps, reg *registry.Registry, sup *supervise.Supervisor, locks *tenantScanLocks, cleanups *place.Cleanups,
	work *tenantWork, memory check.Memory) (*correct.Engines, error) {
	key, err := d.Keys.Derive(secret.ConfirmToken)
	if err != nil {
		return nil, err
	}
	values := settingvalue.Reader{Master: d.Master}
	// The engine's stores and the pipelines' share the server's cleanups,
	// so whichever opens a storage first removes what a crash left there
	// and none removes another's work in progress (writingOpener).
	opener := writingOpener(values, d.Keys, cleanups)
	return correct.NewEngines(correct.EnginesConfig{
		Tenants:  registryTenants{master: d.Master, registry: reg},
		Interval: CorrectionPassInterval,
		Log:      d.Log,
		// No purge of a tenant runs until the holds found on its storages
		// are adopted since this start and the catalog's last activation.
		HoldsAdopted: (&purgeAdoptions{work: work}).check,
		Configure: func(ctx context.Context, name string, cs tenant.Corrections) (correct.Config, func() error, error) {
			p, ok := cs.(tenant.Pipeline)
			if !ok {
				return correct.Config{}, nil, fault.New(fault.Internal, "the tenant's database keeps no catalog")
			}
			src, ok := cs.(settingvalue.Source)
			if !ok {
				return correct.Config{}, nil, fault.New(fault.Internal, "the tenant's database keeps no settings")
			}
			full, err := dbOf(cs)
			if err != nil {
				return correct.Config{}, nil, err
			}
			row, err := d.Master.TenantByName(ctx, name)
			if err != nil {
				return correct.Config{}, nil, err
			}
			// The owner records are read again before each step that writes
			// to a storage, so a tenant another installation took over since
			// the registry's last check, or one handed off, writes nothing
			// more; and while a switch runs, a step is refused as a write to
			// its primary is, and a purge running asks the gate.
			fence, pause, err := work.correctionWrites(ctx, name, full, func() error { return reg.CheckFence(name) })
			if err != nil {
				return correct.Config{}, nil, err
			}
			judge, err := editJudge(ctx, values, src, p, memory)
			if err != nil {
				return correct.Config{}, nil, err
			}
			storages := newTenantStorages(opener, d.Master, name, p)
			return correct.Config{
				Judge:       judge,
				TenantID:    row.ID,
				Keys:        d.Keys,
				OpenStorage: storages.Open,
				Now:         clock.Now,
				Grace:       values.OnTenant(src).DeleteGrace,
				ConfirmKey:  key,
				LockScan:    locks.of(name).Lock,
				RemoveIncoming: func(ctx context.Context, correctionID string, paths []string) error {
					for _, path := range paths {
						if err := sup.RemoveWithdrawn(ctx, name, correctionID, path); err != nil {
							return err
						}
					}
					return nil
				},
				Fence: fence,
				Pause: pause,
				// A relabel's publish owes the records of the holds it moved
				// under their new labels: written at once, not at the
				// hold-record worker's next pass.
				Published: func(ctx context.Context, c tenant.Correction) {
					if c.Kind == record.KindRelabel {
						work.WriteHoldsOwed(ctx, name)
					}
				},
			}, storages.Close, nil
		},
	})
}
