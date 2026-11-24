package receive

import (
	"context"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Start reconciles the incoming area with the database (Recover), queues the
// units a crash left missing, and starts the batcher and the workers. Files
// are accepted from then on. It returns what Recover found. A second Start
// is an error. A Start that fails ends the pipeline as Stop would: it
// releases the incoming area New opened, and the caller makes a new
// pipeline to try again; files it found landed and could not record yet are
// found again by the next one's Recover.
func (p *Pipeline) Start(ctx context.Context) (RecoverReport, error) {
	p.mu.Lock()
	ended := p.stopping
	p.mu.Unlock()
	if p.running.Load() || ended {
		return RecoverReport{}, fault.New(fault.Internal, "the reception pipeline is already running or has ended")
	}
	rep, err := p.start(ctx)
	if err != nil {
		// Without this every failed start, retried by the supervisor for
		// as long as the database refuses, left the area's directory open.
		_ = p.Stop(context.WithoutCancel(ctx))
	}
	return rep, err
}

// start is Start up to its end; no goroutine is started before its last
// failure can happen.
func (p *Pipeline) start(ctx context.Context) (RecoverReport, error) {
	p.started = p.now()
	p.workCtx, p.stopWork = context.WithCancel(context.WithoutCancel(ctx))
	p.stepCtx, p.abort = context.WithCancel(context.WithoutCancel(ctx))
	rep, err := p.Recover(ctx)
	if err != nil {
		return rep, err
	}
	if err := p.repairWork(ctx); err != nil {
		return rep, err
	}
	// A unit of the pipeline's kinds still leased now was held by a process
	// that is gone: one server serves an installation (it holds the data
	// directory's lock), and this pipeline's workers have not started.
	// Given back now, it is taken up at once rather than when its lease
	// runs out, which kept a restarted server from archiving for up to a
	// lease after a crash. Only the pipeline's own kinds: a send unit may
	// be held live by the same server's senders, and giving it back would
	// end its fence while its batch is in flight, so the batch could be
	// sent twice and an attempt spent at every start of a pipeline.
	if rw, ok := p.store.(tenant.WorkReclaim); ok {
		n, err := rw.ReclaimWorkOfKinds(ctx, claimOrder, p.now())
		if err != nil {
			return rep, err
		}
		if n > 0 {
			p.log.Info("work a stopped process held was taken back", "units", n)
		}
	}
	stop := make(chan struct{})
	p.batchStop = stop
	p.wg.Add(1 + p.cfg.Workers)
	go func() {
		defer p.wg.Done()
		p.batch.run(stop)
	}()
	for i := range p.cfg.Workers {
		go p.worker(p.cfg.Owner + "-" + strconv.Itoa(i))
	}
	p.wg.Add(1)
	go p.framePass()
	p.running.Store(true)
	if rep.Missing > 0 || rep.Rechecked > 0 || rep.Removed > 0 {
		p.log.Info("the incoming area was reconciled", "rechecked", rep.Rechecked, "renamed", rep.Renamed,
			"removed", rep.Removed, "temp", rep.Temp, "missing", rep.Missing, "strangers", rep.Strangers)
	}
	return rep, nil
}

// Stop stops taking files, waits for every Accept in flight, writes the
// batches held, and stops the workers: a step in flight is let finish while
// ctx lasts, and is cut short when ctx ends (its unit stays leased and is
// taken up after its lease, by this process's next start or another). It
// returns when every goroutine the pipeline started has ended, then closes
// the incoming area.
func (p *Pipeline) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	p.mu.Unlock()
	p.accepting.Wait()
	if !p.running.Load() {
		return p.area.Close()
	}
	close(p.batchStop)
	p.stopWork()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		p.abort()
		<-done
	}
	p.abort()
	p.running.Store(false)
	return p.area.Close()
}
