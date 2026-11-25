package storageswitch

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReasonStorageSwitching is the reason a write refused while its storage
// is switched to its replica carries (access.proto, the unavailable kind):
// the message starts with it, so a client that reads only the message can
// tell it from a storage that does not answer.
const ReasonStorageSwitching = "STORAGE_SWITCHING"

// ErrStorageSwitching is the case, inside the unavailable kind, of a write
// refused because its storage is being switched to its replica: the API
// answers it with a retry delay (RetryAfter).
var ErrStorageSwitching = errors.New("the storage is being switched to its replica")

// RetryAfter is the delay a write refused with ErrStorageSwitching is told
// to wait before it tries again (google.rpc.RetryInfo; RFC 9110 10.2.3):
// a switch's pause lasts its drain and one transaction.
const RetryAfter = 30 * time.Second

// The defaults of a gate.
const (
	// DefaultRowTTL is how long a gate keeps what the tenant's database
	// said of a storage's switch before it asks again: a write asks at most
	// once a second per storage, not once per write.
	DefaultRowTTL = time.Second
	// DefaultWait is how long a writer waits at a paused gate before it is
	// refused: a reception's step then fails and is tried again after its
	// retry delay, rather than hold a worker for the whole drain.
	DefaultWait = 30 * time.Second
)

// Rows answers the tenant's switch of a storage, which a gate reads so a
// switch accepted and not yet ended pauses writes even when no Switcher of
// this process runs it: between a start and its refusal of a switch the
// last server left accepted.
type Rows interface {
	// LastSwitch answers the switch of the storage named, as primary or as
	// replica, accepted last (tenant.Switching).
	LastSwitch(ctx context.Context, storage string) (tenant.StorageSwitch, bool, error)
}

// Clock is the gate's and the Switcher's only source of time: Now for every
// time written or compared, After for every wait.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Gate is the one place every write to a tenant's primary storage asks
// before it lands bytes there: a write enters, does its work, and releases;
// a switch pauses the gate of its primary, waits until the writers in
// flight have released, and resumes it once its swap has committed or it
// was refused. It is safe for concurrent use.
type Gate struct {
	rows  Rows
	clock Clock
	ttl   time.Duration
	wait  time.Duration

	mu      sync.Mutex
	paused  map[string]chan struct{}
	writers map[string]int
	idle    map[string]chan struct{}
	cache   map[string]cachedRow
	refused map[string]int64
}

// cachedRow is what the database said of a storage's switch, and when.
type cachedRow struct {
	switching bool
	read      time.Time
}

// GateConfig is what a gate is given.
type GateConfig struct {
	// Rows answers the tenant's switches; nil reads none, and only a pause
	// of this process holds writes.
	Rows Rows
	// Clock is the gate's clock; nil is the machine's.
	Clock Clock
	// RowTTL is how long an answer of Rows is kept; 0 is DefaultRowTTL.
	RowTTL time.Duration
	// Wait is how long a writer waits at a paused gate before it is
	// refused; 0 is DefaultWait, a negative value refuses at once.
	Wait time.Duration
}

// NewGate returns a gate, open for every storage.
func NewGate(cfg GateConfig) *Gate {
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if cfg.RowTTL <= 0 {
		cfg.RowTTL = DefaultRowTTL
	}
	if cfg.Wait == 0 {
		cfg.Wait = DefaultWait
	}
	return &Gate{rows: cfg.Rows, clock: cfg.Clock, ttl: cfg.RowTTL, wait: max(cfg.Wait, 0), paused: map[string]chan struct{}{},
		writers: map[string]int{}, idle: map[string]chan struct{}{}, cache: map[string]cachedRow{}, refused: map[string]int64{}}
}

// Refusal returns the error of a write to storage refused while it is
// switched: UNAVAILABLE, ErrStorageSwitching, its message starting with the
// reason.
func Refusal(storage string) error {
	return fault.Wrap(fault.Unavailable, ErrStorageSwitching, ReasonStorageSwitching+": the storage "+storage+
		" is being switched to its replica; writes to it resume once the switch ends, try again shortly")
}

