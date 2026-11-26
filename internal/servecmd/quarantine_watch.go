package servecmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// DefaultQuarantineInterval is how often each tenant's quarantine is
// measured against its warning settings. A warning is for people who look at
// a status or a dashboard, not an alarm to the second, and the measure reads
// one aggregate, so every few minutes is soon enough and cheap.
const DefaultQuarantineInterval = 5 * time.Minute

// quarantineOpTimeout bounds one look: a few reads and one batch of events.
const quarantineOpTimeout = 30 * time.Second

// The names of the metrics of a tenant's quarantine. Their label names the
// tenant, which an admin makes, never a request.
const (
	metricQuarantineOldest = "nopacs_quarantine_oldest_seconds"
	metricQuarantineBytes  = "nopacs_quarantine_held_bytes"
)

// quarantineConfig is what a quarantineWatch is made with.
type quarantineConfig struct {
	// Tenant names the tenant, for the metrics and the log.
	Tenant string
	// Aging answers what the tenant holds in quarantine now.
	Aging func(ctx context.Context) (tenant.QuarantineAging, error)
	// WarnAge and WarnBytes return quarantine.warn-age and
	// quarantine.warn-bytes as they apply on the tenant now, read at each
	// look so a change applies without a restart.
	WarnAge   func(ctx context.Context) (time.Duration, error)
	WarnBytes func(ctx context.Context) (int64, error)
	// Append writes events to the tenant's feed.
	Append func(ctx context.Context, events []tenant.EventAppend) (int, error)
	// Now is the server's clock, which the oldest file's age is measured by.
	Now func() time.Time
	// NewID makes the key of each event.
	NewID func() string
	// Wake, when set, is called after an event was written, so a watcher of
	// the feed reads it at once.
	Wake func()
	// Interval is how often Run looks; zero is DefaultQuarantineInterval.
	Interval time.Duration
	// Log receives what the watch does; nil discards it.
	Log *slog.Logger
}

// quarantineWatch measures a tenant's quarantine against its two warning
// settings: it keeps the oldest file's age and the bytes held for the
// metrics, and writes one event when a warning first stands and one when it
// stands no more, never one per look. Which warnings stand is held in memory:
// after a restart, a warning that still stands is reported once more. A
// warning never removes anything; only a quarantine retention rule does. It
// is safe for concurrent use; Run is called once.
type quarantineWatch struct {
	cfg quarantineConfig
	log *slog.Logger

	mu sync.Mutex
	// agePassed and bytesPassed are the warnings reported as standing.
	agePassed, bytesPassed bool
	// last is what the last look measured; measured says there was one.
	last     tenant.QuarantineWarning
	measured bool
}

