package servecmd

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deidjob"
	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/events"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/send"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// The tenant registry's settings in serve. They are constants, not flags:
// none is something an operator has a reason to change yet, and each is sized
// so a dead database costs seconds, not minutes, while a slow but working one
// is not called dead. A flag can be added when a measured need shows.
const (
	// TenantCheckInterval is how often every tenant is checked, and the
	// registry reconciled with the master's list: 30 seconds, so a tenant
	// whose database comes back is served again within half a minute, at
	// the cost of one ping per tenant per interval.
	TenantCheckInterval = 30 * time.Second
	// TenantOpenTimeout bounds one tenant's open, ping and migration: 10
	// seconds, ample for a SQLite file or a reachable Postgres server and the
	// migrations this version ships, and the longest a dead database delays
	// the start of serve, since the tenants are opened in parallel.
	TenantOpenTimeout = 10 * time.Second
	// TenantPingTimeout bounds one check of an open tenant: 2 seconds, so a
	// database that stops answering is seen within one interval and a ping
	// never holds a check round for long.
	TenantPingTimeout = 2 * time.Second
	// TenantMaxOpening is how many tenants are opened at once: 16, enough to
	// open a thousand SQLite tenants in a few seconds without opening them
	// all in the same instant, against the process's limit of open files
	// and a Postgres server's limit of connections.
	TenantMaxOpening = 16
	// tenantNamesTTL is how long the names behind the tenants-by-state
	// metric are kept before the master is read again (see stateCounts).
	tenantNamesTTL = 10 * time.Second
	// tenantNamesTimeout bounds that read, so a scrape never waits long on a
	// master that does not answer.
	tenantNamesTimeout = 2 * time.Second
)