// Enter admits a write to storage and returns its release, which the
// writer calls once, after its last effect on the storage (its bytes put
// and its catalog rows committed). While the storage is paused it waits,
// up to the gate's wait, then answers Refusal; an ended ctx answers its
// error. A database that cannot say whether the storage is switching
// answers its error: a write is never let through on a guess.
func (g *Gate) Enter(ctx context.Context, storage string) (func(), error) {
	timeout := g.clock.After(g.wait)
	for {
		g.mu.Lock()
		if ch, ok := g.paused[storage]; ok {
			g.mu.Unlock()
			if err := g.await(ctx, timeout, ch, storage); err != nil {
				return nil, err
			}
			continue
		}
		// Counted before the database is asked: a pause that begins now
		// waits for this writer to leave, whatever the answer.
		g.writers[storage]++
		g.mu.Unlock()
		switching, err := g.rowSwitching(ctx, storage)
		if err != nil {
			g.leave(storage)
			return nil, err
		}
		if !switching {
			var once sync.Once
			return func() { once.Do(func() { g.leave(storage) }) }, nil
		}
		g.leave(storage)
		if err := g.await(ctx, timeout, nil, storage); err != nil {
			return nil, err
		}
	}
}

// await waits for ch to close (a pause of this process ending) or, with no
// ch, for the row's answer to be asked again; it answers the refusal once
// timeout fires, ctx's error once it ends.
func (g *Gate) await(ctx context.Context, timeout <-chan time.Time, ch <-chan struct{}, storage string) error {
	var again <-chan time.Time
	if ch == nil {
		again = g.clock.After(g.ttl)
	}
	select {
	case <-ch:
		return nil
	case <-again:
		return nil
	case <-timeout:
		g.CountRefused(storage)
		return Refusal(storage)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Switching reports whether writes to storage are paused now, without
// waiting: for a listener that refuses an association before any file
// comes, which counts it with CountRefused.
func (g *Gate) Switching(ctx context.Context, storage string) (bool, error) {
	g.mu.Lock()
	_, paused := g.paused[storage]
	g.mu.Unlock()
	if paused {
		return true, nil
	}
	return g.rowSwitching(ctx, storage)
}

// CountRefused counts one write, association or request refused because
// storage is switching; the switch records the count of its pause.
func (g *Gate) CountRefused(storage string) {
	g.mu.Lock()
	g.refused[storage]++
	g.mu.Unlock()
}

// refusedCount returns the refusals counted for storage so far.
func (g *Gate) refusedCount(storage string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[storage]
}

// leave ends one writer's hold on storage.
func (g *Gate) leave(storage string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writers[storage]--
	if g.writers[storage] > 0 {
		return
	}
	delete(g.writers, storage)
	if ch, ok := g.idle[storage]; ok {
		close(ch)
		delete(g.idle, storage)
	}
}

// pause holds every new write to storage until resume. It reports false
// when storage is paused already.
func (g *Gate) pause(storage string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.paused[storage]; ok {
		return false
	}
	g.paused[storage] = make(chan struct{})
	return true
}

// resume lets writes to storage in again, and forgets what the database
// said of it, which the switch's end has just changed.
func (g *Gate) resume(storage string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch, ok := g.paused[storage]; ok {
		close(ch)
		delete(g.paused, storage)
	}
	delete(g.cache, storage)
}

// idleOf returns a channel closed once no writer holds storage.
func (g *Gate) idleOf(storage string) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.writers[storage] == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	ch, ok := g.idle[storage]
	if !ok {
		ch = make(chan struct{})
		g.idle[storage] = ch
	}
	return ch
}

// rowSwitching reports whether the tenant's database holds a switch of
// storage, as its primary, accepted and not yet swapped or refused, read
// again once the cached answer is older than the gate's TTL.
func (g *Gate) rowSwitching(ctx context.Context, storage string) (bool, error) {
	if g.rows == nil {
		return false, nil
	}
	now := g.clock.Now()
	g.mu.Lock()
	c, ok := g.cache[storage]
	g.mu.Unlock()
	if ok && !now.Before(c.read) && now.Sub(c.read) < g.ttl {
		return c.switching, nil
	}
	sw, found, err := g.rows.LastSwitch(ctx, storage)
	if err != nil {
		return false, err
	}
	switching := found && sw.Primary == storage && sw.State == tenant.SwitchAccepted
	g.mu.Lock()
	g.cache[storage] = cachedRow{switching: switching, read: now}
	g.mu.Unlock()
	return switching, nil
}

// systemClock is the machine's clock.
type systemClock struct{}

// Now returns time.Now().
func (systemClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
