package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Driver opens the tenant databases of one database type. Package driver
// implements it for each type (driver.SQLiteTenants); the server gives the
// registry one per type it supports, by the name a tenant's row records.
//
// An error from Open or Location must never contain the address, nor any
// part of it (host, user, password), in its message or its cause: the
// registry shows a driver error's message (fault.Message) in a tenant's state
// reason, its errors and its log, and never its Error() text, but a cause
// may still reach a log a person reads elsewhere. The message should say what
// went wrong in words an operator can act on, since it is all they see.
type Driver interface {
	// Open opens the database of the tenant whose id is id and whose name is
	// name, at address, the plaintext of the tenant's sealed address. The
	// store binds its cursors to id (tenant.Store). It does not migrate. It
	// returns Invalid for an address or a database file this driver refuses,
	// Unavailable for a database that may answer later, and Internal for the
	// rest.
	Open(ctx context.Context, id, name, address string) (tenant.Store, error)
	// Location says where the tenant's data is, holding no user and no
	// password: a file's path, or host:port/database. It returns Invalid
	// for an address this driver does not take, and is called before a
	// tenant is created, to refuse such an address before anything is
	// written. The registry replaces its error with a fixed message of the
	// same kind.
	Location(name, address string) (string, error)
}

// StateChangeInterval is the shortest time between two writes to the master
// of a tenant's change between ready and unreachable, or of a change of
// reason alone. A database that flaps writes one change a minute, not one
// per check. It bounds the writes to the master only: State answers the
// current view at once.
const StateChangeInterval = time.Minute

// The methods of the audit entries the registry writes on its own. Their
// actor is the system: no key, no local user, no address, and the detail
// "actor" set to "system".
const (
	// SystemMethod records a change of a tenant's state.
	SystemMethod = "nopacs.registry/SetTenantState"
	// StartMethod records a creation started: the tenant's row written as
	// upgrading. It carries the request's id; the request's own entry is
	// written when the creation settles.
	StartMethod = "nopacs.registry/CreateTenantStarted"
)

// The details of the registry's audit entries.
const (
	DetailActor        = "actor"
	DetailDatabaseType = "database_type"
	DetailLocation     = "data_location"
	// DetailRequestEntry, on a StartMethod entry, is the id of the request's
	// own entry, written when the creation settles.
	DetailRequestEntry = "request_entry"
	// DetailCreationRequest, on the state change that finishes a creation a
	// crash interrupted, is the id of the request that began it.
	DetailCreationRequest = "creation_request"
	// DetailUndone, on a failed creation's request entry, is "true": the row
	// the creation wrote was removed again.
	DetailUndone = "undone"
	// ActorSystem is the value of DetailActor on the registry's entries.
	ActorSystem = "system"
)

// Config is what the registry needs. Every field but Log and NewID is
// required: the clock, the interval and the deadlines are given, never
// package variables, so a test sets them.
type Config struct {
	// Master is the master database, where tenants and their states are.
	Master master.Store
	// Keys opens each tenant's sealed address.
	Keys *secret.Keys
	// Drivers opens tenant databases, by database type.
	Drivers map[string]Driver
	// Now is the clock: audit times, creation times, the time a state was
	// reached, and the once-a-minute limit on state writes are read from it.
	Now func() time.Time
	// Interval is how often every tenant is checked, and the registry
	// reconciled with the master, after Start.
	Interval time.Duration
	// OpenTimeout is the deadline of one tenant's open, ping and migration.
	OpenTimeout time.Duration
	// PingTimeout is the deadline of one check of a tenant whose store is
	// open. It is short, and takes no opening slot, so a ready tenant that
	// goes down is seen soon whatever else is being opened.
	PingTimeout time.Duration
	// MaxOpening bounds how many tenants are opened at once, so a start with
	// many tenants does not open them all in the same instant.
	MaxOpening int
	// Log receives a line for each change of a tenant's state and each
	// failure, with no address in it. Nil logs nothing.
	Log *slog.Logger
	// NewID makes the ids of the registry's audit entries. Nil uses 128
	// random bits in hex.
	NewID func() string
	// Fence, when not nil, checks the owner record of each of a tenant's
	// storages (Fence): after each open and each check of a tenant whose
	// database answers, and when CheckFence asks. A reason it returns
	// makes the tenant fenced. Nil fences nothing.
	Fence Fence
	// Unregistered, when not nil, is called with a tenant's name each time
	// the registry lets it go (it was removed, or the master no longer
	// lists it), after it is gone from the registry and before its
	// database is closed, so what runs for the tenant elsewhere (its
	// sends) can stop at once. It must not block or call the registry.
	Unregistered func(name string)
}