// APIServices builds the API over an open installation: the tenant registry,
// with the SQLite and Postgres tenant drivers under the database types a
// tenant's row records, then the handlers of package internal/api.
//
// The registry is started before the server listens: Start opens every
// tenant in parallel, TenantMaxOpening at a time, each under
// TenantOpenTimeout, and returns once each is ready, unreachable or failed.
// So serve answers its first request with every tenant already tried once;
// a tenant whose database did not answer in time is unreachable, answers
// UNAVAILABLE to a request into it while the others are served, and is
// tried again each TenantCheckInterval. The alternative, listening first and
// opening behind, would answer a tenant that is fine as unavailable for the
// first seconds of every start, and would let a creation race the start's
// own reading of the tenants.
func APIServices(d Deps) (Built, error) {
	// The senders are made after the registry and the receivers, which
	// tell them of a tenant let go and of a reception that may have made
	// forward jobs; until they are made there is nobody to tell.
	var sending atomic.Pointer[send.Supervisor]
	var deidJobs atomic.Pointer[deidjob.Supervisor]
	var scrubJobs atomic.Pointer[scrub.Supervisor]
	var importJobs atomic.Pointer[dirimport.Supervisor]
	var tenantJobs atomic.Pointer[tenantWork]
	pokeSenders := func(name string) {
		if s := sending.Load(); s != nil {
			s.Poke(name)
		}
	}
	installation, err := server.ReadInstallationID(d.DataDir)
	if err != nil {
		return Built{}, err
	}
	// The owner record of each of a tenant's storages is read when the
	// tenant opens and at each of the registry's checks; one that names
	// another installation, tenant or a later generation fences the tenant.
	// One record, for the whole server, of the storages whose crash
	// leftovers its first open removed, shared by every store it opens to
	// write (the owner records', the pipelines', the corrections', a
	// storage test's), so no later store removes the work in progress of
	// one already writing there.
	cleanups := &place.Cleanups{}
	ownerCheck := backup.OwnerCheck{Installation: installation, Keys: d.Keys,
		Opener: place.Opener{Values: settingvalue.Reader{Master: d.Master}, Keys: d.Keys, StaleUploadAge: -1, Cleanups: cleanups}}
	reg, err := registry.New(registry.Config{
		// A tenant a check fences stops its background work at once, not at
		// the work's next reading of the tenants.
		Fence: fenceNotice(ownerCheck.Fence, func(name string) {
			if s := tenantJobs.Load(); s != nil {
				s.Fenced(name)
			}
		}),
		Master: d.Master,
		Keys:   d.Keys,
		Drivers: map[string]registry.Driver{
			driver.SQLiteDriver:   driver.SQLiteTenants{DataDir: d.DataDir},
			driver.PostgresDriver: driver.PostgresTenants{},
		},
		Now:         clock.Now,
		Interval:    TenantCheckInterval,
		OpenTimeout: TenantOpenTimeout,
		PingTimeout: TenantPingTimeout,
		MaxOpening:  TenantMaxOpening,
		Log:         d.Log,
		NewID:       server.NewID,
		// A tenant let go stops its sends at once, not at the senders'
		// next reading of the tenants.
		Unregistered: func(name string) {
			if s := sending.Load(); s != nil {
				s.TenantRemoved(name)
			}
			if s := deidJobs.Load(); s != nil {
				s.TenantRemoved(name)
			}
			if s := scrubJobs.Load(); s != nil {
				s.TenantRemoved(name)
			}
			if s := importJobs.Load(); s != nil {
				s.TenantRemoved(name)
			}
			if s := tenantJobs.Load(); s != nil {
				s.TenantRemoved(name)
			}
		},
	})
	if err != nil {
		return Built{}, err
	}
	if err := reg.Start(context.Background()); err != nil {
		closeRegistry(reg)
		return Built{}, err
	}
	// The supervisor runs every tenant's receivers and reception pipeline,
	// and answers each receiver's health. The removal of discarded and
	// rejected files is left to the handlers' defaults: nothing is removed
	// from the incoming area by a handler (the pipeline removes it at its
	// next start). A quarantined file's header is read through the pipeline
	// that owns the incoming area.
	// One broadcaster wakes the streams of a tenant after a commit that
	// wrote its events; its timer covers a wake missed.
	feeds, err := events.New(events.Config{})
	if err != nil {
		closeRegistry(reg)
		return Built{}, err
	}
	// The running stow receivers, which the supervisor starts and stops
	// and the STOW-RS route finds by the caller's key.
	stowReceivers := stow.NewDirectory()
	// One bound on de-identification's memory (deid.memory) for the whole
	// server: every tenant's reception pipeline and the API's trial runs
	// take from it.
	deidMemory := check.NewBudget(settingvalue.Reader{Master: d.Master}.DeidMemory, check.BoundInterval)
	// Each tenant's scan locks, shared by its pipelines and its corrections
	// engine, and its running pipeline, which a replace makes its file
	// through.
	scanLocks, pipes := &tenantScanLocks{}, &runningPipelines{}
	// Each tenant's background work that keeps its copies whole, and the
	// switcher whose gate every write to a primary asks: made before the
	// reception, whose pipelines ask the gate, and the corrections, whose
	// purges do. One lock per tenant serializes every advance of its owner
	// records (an export's, a reclaim's, a switch's).
	owners := &ownerLocks{}
	work, err := newTenantWork(d, reg, cleanups, installation, owners)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	work.wakeFeed = feeds.Wake
	tenantJobs.Store(work)
	sup, err := newReception(d, reg, feeds.Wake, pokeSenders, stowReceivers, deidMemory, scanLocks, pipes, cleanups, work)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	// Every tenant's corrections engine, and the pass that takes up what a
	// stop left and purges what is due.
	engines, err := newCorrections(d, reg, sup, scanLocks, cleanups, work)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	work.setEngines(engines)
	// The senders, which read archived bytes under the same count of reads
	// open as the API, so one bound covers both.
	reads := &checkedread.Slots{Limits: readLimits(settingvalue.Reader{Master: d.Master}, time.Now)}
	// The reads a replica served because a primary could not, for the
	// metrics, counted by the API, the senders and the deid jobs alike, and
	// warned of in the server's log at most once a minute a primary storage.
	readCounts := &checkedread.Counts{Log: d.Log}
	senders, err := newSending(d, reg, feeds.Wake, reads, readCounts)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	sending.Store(senders)
	// The deid jobs, which read archived bytes under the same count.
	deids, err := newDeidJobs(d, reg, feeds.Wake, reads, readCounts, deidMemory)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	deidJobs.Store(deids)
	// The scrubs, which read archived bytes under the same count.
	scrubs, err := newScrubs(d, reg, feeds.Wake, reads)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	scrubJobs.Store(scrubs)
	// The rebuilds, which read the storages' meta/ trees and write each
	// tenant's shadow catalog.
	rebuilds, err := newRebuilds(d, reg, feeds.Wake, handoffs{sup: sup, engines: engines, locks: owners, work: work})
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	// The import units, which hand files to the tenants' reception
	// pipelines the supervisor runs.
	imports, err := newImports(d, reg, sup, feeds.Wake)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	importJobs.Store(imports)
	// The sealing pass of every audit log.
	sealer, err := newSealer(d, reg)
	if err != nil {
		_ = feeds.Close()
		closeRegistry(reg)
		return Built{}, err
	}
	h, err := api.New(api.Config{Access: d.Access, Registry: reg, Master: d.Master, NewID: server.NewID, Log: d.Log, Keys: d.Keys, Now: clock.Now,
		Receivers: receiverHealth{sup}, Live: receiverHealth{sup}, Feeds: feeds, Incoming: pipelineDecisions{sup}, Headers: heldHeaders{sup}, HeldFiles: heldHeaders{sup}, STOW: stowReceivers,
		Reads: reads, ReadCounts: readCounts, SenderHealths: senderHealths{s: senders, deid: deids, scrub: scrubs, imports: imports, rebuilds: rebuilds}, SenderTests: senderTests{senders}, DeidMemory: deidMemory,
		Installation: installation, ImportGuarded: importGuarded(d.DataDir), Corrections: engines, ReplaceSources: pipes,
		Handoffs: handoffs{sup: sup, engines: engines, owners: ownerCheck, locks: owners, work: work}, StorageCleanups: cleanups, SealPause: sealer.Pause,
		Holds: work.HoldWriter})
	if err != nil {
		closeRegistry(reg)
		return Built{}, err
	}
	counts := &stateCounts{master: d.Master, registry: reg, now: clock.Now}
	return Built{
		Register:       h.Register,
		Methods:        api.Methods{},
		WithDecision:   api.WithDecision,
		Registry:       reg,
		TenantsByState: counts.byState,
		// A tenant's networks live in its own database; each answer is kept
		// a few seconds, so a request into a tenant does not read it. The
		// rule is read through Reader: it guards every request, so a moved
		// or fenced tenant still applies its rule, and a request the rule
		// lets through is refused afterward with the tenant's state
		// (FAILED_PRECONDITION, TENANT_MOVED for a change, TENANT_FENCED for
		// a change or a read of a fenced tenant's data), never failed as an
		// internal error by the check.
		TenantNetworks: access.CachedTenantNetworks(func(ctx context.Context, t access.TenantRef) (settings.Networks, error) {
			st, err := reg.Reader(t.Name)
			if err != nil {
				return nil, err
			}
			return settingvalue.Reader{Master: d.Master}.OnTenant(st).TenantNetworks(ctx)
		}, tenantNetworksRefresh, time.Now),
		// The download of an instance over plain HTTP, and a tenant's
		// DICOMweb services (QIDO-RS, WADO-RS) on one route for their
		// subtree, each checked and audited as every route is.
		PatternRoutes: []server.PatternRoute{
			{Pattern: api.DownloadPattern, Need: api.DownloadNeed, Handler: h.Download},
			{Pattern: api.DICOMwebPattern, Need: api.DICOMwebNeed, Handler: h.DICOMweb},
			// STOW-RS, a POST on the same subtree, served by the tenant's
			// stow receiver that lists the caller's key.
			{Pattern: api.STOWPattern, Need: api.STOWNeed, Handler: h.STOW},
		},
		// No deadline applies to a read of a subject's instances, whose
		// stream may take hours; a client that stops reading is cut by the
		// server's stall bound instead. DICOMweb's one route carries both
		// retrieval, which may stream a whole study, and search, which is
		// bounded by its own row limits, so the route is long as a whole.
		// A store by STOW-RS may send a whole study in one request; a client
		// that stops sending is cut by the receiver's read stall bound.
		// An export of a long audit log, a tenant's export and a backup
		// stream for as long as they take, as their contract says when the
		// client sends no deadline; a backup's client that stops reading is
		// cut by the stall bound, which ends the read transactions it held.
		LongMethods: []string{nopacsv1connect.DataServiceReadFilesProcedure, api.DownloadPattern, api.DICOMwebPattern, api.STOWPattern,
			nopacsv1connect.AuditSealServiceExportAuditProcedure, nopacsv1connect.BackupServiceExportTenantProcedure,
			nopacsv1connect.BackupServiceCreateBackupProcedure},
		// Stopped last first: the event keeper, the imports (each finishing
		// the file it hands in), the scrubs, the deid jobs and the senders
		// (each giving its unit back), then the receivers and pipelines,
		// then each tenant's background work and switches (a switch before
		// its swap refused, once no pipeline writes), then the sealing
		// pass's timer, then the feeds. The corrections'
		// engines start with them but stop after the drain (AfterDrain).
		Reception: runners{closer{feeds}, sealer, work, sup, startOnly{engines}, senders, deids, scrubs, rebuilds, imports, events.NewKeeper(events.KeeperConfig{
			Tenants: keptFeeds{tenants: registryTenants{master: d.Master, registry: reg}, registry: reg, values: settingvalue.Reader{Master: d.Master}},
			Logger:  d.Log,
		})},
		// Once the server has drained its requests: the corrections' pass
		// and background work (each at its next step), whose storages a
		// request in flight may still write through, so they are closed only
		// once no request holds them; then the audit's last pass, which seals
		// the entries the requests and the engines wrote.
		AfterDrain: stopThen(engines, sealer.Last),
		Counters:   func() []obs.Counter { return append(sealCounters(sealer.Stats()), readCounters(readCounts)...) },
		Gauges:     func() []obs.Gauge { return append(sealGauges(sealer.Stats(), time.Now()), copyGauges(work, scrubs)...) },
	}, nil
}

