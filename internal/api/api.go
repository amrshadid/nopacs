package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive/stow"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RequestIDHeader is the response header that carries the request's id
// (access.proto, "The request id"). The server sets it; a handler that has no
// Decision reads it back to name the request in its log line.
const RequestIDHeader = "Nopacs-Request-Id"

// internalMessage is the whole message of every INTERNAL answer: the contract
// says it tells nothing about the cause, which is in the log.
const internalMessage = "internal error"

// Config is what New needs. Access, Registry, Master, NewID and Keys are
// required; every other field has a default.
type Config struct {
	// Access is the access package: keys and grants are changed through it,
	// and reads are recorded through its audit writer.
	Access *access.Access
	// Registry holds every tenant's store; tenants are created and removed
	// through it. It seals a tenant's database address with the key file it
	// was given, so this package needs no key file of its own.
	Registry *registry.Registry
	// Master is the master database: tenants, the installation's settings and
	// its audit log are read from it.
	Master master.Store
	// NewID makes a new tenant's id: 1 to 64 of A-Z, a-z, 0-9, "_" and "-",
	// unique among tenants.
	NewID func() string
	// Log receives a line for each internal fault, with the request's id and
	// its cause, and for each read whose audit entry the tenant's log
	// refused. Nil discards.
	Log *slog.Logger
	// Settings is the list of settings the server knows. Nil is
	// settings.MustBuiltin(), the list the binary ships; a test passes a
	// table with definitions of its own added, so no setting made for tests
	// ships.
	Settings *settings.Table
	// Keys is the installation key file. An S3 storage's secret access key
	// is sealed with it, bound to the storage, before it reaches the
	// tenant's store, and opened with it only to test the storage.
	Keys *secret.Keys
	// Now is the server's clock, which the time of every change made
	// through the tenant's store is read from. Nil is time.Now.
	Now func() time.Time
	// Receivers answers a receiver's health. Nil answers every receiver
	// down, as a server that runs none (notStarted).
	Receivers Receivers
	// Incoming removes files from the incoming area after a discard or a
	// rejection is recorded. Nil removes nothing and leaves them to the
	// reception pipeline (leaveForPipeline).
	Incoming Incoming
	// Headers reads a quarantined file's header. Nil reads none (noHeaders).
	Headers Headers
	// HeldFiles reads a quarantined file's bytes for a trial run of a
	// profile. Nil reads none (noHeldFiles).
	HeldFiles HeldFiles
	// Live answers what the server holds in memory of a tenant, for a
	// status. Nil holds nothing (noLive).
	Live Live
	// Feeds streams tenants' events for Watch. Nil answers every Watch
	// unavailable.
	Feeds Feeds
	// SenderHealths answers a sender's health, and is told when a tenant's
	// senders change if it implements SenderChanges. Nil answers every
	// sender down when disabled and unknown otherwise, as a server that
	// sends nothing yet (unknownSenders).
	SenderHealths SenderHealths
	// SenderTests tries a sender's peer for TestSender. Nil answers every
	// test unavailable (noSenderTests).
	SenderTests SenderTester
	// STOW finds the running stow receiver that takes a key's STOW-RS
	// requests. Nil takes none: every such request is refused.
	STOW *stow.Directory
	// Reads counts the reads of archived bytes open in the server, shared
	// with the senders, so one bound (checkedread.MaxServerReads) covers
	// the API's streams and downloads and the instances senders read. Nil
	// gives the API a count of its own.
	Reads *checkedread.Slots
	// DeidMemory is the server's bound on de-identification's memory
	// (deid.memory), shared with the reception pipelines and the deid
	// jobs, which a trial run takes its copies of a file from. Nil gives
	// the API a bound of its own at deid.memory.
	DeidMemory check.Memory
	// Installation is the installation's id, which every audit checkpoint
	// it signs names. Empty answers every AuditSealService call as an
	// internal fault: a server always has one.
	Installation string
	// ImportGuarded are the server's own directories no import root may
	// be, contain or lie inside: its data directory, its incoming area and
	// the directory of its key file (place.CheckImportRoots). Nil guards
	// none beyond the storage roots and the other import roots.
	ImportGuarded []string
	// Corrections holds every tenant's corrections engine, which
	// CorrectionService's changes act through, and runs what they leave to
	// the background. Nil answers each change of a correction unavailable;
	// the reads of corrections need none.
	Corrections *correct.Engines
	// ReplaceSources makes the file a replace puts from a held quarantined
	// file. Nil answers every replace unavailable (noReplaceSources).
	ReplaceSources ReplaceSources
	// Handoffs drains a tenant's writers for an export and checks its
	// owner records for a reclaim. Nil answers both unavailable.
	Handoffs Handoffs
}

