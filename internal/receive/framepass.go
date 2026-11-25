package receive

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// framePassRetry is how long the pass of frame tables waits after a pass
// that stopped on a failure (a storage or the database not answering)
// before it passes again from the first scan.
const framePassRetry = time.Minute

// framePass gives the metadata sidecars written before frame tables
// existed their tables, and the scans archived before the catalog kept
// their header values those values (frames.Run), once a start: every
// sidecar written after it starts has its tables already, and every scan
// committed after it its values, because the archive step writes them, so
// one pass that ends leaves none to do. A pass that stops on a
// failure is run again after framePassRetry, and the next start runs one
// again, which finds the scans done by reading one small range of each.
// When it has ended the sidecars of the form before the scan's header and
// the catalog's values are rewritten from the catalog (upgradePass), in the
// same goroutine, so a sidecar has its tables before it is rewritten.
// It runs while the workers run and ends with them.
func (p *Pipeline) framePass() {
	defer p.wg.Done()
	ctx := p.workCtx
	p.frameTables(ctx)
	if ctx.Err() == nil {
		p.upgradePass(ctx)
	}
}

// frameTables runs the pass of frame tables until it ends without a failure
// or the pipeline stops.
func (p *Pipeline) frameTables(ctx context.Context) {
	cfg := frames.Config{
		Catalog: p.store,
		OpenStorage: func(ctx context.Context, name string) (blob.Store, error) {
			return p.cfg.OpenStorage(ctx, name)
		},
		Enter:       p.enterWrite,
		Lock:        p.passLock,
		HeaderLimit: p.headerLimit,
		Busy:        p.busy,
		Wake:        p.cfg.Wake,
		Clock:       p.cfg.Clock,
		Logger:      p.log,
	}
	if ev, ok := p.store.(tenant.Events); ok {
		cfg.Events = ev
	}
	for ctx.Err() == nil {
		rep, err := frames.Run(ctx, cfg)
		if rep.ScansDone > 0 {
			p.log.Info("frame tables were added to scans archived before them", "scans", rep.ScansDone,
				"instances", rep.InstancesDone, "checked", rep.ScansChecked)
		}
		if rep.ScansFilled > 0 {
			p.log.Info("the catalog's header values were filled in for scans archived before it kept them", "scans", rep.ScansFilled)
		}
		if err == nil {
			return
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		p.log.Warn("the pass of frame tables stopped; it passes again later", "reason", fault.Message(err))
		select {
		case <-ctx.Done():
			return
		case <-p.cfg.Clock.After(framePassRetry):
		}
	}
}

// enterWrite asks the pipeline's write gate (Config.Pause) before a pass
// rewrites a scan's sidecar on storage, as the archive step asks it before
// it writes: a switch of the storage to its replica then waits for the
// rewrite, or the rewrite for the switch. No gate admits every write.
func (p *Pipeline) enterWrite(ctx context.Context, storage string) (func(), error) {
	if p.cfg.Pause == nil {
		return func() {}, nil
	}
	return p.cfg.Pause.Enter(ctx, storage)
}

// passLock takes the lock the archive step holds while it writes the scan's
// metadata and manifest on a storage, and returns its release.
func (p *Pipeline) passLock(storage string, path label.Path) func() {
	key, err := layout.ManifestKey(path)
	if err != nil {
		// A scan the catalog lists has labels that make keys; the lock is
		// then still a lock of its own, by the labels.
		key = path.Subject + "/" + path.Session + "/" + path.Scan
	}
	return p.lockScan(storage, key)
}

// upgradePass rewrites the sidecars of the form before the scan's header and
// the catalog's values (version 2) as version 3 from the catalog's rows
// (frames.Upgrade), once a start: the archive step writes version 3 for every
// scan it makes after it starts, and for one it merges into a version 3
// sidecar, so a pass that ends leaves none to do. A scan whose sidecar the
// catalog does not agree with is left as it is and reported in the log. A
// pass that stops on a failure is run again after framePassRetry, from the
// first scan, which finds the scans done by reading the first bytes of
// each, and the next start runs one again.
func (p *Pipeline) upgradePass(ctx context.Context) {
	cfg := frames.UpgradeConfig{
		Catalog: p.store,
		OpenStorage: func(ctx context.Context, name string) (blob.Store, error) {
			return p.cfg.OpenStorage(ctx, name)
		},
		Enter:  p.enterWrite,
		Lock:   p.passLock,
		Busy:   p.busy,
		Clock:  p.cfg.Clock,
		Logger: p.log,
	}
	for ctx.Err() == nil {
		rep, err := frames.Upgrade(ctx, cfg)
		if rep.ScansDone > 0 {
			p.log.Info("scans archived before the catalog's values were kept in their metadata were rewritten with them",
				"scans", rep.ScansDone, "instances", rep.InstancesDone, "checked", rep.ScansChecked)
		}
		if rep.ScansLeft > 0 || rep.ScansWaiting > 0 {
			p.log.Warn("some scans' metadata was left as it is", "disagreeing", rep.ScansLeft, "waiting for frame tables", rep.ScansWaiting)
		}
		if err == nil {
			return
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		p.log.Warn("the rewrite of older metadata stopped; it passes again later", "reason", fault.Message(err))
		select {
		case <-ctx.Done():
			return
		case <-p.cfg.Clock.After(framePassRetry):
		}
	}
}

// busy reports whether reception is at work: a step in flight, or a file
// landed since the last time it was asked.
func (p *Pipeline) busy() bool {
	landed := p.stats.Landed.Load()
	moved := p.landedSeen.Swap(landed) != landed
	return moved || p.stepping.Load() > 0
}
