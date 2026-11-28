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

// DefaultQuotaInterval is how often each tenant's use is measured against
// its storage quota for the warning and the metrics. The refusal at the
// bound does not wait for it: each new file is checked as it starts
// (receive.ReasonOverQuota). A minute is soon enough for a warning, and a
// look reads three running totals.
const DefaultQuotaInterval = time.Minute

// quotaOpTimeout bounds one look: two reads and one event.
const quotaOpTimeout = 30 * time.Second

// The names of the metrics of a tenant's quota. Their label names the
// tenant, which an admin makes, never a request.
const (
	metricQuotaUsedBytes     = "nopacs_quota_used_bytes"
	metricQuotaBoundBytes    = "nopacs_quota_bound_bytes"
	metricQuotaUsedInstances = "nopacs_quota_used_instances"
	metricQuotaBoundInstance = "nopacs_quota_bound_instances"
	metricQuotaState         = "nopacs_quota_state"
)

// quotaConfig is what a quotaWatch is made with.
type quotaConfig struct {
	// Tenant names the tenant, for the metrics and the log.
	Tenant string
	// Use answers what the tenant holds now (tenant.Quotas).
	Use func(ctx context.Context) (tenant.QuotaUse, error)
	// Bounds returns the tenant's quota settings as they apply on it now,
	// read at each look so a change applies without a restart.
	Bounds func(ctx context.Context) (tenant.QuotaBounds, error)
	// Append writes events to the tenant's feed.
	Append func(ctx context.Context, events []tenant.EventAppend) (int, error)
	// Now is the server's clock, the time of each event.
	Now func() time.Time
	// NewID makes the key of each event.
	NewID func() string
	// Wake, when set, is called after an event was written, so a watcher of
	// the feed reads it at once.
	Wake func()
	// Interval is how often Run looks; zero is DefaultQuotaInterval.
	Interval time.Duration
	// Log receives what the watch does; nil discards it.
	Log *slog.Logger
}

// quotaMeasure is one look at a tenant's use against its bounds.
type quotaMeasure struct {
	use    tenant.QuotaUse
	bounds tenant.QuotaBounds
	state  tenant.QuotaState
}

// quotaWatch measures a tenant's use against its storage quota: it keeps
// the figures for the metrics, and writes one event each time the state
// changes (under, warned, reached), never one per look or per file. The
// state reported is held in memory: after a restart, a state other than
// under that still stands is reported once more. It is safe for concurrent
// use; Run is called once.
type quotaWatch struct {
	cfg quotaConfig
	log *slog.Logger

	mu sync.Mutex
	// reported is the state the last event reported, under before any.
	reported tenant.QuotaState
	// last is what the last look measured; measured says there was one.
	last     quotaMeasure
	measured bool
}

