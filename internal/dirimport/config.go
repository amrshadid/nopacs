package dirimport

import (
	"context"
	"log/slog"
	"time"

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

// Installation is what the installation's settings say that the
// supervisor reads, live: package settingvalue's Reader is one.
type Installation interface {
	// ImportServerMaxUnits returns import.server-max-units.
	ImportServerMaxUnits(ctx context.Context) (int, error)
	// ImportMaxDirEntries returns import.max-dir-entries.
	ImportMaxDirEntries(ctx context.Context) (int, error)
}

// TenantSettings is what one tenant's settings say that the supervisor
// reads: package settingvalue's Tenant is one.
type TenantSettings interface {
	// ImportMaxUnits returns import.max-units as it applies on the tenant.
	ImportMaxUnits(ctx context.Context) (int, error)
}

// Roots finds the directory of an import root for one tenant's unit.
type Roots interface {
	// Root returns the absolute path of the directory the root named name
	// stands for now, every link in it followed, or an error when the
	// installation no longer names it, the tenant is no longer allotted
	// it, or it no longer resolves to a directory it may read: the unit
	// then fails with tenant.ImportReasonRootUnavailable. st is the
	// tenant's database, for its settings. No error holds a path.
	Root(ctx context.Context, tenantName string, st tenant.Store, name string) (string, error)
}

// The fixed bounds of the supervisor.
const (
	// Lease is how long a claim holds a unit; RenewInterval how often a
	// worker renews it.
	Lease         = time.Minute
	RenewInterval = 20 * time.Second
	// RefusedDelay is how long a unit given back because the archive
	// refused a file for now (the incoming area nearly full, the tenant
	// not ready) waits before it is due again; WaitDelay is how long one
	// whose receiver is disabled waits.
	RefusedDelay = 5 * time.Second
	WaitDelay    = 30 * time.Second
	// SettlingGrace is how long, past the receiver's longest open time,
	// a job whose walks have all ended may wait for its files' outcomes
	// before it ends partial (tenant.ImportReasonSettlingTimedOut).
	SettlingGrace = time.Hour
	// DefaultInterval is how often the list of tenants is read again, and
	// settling jobs looked at; a backstop, since the API pokes the
	// supervisor after every job it makes, cancels or retries.
	DefaultInterval = 30 * time.Second
	// DefaultPollInterval is how long a tenant with no unit due waits
	// before it looks again; each empty look in a row doubles it, up to
	// MaxPollInterval, and a poke ends the wait.
	DefaultPollInterval = 2 * time.Second
	MaxPollInterval     = 30 * time.Second
	// StallBound is how long a unit's walk may go without progress (a
	// batch of names read, an entry judged, bytes of a file read) before
	// it is taken for a file system that does not answer and fails for now.
	// Ten minutes is far past any one read of a healthy disk or network
	// share, and short beside an import's hours.
	StallBound = 10 * time.Minute
	// RootCheckInterval is how often a unit in flight reads its root again
	// (the installation still names it, the tenant is still allotted it,
	// it still resolves to the directory opened): a root taken away stops
	// the units walking it within this bound.
	RootCheckInterval = 30 * time.Second
	// opTimeout bounds one read or write of a tenant's database.
	opTimeout = 10 * time.Second
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
	// Roots finds a root's directory.
	Roots Roots
	// Sink returns the tenant's reception pipeline, which files are handed
	// to; nil while it has none, and the unit waits.
	Sink func(tenantName string) receive.Sink
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
	// Logger receives the supervisor's lines: names, ids, counts and fixed
	// phrases, never a path. Nil discards them.
	Logger *slog.Logger

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets (export_test.go).
type seams struct {
	// renew is the interval of lease renewal; 0 is RenewInterval.
	renew time.Duration
	// judged, when set, is called with each entry's path after it is
	// recorded, so a test can stop a unit midway.
	judged func(path string)
	// stall is the bound of a walk without progress; 0 is StallBound.
	stall time.Duration
	// rootCheck is how often a unit reads its root again; 0 is
	// RootCheckInterval.
	rootCheck time.Duration
	// keep is the bytes of names a directory above the one read keeps; 0
	// is keptNamesBytes.
	keep int
	// open, when set, is called with a file's path before it is opened,
	// so a test can hold the walk as a file system that does not answer.
	open func(path string)
	// held, when set, is called with the bytes of names a walk holds each
	// time they grow.
	held func(bytes int)
	// ended, when set, is called with a unit's folder, how it ended and
	// the reason it was given back or failed with ("" when its walk ended)
	// after its last write.
	ended func(folder string, how end, reason string)
}

// check fills the defaults of c and refuses one that lacks a port.
func (c Config) check() (Config, error) {
	switch {
	case c.Tenants == nil:
		return c, fault.New(fault.Internal, "dirimport: the supervisor is given no tenants")
	case c.Installation == nil:
		return c, fault.New(fault.Internal, "dirimport: the supervisor is given no installation settings")
	case c.TenantSettings == nil:
		return c, fault.New(fault.Internal, "dirimport: the supervisor is given no tenant settings")
	case c.Roots == nil:
		return c, fault.New(fault.Internal, "dirimport: the supervisor is given no roots")
	case c.Sink == nil:
		return c, fault.New(fault.Internal, "dirimport: the supervisor is given no reception pipeline")
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
	if c.seams.stall <= 0 {
		c.seams.stall = StallBound
	}
	if c.seams.rootCheck <= 0 {
		c.seams.rootCheck = RootCheckInterval
	}
	if c.seams.keep <= 0 {
		c.seams.keep = keptNamesBytes
	}
	return c, nil
}
