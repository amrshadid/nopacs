package supervise

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// receiverRun is one enabled receiver of a tenant: the row its listener was
// made from, the listener while it runs, and when to try again after a
// failure to start.
type receiverRun struct {
	// row is the receiver's row as last read.
	row tenant.Receiver
	// made is the row the listener was made from; a row whose listener's
	// part differs restarts it.
	made tenant.Receiver
	// storage is the row's storage, which the listener's Refusal reads on
	// its own goroutine.
	storage  atomic.Pointer[string]
	listener Listener
	// nextTry and backoff schedule the next start after a failure.
	nextTry time.Time
	backoff time.Duration
}

// stop stops the listener, if one runs.
func (r *receiverRun) stop(ctx context.Context) {
	if r.listener == nil {
		return
	}
	_ = r.listener.Stop(ctx)
	r.listener = nil
}

// apply makes the listeners match rows: it stops the listeners of receivers
// gone, disabled or changed in their listener's part first, so a port one
// receiver leaves is free for another, then starts every enabled receiver
// that is due.
func (t *tenantRun) apply(rows []tenant.Receiver) {
	now := t.sup.cfg.Now()
	want := make(map[string]tenant.Receiver, len(rows))
	for _, r := range rows {
		if runs(r) {
			want[r.Name] = r
		}
	}
	for name, run := range t.receivers {
		r, ok := want[name]
		switch {
		case !ok:
			run.stop(t.sup.ctx)
			delete(t.receivers, name)
			t.forgetHealth(name)
		case !r.Enabled:
			run.stop(t.sup.ctx)
			delete(t.receivers, name)
		case run.listener != nil && !sameListener(run.made, r):
			t.setHealth(r, Health{State: tenant.ReceiverStarting, Since: now})
			run.stop(t.sup.ctx)
			run.nextTry, run.backoff = time.Time{}, 0
		}
	}
	for _, r := range rows {
		if !runs(r) {
			continue
		}
		if !r.Enabled {
			t.setHealth(r, down(tenant.DownDisabled, tenant.PhraseDisabled, r.Updated))
			continue
		}
		run := t.receivers[r.Name]
		if run == nil {
			run = &receiverRun{}
			t.receivers[r.Name] = run
		}
		if run.listener == nil && !sameListener(run.made, r) {
			// A row changed while its listener was down is tried at once.
			run.nextTry, run.backoff = time.Time{}, 0
		}
		run.row = r
		storage := r.Storage
		run.storage.Store(&storage)
		if c, ok := run.listener.(Checker); ok {
			if err := c.Check(now); err != nil {
				run.stop(t.sup.ctx)
				t.startFailed(run, err, now)
			}
		}
		if run.listener == nil && !time.Now().Before(run.nextTry) {
			t.start(run)
		}
		if run.listener != nil {
			t.setHealth(r, t.upHealth(r, now))
		}
	}
}

// start makes and starts the listener of run's row. A failure leaves the
// receiver down with the reason the listener gave and schedules the next
// try, doubling the wait each time.
func (t *tenantRun) start(run *receiverRun) {
	r := run.row
	now := t.sup.cfg.Now()
	if run.backoff == 0 {
		// A retry keeps the receiver down with its reason, so the feed says
		// down once and up once, not once per try.
		t.setHealth(r, Health{State: tenant.ReceiverStarting, Since: now})
	}
	run.made = r
	var bind string
	var err error
	if r.Type == tenant.ReceiverDIMSE {
		bind, err = t.bindAddress(r)
	}
	if err == nil {
		var l Listener
		l, err = t.sup.cfg.NewListener(ListenerSpec{
			Tenant: t.name, Receiver: r, BindAddress: bind, Sink: t.sink,
			Refusal: func() tenant.AssociationFailure { return t.refusal(run) },
			Report: func(f tenant.AssociationFailure, c tenant.GroupCaller) {
				if t.failures.add(r.Name, f, c, t.sup.cfg.Now()) {
					t.wake()
				}
			},
		})
		if err == nil {
			if err = l.Start(t.sup.ctx); err == nil {
				run.listener = l
				run.nextTry, run.backoff = time.Time{}, 0
				t.log.Info("receiver: started", "receiver", r.Name)
				return
			}
		}
	}
	t.startFailed(run, err, now)
}

