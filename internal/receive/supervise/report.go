package supervise

import (
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The grouping of refused callers (monitor.proto, AssociationFailed).
const (
	// groupWindow is how long further failures of a group are counted into
	// one event after its first.
	groupWindow = time.Minute
	// maxHostsPerWindow is how many distinct hosts failing at one receiver
	// in a window get events of their own; the rest are folded together.
	maxHostsPerWindow = 64
	// maxFoldedHosts bounds the hosts a folded event counts one by one, so
	// a flood from many addresses costs bounded memory; past it the count
	// of hosts stops growing and the count of failures goes on.
	maxFoldedHosts = 4096
	// maxPending bounds the events waiting to be written; past it a
	// failure is counted into its group's end-of-window event instead.
	maxPending = 1024
)

// groupKey is one group: a receiver, a failure and a host.
type groupKey struct {
	receiver string
	failure  tenant.AssociationFailure
	host     string
}

// group is a group whose first failure made its event: the failures since,
// counted until its window ends.
type group struct {
	caller tenant.GroupCaller
	until  time.Time
	more   int64
}

// window is one receiver's current window of distinct hosts.
type window struct {
	until time.Time
	hosts map[string]bool
	// folded counts the failures of hosts past the first 64, by failure,
	// and the distinct hosts among them.
	folded      map[tenant.AssociationFailure]int64
	foldedHosts map[string]bool
}

// failureGroups groups one tenant's refused callers into events. Listeners
// call add on their own goroutines; the tenant's worker writes the events.
type failureGroups struct {
	t *tenantRun

	mu      sync.Mutex
	groups  map[groupKey]*group
	windows map[string]*window
	pending []tenant.EventPayload
}

// newFailureGroups returns the grouping of t's refused callers.
func newFailureGroups(t *tenantRun) *failureGroups {
	return &failureGroups{t: t, groups: map[groupKey]*group{}, windows: map[string]*window{}}
}

// add counts one refused caller at now, and reports whether an event is
// waiting to be written: the first failure of a group makes one at once.
func (f *failureGroups) add(receiver string, failure tenant.AssociationFailure, caller tenant.GroupCaller, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire(now)
	k := groupKey{receiver, failure, caller.Address}
	if g := f.groups[k]; g != nil {
		g.more++
		return false
	}
	w := f.windows[receiver]
	if w == nil {
		w = &window{until: now.Add(groupWindow), hosts: map[string]bool{}}
		f.windows[receiver] = w
	}
	if !w.hosts[caller.Address] && len(w.hosts) >= maxHostsPerWindow {
		if w.folded == nil {
			w.folded, w.foldedHosts = map[tenant.AssociationFailure]int64{}, map[string]bool{}
		}
		w.folded[failure]++
		if len(w.foldedHosts) < maxFoldedHosts {
			w.foldedHosts[caller.Address] = true
		}
		return false
	}
	w.hosts[caller.Address] = true
	g := &group{caller: caller, until: now.Add(groupWindow)}
	f.groups[k] = g
	if len(f.pending) >= maxPending {
		g.more++
		return false
	}
	c := caller
	f.pending = append(f.pending, &tenant.AssociationFailed{Receiver: receiver, Caller: &c, Failure: failure, Count: 1, Hosts: 1})
	return true
}

// expire ends the groups and windows whose minute has passed at now, each
// making its end-of-window event when it counted anything. Called with mu
// held.
func (f *failureGroups) expire(now time.Time) {
	for k, g := range f.groups {
		if now.Before(g.until) {
			continue
		}
		delete(f.groups, k)
		if g.more > 0 {
			c := g.caller
			f.pending = append(f.pending, &tenant.AssociationFailed{Receiver: k.receiver, Caller: &c, Failure: k.failure, Count: g.more, Hosts: 1})
		}
	}
	for receiver, w := range f.windows {
		if now.Before(w.until) {
			continue
		}
		delete(f.windows, receiver)
		f.pending = append(f.pending, w.foldedEvents(receiver)...)
	}
}

// foldedEvents returns the events of a window's folded failures, one per
// failure, with no caller.
func (w *window) foldedEvents(receiver string) []tenant.EventPayload {
	var out []tenant.EventPayload
	for failure, n := range w.folded {
		out = append(out, &tenant.AssociationFailed{Receiver: receiver, Failure: failure, Count: n, Hosts: int64(max(1, len(w.foldedHosts)))})
	}
	return out
}

// flush ends what expired at now and writes the events waiting.
func (f *failureGroups) flush(now time.Time) {
	f.mu.Lock()
	f.expire(now)
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	f.t.appendEvents(pending, now)
}

// flushAll writes every group's count now, whatever is left of its window,
// for a stop: the counts would be lost otherwise.
func (f *failureGroups) flushAll(now time.Time) {
	f.flush(now.Add(groupWindow + time.Nanosecond))
}

// nextFlush returns how long until the next group or window ends, 0 when
// none is open.
func (f *failureGroups) nextFlush(now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var soonest time.Time
	consider := func(t time.Time) {
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	for _, g := range f.groups {
		consider(g.until)
	}
	for _, w := range f.windows {
		consider(w.until)
	}
	if soonest.IsZero() {
		return 0
	}
	return max(soonest.Sub(now), time.Millisecond)
}
