package send

import (
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// senderRun is one sender of a tenant: its row as last read, its health,
// its rate and its pool of workers.
type senderRun struct {
	t    *tenantRun
	name string
	row  atomic.Pointer[tenant.Sender]
	// reach is where and as whom the sender connects as last applied
	// (reachOf); a change stops the units in flight.
	reach  string
	health *health
	rate   rateLimit
	// open counts the associations (or requests) its workers hold now.
	open atomic.Int64

	mu      sync.Mutex
	workers []*worker
	nudges  chan struct{}
}

// worker is one of a sender's workers, which claims the sender's units one
// at a time.
type worker struct {
	s     *senderRun
	owner string
	// quit asks it to end after its unit; stop to end its unit after the
	// instance in flight, giving the unit back.
	quit, stop chan struct{}
	quitOnce   sync.Once
	stopOnce   sync.Once
	done       chan struct{}
	// why is the reason its unit is given back with when it is stopped.
	why atomic.Value
}

// newSenderRun returns the run of a sender first read as row.
func newSenderRun(t *tenantRun, row tenant.Sender, now time.Time) *senderRun {
	sr := &senderRun{t: t, name: row.Name, health: newHealth(row.Name, now), nudges: make(chan struct{})}
	r := row
	sr.row.Store(&r)
	return sr
}

// destination is what a sender's health depends on: where it connects.
func destination(s tenant.Sender) string {
	tlsOf := func(t *tenant.SenderTLS) string {
		if t == nil {
			return ""
		}
		return t.CAFile + "\x00" + t.CertificateFile + "\x00" + t.PrivateKeyFile + "\x00" + t.ServerName
	}
	if s.Type == tenant.SenderSTOW {
		return "stow\x00" + s.STOW.URL + "\x00" + tlsOf(s.STOW.TLS)
	}
	return "dimse\x00" + s.DIMSE.Host + "\x00" + strconv.Itoa(s.DIMSE.Port) + "\x00" + tlsOf(s.DIMSE.TLS)
}

// reachOf is what a sender's connections are made with: its destination,
// its AE titles (PS3.8 9.3.2), its authentication and the digest of its
// sealed credential, never the credential.
func reachOf(s tenant.Sender, credential [32]byte) string {
	return destination(s) + "\x00" + s.DIMSE.CalledAETitle + "\x00" + s.DIMSE.CallingAETitle + "\x00" + string(s.STOW.Authentication) +
		"\x00" + s.STOW.Username + "\x00" + s.STOW.HeaderName + "\x00" + hex.EncodeToString(credential[:])
}

// update applies the sender's row as now read, with the digest of its
// sealed credential: its health unknown when where it connects changed or
// a reason that was not its peer's ended; the units in flight given back
// and the pool started again when its destination, authentication or
// credential changed, so nothing more goes to the old peer or with the old
// credential; its pool grown or shrunk to its max_associations; or stopped
// when it is disabled.
func (sr *senderRun) update(row tenant.Sender, credential [32]byte, now time.Time) {
	r := row
	sr.row.Store(&r)
	sr.rate.setRate(row.MaxBytesPerSecond)
	h := sr.health.get()
	reach := reachOf(row, credential)
	moved := sr.reach != "" && sr.reach != reach
	sr.reach = reach
	switch {
	case !row.Enabled:
		sr.report(sr.health.set(tenant.SenderDown, tenant.SenderDownDisabled, phraseDisabled, now), now)
		sr.stopAll(false, reasonDisabled)
		return
	case moved:
		sr.report(sr.health.unknown(now), now)
		sr.stopAll(false, reasonChanged)
	case h.State == tenant.SenderDown && h.DownReason != tenant.SenderDownPeerFailing:
		sr.report(sr.health.unknown(now), now)
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for len(sr.workers) < row.MaxAssociations {
		w := &worker{s: sr, owner: "s-" + sr.name + "-" + strconv.Itoa(len(sr.workers)),
			quit: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
		sr.workers = append(sr.workers, w)
		sr.t.sup.wg.Add(1)
		go w.run()
	}
	for len(sr.workers) > row.MaxAssociations {
		last := sr.workers[len(sr.workers)-1]
		sr.workers = sr.workers[:len(sr.workers)-1]
		last.quitOnce.Do(func() { close(last.quit) })
	}
}

// stopAll stops every worker of the sender, each giving its unit back
// after the instance in flight with reason, and waits for them when wait
// is set. A
// sender disabled or removed is not waited for, so one peer slow to answer
// its instance does not hold the reading of the tenant's other senders;
// the supervisor's Stop waits for every worker all the same.
func (sr *senderRun) stopAll(wait bool, reason string) {
	sr.mu.Lock()
	ws := sr.workers
	sr.workers = nil
	sr.mu.Unlock()
	for _, w := range ws {
		w.stopOnce.Do(func() {
			w.why.Store(reason)
			close(w.stop)
		})
	}
	for _, w := range ws {
		if wait {
			<-w.done
		}
	}
}

// nudge ends the wait of every idle worker of the sender.
func (sr *senderRun) nudge() {
	sr.mu.Lock()
	close(sr.nudges)
	sr.nudges = make(chan struct{})
	sr.mu.Unlock()
}

// waitChan returns the channel the next nudge closes.
func (sr *senderRun) waitChan() <-chan struct{} {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.nudges
}

// report writes a change of the sender's health as an event; nil writes
// nothing.
func (sr *senderRun) report(c change, now time.Time) {
	if c == nil {
		return
	}
	norm := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.UTC().Truncate(time.Microsecond)
	}
	c.Since, c.NextTry, c.LastSuccess = norm(c.Since), norm(c.NextTry), norm(c.LastSuccess)
	sr.t.log.Info("sender: health changed", "sender", sr.name, "state", string(c.State), "reason", string(c.DownReason),
		"fault", string(c.Fault), "detail", c.Detail)
	sr.t.appendEvents([]tenant.EventPayload{c}, now)
}

// run claims and works the sender's units until asked to end.
func (w *worker) run() {
	defer w.s.t.sup.wg.Done()
	defer close(w.done)
	sup := w.s.t.sup
	wait := sup.cfg.PollInterval
	for {
		select {
		case <-w.quit:
			return
		case <-w.stop:
			return
		default:
		}
		nudged := w.s.waitChan()
		worked, err := w.once()
		switch {
		case worked:
			wait = sup.cfg.PollInterval
			continue
		case err != nil:
			w.s.t.log.Warn("sender: looking for work failed", "sender", w.s.name, "reason", fault.Message(err))
		}
		t := time.NewTimer(w.idleWait(wait, sup.cfg.Now()))
		select {
		case <-w.quit:
		case <-w.stop:
		case <-nudged:
			wait = sup.cfg.PollInterval
		case <-t.C:
			wait = min(2*wait, MaxPollInterval)
		}
		t.Stop()
	}
}

// idleWait returns how long an idle worker waits before it looks again:
// wait, but no later than the sender's next try of a down peer (its
// health's NextTry), so the try is made when it is due rather than up to
// MaxPollInterval after; at least a millisecond, so a try due already is
// not looked for in a tight loop while another worker makes it.
func (w *worker) idleWait(wait time.Duration, now time.Time) time.Duration {
	if d, ok := w.s.health.untilProbe(now); ok && d < wait {
		return max(d, time.Millisecond)
	}
	return wait
}

// once tries the down peer when due, or claims and works one unit, and
// reports whether it did either.
func (w *worker) once() (bool, error) {
	sr := w.s
	sup := sr.t.sup
	now := sup.cfg.Now()
	p := sr.t.pipeline()
	if p == nil {
		return false, nil
	}
	if !sr.health.workable() {
		if !sr.health.probeDue(now) {
			return false, nil
		}
		defer sr.health.probed()
		// A probe is a connection like any other: it holds one of the
		// tenant's and the installation's associations while it lasts.
		release, ok := sr.t.acquire()
		if !ok {
			return false, nil
		}
		defer release()
		sr.open.Add(1)
		defer sr.open.Add(-1)
		w.probe(p, *sr.row.Load())
		return sr.health.workable(), nil
	}
	release, ok := sr.t.acquire()
	if !ok {
		return false, nil
	}
	defer release()
	ctx, cancel := context.WithTimeout(sup.hard, opTimeout)
	unit, err := p.ClaimWork(ctx, tenant.SendKind(sr.name), w.owner, now, Lease)
	cancel()
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return false, nil
	case err != nil:
		return false, err
	}
	// Others may find work too, now that some is due.
	sr.nudge()
	sr.open.Add(1)
	defer sr.open.Add(-1)
	w.work(p, unit)
	return true, nil
}

// probe tries the down peer once, by C-ECHO or the stow probe, and records
// the outcome in the sender's health.
func (w *worker) probe(p tenant.Pipeline, row tenant.Sender) {
	sr := w.s
	sup := sr.t.sup
	ctx, cancel := context.WithTimeout(sup.hard, row.ConnectTimeout+row.ResponseTimeout)
	defer cancel()
	var err error
	if row.Type == tenant.SenderSTOW {
		err = sup.probeStow(ctx, sr.t.name, p, row.Name)
	} else {
		err = sup.probeDIMSE(ctx, sr.t.name, row)
	}
	now := sup.cfg.Now()
	if err == nil {
		sr.report(sr.health.connected(now), now)
		return
	}
	var ce *connectError
	if !errors.As(err, &ce) {
		ce = &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
	}
	c, _ := sr.health.failed(ce.fault, ce.detail, now)
	sr.report(c, now)
}

// connectFailed records a connection of a unit that failed: the sender's
// health counts it, and the unit is given back without spending an attempt
// when that made the sender down, and with one otherwise.
func (u *unitRun) connectFailed(ce *connectError) outcome {
	now := u.sup.cfg.Now()
	c, down := u.sr.health.failed(ce.fault, ce.detail, now)
	u.sr.report(c, now)
	u.log.Info("sender: a connection to the peer failed", "fault", string(ce.fault), "detail", ce.detail, "cause", ce.Error())
	if down {
		return outcome{release: true, reason: reasonSenderDown, due: now}
	}
	return outcome{temporary: true, reason: reasonConnect}
}

// connected records a connection of a unit that succeeded.
func (u *unitRun) connected() {
	now := u.sup.cfg.Now()
	u.sr.report(u.sr.health.connected(now), now)
}
