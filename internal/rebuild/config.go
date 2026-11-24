package rebuild

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Tenants is where the supervisor learns which tenants exist and reaches
// each one's database.
type Tenants interface {
	// Names returns the names of every tenant of the installation.
	Names(ctx context.Context) ([]string, error)
	// Store returns the open database of the tenant named name. A tenant
	// that is not ready is an error.
	Store(name string) (tenant.Store, error)
}

// Storages opens the storages a unit reads, each once, and closes them at
// the unit's end: package checkedread's Stores is one.
type Storages interface {
	// Get returns the storage named name, opening it on first use.
	Get(ctx context.Context, name string) (blob.Store, error)
	// Close closes every storage opened.
	Close()
}

// Storage is all a rebuild does with a storage: read an object and list
// keys. Every storage a unit gets is held as this type, so a write or a
// delete of a storage does not compile in this package.
type Storage interface {
	// Read returns length bytes of the object at key from offset, -1 for
	// the rest of it (blob.Store's Read).
	Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// List returns a page of keys (blob.Store's List).
	List(ctx context.Context, opts blob.ListOptions) (blob.Page, error)
}

// The fixed bounds of rebuilds.
const (
	// MaxUnits is the most rebuild units the server works at once: a
	// rebuild is rare, reads every scan's files and writes a shadow of the
	// whole catalog.
	MaxUnits = 1
	// Lease is how long a claim holds a unit; RenewInterval how often a
	// worker renews it.
	Lease         = time.Minute
	RenewInterval = 20 * time.Second
	// DefaultInterval is how often the list of tenants is read again.
	DefaultInterval = 30 * time.Second
	// DefaultPollInterval is how long a tenant with no unit due waits
	// before it looks again; each empty look in a row doubles it, up to
	// MaxPollInterval.
	DefaultPollInterval = 2 * time.Second
	MaxPollInterval     = 30 * time.Second
	// opTimeout bounds one read or write of a tenant's database, and
	// activationTimeout the activation's one transaction over the whole
	// catalog.
	opTimeout         = 30 * time.Second
	activationTimeout = 30 * time.Minute
	// maxListPages bounds the pages of one listing a unit reads, so a
	// storage that never ends a listing cannot hold a unit for ever.
	maxListPages = 1 << 20
)

// Config is what a Supervisor is handed.
type Config struct {
	// Tenants lists the tenants and opens their databases.
	Tenants Tenants
	// OpenStorages returns the storages of the tenant named name, none open
	// yet, for one unit: a rebuild only reads them.
	OpenStorages func(name string, p tenant.Pipeline) Storages
	// Wake is called with a tenant's name after a write that may have
	// written events of it; nil does nothing.
	Wake func(tenantName string)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Interval is how often the list of tenants is read again; 0 is
	// DefaultInterval.
	Interval time.Duration
	// PollInterval is the first wait of a tenant with no unit due; 0 is
	// DefaultPollInterval.
	PollInterval time.Duration
	// Logger receives the supervisor's lines: names, labels, ids, counts
	// and fixed phrases, never a key or a UID. Nil discards them.
	Logger *slog.Logger
	// Decide, when set, answers how a correction record is applied: the
	// record to apply in its place (its state taken from elsewhere than the
	// record), or false to apply nothing of it. A restore sets it, so the
	// tenant's rows as the backup holds them, not records anyone who can
	// write to a storage could have written, decide what a record hides.
	// Nil applies each record as it reads.
	Decide func(ctx context.Context, p tenant.Pipeline, c record.Correction) (record.Correction, bool, error)
	// Keys checks each correction record's MAC (record.DecodeCorrection):
	// the installation keys. Required, so no rebuild applies a record it
	// has not checked; a record that does not verify hides nothing and is
	// counted (tenant.TallyUnauthenticatedCorrection).
	Keys record.Keyring
	// TenantID returns the id of the tenant named name, which its records
	// are authenticated for. Required.
	TenantID func(ctx context.Context, name string) (string, error)

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets (export_test.go).
type seams struct {
	// renew is the interval of lease renewal; 0 is RenewInterval.
	renew time.Duration
	// afterScan, when set, is called after each scan's last write with
	// its key, so a test can stop a unit between two scans.
	afterScan func(key string)
	// beforeDrop, when set, is called with a job's id before its shadow is
	// dropped, so a test can read the shadow with a tool of its own.
	beforeDrop func(jobID string)
	// batch is the most instances one write of a scan carries; 0 is
	// tenant.MaxRebuildBatch. A test makes it small to write the corpus's
	// scans in several batches.
	batch int
}

// check fills the defaults of c and refuses one that lacks a port.
func (c Config) check() (Config, error) {
	switch {
	case c.Tenants == nil:
		return c, fault.New(fault.Internal, "rebuild: the supervisor is given no tenants")
	case c.OpenStorages == nil:
		return c, fault.New(fault.Internal, "rebuild: the supervisor is given no way to open storages")
	case c.Keys == nil || c.Keys.SealVersion() == 0 || c.TenantID == nil:
		return c, fault.New(fault.Internal, "rebuild: the supervisor is given no installation keys or tenant ids to check correction records with")
	}
	if c.Wake == nil {
		c.Wake = func(string) {}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.seams.renew <= 0 {
		c.seams.renew = RenewInterval
	}
	if c.seams.batch <= 0 || c.seams.batch > tenant.MaxRebuildBatch {
		c.seams.batch = tenant.MaxRebuildBatch
	}
	return c, nil
}