// State is a tenant's state as the registry sees it now.
type State struct {
	// State is the tenant's state.
	State master.TenantState
	// Reason says why, in the registry's words and its driver's safe
	// message; empty when the tenant is ready.
	Reason string
	// Since is when the tenant reached this state and reason, by
	// Config.Now; the zero time before its first check.
	Since time.Time
}

// Removed says where a removed tenant's data was left.
type Removed struct {
	// Driver is the database type.
	Driver string
	// Location is where the data is, with no user and no password; empty
	// when it cannot be told (the address did not open).
	Location string
}

// Registry holds one open store per tenant. It is safe for concurrent use.
type Registry struct {
	cfg Config
	log *slog.Logger

	// entries maps a tenant's name to its entry. The map is never changed
	// once stored: a change copies it under mu and stores the copy, so
	// Store reads it with one atomic load and no lock.
	entries atomic.Pointer[map[string]*entry]
	mu      sync.Mutex
	closed  atomic.Bool
	started bool // under mu

	// slots bounds the opens running at once; pings take none.
	slots chan struct{}

	// names serializes Create and Remove of the same name, so a removal
	// cannot slip between a creation's reading of the master and its
	// registering of the tenant. listing is held for writing while Start or
	// a reconciliation reads the tenants and registers them, and for reading
	// by Create and Remove, so neither changes a tenant between the two.
	names       nameLocks
	listing     sync.RWMutex
	reconciling atomic.Bool

	// ctx is canceled by Close; every entry's context derives from it.
	ctx    context.Context
	cancel context.CancelFunc
	// wg counts every goroutine the registry starts, and every Start,
	// Create and Remove in progress, so Close waits for all of them.
	wg sync.WaitGroup
	// inflight counts the driver calls running now, which Close reports
	// when it has to abandon them.
	inflight atomic.Int64

	// afterRow, when set by a test, runs in Create after the tenant's row
	// is written and before its database is opened; an error from it ends
	// Create there, as a crash would.
	afterRow func() error
	// fileLimit returns the process's limit on open files, or -1 when it
	// cannot be read; a test replaces it.
	fileLimit func() int64
}

// entry is one tenant.
type entry struct {
	id, name, driver string
	sealed           []byte

	// view is what Store and State read: the state and the store a request
	// gets.
	view atomic.Pointer[view]

	// ctx ends when the tenant is removed or the registry closed, which
	// ends an open in progress for it.
	ctx    context.Context
	cancel context.CancelFunc

	// opening is set while a driver call that outlived its deadline is
	// still running, so a check does not start a second one beside it.
	opening atomic.Bool

	// op orders the changes of this tenant: an open, a check, a creation's
	// steps, a removal. Store never takes it, so a slow tenant holding it
	// delays only its own changes.
	op sync.Mutex
	// The fields below are guarded by op.
	store         tenant.Store
	written       master.TenantState // the state the master holds
	writtenReason string             // and its reason
	lastWrite     time.Time          // when the registry last wrote it
	lastKind      fault.Kind         // the kind of the last failure to open
	removed       bool
	// holdWrite is set by Create while it opens the tenant: the state write
	// that settles the creation is its own, with the request's entry.
	holdWrite bool
	// readOnly is the read-only state the tenant is held in (moved or
	// fenced; "" for none), with its reason: read from the master's row
	// at registration, set by SetHandoff and by a fence check. A database
	// that answers then shows this state rather than ready.
	readOnly       master.TenantState
	readOnlyReason string
}

// view is a tenant's state and its store, replaced whole.
type view struct {
	state  master.TenantState
	reason string
	since  time.Time
	store  tenant.Store
}

// New returns a registry that has opened nothing yet. Start opens every
// tenant and starts watching them.
func New(cfg Config) (*Registry, error) {
	switch {
	case cfg.Master == nil:
		return nil, fault.New(fault.Internal, "registry: no master database")
	case cfg.Keys == nil:
		return nil, fault.New(fault.Internal, "registry: no key file")
	case len(cfg.Drivers) == 0:
		return nil, fault.New(fault.Internal, "registry: no database driver")
	case cfg.Now == nil:
		return nil, fault.New(fault.Internal, "registry: no clock")
	case cfg.Interval <= 0:
		return nil, fault.New(fault.Internal, "registry: no interval between checks")
	case cfg.OpenTimeout <= 0:
		return nil, fault.New(fault.Internal, "registry: no deadline for an open")
	case cfg.PingTimeout <= 0:
		return nil, fault.New(fault.Internal, "registry: no deadline for a check")
	case cfg.MaxOpening <= 0:
		return nil, fault.New(fault.Internal, "registry: no limit on opens at once")
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Registry{cfg: cfg, log: log, slots: make(chan struct{}, cfg.MaxOpening), fileLimit: openFileLimit}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	empty := map[string]*entry{}
	r.entries.Store(&empty)
	return r, nil
}

// randomID returns 128 random bits in hex: unique without coordination,
// within master.MaxIDLength.
func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24).
	return hex.EncodeToString(b[:])
}

