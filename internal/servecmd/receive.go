package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// IncomingDirName holds the incoming area: <data-dir>/incoming/<tenant>.
// Each tenant's receiver certificates are in server.TLSDir.
const IncomingDirName = "incoming"

// Runner is a part of the server that runs beside the API: started once the
// server listens, stopped before the server stops, so it never uses a
// tenant's database after the registry has closed it.
type Runner interface {
	// Start starts it; it returns without waiting for what it starts.
	Start(ctx context.Context) error
	// Stop stops it within ctx.
	Stop(ctx context.Context) error
}

// newReception returns the supervisor of every tenant's receivers and
// reception pipeline, over the registry's tenants. forwarded is told of a
// tenant whose reception reached done, which may have made forward jobs.
// memory is the server's bound on de-identification's memory, which every
// pipeline's check after takes its copy of a file from.
//
// Each tenant's pipelines take its scan locks from locks, shared with its
// corrections engine, and each is published in pipes while it runs, for
// the replaces made through it. Its storages are opened to write with
// cleanups, the server's one (writingOpener).
//
// Each pipeline asks its tenant's switch gate before it writes to a
// primary (receive.Config.Pause), and the switcher is made, ending the
// switches a stopped server left, before the pipeline starts; a commit
// wakes the tenant's replicator as well as its event streams.
func newReception(d Deps, reg *registry.Registry, wake, forwarded func(tenantName string), stowReceivers *stow.Directory, memory check.Memory,
	locks *tenantScanLocks, pipes *runningPipelines, cleanups *place.Cleanups, work *tenantWork) (*supervise.Supervisor, error) {
	incomingRoot := filepath.Join(d.DataDir, IncomingDirName)
	if err := os.MkdirAll(incomingRoot, 0o700); err != nil {
		return nil, fault.Wrap(fault.Unavailable, err, "the incoming area cannot be made in the data directory")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	values := settingvalue.Reader{Master: d.Master}
	// The pipeline keeps a storage open across steps (tenantStorages); what
	// a crash left on it is removed by the first store of the process that
	// opens it (writingOpener). The supervisor's look at a storage's space
	// opens and closes it each time, for reading.
	pipelineOpener := writingOpener(values, d.Keys, cleanups)
	spaceOpener := place.Opener{Values: values, Keys: d.Keys, StaleUploadAge: -1}
	return supervise.New(supervise.Config{
		Tenants:      registryTenants{master: d.Master, registry: reg},
		Installation: d.Master,
		StartPipeline: func(ctx context.Context, name string, st tenant.Pipeline, g supervise.Gates) (supervise.Pipeline, error) {
			// The tenant's keyed hash for its naming rules: without it a
			// default rule names nothing, so the pipeline does not start
			// and the supervisor shows the receivers down until it can.
			hasher, err := reg.NamingHasher(ctx, name)
			if err != nil {
				return nil, err
			}
			full, err := dbOf(st)
			if err != nil {
				return nil, err
			}
			pause, committed, err := work.receptionWrites(ctx, name, full, wake)
			if err != nil {
				return nil, err
			}
			storages := newTenantStorages(pipelineOpener, d.Master, name, st)
			p, err := receive.New(receive.Config{
				Tenant: name, Store: st, IncomingRoot: incomingRoot, Logger: log, Clock: clock.System{},
				OpenStorage: storages.Open, Hasher: hasher, HeaderLimit: values.HeaderLimit,
				// deid.pixel-limit as it applies on the tenant, read live.
				PixelLimit: func(ctx context.Context) (int64, error) {
					src, ok := st.(settingvalue.Source)
					if !ok {
						return 0, fault.New(fault.Internal, "the tenant's database keeps no settings")
					}
					return values.OnTenant(src).PixelLimit(ctx)
				},
				LowDisk: g.LowDisk, OverLimit: g.OverLimit, Pause: pause, Wake: committed,
				Forwarded: func() { forwarded(name) },
				// The keys of a receiver that de-identifies, read when its
				// first file comes and kept by the pipeline; never logged.
				DeidKeys: func(ctx context.Context) (engine.Keys, error) { return reg.DeidKeys(ctx, name) },
				Check: func(maxFileBytes int64) receive.Checker {
					return check.New(check.Options{MaxFileBytes: maxFileBytes, Memory: memory})
				},
				ScanLocks: locks.of(name),
			})
			if err != nil {
				return nil, err
			}
			if _, err := p.Start(ctx); err != nil {
				_ = storages.Close()
				return nil, err
			}
			pipes.set(name, p)
			return pipelineRun{Pipeline: p, storages: storages, name: name, pipes: pipes}, nil
		},
		OpenStorage: func(ctx context.Context, tenantName string, st tenant.Pipeline, storage string) (blob.Store, error) {
			t, err := d.Master.TenantByName(ctx, tenantName)
			if err != nil {
				return nil, err
			}
			row, err := st.Storage(ctx, storage)
			if err != nil {
				return nil, err
			}
			return spaceOpener.OpenForReading(ctx, place.TenantOf(t.ID, tenantName, st), row)
		},
		NewListener: func(spec supervise.ListenerSpec) (supervise.Listener, error) {
			if spec.Receiver.Type == tenant.ReceiverSTOW {
				// No port: found by the STOW-RS route while it runs.
				return stow.New(stow.Config{
					Tenant: spec.Tenant, Receiver: spec.Receiver, Sink: spec.Sink, Directory: stowReceivers,
					Refusal: stow.Refusal(spec.Refusal), Report: stow.Report(spec.Report),
					Logger: log.With("tenant", spec.Tenant),
				})
			}
			return dimse.New(dimse.Config{
				Receiver: spec.Receiver, BindAddress: spec.BindAddress, Sink: spec.Sink,
				TLSDir:  server.TLSDir(d.DataDir, spec.Tenant),
				Refusal: dimse.Refusal(spec.Refusal), Report: dimse.Report(spec.Report),
				Logger: log.With("tenant", spec.Tenant),
			})
		},
		IncomingUsage: func(ctx context.Context) (blob.Usage, error) {
			s, err := dir.OpenForReading(incomingRoot)
			if err != nil {
				return blob.Usage{}, err
			}
			defer func() { _ = s.Close() }()
			return s.Usage(ctx)
		},
		// A receiver whose storage switches to its replica refuses new
		// associations as a temporary failure while the switch runs.
		Switches: work.switchGate,
		Wake:     wake,
		Logger:   log,
	})
}

// pipelineRun is one tenant's running pipeline with the storages it keeps
// open, closed once it has stopped.
type pipelineRun struct {
	*receive.Pipeline
	storages *tenantStorages
	// name is the tenant's, under which pipes publishes the pipeline.
	name  string
	pipes *runningPipelines
}

// Stop withdraws the pipeline from the replaces, stops it, then closes its
// storages: no step holds one once Stop has returned.
func (r pipelineRun) Stop(ctx context.Context) error {
	r.pipes.drop(r.name, r.Pipeline)
	err := r.Pipeline.Stop(ctx)
	return errors.Join(err, r.storages.Close())
}

// registryTenants is the supervisor's view of the installation's tenants:
// their names from the master, their databases from the registry.
type registryTenants struct {
	master   master.Store
	registry *registry.Registry
}

// Names reads every tenant's name from the master, page after page.
func (r registryTenants) Names(ctx context.Context) ([]string, error) {
	var out []string
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		batch, next, err := r.master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, t := range batch {
			out = append(out, t.Name)
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// Store returns the tenant's open database from the registry.
func (r registryTenants) Store(name string) (tenant.Store, error) { return r.registry.Store(name) }

// receiverHealth answers the API's question of a receiver's health from
// the supervisor, in the API's terms, and passes the API's notice of a
// change of receivers to it.
type receiverHealth struct{ s *supervise.Supervisor }

var _ api.ReceiverChanges = receiverHealth{}

// downReasons maps the contract's down reasons to the API's.
var downReasons = map[tenant.ReceiverDownReason]api.DownReason{
	tenant.DownDisabled: api.DownDisabled, tenant.DownAddressInUse: api.DownAddressInUse,
	tenant.DownPermissionDenied: api.DownPermissionDenied, tenant.DownAddressNotAvailable: api.DownAddressNotAvailable,
	tenant.DownTLS: api.DownTLS, tenant.DownNearlyFull: api.DownNearlyFull, tenant.DownTenantNotReady: api.DownTenantNotReady,
	tenant.DownStopping: api.DownStopping, tenant.DownOther: api.DownOther, tenant.DownIncomingLimit: api.DownIncomingLimit,
}

// ReceiversChanged tells the supervisor at once that a tenant's receivers
// changed (api.ReceiverChanges), so a receiver added, changed or removed is
// started, restarted or stopped without waiting for its periodic read.
func (h receiverHealth) ReceiversChanged(tenantName string) { h.s.Poke(tenantName) }

// Health returns the supervisor's health of the receiver.
func (h receiverHealth) Health(tenantName string, r tenant.Receiver) api.Health {
	s := h.s.Health(tenantName, r)
	out := api.Health{Detail: s.Detail, Since: s.Since}
	switch s.State {
	case tenant.ReceiverStarting:
		out.State = api.HealthStarting
	case tenant.ReceiverUp:
		out.State = api.HealthUp
	default:
		out.State = api.HealthDown
		out.Reason = downReasons[s.Reason]
		if out.Reason == api.DownNone {
			out.Reason = api.DownOther
		}
	}
	return out
}

var _ api.Live = receiverHealth{}

// TenantLive returns what the supervisor holds of the tenant, as the
// handlers' status reads it.
func (h receiverHealth) TenantLive(tenantName string) (api.TenantLive, bool) {
	v, ok := h.s.View(tenantName)
	if !ok {
		return api.TenantLive{}, false
	}
	out := api.TenantLive{
		Storages: make(map[string]api.StorageSpace, len(v.Storages)),
		Throughput: api.Throughput{FilesReceived: v.Throughput.FilesReceivedPerSecond, BytesReceived: v.Throughput.BytesReceivedPerSecond,
			FilesArchived: v.Throughput.FilesArchivedPerSecond, BytesArchived: v.Throughput.BytesArchivedPerSecond},
	}
	for _, r := range v.Receivers {
		out.Receivers = append(out.Receivers, r.Receiver)
	}
	for name, s := range v.Storages {
		out.Storages[name] = api.StorageSpace{FreeBytes: s.FreeBytes, NearlyFull: s.NearlyFull}
	}
	return out, true
}

// pipelineDecisions is the API's Incoming: a release and a retry run
// through the tenant's running reception pipeline, which gives a released
// file's bytes their landed name before the store records the release and
// queues a retry's first unit at once; the removal of a rejected or
// discarded file's bytes is handed to it too. With no pipeline running (the
// tenant's database not ready), a release or a retry is refused as
// unavailable, so nothing is recorded that the bytes do not follow, and a
// removal is kept and done by the next pipeline once it has started.
type pipelineDecisions struct{ s *supervise.Supervisor }

var _ api.Incoming = pipelineDecisions{}

// errNoPipeline answers a release or a retry of a tenant whose reception
// pipeline does not run.
var errNoPipeline = fault.New(fault.Unavailable, "the tenant's reception is not running now; try again shortly")

// Release releases through the tenant's pipeline.
func (d pipelineDecisions) Release(ctx context.Context, tenantName string, _ tenant.Pipeline, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	p, ok := d.s.Decisions(tenantName)
	if !ok {
		return tenant.QuarantinedFile{}, errNoPipeline
	}
	return p.Release(ctx, rel, audit, guard)
}

// Retry retries through the tenant's pipeline, which reads its own clock.
func (d pipelineDecisions) Retry(ctx context.Context, tenantName string, _ tenant.Pipeline, id, newReceptionID, receiver string, _ time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Reception, tenant.Reception, error) {
	p, ok := d.s.Decisions(tenantName)
	if !ok {
		return tenant.Reception{}, tenant.Reception{}, errNoPipeline
	}
	return p.Retry(ctx, id, newReceptionID, receiver, audit, guard)
}

// RemoveDiscarded hands the removal to the tenant's pipeline, which
// keeps it while none runs and does it once the next has started.
func (d pipelineDecisions) RemoveDiscarded(ctx context.Context, tenantName, receptionID string) error {
	return d.s.RemoveDiscarded(ctx, tenantName, receptionID)
}

// RemoveRejected hands the removal to the tenant's pipeline, which
// keeps it while none runs and does it once the next has started.
func (d pipelineDecisions) RemoveRejected(ctx context.Context, tenantName, incoming string) error {
	return d.s.RemoveRejected(ctx, tenantName, incoming)
}

// heldHeaders reads a quarantined file's header through the tenant's running
// pipeline (api.Headers).
type heldHeaders struct{ s *supervise.Supervisor }

var (
	_ api.Headers   = heldHeaders{}
	_ api.HeldFiles = heldHeaders{}
)

// HeldFile reads the bytes of the held file through the tenant's running
// pipeline, for a trial run of a profile. A file the pipeline cannot give
// (none runs, the file is gone from the incoming area, it is past the
// bound) is ErrNoHeldFile.
func (h heldHeaders) HeldFile(ctx context.Context, tenantName string, f tenant.QuarantinedFile, limit int64) ([]byte, error) {
	if f.Incoming == "" {
		return nil, api.ErrNoHeldFile
	}
	b, ok, err := h.s.HeldFile(ctx, tenantName, f.Incoming, limit)
	switch {
	case !ok, fault.KindOf(err) == fault.NotFound, fault.KindOf(err) == fault.Precondition:
		return nil, api.ErrNoHeldFile
	case err != nil:
		return nil, err
	}
	return b, nil
}

// Header reads the header of the held file within the receiver header limit
// and writes it as DICOM JSON without bulk data (PS3.18 F.2). The cases the
// contract answers as a header it cannot give are ErrNoHeader: no pipeline
// runs for the tenant, the file is gone from the incoming area, its header
// runs past the bound or it is not a whole DICOM file.
func (h heldHeaders) Header(ctx context.Context, tenantName string, f tenant.QuarantinedFile, limit int) (string, bool, error) {
	if f.Incoming == "" {
		return "", false, api.ErrNoHeader
	}
	hdr, ok, err := h.s.HeldHeader(ctx, tenantName, f.Incoming)
	if !ok {
		return "", false, api.ErrNoHeader
	}
	var big *header.TooLargeError
	var notDICOM *header.NotDICOMError
	switch {
	case errors.As(err, &big), errors.As(err, &notDICOM), fault.KindOf(err) == fault.NotFound:
		return "", false, api.ErrNoHeader
	case err != nil:
		return "", false, err
	}
	text, err := hdr.DICOMJSONShort()
	if err != nil {
		return "", false, api.ErrNoHeader
	}
	if len(text) > limit {
		return "", true, nil
	}
	return string(text), false, nil
}
