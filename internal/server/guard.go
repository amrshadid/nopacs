package server

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
)

// AccessRules are the installation's settings the front step applies to
// every request off the local socket before its key is read: whose
// forwarded address is believed, the networks allowed and denied, and the
// limits on failed calls.
type AccessRules struct {
	// TrustedProxies is access.trusted-proxies.
	TrustedProxies settings.Networks
	// ForwardedHeader is access.forwarded-header; empty reads as its
	// default, X-Forwarded-For.
	ForwardedHeader settings.ForwardedHeader
	// Allowed is access.allowed-networks; empty for every network.
	Allowed settings.Networks
	// Denied is access.denied-networks.
	Denied settings.Networks
	// FailureLimit is access.failure-limit.
	FailureLimit int
	// FailureWindow is access.failure-window.
	FailureWindow time.Duration
	// BlockTime is access.block-time.
	BlockTime time.Duration
}

// DefaultAccessRules returns the rules of an installation that set none of
// the settings: the settings' built-in defaults.
func DefaultAccessRules() AccessRules {
	return AccessRules{FailureLimit: 10, FailureWindow: time.Minute, BlockTime: 15 * time.Minute}
}

// rulesRefresh is how long the front step keeps the rules it read before it
// reads them again: a change of a setting applies within this time, and the
// master is read at most once in it, not once a request.
const rulesRefresh = 5 * time.Second

// rulesReadTimeout bounds one read of the rules: a handful of settings read
// from the master, done in milliseconds.
const rulesReadTimeout = 5 * time.Second

// guard is the front step's state of access: the rules as last read, and the
// counts of failed calls by address and by key.
type guard struct {
	read func(ctx context.Context) (AccessRules, error)
	log  *slog.Logger
	now  func() time.Time
	// delay is how long a slowed refusal is held: failureDelay, but in a
	// test.
	delay time.Duration
	// held counts the refusals held for delay, so the holds are counted
	// and not inferred from how long requests took.
	held atomic.Int64

	mu    sync.Mutex
	rules AccessRules
	have  bool
	at    time.Time
	// reading is closed when the read in flight ends; nil when none is.
	reading chan struct{}

	addresses *failureTable
	keys      *failureTable
}

// newGuard returns a guard that reads its rules with read, or applies
// DefaultAccessRules when read is nil.
func newGuard(read func(ctx context.Context) (AccessRules, error), log *slog.Logger) *guard {
	return &guard{
		read: read, log: log, now: time.Now, delay: failureDelay,
		addresses: newFailureTable(maxTrackedAddresses),
		keys:      newFailureTable(maxTrackedKeys),
	}
}

// current returns the rules, read again when older than rulesRefresh. When a
// read fails the rules last read are kept, and logged; with none read yet,
// it reports false, and the request is answered unavailable: rules that
// cannot be read are never taken as no rules.
//
// One read is in flight at a time, made outside the guard's lock: while it
// runs, other requests are judged by the rules last read, or, before any,
// wait for it. The read runs on a context of its own, which the request's
// cancellation does not end (bounded by rulesReadTimeout), so a caller who
// resets its request at once cannot keep the old rules in force, a deny
// list just written included, by being the request that reads them.
func (g *guard) current(ctx context.Context) (AccessRules, bool) {
	if g.read == nil {
		return DefaultAccessRules(), true
	}
	g.mu.Lock()
	if g.have && g.now().Sub(g.at) < rulesRefresh {
		r := g.rules
		g.mu.Unlock()
		return r, true
	}
	if wait := g.reading; wait != nil {
		if g.have {
			r := g.rules
			g.mu.Unlock()
			return r, true
		}
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return AccessRules{}, false
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.rules, g.have
	}
	done := make(chan struct{})
	g.reading = done
	g.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rulesReadTimeout)
	r, err := g.read(rctx)
	cancel()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.reading = nil
	close(done)
	now := g.now()
	if err != nil {
		g.log.Warn("server: the access settings could not be read; the last ones read are kept", "error", err, "have", g.have)
		if g.have {
			// Tried again after another refresh, not on every request.
			g.at = now
		}
		return g.rules, g.have
	}
	g.rules, g.have, g.at = r, true, now
	return r, true
}

// installationRefusal returns the installation rule that refuses a request
// from o, or "": the denied networks first, then the allowed networks. The
// local socket is never refused by them.
func installationRefusal(r AccessRules, o origin) string {
	if o.local {
		return ""
	}
	switch {
	case r.Denied.Contains(o.addr):
		return access.RuleDeniedNetworks
	case len(r.Allowed) > 0 && !r.Allowed.Contains(o.addr):
		return access.RuleAllowedNetworks
	}
	return ""
}

// wait holds a slowed refusal for the guard's delay, or until ctx ends.
func (g *guard) wait(ctx context.Context) {
	g.held.Add(1)
	t := time.NewTimer(g.delay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// failAddress counts a request from o refused for its credential, and
// records and raises a block it starts. It returns whether the refusal is
// to be slowed. The local socket is not counted.
func (s *Server) failAddress(o origin, r AccessRules, facts access.Request) bool {
	if o.local || !o.addr.IsValid() {
		return false
	}
	v := s.guard.addresses.fail(addressKey(o.addr), s.guard.now(), r.FailureLimit, r.FailureWindow, r.BlockTime)
	if v.blocked {
		_ = s.access.RecordRefusal(facts, "", access.RuleAddressBlocked, true, v.until)
	}
	return v.slow
}

// failKey counts a request with the key keyID that the check refused for
// its grants, and records and raises a block it starts. It returns whether
// the refusal is to be slowed. The local socket is not counted.
func (s *Server) failKey(ctx context.Context, o origin, keyID string, facts access.Request) bool {
	if o.local || keyID == "" {
		return false
	}
	r, ok := s.guard.current(ctx)
	if !ok {
		return false
	}
	v := s.guard.keys.fail(keyID, s.guard.now(), r.FailureLimit, r.FailureWindow, r.BlockTime)
	if v.blocked {
		_ = s.access.RecordRefusal(facts, keyID, access.RuleKeyBlocked, true, v.until)
	}
	return v.slow
}
