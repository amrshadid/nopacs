package supervise

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Health is what the supervisor knows of a receiver now.
type Health struct {
	// State is starting, up or down.
	State tenant.ReceiverState
	// Reason is why it is down; empty unless State is down.
	Reason tenant.ReceiverDownReason
	// Detail is the server's phrase for the reason; empty when not down.
	Detail string
	// Since is when it entered this state with this reason.
	Since time.Time
}

// same reports whether h and o are one state, whatever their times.
func (h Health) same(o Health) bool {
	return h.State == o.State && h.Reason == o.Reason && h.Detail == o.Detail
}

// down returns the health of a receiver down for reason, since now.
func down(reason tenant.ReceiverDownReason, detail string, now time.Time) Health {
	return Health{State: tenant.ReceiverDown, Reason: reason, Detail: detail, Since: now}
}

// healthKey names one receiver of one tenant.
type healthKey struct{ tenant, receiver string }

// healthEntry is a receiver's health and the row it is the health of.
type healthEntry struct {
	health Health
	// updated is the Updated time of the row the health belongs to: a row
	// read later with a newer time is a change not yet applied.
	updated time.Time
}

// Health returns the health of receiver r of the tenant named tenantName,
// as configured now: r is the row as the caller read it. It reads memory
// only, so the API may call it once for every receiver it answers. A row
// newer than the one the health belongs to, or one the supervisor has not
// seen, is a change not yet applied: the tenant's worker is poked to apply
// it, and the receiver is answered starting (down, disabled, when the row
// disables it).
func (s *Supervisor) Health(tenantName string, r tenant.Receiver) Health {
	s.healthMu.RLock()
	e, ok := s.health[healthKey{tenantName, r.Name}]
	s.healthMu.RUnlock()
	fresh := ok && !r.Updated.After(e.updated)
	if !fresh {
		s.Poke(tenantName)
	}
	switch {
	case !r.Enabled && fresh:
		return e.health
	case !r.Enabled:
		return down(tenant.DownDisabled, tenant.PhraseDisabled, r.Updated)
	case !fresh:
		return Health{State: tenant.ReceiverStarting, Since: r.Updated}
	}
	return e.health
}

// setHealth records the health of the receiver r of t, and when its state
// changed (other than to starting, which passes in a moment), appends the
// event that says so first, so a reader who sees the new health finds its
// event in the feed. Only t's worker calls it.
func (t *tenantRun) setHealth(r tenant.Receiver, h Health) {
	s := t.sup
	k := healthKey{t.name, r.Name}
	s.healthMu.Lock()
	old, had := s.health[k]
	if had && old.health.same(h) {
		old.updated = r.Updated
		s.health[k] = old
		s.healthMu.Unlock()
		return
	}
	s.healthMu.Unlock()
	// A receiver disabled since before this process started did not
	// change, and starting passes in a moment.
	if h.State != tenant.ReceiverStarting && (had || h.Reason != tenant.DownDisabled) {
		t.log.Info("receiver: health changed", "receiver", r.Name, "state", string(h.State), "reason", string(h.Reason), "detail", h.Detail)
		t.appendEvent(&tenant.ReceiverHealthChanged{Receiver: r.Name, State: h.State, DownReason: h.Reason,
			Detail: h.Detail, Since: h.Since.UTC().Truncate(time.Microsecond)}, h.Since)
	}
	s.healthMu.Lock()
	s.health[k] = healthEntry{health: h, updated: r.Updated}
	s.healthMu.Unlock()
}

// forgetHealth drops the health of a receiver whose row is gone.
func (t *tenantRun) forgetHealth(name string) {
	t.sup.healthMu.Lock()
	delete(t.sup.health, healthKey{t.name, name})
	t.sup.healthMu.Unlock()
}

// appendEvent writes one event the server reports into the tenant's feed,
// when its database keeps one, and wakes the feed's watchers. A failure is
// logged: the state it reports is still in memory and in the status.
func (t *tenantRun) appendEvent(p tenant.EventPayload, at time.Time) {
	t.appendEvents([]tenant.EventPayload{p}, at)
}

// appendEvents writes events the server reports, all at the time at.
func (t *tenantRun) appendEvents(ps []tenant.EventPayload, at time.Time) {
	if len(ps) == 0 {
		return
	}
	st, err := t.sup.cfg.Tenants.Store(t.name)
	if err != nil {
		return
	}
	feed, ok := st.(tenant.Events)
	if !ok {
		return
	}
	batch := make([]tenant.EventAppend, 0, len(ps))
	for _, p := range ps {
		ev, err := tenant.NewEvent(p, at.UTC().Truncate(time.Microsecond))
		if err != nil {
			t.log.Error("receivers: an event the supervisor made is not valid; it is not written", "reason", fault.Message(err))
			continue
		}
		batch = append(batch, tenant.EventAppend{Key: newKey(), Event: ev})
	}
	for len(batch) > 0 {
		n := min(len(batch), tenant.MaxEventBatch)
		ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
		_, err := feed.AppendEvents(ctx, batch[:n])
		cancel()
		if err != nil {
			t.log.Warn("receivers: events could not be written", "count", n, "reason", fault.Message(err))
			return
		}
		batch = batch[n:]
	}
	t.sup.cfg.Wake(t.name)
}
