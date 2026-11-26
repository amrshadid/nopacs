package servecmd

import (
	"context"
	"slices"

	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fencedEvents is a tenant's database as the lag watch writes it: an event
// appended asks the registry's fence first, as every other write of the
// background work does (fencedDB), so a tenant another installation took
// over gets no event from this server.
type fencedEvents struct {
	tenant.Replication
	events tenant.Events
	fence  func() error
}

// lagDBOf returns the tenant's database as the lag watch reads and writes
// it, its events fenced by fence, and false for a database that keeps no
// events (every driver keeps them; a test's wrapper may not): the rest of
// the background work runs without the watch then.
func lagDBOf(st any, db tenantDB, fence func() error) (fencedEvents, bool) {
	ev, ok := st.(tenant.Events)
	if !ok {
		return fencedEvents{}, false
	}
	return fencedEvents{Replication: db, events: ev, fence: fence}, true
}

// AppendEvents asks the fence, then appends.
func (d fencedEvents) AppendEvents(ctx context.Context, events []tenant.EventAppend) (int, error) {
	if d.fence != nil {
		if err := d.fence(); err != nil {
			return 0, err
		}
	}
	return d.events.AppendEvents(ctx, events)
}

// The names of the metrics of replicas' lag and scrubs' coverage. Their
// labels name tenants and storages, which an admin makes, never a request,
// so the number of series stays bounded by the installation's own size.
const (
	metricReplicaLag      = "nopacs_replica_lag_seconds"
	metricScansBehind     = "nopacs_replica_scans_behind"
	metricVerifiedShare   = "nopacs_scrub_verified_share"
	metricOldestUnchecked = "nopacs_scrub_oldest_unverified_seconds"
	metricOverdue         = "nopacs_scrub_overdue_objects"
)

// replicaGauges returns each replica's lag and scans not in step, by
// tenant and replica, as each tenant's lag watch last read them: how long
// the oldest scan not in step on it has waited, measured by the server's
// clock from the commit that made it pending.
func (w *tenantWork) replicaGauges() []obs.Gauge {
	w.mu.Lock()
	names := make([]string, 0, len(w.sets))
	for n := range w.sets {
		names = append(names, n)
	}
	sets := make(map[string]*tenantSet, len(w.sets))
	for n, s := range w.sets {
		sets[n] = s
	}
	w.mu.Unlock()
	slices.Sort(names)
	const (
		lagHelp    = "How long the oldest scan not in step on a replica has waited, by tenant and replica, 0 when every scan is in step."
		behindHelp = "Scans not in step on a replica (pending, copying or damaged), by tenant and replica."
	)
	var lags, behind []obs.Gauge
	for _, n := range names {
		if sets[n].lag == nil {
			continue
		}
		for _, r := range sets[n].lag.Lags() {
			labels := []string{"tenant", n, "replica", r.Replica}
			lags = append(lags, obs.Gauge{Name: metricReplicaLag, Help: lagHelp, Value: r.Lag.Seconds(), Labels: labels})
			behind = append(behind, obs.Gauge{Name: metricScansBehind, Help: behindHelp, Value: float64(r.ScansBehind), Labels: labels})
		}
	}
	if len(lags) == 0 {
		return []obs.Gauge{{Name: metricReplicaLag, Help: lagHelp, Empty: true}, {Name: metricScansBehind, Help: behindHelp, Empty: true}}
	}
	return append(lags, behind...)
}

// scrubGauges returns each storage's coverage by tenant and storage, as
// the last scrub of it that ended in this process left it
// (scrub.Supervisor.Coverages): the share of its objects read whole within
// scrub.interval, how long the object waiting longest has gone unread, and
// how many are overdue.
func scrubGauges(covs []scrub.TenantCoverage) []obs.Gauge {
	const (
		shareHelp   = "Share of a storage's objects a scrub read whole within scrub.interval, by tenant and storage, as its last scrub left it."
		oldestHelp  = "How long a storage's object waiting longest has gone unread by a scrub, by tenant and storage, 0 when every one is within scrub.interval."
		overdueHelp = "Objects of a storage not read whole by a scrub within scrub.interval of their last read, by tenant and storage."
	)
	if len(covs) == 0 {
		return []obs.Gauge{{Name: metricVerifiedShare, Help: shareHelp, Empty: true}, {Name: metricOldestUnchecked, Help: oldestHelp, Empty: true},
			{Name: metricOverdue, Help: overdueHelp, Empty: true}}
	}
	out := make([]obs.Gauge, 0, 3*len(covs))
	for _, family := range []string{metricVerifiedShare, metricOldestUnchecked, metricOverdue} {
		for _, c := range covs {
			g := obs.Gauge{Name: family, Labels: []string{"tenant", c.Tenant, "storage", c.Storage}}
			switch family {
			case metricVerifiedShare:
				g.Help, g.Value = shareHelp, c.Share
			case metricOldestUnchecked:
				g.Help, g.Value = oldestHelp, c.OldestUnverified.Seconds()
			default:
				g.Help, g.Value = overdueHelp, float64(c.Overdue)
			}
			out = append(out, g)
		}
	}
	return out
}

// copyGauges returns the metrics of the copies being kept whole: each
// replica's lag (work's) and each storage's scrub coverage (scrubs').
func copyGauges(work *tenantWork, scrubs *scrub.Supervisor) []obs.Gauge {
	var out []obs.Gauge
	if work != nil {
		out = work.replicaGauges()
	}
	return append(out, scrubGaugesOf(scrubs)...)
}

// scrubGaugesOf returns scrubGauges over the supervisor's coverages; a nil
// supervisor describes the metrics with no sample.
func scrubGaugesOf(s *scrub.Supervisor) []obs.Gauge {
	if s == nil {
		return scrubGauges(nil)
	}
	return scrubGauges(s.Coverages())
}