// closeRegistry closes a registry that will not be served, within the
// server's close deadline.
func closeRegistry(reg *registry.Registry) {
	ctx, cancel := context.WithTimeout(context.Background(), server.DefaultClose)
	defer cancel()
	_ = reg.Close(ctx)
}

// stateCounts counts the tenants in each state for /metrics. A tenant's
// state is the registry's current view, which costs no database read; only
// the names come from the master, since the registry does not list its
// tenants. They are read with one paged listing at most every
// tenantNamesTTL, so a scrape every few seconds costs the master nothing
// most of the time; a tenant created or removed shows within that time. A
// tenant the registry does not hold yet counts in the master's state.
type stateCounts struct {
	master   master.Store
	registry *registry.Registry
	now      func() time.Time

	mu    sync.Mutex
	rows  []master.Tenant
	until time.Time
}

// byState returns how many tenants are in each state.
func (s *stateCounts) byState() map[string]int {
	rows := s.names()
	out := make(map[string]int, 4)
	for _, row := range rows {
		state := row.State
		if v, ok := s.registry.State(row.Name); ok {
			state = v.State
		}
		out[string(state)]++
	}
	return out
}

// names returns the tenants as last read from the master, reading them
// again when the copy is older than tenantNamesTTL. A read that fails keeps
// the last copy, so a master that does not answer leaves the counts as they
// were rather than empty.
func (s *stateCounts) names() []master.Tenant {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Before(s.until) {
		return s.rows
	}
	ctx, cancel := context.WithTimeout(context.Background(), tenantNamesTimeout)
	defer cancel()
	var rows []master.Tenant
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		batch, next, err := s.master.ListTenants(ctx, page)
		if err != nil {
			return s.rows
		}
		rows = append(rows, batch...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	s.rows, s.until = rows, now.Add(tenantNamesTTL)
	return rows
}
