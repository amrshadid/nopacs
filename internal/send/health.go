package send

import (
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The fixed phrases of a sender's state (SenderHealth.detail).
const (
	phraseDisabled       = "the sender is disabled"
	phraseTenantNotReady = "the tenant's database is not ready"
	phraseStopping       = "the server is stopping"
	phrasePeerFailing    = "three connections in a row failed"
)

// health is one sender's health, the contract's SenderHealth, kept in
// memory only: a database restored or moved never answers a health that
// was true of another time.
type health struct {
	mu sync.Mutex
	h  tenant.SenderHealthChanged
	// seen says the sender's state was ever observed, so a sender
	// disabled since before the server started writes no event at its
	// first reading.
	seen bool
	// probing says a worker is trying the down peer again now.
	probing bool
}

// newHealth returns the health of a sender the server has not connected to.
func newHealth(name string, now time.Time) *health {
	return &health{h: tenant.SenderHealthChanged{Sender: name, State: tenant.SenderUnknown, Since: now}}
}

// change is a health that changed, to be written as an event; nil when it
// did not change in a way an event reports.
type change = *tenant.SenderHealthChanged

// get returns the health now.
func (h *health) get() tenant.SenderHealthChanged {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.h
}

// set moves the state to state with reason and detail, keeping the counts,
// and returns the change when the state or reason is new.
func (h *health) set(state tenant.SenderState, reason tenant.SenderDownReason, detail string, now time.Time) change {
	h.mu.Lock()
	defer h.mu.Unlock()
	first := !h.seen
	h.seen = true
	if h.h.State == state && h.h.DownReason == reason {
		return nil
	}
	h.h.State, h.h.DownReason, h.h.Detail, h.h.Since = state, reason, detail, now
	if reason != tenant.SenderDownPeerFailing {
		h.h.NextTry = time.Time{}
	}
	if first && reason == tenant.SenderDownDisabled {
		return nil
	}
	c := h.h
	return &c
}

// unknown makes the health unknown, as after a change of destination or
// when a reason that is not the peer's ends (enabled again, the tenant
// ready again): the next connection says.
func (h *health) unknown(now time.Time) change {
	h.mu.Lock()
	h.h.FailedConnections, h.h.Fault = 0, ""
	h.mu.Unlock()
	return h.set(tenant.SenderUnknown, "", "", now)
}

// connected records a connection that succeeded: up, no failures in a
// row.
func (h *health) connected(now time.Time) change {
	h.mu.Lock()
	h.h.FailedConnections, h.h.Fault, h.h.LastSuccess = 0, "", now
	h.mu.Unlock()
	return h.set(tenant.SenderUp, "", "", now)
}

// failed records a connection that failed with fault and its phrase, and
// returns the change and whether the sender is down for it now: after
// DownAfter in a row, tried again after tenant.RetryDelay of the failures
// past the first DownAfter-1 (10 seconds, doubling to an hour). A change of
// the fault alone is reported too, since the health answers it.
func (h *health) failed(fault tenant.SenderFault, detail string, now time.Time) (change, bool) {
	h.mu.Lock()
	h.h.FailedConnections++
	n := h.h.FailedConnections
	faultChanged := h.h.Fault != fault
	h.h.Fault = fault
	down := n >= DownAfter
	if down {
		h.h.NextTry = now.Add(tenant.RetryDelay(n - DownAfter + 1))
	}
	if !down || (h.h.State == tenant.SenderDown && h.h.DownReason == tenant.SenderDownPeerFailing) {
		if h.h.State != tenant.SenderUp {
			h.h.Detail = detail
		}
		h.seen = true
		c := h.h
		h.mu.Unlock()
		if faultChanged {
			return &c, down
		}
		return nil, down
	}
	h.mu.Unlock()
	return h.set(tenant.SenderDown, tenant.SenderDownPeerFailing, detail, now), true
}

// workable reports whether units of the sender may be claimed now: not
// down for any reason.
func (h *health) workable() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.h.State != tenant.SenderDown
}

// probeDue reports whether the peer of a sender down for its failures is
// due to be tried again at now, and if so marks one worker as trying it;
// the caller calls probed when done.
func (h *health) probeDue(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probing || h.h.State != tenant.SenderDown || h.h.DownReason != tenant.SenderDownPeerFailing || now.Before(h.h.NextTry) {
		return false
	}
	h.probing = true
	return true
}

// untilProbe returns how long after now the peer of a sender down for its
// failures is due to be tried again, and false when no try is due: the
// sender is not down for its failures, or a worker tries it now. A try
// already due is zero.
func (h *health) untilProbe(now time.Time) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probing || h.h.State != tenant.SenderDown || h.h.DownReason != tenant.SenderDownPeerFailing {
		return 0, false
	}
	return max(h.h.NextTry.Sub(now), 0), true
}

// probed ends a try of the peer.
func (h *health) probed() {
	h.mu.Lock()
	h.probing = false
	h.mu.Unlock()
}