// Handlers is the services of the contract this package serves, each an
// implementation of the generated Connect handler interface, ready to
// register on a connect.Server behind the server's access check.
type Handlers struct {
	// Tenants is TenantService.
	Tenants nopacsv1connect.TenantServiceHandler
	// Access is AccessService.
	Access nopacsv1connect.AccessServiceHandler
	// Settings is SettingsService.
	Settings nopacsv1connect.SettingsServiceHandler
	// Audit is AuditService.
	Audit nopacsv1connect.AuditServiceHandler
	// Storages is StorageService.
	Storages nopacsv1connect.StorageServiceHandler
	// Receivers is ReceiverService.
	Receivers nopacsv1connect.ReceiverServiceHandler
	// Receptions is ReceptionService.
	Receptions nopacsv1connect.ReceptionServiceHandler
	// Quarantine is QuarantineService.
	Quarantine nopacsv1connect.QuarantineServiceHandler
	// Monitor is MonitorService.
	Monitor nopacsv1connect.MonitorServiceHandler
	// Metadata is MetadataService.
	Metadata nopacsv1connect.MetadataServiceHandler
	// Catalog is CatalogService.
	Catalog nopacsv1connect.CatalogServiceHandler
	// Data is DataService.
	Data nopacsv1connect.DataServiceHandler
	// Senders is SenderService.
	Senders nopacsv1connect.SenderServiceHandler
	// Jobs is JobService.
	Jobs nopacsv1connect.JobServiceHandler
	// Profiles is ProfileService.
	Profiles nopacsv1connect.ProfileServiceHandler
	// Deid is DeidService.
	Deid nopacsv1connect.DeidServiceHandler
	// AuditSeal is AuditSealService.
	AuditSeal nopacsv1connect.AuditSealServiceHandler
	// Corrections is CorrectionService.
	Corrections nopacsv1connect.CorrectionServiceHandler
	// Backups is BackupService.
	Backups nopacsv1connect.BackupServiceHandler

	// c is what the services share, kept for the test seams
	// (export_test.go).
	c *core
}

