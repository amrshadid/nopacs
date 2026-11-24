package deidjob

import (
	"context"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
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

// Installation is what the installation's settings say that a unit reads,
// read live at each unit: package settingvalue's Reader is one.
type Installation interface {
	// HeaderLimit returns receiver.header-limit, the bound of a file's
	// header, which a dry run reads within as a reception does.
	HeaderLimit(ctx context.Context) (int64, error)
}

// TenantSettings is what one tenant's settings say that a unit reads:
// package settingvalue's Tenant is one.
type TenantSettings interface {
	// FailuresKept returns job.failures-kept as it applies on the tenant.
	FailuresKept(ctx context.Context) (int, error)
	// PixelLimit returns deid.pixel-limit as it applies on the tenant.
	PixelLimit(ctx context.Context) (int64, error)
}

// Storages opens the storages a unit reads from, each once, and closes
// them at the unit's end: package checkedread's Stores is one.
type Storages interface {
	// Get returns the storage named name, opening it on first use.
	Get(ctx context.Context, name string) (blob.Store, error)
	// Close closes every storage opened.
	Close()
}

// The fixed bounds of deid jobs.
const (
	// MaxUnits is the most units of deid jobs the server works at once,
	// and MaxTenantUnits the most of one tenant's: each holds a file and
	// its copies in memory, so the bound is the memory's, and a fixed one
	// rather than a setting a tenant could raise.
	MaxUnits       = 2
	MaxTenantUnits = 1
	// MaxFileBytes is the largest archived file a unit reads into memory.
	MaxFileBytes = 64 << 20
	// MaxPixelBytes bounds the image a dry run's pixel rule decodes, below
	// the tenant's deid.pixel-limit when that is higher: a job runs in the
	// background beside receptions, and its memory must stay its own size.
	// A file past it would be held as pixels_too_large by the dry run (and
	// released at reception, where the tenant's own bound holds).
	MaxPixelBytes = 256 << 20
	// MaxBatch is the most instances one progress write records, and
	// BatchInterval the longest a batch is held before it is written: the
	// most a crash judges again, and how soon a cancel is seen.
	MaxBatch      = 64
	BatchInterval = time.Second
	// Lease is how long a claim holds a unit; RenewInterval how often a
	// worker renews it.
	Lease         = time.Minute
	RenewInterval = 20 * time.Second
	// SlotDelay is how long a unit given back for want of a read slot
	// waits before it is due again.
	SlotDelay = 5 * time.Second
	// DefaultInterval is how often the list of tenants is read again; a
	// backstop, since the API pokes the supervisor after every job it
	// makes, cancels or retries.
	DefaultInterval = 30 * time.Second
	// DefaultPollInterval is how long a tenant with no unit due waits
	// before it looks again; each empty look in a row doubles it, up to
	// MaxPollInterval, and a poke ends the wait.
	DefaultPollInterval = 2 * time.Second
	MaxPollInterval     = 30 * time.Second
	// opTimeout bounds one read or write of a tenant's database.
	opTimeout = 10 * time.Second
	// readBuffer is the size of each of the two buffers of a checked read.
	readBuffer = 256 << 10
)

// Config is what a Supervisor is handed.
type Config struct {
	// Tenants lists the tenants and opens their databases.
	Tenants Tenants
	// Installation reads the installation's settings.
	Installation Installation
	// TenantSettings returns the reader of the settings that apply on the
	// tenant whose database is st.
	TenantSettings func(st tenant.Store) TenantSettings
	// OpenStorages returns the storages of the tenant named name, none
	// open yet, for one unit.
	OpenStorages func(name string, p tenant.Pipeline) Storages
	// UIDKey returns the tenant's UID key alone, which a check uses to
	// remap a file's UIDs as its reception did and look them up
	// (tenant.JobWork.SeenUIDs); the unit zeroes it when it ends. The
	// tenant's date and naming keys are never read. A dry run never calls
	// it.
	UIDKey func(ctx context.Context, tenantName string) ([]byte, error)
	// Reads is the server's count of reads of archived bytes open, shared
	// with the API and the senders so one bound covers them all.
	// Required.
	Reads *checkedread.Slots
	// Memory is the server's bound on de-identification's memory
	// (deid.memory), shared with the reception pipelines and the API's
	// trial runs: each instance a unit judges takes its copies from it
	// (receive.TrialOptions.Memory). Nil bounds a unit by MaxFileBytes and
	// MaxUnits alone.
	Memory check.Memory
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
	// and fixed phrases, never a value read from a file. Nil discards
	// them.
	Logger *slog.Logger

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets (export_test.go).
type seams struct {
	// renew is the interval of lease renewal; 0 is RenewInterval.
	renew time.Duration
	// judged, when set, is called after each instance is judged and
	// added to its batch, so a test can stop or cancel a unit midway.
	judged func(sopInstanceUID string)
	// tried, when set, is given each dry run's trial, so a test can see
	// what the unit's keys made.
	tried func(t receive.Trial)
	// maxFileBytes is the largest file a unit reads; 0 is MaxFileBytes.
	maxFileBytes int64
	// keys makes a unit's own keys; nil is throwawayKeys.
	keys func() (engine.Keys, error)
}

// check fills the defaults of c and refuses one that lacks a port.
func (c Config) check() (Config, error) {
	switch {
	case c.Tenants == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no tenants")
	case c.Installation == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no installation settings")
	case c.TenantSettings == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no tenant settings")
	case c.OpenStorages == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no way to open storages")
	case c.UIDKey == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no way to read a tenant's UID key")
	case c.Reads == nil:
		return c, fault.New(fault.Internal, "deidjob: the supervisor is given no read slots")
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
	return c, nil
}