// startFailed leaves run's receiver down with the reason err gives and
// schedules the next start, doubling the wait each time: after a start
// that failed, or a running listener whose Check failed.
func (t *tenantRun) startFailed(run *receiverRun, err error, now time.Time) {
	r := run.row
	reason, detail := downOf(err)
	run.backoff = min(max(2*run.backoff, t.sup.cfg.MinBackoff), t.sup.cfg.MaxBackoff)
	// The machine's monotonic clock, as every due time (reconcile).
	run.nextTry = time.Now().Add(run.backoff)
	t.log.Warn("receiver: could not start; it is tried again", "receiver", r.Name, "reason", string(reason),
		"detail", detail, "retry_in", run.backoff.String())
	t.setHealth(r, down(reason, detail, now))
}

// downOf returns the down reason and phrase of a listener's start error:
// the listener's own when it gives one (a method Down), else "other".
func downOf(err error) (tenant.ReceiverDownReason, string) {
	var d interface {
		Down() (tenant.ReceiverDownReason, string)
	}
	if errors.As(err, &d) {
		reason, detail := d.Down()
		if detail == "" || slices.Contains(tenant.HealthPhrases(reason), detail) {
			return reason, detail
		}
	}
	return tenant.DownOther, tenant.PhraseCouldNotStart
}

// bindAddress returns the address a receiver listens on: its own, or the
// first the installation's receiver.bind-addresses lists, or every address
// when it lists none (receiver.proto, bind_address).
func (t *tenantRun) bindAddress(r tenant.Receiver) (string, error) {
	if r.DIMSE.BindAddress != "" {
		return r.DIMSE.BindAddress, nil
	}
	ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
	defer cancel()
	addrs, err := t.sup.values().BindAddresses(ctx)
	if err != nil || len(addrs) == 0 {
		return "", err
	}
	return addrs[0].String(), nil
}

// nextRetry returns how long until the next receiver is due to be tried
// again, and whether one is.
func (t *tenantRun) nextRetry() (time.Duration, bool) {
	var soonest time.Time
	for _, run := range t.receivers {
		if run.listener == nil && !run.nextTry.IsZero() && (soonest.IsZero() || run.nextTry.Before(soonest)) {
			soonest = run.nextTry
		}
	}
	if soonest.IsZero() {
		return 0, false
	}
	return time.Until(soonest), true
}

// refreshHealth sets the health of every running receiver from the gates,
// after they changed without the rows being read again.
func (t *tenantRun) refreshHealth(now time.Time) {
	for _, run := range t.receivers {
		if run.listener != nil {
			t.setHealth(run.row, t.upHealth(run.row, now))
		}
	}
}

// runs reports whether the supervisor runs a listener for a receiver of
// r's type: a DICOM listener for a DIMSE receiver, the endpoint that takes
// STOW-RS requests for a stow receiver. A type it does not know is left
// alone, with no health.
func runs(r tenant.Receiver) bool {
	return r.Type == tenant.ReceiverDIMSE || r.Type == tenant.ReceiverSTOW
}

// sameListener reports whether two rows make the same listener: the same
// type and configuration of it, and the same largest file, which bounds
// what the listener reads of a data set; for a stow receiver, whether it
// de-identifies too, which decides whether its answers give Retrieve URLs
// (a restart of one costs no port). A change of anything else is read by
// the pipeline from the row and needs no restart.
func sameListener(a, b tenant.Receiver) bool {
	da, db := a.DIMSE, b.DIMSE
	if a.Type != b.Type || a.STOW.MaxRequestBytes != b.STOW.MaxRequestBytes || !slices.Equal(a.STOW.Keys, b.STOW.Keys) {
		return false
	}
	if a.Type == tenant.ReceiverSTOW && a.Deidentifies() != b.Deidentifies() {
		return false
	}
	if a.Name != b.Name || a.MaxFileBytes != b.MaxFileBytes || da.Port != db.Port || da.BindAddress != db.BindAddress ||
		da.AETitle != db.AETitle || da.AnyCaller != db.AnyCaller {
		return false
	}
	if (da.TLS == nil) != (db.TLS == nil) || (da.TLS != nil && *da.TLS != *db.TLS) {
		return false
	}
	return slices.Equal(da.Callers, db.Callers) && slices.Equal(da.SOPClasses, db.SOPClasses) &&
		slices.Equal(da.TransferSyntaxes, db.TransferSyntaxes)
}

// sameStore reports whether a and b are one open database. A store whose
// dynamic type cannot be compared is taken as another, which restarts the
// pipeline on it rather than keep one over a closed database.
func sameStore(a, b tenant.Store) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