// Store returns the store of the tenant named name, for one request. It is
// on the path of every request: one atomic load of the map, one map read,
// one atomic load of the tenant's view, no lock and no call to the master.
// An unknown tenant is NotFound; a tenant that is not ready is Unavailable,
// with its state and reason in the message.
//
// The caller must not keep the store past the request: a removal or a
// reopen closes it, after which it fails as Unavailable.
func (r *Registry) Store(name string) (tenant.Store, error) {
	e := (*r.entries.Load())[name]
	if e == nil {
		if r.closed.Load() {
			return nil, fault.New(fault.Unavailable, "the server is shutting down")
		}
		return nil, fault.New(fault.NotFound, "no such tenant")
	}
	v := e.view.Load()
	if v.state != master.StateReady || v.store == nil {
		return nil, notReady(name, v)
	}
	return v.store, nil
}

// Reader is Store for the server's own reads of a tenant's database: it
// also returns the store of a tenant that is moved or fenced
// (master.TenantState.ReadOnly), for what must still read it there: the
// tenant's network rule that guards every request, the installation's
// checks of a setting against every tenant, a backup, a reclaim, the counts
// of a status. A request for the tenant's data takes its store from
// DataReader, which refuses a fenced tenant; whatever writes, a change, a
// reception, a job's unit, takes its store from Store, which refuses both.
func (r *Registry) Reader(name string) (tenant.Store, error) {
	e := (*r.entries.Load())[name]
	if e == nil {
		return r.Store(name)
	}
	if v := e.view.Load(); v.state.ReadOnly() && v.store != nil {
		return v.store, nil
	}
	return r.Store(name)
}

// DataReader is Store for a request that reads a tenant's data (its
// catalog, metadata, files, settings, audit or events): it also returns
// the store of a moved tenant, whose data is served while the owner records
// of its storages still name this installation, and refuses a fenced one
// with master.FencedReadError. A moved
// tenant another installation has since taken over is served until the
// next fence check (Config.Interval, or a CheckFence) finds the record and
// makes it fenced.
func (r *Registry) DataReader(name string) (tenant.Store, error) {
	e := (*r.entries.Load())[name]
	if e == nil {
		return r.Store(name)
	}
	v := e.view.Load()
	switch {
	case v.state == master.StateFenced:
		return nil, master.FencedReadError(name, v.reason)
	case v.state == master.StateMoved && v.store != nil:
		return v.store, nil
	}
	return r.Store(name)
}

// notReady is the refusal of a tenant that is not ready. It is kept out of
// Store so the path of a ready tenant stays small. Its message holds only the
// registry's phrases and the reason, which is made of the same. A moved or
// fenced tenant is refused as the master says (master.ReadOnlyError).
func notReady(name string, v *view) error {
	if v.state.ReadOnly() {
		return master.ReadOnlyError(name, v.state, v.reason)
	}
	msg := "the tenant " + name + " is " + string(v.state)
	if v.reason != "" {
		msg += ": " + v.reason
	}
	return fault.New(fault.Unavailable, msg)
}

// State returns the state the registry sees now for the tenant named name,
// with its reason and the time it was reached, and whether it knows the
// tenant. It is correct the moment a check completes, and is what the API
// answers as a tenant's state; the master's row may lag it by up to
// StateChangeInterval.
func (r *Registry) State(name string) (State, bool) {
	e := (*r.entries.Load())[name]
	if e == nil {
		return State{}, false
	}
	v := e.view.Load()
	return State{State: v.state, Reason: v.reason, Since: v.since}, true
}

// enter counts a call in progress, so Close waits for it, or refuses it
// once Close has begun. Checked under mu, where Close sets closed, so no
// call is counted after Close starts waiting.
func (r *Registry) enter() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return fault.New(fault.Unavailable, "the server is shutting down")
	}
	r.wg.Add(1)
	return nil
}

// goOwned starts fn in a goroutine counted by wg, which recovers a panic in
// it and logs the stack, so no goroutine of the registry can take the
// process down.
func (r *Registry) goOwned(what string, fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.recoverLog(what)
		fn()
	}()
}

// recoverLog recovers a panic and logs where it happened and the stack. The
// panic's value is not logged: it may hold anything a driver had in hand.
func (r *Registry) recoverLog(what string) {
	if p := recover(); p != nil {
		r.log.Error("a panic was recovered", "in", what, "stack", string(debug.Stack()))
	}
}