// newQuotaWatch returns a watch over cfg.
func newQuotaWatch(cfg quotaConfig) (*quotaWatch, error) {
	if cfg.Use == nil || cfg.Bounds == nil || cfg.Append == nil || cfg.Now == nil || cfg.NewID == nil {
		return nil, fault.New(fault.Invalid, "servecmd: the quota watch needs the use, the bounds, the feed, the clock and ids")
	}
	if cfg.Interval < 0 {
		return nil, fault.New(fault.Invalid, "servecmd: the quota watch's interval is negative")
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultQuotaInterval
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &quotaWatch{cfg: cfg, log: log, reported: tenant.QuotaUnder}, nil
}

// Run looks every Interval until ctx ends, and returns nil then.
func (q *quotaWatch) Run(ctx context.Context) error {
	t := time.NewTicker(q.cfg.Interval)
	defer t.Stop()
	for {
		if err := q.Check(ctx); err != nil && ctx.Err() == nil {
			q.log.Warn("quota: the tenant's use could not be measured; it is measured again", "kind", fault.KindOf(err).String(),
				"reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Check measures the use once, keeps the figures, and writes an event when
// the state changed since the last event. An event that cannot be written
// leaves the state reported as it was, to be reported at the next look.
func (q *quotaWatch) Check(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, quotaOpTimeout)
	defer cancel()
	bounds, err := q.cfg.Bounds(cctx)
	if err != nil {
		return err
	}
	use, err := q.cfg.Use(cctx)
	if err != nil {
		return err
	}
	m := quotaMeasure{use: use, bounds: bounds, state: bounds.State(use)}
	q.mu.Lock()
	q.last, q.measured = m, true
	changed := m.state != q.reported
	q.mu.Unlock()
	if !changed {
		return nil
	}
	e, err := tenant.NewEvent(&tenant.QuotaChanged{State: m.state, UsedBytes: use.Bytes(), UsedInstances: use.Instances(),
		BytesBound: bounds.Bytes, InstancesBound: bounds.Instances}, q.cfg.Now())
	if err != nil {
		return err
	}
	if n, err := q.cfg.Append(cctx, []tenant.EventAppend{{Key: q.cfg.NewID(), Event: e}}); err != nil {
		return err
	} else if n != 1 {
		return errors.New("the quota event was not appended")
	}
	q.mu.Lock()
	q.reported = m.state
	q.mu.Unlock()
	switch m.state {
	case tenant.QuotaReached:
		q.log.Warn("quota: the tenant reached its storage quota; new files are refused until it is under it", "tenant", q.cfg.Tenant)
	case tenant.QuotaWarned:
		q.log.Warn("quota: the tenant passed quota.warn of its storage quota", "tenant", q.cfg.Tenant)
	default:
		q.log.Info("quota: the tenant is back under its storage quota's warning", "tenant", q.cfg.Tenant)
	}
	if q.cfg.Wake != nil {
		q.cfg.Wake()
	}
	return nil
}

// Measured returns what the last look measured, and false before the first.
func (q *quotaWatch) Measured() (quotaMeasure, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.last, q.measured
}

// quotaStateValue is a state as the gauge nopacs_quota_state writes it.
func quotaStateValue(s tenant.QuotaState) float64 {
	switch s {
	case tenant.QuotaWarned:
		return 1
	case tenant.QuotaReached:
		return 2
	}
	return 0
}

// quotaWatches holds the running watches by tenant, for the metrics. The
// zero value is ready to use.
type quotaWatches struct {
	// interval is how often each watch looks; 0 is DefaultQuotaInterval.
	interval time.Duration

	mu sync.Mutex
	m  map[string]*quotaWatch
}

// set records the watch of the tenant named, replacing an earlier one.
func (r *quotaWatches) set(name string, q *quotaWatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*quotaWatch{}
	}
	r.m[name] = q
}

// drop forgets the watch of the tenant named, if it is q.
func (r *quotaWatches) drop(name string, q *quotaWatch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[name] == q {
		delete(r.m, name)
	}
}

// gauges returns each tenant's use, bounds and state, as each tenant's
// watch last measured them; every metric is described with no sample when
// no watch has measured yet. A bound not set is written 0.
func (r *quotaWatches) gauges() []obs.Gauge {
	type family struct {
		name, help string
		value      func(quotaMeasure) float64
	}
	families := []family{
		{metricQuotaUsedBytes, "Bytes of the files a tenant holds against its storage quota, by tenant.",
			func(m quotaMeasure) float64 { return float64(m.use.Bytes()) }},
		{metricQuotaBoundBytes, "A tenant's quota.bytes, by tenant; 0 for no bound.",
			func(m quotaMeasure) float64 { return float64(m.bounds.Bytes) }},
		{metricQuotaUsedInstances, "Files a tenant holds against its storage quota, by tenant.",
			func(m quotaMeasure) float64 { return float64(m.use.Instances()) }},
		{metricQuotaBoundInstance, "A tenant's quota.instances, by tenant; 0 for no bound.",
			func(m quotaMeasure) float64 { return float64(m.bounds.Instances) }},
		{metricQuotaState, "Where a tenant's use stands against its storage quota, by tenant: 0 under, 1 past quota.warn, 2 at a bound.",
			func(m quotaMeasure) float64 { return quotaStateValue(m.state) }},
	}
	r.mu.Lock()
	names := make([]string, 0, len(r.m))
	watches := make(map[string]*quotaWatch, len(r.m))
	for n, q := range r.m {
		names = append(names, n)
		watches[n] = q
	}
	r.mu.Unlock()
	slices.Sort(names)
	var measures []quotaMeasure
	var labels [][]string
	for _, n := range names {
		if m, ok := watches[n].Measured(); ok {
			measures = append(measures, m)
			labels = append(labels, []string{"tenant", n})
		}
	}
	var out []obs.Gauge
	for _, f := range families {
		if len(measures) == 0 {
			out = append(out, obs.Gauge{Name: f.name, Help: f.help, Empty: true})
			continue
		}
		for i, m := range measures {
			out = append(out, obs.Gauge{Name: f.name, Help: f.help, Value: f.value(m), Labels: labels[i]})
		}
	}
	return out
}

// watch returns the function that runs the quota watch of the tenant named
// over its database st, for the tenant's background work w to start. It
// records the watch for the metrics while it runs. A database that keeps
// no events or no counts is left unwatched and said so.
func (r *quotaWatches) watch(w *tenantWork, name string, st tenantDB, log *slog.Logger) func(ctx context.Context) {
	events, ok := lagDBOf(st, st, w.fenceOf(name))
	counts, counted := st.(tenant.Quotas)
	if !ok || !counted {
		log.Warn("tenant work: the tenant's database keeps no events or no counts; its quota is not watched")
		return func(context.Context) {}
	}
	values := w.values.OnTenant(st)
	q, err := newQuotaWatch(quotaConfig{Tenant: name, Use: counts.QuotaUse, Bounds: values.QuotaBounds,
		Append: events.AppendEvents, Now: w.now, NewID: server.NewID, Interval: r.interval, Log: log, Wake: func() {
			if w.wakeFeed != nil {
				w.wakeFeed(name)
			}
		}})
	if err != nil {
		log.Warn("tenant work: the quota watch could not be made", "reason", fault.Message(err))
		return func(context.Context) {}
	}
	return func(ctx context.Context) {
		r.set(name, q)
		defer r.drop(name, q)
		_ = q.Run(ctx)
	}
}