// New returns the services over cfg. It fails as Internal when a required
// field is missing.
func New(cfg Config) (Handlers, error) {
	if cfg.Access == nil || cfg.Registry == nil || cfg.Master == nil || cfg.NewID == nil || cfg.Keys == nil {
		return Handlers{}, fault.New(fault.Internal, "api: the access package, the registry, the master, an id source and the installation key file are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	known := cfg.Settings
	if known == nil {
		known = settings.MustBuiltin()
	}
	c := &core{
		access: cfg.Access, reg: cfg.Registry, master: cfg.Master, newID: cfg.NewID, log: log, settings: known,
		keys: cfg.Keys, now: cfg.Now, receivers: cfg.Receivers, incoming: cfg.Incoming, headers: cfg.Headers,
		live: cfg.Live, feeds: cfg.Feeds, stowReceivers: cfg.STOW, senders: cfg.SenderHealths, senderTests: cfg.SenderTests,
		reads: cfg.Reads, heldFiles: cfg.HeldFiles, trials: make(chan struct{}, MaxTrialsAtOnce), deidMemory: cfg.DeidMemory,
		installation: cfg.Installation, importGuarded: cfg.ImportGuarded, corrections: cfg.Corrections, replaceSources: cfg.ReplaceSources,
		handoffs: cfg.Handoffs,
	}
	if c.replaceSources == nil {
		c.replaceSources = noReplaceSources{}
	}
	if c.deidMemory == nil {
		c.deidMemory = check.NewBudget(c.values().DeidMemory, check.BoundInterval)
	}
	if c.heldFiles == nil {
		c.heldFiles = noHeldFiles{}
	}
	if c.reads == nil {
		c.reads = &checkedread.Slots{}
	}
	if c.live == nil {
		c.live = noLive{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.receivers == nil {
		c.receivers = notStarted{}
	}
	if c.senders == nil {
		c.senders = unknownSenders{}
	}
	if c.senderTests == nil {
		c.senderTests = noSenderTests{}
	}
	if c.incoming == nil {
		c.incoming = leaveForPipeline{}
	}
	if c.headers == nil {
		c.headers = noHeaders{}
	}
	return Handlers{
		Tenants:     tenantService{c},
		Access:      accessService{c},
		Settings:    settingsService{c},
		Audit:       auditService{c},
		Storages:    storageService{c},
		Receivers:   receiverService{c},
		Receptions:  receptionService{c},
		Quarantine:  quarantineService{c},
		Monitor:     monitorService{c},
		Metadata:    metadataService{c},
		Catalog:     catalogService{c},
		Data:        dataService{c},
		Senders:     senderService{c},
		Jobs:        jobService{c},
		Profiles:    profileService{c},
		Deid:        deidService{c},
		AuditSeal:   auditSealService{c},
		Corrections: correctionService{c},
		Backups:     backupService{c},
		c:           c,
	}, nil
}

// Register registers the services on srv.
func (h Handlers) Register(srv *connect.Server) {
	nopacsv1connect.RegisterTenantServiceHandler(srv, h.Tenants)
	nopacsv1connect.RegisterAccessServiceHandler(srv, h.Access)
	nopacsv1connect.RegisterSettingsServiceHandler(srv, h.Settings)
	nopacsv1connect.RegisterAuditServiceHandler(srv, h.Audit)
	nopacsv1connect.RegisterStorageServiceHandler(srv, h.Storages)
	nopacsv1connect.RegisterReceiverServiceHandler(srv, h.Receivers)
	nopacsv1connect.RegisterReceptionServiceHandler(srv, h.Receptions)
	nopacsv1connect.RegisterQuarantineServiceHandler(srv, h.Quarantine)
	nopacsv1connect.RegisterMonitorServiceHandler(srv, h.Monitor)
	nopacsv1connect.RegisterMetadataServiceHandler(srv, h.Metadata)
	nopacsv1connect.RegisterCatalogServiceHandler(srv, h.Catalog)
	nopacsv1connect.RegisterDataServiceHandler(srv, h.Data)
	nopacsv1connect.RegisterSenderServiceHandler(srv, h.Senders)
	nopacsv1connect.RegisterJobServiceHandler(srv, h.Jobs)
	nopacsv1connect.RegisterProfileServiceHandler(srv, h.Profiles)
	nopacsv1connect.RegisterDeidServiceHandler(srv, h.Deid)
	nopacsv1connect.RegisterAuditSealServiceHandler(srv, h.AuditSeal)
	nopacsv1connect.RegisterCorrectionServiceHandler(srv, h.Corrections)
	nopacsv1connect.RegisterBackupServiceHandler(srv, h.Backups)
}

// decisionKey is the context key of the Decision. Unexported, so only
// WithDecision can set it.
type decisionKey struct{}

// WithDecision returns ctx carrying d, the Decision access.Check made for the
// request. The server calls it once per request, after the check allowed the
// request and before the handler runs.
func WithDecision(ctx context.Context, d access.Decision) context.Context {
	return context.WithValue(ctx, decisionKey{}, d)
}

// DecisionFrom returns the Decision WithDecision put in ctx, and whether
// there is one that carries a caller. A zero Decision is reported as none:
// only access.Check makes one that carries rights.
func DecisionFrom(ctx context.Context) (access.Decision, bool) {
	d, ok := ctx.Value(decisionKey{}).(access.Decision)
	if !ok {
		return access.Decision{}, false
	}
	p := d.Principal()
	if _, isKey := p.Key(); !isKey && !p.IsLocalOperator() {
		return access.Decision{}, false
	}
	return d, true
}

// core is what every service shares.
type core struct {
	access *access.Access
	reg    *registry.Registry
	master master.Store
	newID  func() string
	log    *slog.Logger
	// settings is the list of settings the server knows.
	settings *settings.Table
	// keys seals and opens an S3 storage's secret access key.
	keys *secret.Keys
	// now is the server's clock.
	now func() time.Time
	// receivers answers a receiver's health.
	receivers Receivers
	// senders answers a sender's health.
	senders SenderHealths
	// senderTests tries a sender's peer.
	senderTests SenderTester
	// incoming carries a release, a retry and the removal of bytes to the
	// tenant's reception pipeline.
	incoming Incoming
	// headers reads a quarantined file's header.
	headers Headers
	// heldFiles reads a quarantined file's bytes.
	heldFiles HeldFiles
	// bounds holds the buckets that bound forward lookups and lookup
	// imports (DeidService).
	bounds rateBuckets
	// trials holds a place for each trial run or explanation running, at
	// most MaxTrialsAtOnce, since each holds its files in memory.
	trials chan struct{}
	// live answers what is held in memory of a tenant.
	live Live
	// feeds streams tenants' events.
	feeds Feeds
	// stowReceivers finds the stow receiver that takes a key's requests.
	stowReceivers *stow.Directory
	// reads counts the streams of ReadFiles and the downloads open now,
	// for their bounds, in the server's one count, which the senders'
	// reads share (Config.Reads).
	reads *checkedread.Slots
	// deidMemory bounds what trial runs hold in memory, with the rest of
	// de-identification's work (Config.DeidMemory).
	deidMemory check.Memory
	// installation is the installation's id (Config.Installation).
	installation string
	// importGuarded are the directories no import root may touch
	// (Config.ImportGuarded).
	importGuarded []string
	// corrections holds the tenants' corrections engines
	// (Config.Corrections); nil for none.
	corrections *correct.Engines
	// replaceSources makes a replace's file (Config.ReplaceSources).
	replaceSources ReplaceSources
	// handoffs drains a tenant's writers and checks its owner records
	// (Config.Handoffs); nil for none.
	handoffs Handoffs
	// exports holds a place while an export runs (one backup or export at
	// a time on a server, as the contract says) and the tenants a reclaim
	// takes back, so no reclaim runs beside an export of its tenant.
	exports handoffSlot
	// places orders the changes that depend on where the installation lets
	// data go (its roots, endpoints, ports and bind addresses) against the
	// changes of those settings: a storage or a receiver is checked and
	// stored under a read lock, a setting that could take away what one
	// uses is checked and stored under the write lock, so neither passes the
	// other unseen. One process serves an installation (the lock on its
	// data directory), so a lock in memory is enough.
	places sync.RWMutex
	// seams are set by tests only (export_test.go).
	seams struct {
		// probe, when set, stands for place.SystemProbe in the choice of a
		// free port.
		probe place.Probe
		// importRoots, when set, stands for the import roots an import
		// job's check reads (core.importRoots), so a test can hold the
		// check as a file system that does not answer.
		importRoots dirimport.Roots
		// importCheck, when not zero, stands for importCheckTimeout.
		importCheck time.Duration
		// summarySend, when set, is the error an export's summary meets
		// in place of its Send, after the data was streamed.
		summarySend error
	}
}

// decision returns the request's Decision. A request without one reached the
// handler without the server's check: it is refused as INTERNAL, logged, and
// never decided here (doc.go, "What this package must never do").
func (c *core) decision(ctx context.Context, procedure string) (access.Decision, error) {
	if d, ok := DecisionFrom(ctx); ok {
		return d, nil
	}
	requestID := ""
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		requestID = info.ResponseHeader().Get(RequestIDHeader)
	}
	c.log.Error("api: a request reached its handler without an access decision and was refused",
		"method", procedure, "request_id", requestID)
	return access.Decision{}, connect.NewError(connect.CodeInternal, internalMessage)
}

// changing returns the Decision of a request that changes something, after
// the access package has checked the caller's key again (access.Confirm),
// before the handler validates the request or reads anything it names. So a
// key that lapsed since the check is answered UNAUTHENTICATED or
// PERMISSION_DENIED whatever it asked, as the contract's order of checks
// says, and never told whether what it named exists or whether its request
// was valid. The change itself checks the key again in its transaction. A
// refusal is recorded by the access package.
func (c *core) changing(ctx context.Context, procedure string) (access.Decision, error) {
	d, err := c.decision(ctx, procedure)
	if err != nil {
		return d, err
	}
	if d, err = c.access.Confirm(ctx, d); err != nil {
		return d, c.toAPI(d, err)
	}
	return d, nil
}

// toAPI returns the API error for err, an error a package below already
// recorded in the audit log or one that needs no entry. An internal fault's
// cause is logged with the request's id and replaced by the fixed message; an
// unavailable one is logged too, since a database that does not answer is
// for the operator to see.
func (c *core) toAPI(d access.Decision, err error) error {
	switch fault.KindOf(err) {
	case fault.Internal:
		e := d.Entry(master.OutcomeFailed, "", access.Facts{})
		c.log.Error("api: internal error", "request_id", e.RequestID, "method", e.Method, "error", err.Error())
		return connect.NewError(connect.CodeInternal, internalMessage)
	case fault.Unavailable:
		e := d.Entry(master.OutcomeFailed, "", access.Facts{})
		c.log.Warn("api: unavailable", "request_id", e.RequestID, "method", e.Method, "error", err.Error())
	}
	return codes.ToConnect(err)
}

// outcomeOf returns the outcome and the reason an audit entry records for a
// request that did not complete with err. A refusal is refused; anything else
// the caller was allowed to try is failed. The reason is the message the
// caller may see, except for an internal fault, whose cause is only in the
// log, under the same request id.
func outcomeOf(err error) (master.Outcome, string) {
	switch fault.KindOf(err) {
	case fault.Denied:
		return master.OutcomeRefused, fault.Message(err)
	case fault.Internal:
		return master.OutcomeFailed, "internal error; the cause is in the server's log under this request's id"
	}
	return master.OutcomeFailed, fault.Message(err)
}

// record hands the audit entry of a request that changed nothing (a read, or
// a request that did not complete) to the access package's audit writer, in
// the one place that chooses its log: the tenant's log when st, the store of
// the tenant the request concerns, is in hand (the contract: "one entry in
// the log of the scope asked for"), and the installation's log otherwise. It
// never blocks and never fails the request: an entry the tenant's log does
// not take is written to the installation's log with access.DetailRerouted,
// and one that cannot be queued is counted. facts is what the handler learned
// after the check.
func (c *core) record(d access.Decision, st tenant.Store, outcome master.Outcome, reason string, facts access.Facts) {
	if st != nil {
		c.access.RecordTo(st, d, outcome, reason, facts)
		return
	}
	c.access.Record(d, outcome, reason, facts)
}

// fail records a request that did not complete in the installation's log,
// and returns its API error. It is for an error no package below has
// recorded, met before any tenant's store was in hand, in a request whose
// facts the check already has.
func (c *core) fail(d access.Decision, err error) error {
	return c.failWith(d, nil, access.Facts{}, err)
}

// failIn is fail for a request into a tenant whose store is in hand: its
// entry goes to the tenant's log.
func (c *core) failIn(d access.Decision, st tenant.Store, err error) error {
	return c.failWith(d, st, access.Facts{}, err)
}

// failWith records a request that did not complete, in the log record
// chooses for st, with the facts the handler learned, and returns its API
// error. A change the master refused because the caller's key may no longer
// act is answered as the check answers such a key (access.RefusedAtChange)
// and recorded as refused in the installation's log, whatever st is, as the
// check's own refusals are: a refused caller causes no write in a tenant's
// database.
func (c *core) failWith(d access.Decision, st tenant.Store, facts access.Facts, err error) error {
	if reason, refusal := access.RefusedAtChange(err); refusal != nil {
		c.record(d, nil, master.OutcomeRefused, reason, facts)
		return c.toAPI(d, refusal)
	}
	outcome, reason := outcomeOf(err)
	c.record(d, st, outcome, reason, facts)
	return c.toAPI(d, err)
}

// done records a read that answered, in the log record chooses for st, after
// the read.
func (c *core) done(d access.Decision, st tenant.Store) {
	c.record(d, st, master.OutcomeDone, "", access.Facts{})
}

// noTenant is the answer for a tenant a lookup after the check did not find
// (it was removed meanwhile, or the registry does not hold it): NOT_FOUND for
// a caller who may list every tenant, and otherwise PERMISSION_DENIED with the
// same message as for no grant, so a later lookup cannot undo what the check
// decided (access.proto, "A tenant that does not exist").
func noTenant(d access.Decision) error {
	if d.Principal().CanListTenants() {
		return fault.New(fault.NotFound, access.MessageNoTenant)
	}
	return fault.New(fault.Denied, access.MessageDenied)
}

// reroutedValue is the value of access.DetailRerouted on the entry of a
// request whose tenant's log could not be had at all, so the entry was
// written to the installation's log from the start.
const reroutedValue = "the tenant's database is not ready; written to the installation's log"

// tenantStore returns the store of the tenant named name, for this request,
// after making sure it is the tenant the check allowed: a tenant removed and
// created again under the same name between the check and now has another
// id, and the grant the check read was on the old one. Such a tenant, and one
// the registry or the master no longer holds, is answered as noTenant says;
// to the caller, the tenant it was allowed on is gone. A tenant that is not
// ready is Unavailable.
//
// The store is read before the master's row: a removal takes the row away
// before it unregisters the store, and a creation writes the row before it
// registers one, so a row that still has the checked id means the store read
// before it was that tenant's too.
//
// A request whose requirement was admin on the installation, though it names
// a tenant (a change of a setting the installation sets on each tenant), has
// no tenant in its Decision: its caller may act on every tenant, so the
// tenant that holds the name now is the one it acts on, and one that holds
// none is answered NotFound, as to any caller who may list every tenant.
func (c *core) tenantStore(ctx context.Context, d access.Decision, name string) (tenant.Store, error) {
	st, serr := c.reg.Store(name)
	if fault.KindOf(serr) == fault.Precondition && readsOnly(ctx, d) {
		// A moved or fenced tenant serves reads; every change is refused
		// with the reason the registry gave (master.ReadOnlyError).
		st, serr = c.reg.Reader(name)
	}
	if fault.KindOf(serr) == fault.NotFound {
		return nil, noTenant(d)
	}
	ref, ok := d.Tenant()
	row, err := c.master.TenantByName(ctx, name)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil, noTenant(d)
	case err != nil:
		return nil, err
	case !ok && d.Principal().Role(master.Installation).Includes(master.RoleAdmin):
		return st, serr
	case !ok || row.ID != ref.ID:
		return nil, noTenant(d)
	}
	return st, serr
}

// readOnlyKey is the context key a plain HTTP route that only reads sets
// (readingOnly).
type readOnlyKey struct{}

// readingOnly returns ctx marked as a request that only reads, for a plain
// HTTP route (a download, a DICOMweb search or retrieval), which has no
// procedure whose contract says so.
func readingOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// readsOnly reports whether the request d decided changes nothing: a plain
// HTTP route marked by readingOnly, or a method whose contract marks it
// idempotency_level NO_SIDE_EFFECTS, as every read of the contract is. A
// method that is not marked so is held to be a change, so a read-only
// tenant refuses it.
func readsOnly(ctx context.Context, d access.Decision) bool {
	if v, _ := ctx.Value(readOnlyKey{}).(bool); v {
		return true
	}
	return noSideEffects(d.Method())
}

// noSideEffects reports whether procedure, a full procedure name such as
// "/nopacs.v1.CatalogService/ListSubjects", is a method of the contract
// marked idempotency_level NO_SIDE_EFFECTS.
func noSideEffects(procedure string) bool {
	service, method, ok := strings.Cut(strings.TrimPrefix(procedure, "/"), "/")
	if !ok {
		return false
	}
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return false
	}
	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return false
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return false
	}
	opts, ok := md.Options().(*descriptorpb.MethodOptions)
	return ok && opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS
}

// sinkFor returns the log every later record of a request on scope goes to,
// chosen once, as soon as the scope is known to be valid: nil for the
// installation, and for a tenant its store (an allowed request that concerns
// a tenant is recorded in its log, whatever its outcome). When the tenant's
// store cannot be had, the request is recorded here and its error returned:
// in the installation's log, with the tenant's name and, when the tenant is
// not ready, access.DetailRerouted. details are the request's audit details,
// which the rerouted entry keeps.
func (c *core) sinkFor(ctx context.Context, d access.Decision, scope access.Scope, details map[string]string) (tenant.Store, error) {
	if scope.IsInstallation() {
		return nil, nil
	}
	st, err := c.tenantStore(ctx, d, scope.Tenant)
	if err == nil {
		return st, nil
	}
	facts := access.Facts{TenantName: scope.Tenant}
	if fault.KindOf(err) == fault.Unavailable {
		facts.Details = make(map[string]string, len(details)+1)
		for k, v := range details {
			facts.Details[k] = v
		}
		facts.Details[access.DetailRerouted] = reroutedValue
	}
	return nil, c.failWith(d, nil, facts, err)
}
