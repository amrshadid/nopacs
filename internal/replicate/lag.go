package replicate

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Replicas is the part of the tenant's database the lag watch reads and
// writes: the replicas' counts, and the events it reports
// (tenant.Replication, tenant.Events).
type Replicas interface {
	ListReplicas(ctx context.Context, primary string, page master.Page) ([]tenant.ReplicaStatus, string, error)
	AppendEvents(ctx context.Context, events []tenant.EventAppend) (int, error)
}

// DefaultLagInterval is how often the lag watch reads the replicas: the
// least replica.lag-alert, so an alert is never more than a minute late.
const DefaultLagInterval = time.Minute

// maxReplicaPages bounds the pages of replicas one look reads.
const maxReplicaPages = 1000

// LagConfig is what a Lag is made with.
type LagConfig struct {
	// Replicas is the tenant's database.
	Replicas Replicas
	// Alert returns replica.lag-alert as it applies on the tenant now, read
	// at each look.
	Alert func(ctx context.Context) (time.Duration, error)
	// Now is the server's clock, which a replica's lag is measured by.
	Now func() time.Time
	// NewID makes the key of each event (master.ValidateID).
	NewID func() string
	// Wake, when set, is called after events were written, so a watcher of
	// the feed reads them at once.
	Wake func()
	// Interval is how often Run looks; zero is DefaultLagInterval.
	Interval time.Duration
	// Log receives what the watch does; nil discards it.
	Log *slog.Logger
}

// ReplicaLag is one replica's lag as the last look read it.
type ReplicaLag struct {
	// Replica names the replica; Primary its primary.
	Replica, Primary string
	// Lag is how long its oldest scan not in step has waited, zero when
	// every scan is in step.
	Lag time.Duration
	// ScansBehind counts its scans pending, copying or damaged.
	ScansBehind int64
}

// Lag watches how far each replica of a tenant is behind its primary: it
// keeps each one's lag for the server's metrics, and reports the replica
// behind with one event when its lag passes replica.lag-alert, and one more
// when it falls back under it, never one per look. What it reported is held
// in memory: after a restart, a replica still behind is reported once more.
// It is safe for concurrent use; Run is called once.
type Lag struct {
	cfg LagConfig
	log *slog.Logger

	mu sync.Mutex
	// behind names the replicas reported behind and not yet recovered.
	behind map[string]bool
	// last is what the last look read, by replica.
	last map[string]ReplicaLag
}

// NewLag returns a lag watch over cfg.
func NewLag(cfg LagConfig) (*Lag, error) {
	if cfg.Replicas == nil || cfg.Alert == nil || cfg.Now == nil || cfg.NewID == nil {
		return nil, fault.New(fault.Invalid, "replicate: the lag watch needs the database, the alert's setting, the clock and ids")
	}
	if cfg.Interval < 0 {
		return nil, fault.New(fault.Invalid, "replicate: the lag watch's interval is negative")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultLagInterval
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Lag{cfg: cfg, log: log, behind: map[string]bool{}, last: map[string]ReplicaLag{}}, nil
}

// Run looks every Interval until ctx ends, and returns nil then.
func (l *Lag) Run(ctx context.Context) error {
	t := time.NewTicker(l.cfg.Interval)
	defer t.Stop()
	for {
		if err := l.Check(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("replicas: the replicas' lag could not be read; it is read again", "kind", fault.KindOf(err).String(),
				"reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check reads every replica's lag once, keeps it, and writes the events
// of the replicas that passed the alert or fell back under it since the
// last look. A replica no longer listed (detached, or switched to be the
// primary) is forgotten without an event. An event that cannot be written
// leaves the replica as it was, to be reported at the next look.
func (l *Lag) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	alert, err := l.cfg.Alert(cctx)
	if err != nil {
		return err
	}
	var all []tenant.ReplicaStatus
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxReplicaPages {
		rows, next, err := l.cfg.Replicas.ListReplicas(cctx, "", page)
		if err != nil {
			return err
		}
		all = append(all, rows...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	now := l.cfg.Now()
	seen := make(map[string]ReplicaLag, len(all))
	var events []tenant.EventAppend
	var flips []string
	l.mu.Lock()
	for _, r := range all {
		lag := ReplicaLag{Replica: r.Replica, Primary: r.Primary, ScansBehind: r.Pending + r.Copying + r.Damaged}
		if !r.OldestQueued.IsZero() && now.After(r.OldestQueued) {
			lag.Lag = now.Sub(r.OldestQueued)
		}
		seen[r.Replica] = lag
		over := lag.Lag > alert
		if over == l.behind[r.Replica] {
			continue
		}
		e, err := tenant.NewEvent(&tenant.ReplicaBehind{Replica: lag.Replica, Primary: lag.Primary, LagMicros: lag.Lag.Microseconds(),
			ScansBehind: lag.ScansBehind, Recovered: !over}, now)
		if err != nil {
			l.mu.Unlock()
			return err
		}
		events = append(events, tenant.EventAppend{Key: l.cfg.NewID(), Event: e})
		flips = append(flips, r.Replica)
	}
	l.last = seen
	for name := range l.behind {
		if _, ok := seen[name]; !ok {
			delete(l.behind, name)
		}
	}
	l.mu.Unlock()
	for len(events) > 0 {
		n := min(len(events), tenant.MaxEventBatch)
		if _, err := l.cfg.Replicas.AppendEvents(cctx, events[:n]); err != nil {
			return err
		}
		l.mu.Lock()
		for _, name := range flips[:n] {
			l.behind[name] = !l.behind[name]
			if l.behind[name] {
				l.log.Warn("replicas: a replica is behind its primary past replica.lag-alert", "replica", name)
			} else {
				delete(l.behind, name)
				l.log.Info("replicas: a replica is back under replica.lag-alert", "replica", name)
			}
		}
		l.mu.Unlock()
		events, flips = events[n:], flips[n:]
		if l.cfg.Wake != nil {
			l.cfg.Wake()
		}
	}
	return nil
}

// Lags returns each replica's lag as the last look read it, by replica.
func (l *Lag) Lags() []ReplicaLag {
	l.mu.Lock()
	out := make([]ReplicaLag, 0, len(l.last))
	for _, r := range l.last {
		out = append(out, r)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b ReplicaLag) int { return cmp.Compare(a.Replica, b.Replica) })
	return out
}