// newQuarantineWatch returns a watch over cfg.
func newQuarantineWatch(cfg quarantineConfig) (*quarantineWatch, error) {
	if cfg.Aging == nil || cfg.WarnAge == nil || cfg.WarnBytes == nil || cfg.Append == nil || cfg.Now == nil || cfg.NewID == nil {
		return nil, fault.New(fault.Invalid, "servecmd: the quarantine watch needs the held files, both settings, the feed, the clock and ids")
	}
	if cfg.Interval < 0 {
		return nil, fault.New(fault.Invalid, "servecmd: the quarantine watch's interval is negative")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultQuarantineInterval
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &quarantineWatch{cfg: cfg, log: log}, nil
}

// Run looks every Interval until ctx ends, and returns nil then.
func (q *quarantineWatch) Run(ctx context.Context) error {
	t := time.NewTicker(q.cfg.Interval)
	defer t.Stop()
	for {
		if err := q.Check(ctx); err != nil && ctx.Err() == nil {
			q.log.Warn("quarantine: what is held could not be measured; it is measured again", "kind", fault.KindOf(err).String(),
				"reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check measures the quarantine once, keeps the figures, and writes an event
// when which warnings stand changed since the last event. An event that
// cannot be written leaves the warnings as they were, to be reported at the
// next look.
func (q *quarantineWatch) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, quarantineOpTimeout)
	defer cancel()
	aging, err := q.cfg.Aging(cctx)
	if err != nil {
		return err
	}
	age, err := q.cfg.WarnAge(cctx)
	if err != nil {
		return err
	}
	bytes, err := q.cfg.WarnBytes(cctx)
	if err != nil {
		return err
	}
	now := q.cfg.Now()
	w := aging.Warning(now, age, bytes)
	q.mu.Lock()
	q.last, q.measured = w, true
	changed := w.AgePassed != q.agePassed || w.BytesPassed != q.bytesPassed
	q.mu.Unlock()
	if !changed {
		return nil
	}
	e, err := tenant.NewEvent(&tenant.QuarantineAgingChanged{OldestAgeMicros: w.OldestAge.Microseconds(), HeldFiles: w.Files, HeldBytes: w.Bytes,
		AgePassed: w.AgePassed, BytesPassed: w.BytesPassed, Recovered: !w.Standing()}, now)
	if err != nil {
		return err
	}
	if n, err := q.cfg.Append(cctx, []tenant.EventAppend{{Key: q.cfg.NewID(), Event: e}}); err != nil {
		return err
	} else if n != 1 {
		return errors.New("the quarantine event was not appended")
	}
	q.mu.Lock()
	q.agePassed, q.bytesPassed = w.AgePassed, w.BytesPassed
	q.mu.Unlock()
	if w.Standing() {
		q.log.Warn("quarantine: what is held passed quarantine.warn-age or quarantine.warn-bytes", "tenant", q.cfg.Tenant,
			"age_passed", w.AgePassed, "bytes_passed", w.BytesPassed)
	} else {
		q.log.Info("quarantine: what is held is back under its warnings", "tenant", q.cfg.Tenant)
	}
	if q.cfg.Wake != nil {
		q.cfg.Wake()
	}
	return nil
}

// Measured returns what the last look measured, and false before the first.
func (q *quarantineWatch) Measured() (tenant.QuarantineWarning, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.last, q.measured
}

// quarantineWatches holds the running watches by tenant, for the metrics.
// The zero value is ready to use.
type quarantineWatches struct {
	// interval is how often each watch looks; 0 is DefaultQuarantineInterval.
	interval time.Duration

	mu sync.Mutex
	m  map[string]*quarantineWatch
}

// set records the watch of the tenant named, replacing an earlier one.
func (r *quarantineWatches) set(name string, q *quarantineWatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*quarantineWatch{}
	}
	r.m[name] = q
}

// drop forgets the watch of the tenant named, if it is q: a newer watch
// of the same tenant stays.
func (r *quarantineWatches) drop(name string, q *quarantineWatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[name] == q {
		delete(r.m, name)
	}
}

// gauges returns each tenant's oldest held file's age and held bytes, as
// each tenant's watch last measured them; both metrics are described with no
// sample when no watch has measured yet.
func (r *quarantineWatches) gauges() []obs.Gauge {
	const (
		oldestHelp = "How long the oldest quarantined file of a tenant has been held, by tenant, 0 when none is held."
		bytesHelp  = "Bytes a tenant holds in quarantine, by tenant."
	)
	r.mu.Lock()
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	watches := make(map[string]*quarantineWatch, len(r.m))
	for n, q := range r.m {
		watches[n] = q
	}
	r.mu.Unlock()
	slices.Sort(names)
	var oldest, held []obs.Gauge
	for _, n := range names {
		w, ok := watches[n].Measured()
		if !ok {
			continue
		}
		labels := []string{"tenant", n}
		oldest = append(oldest, obs.Gauge{Name: metricQuarantineOldest, Help: oldestHelp, Value: w.OldestAge.Seconds(), Labels: labels})
		held = append(held, obs.Gauge{Name: metricQuarantineBytes, Help: bytesHelp, Value: float64(w.Bytes), Labels: labels})
	}
	if len(oldest) == 0 {
		return []obs.Gauge{{Name: metricQuarantineOldest, Help: oldestHelp, Empty: true}, {Name: metricQuarantineBytes, Help: bytesHelp, Empty: true}}
	}
	return append(oldest, held...)
}

// watch returns the function that runs the quarantine watch of the tenant
// named over its database st, for the tenant's background work w to start.
// It records the watch for the metrics while it runs. A database that keeps
// no events is left unwatched and said so.
func (r *quarantineWatches) watch(w *tenantWork, name string, st tenantDB, log *slog.Logger) func(ctx context.Context) {
	events, ok := lagDBOf(st, st, w.fenceOf(name))
	if !ok {
		log.Warn("tenant work: the tenant's database keeps no events; its quarantine is not watched")
		return func(context.Context) {}
	}
	values := w.values.OnTenant(st)
	q, err := newQuarantineWatch(quarantineConfig{Tenant: name, Aging: st.QuarantineAging, WarnAge: values.QuarantineWarnAge,
		WarnBytes: values.QuarantineWarnBytes, Append: events.AppendEvents, Now: w.now, NewID: server.NewID,
		Interval: r.interval, Log: log, Wake: func() {
			if w.wakeFeed != nil {
				w.wakeFeed(name)
			}
		}})
	if err != nil {
		log.Warn("tenant work: the quarantine watch could not be made", "reason", fault.Message(err))
		return func(context.Context) {}
	}
	return func(ctx context.Context) {
		r.set(name, q)
		defer r.drop(name, q)
		_ = q.Run(ctx)
	}
}
