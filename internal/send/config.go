package send

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/settings"
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

// Installation is what the installation's settings say of senders, read
// live at each use: package settingvalue's Reader is one.
type Installation interface {
	// SenderDestinations returns sender.destinations.
	SenderDestinations(ctx context.Context) (settings.Destinations, error)
	// AllowPlainHTTP returns sender.allow-plain-http.
	AllowPlainHTTP(ctx context.Context) (bool, error)
	// MaxAssociations returns sender.max-associations, the bound of all
	// senders of all tenants together.
	MaxAssociations(ctx context.Context) (int, error)
}

// TenantSettings is what one tenant's settings say of its senders and jobs:
// package settingvalue's Tenant is one.
type TenantSettings interface {
	// MaxAssociations returns sender.tenant-max-associations as it applies
	// on the tenant.
	MaxAssociations(ctx context.Context) (int, error)
	// FailuresKept returns job.failures-kept as it applies on the tenant.
	FailuresKept(ctx context.Context) (int, error)
}

// Storages opens the storages a unit reads from, each once, and closes
// them at the unit's end: package checkedread's Stores is one.
type Storages interface {
	// Open opens length bytes of an archived object from offset, from its
	// storage, or from a replica in step when that storage cannot serve
	// it (checkedread.Stores.Open).
	Open(ctx context.Context, obj checkedread.Object, offset, length int64) (checkedread.Opened, error)
	// Mismatch records a read of o whose bytes were found not to be the
	// catalog's: a replica's damage (checkedread.Stores.Mismatch).
	Mismatch(ctx context.Context, o checkedread.Opened, err error) error
	// Close closes every storage opened.
	Close()
}

// The defaults and fixed bounds of sending.
const (
	// DefaultInterval is how often each tenant's senders are read again,
	// and the list of tenants: a backstop, since the API pokes the
	// supervisor after every change of a tenant's senders or jobs.
	DefaultInterval = 30 * time.Second
	// DefaultPollInterval is how long a worker that found no unit due
	// waits before it looks again; each empty look in a row doubles it,
	// up to MaxPollInterval, and a poke ends the wait.
	DefaultPollInterval = 2 * time.Second
	// MaxPollInterval bounds the wait of an idle worker.
	MaxPollInterval = 30 * time.Second
	// Lease is how long a claim holds a unit; RenewInterval how often the
	// worker renews it, a third of it, so two renewals can be lost before
	// another worker may take the unit.
	Lease         = time.Minute
	RenewInterval = 20 * time.Second
	// MaxBatch is the most instances one progress write records, and
	// BatchInterval the longest a batch is held before it is written: the
	// most a crash or a lost lease sends again, and how soon a cancel is
	// seen.
	MaxBatch      = 64
	BatchInterval = time.Second
	// SlotDelay is how long a unit given back for want of a read slot
	// waits before it is due again.
	SlotDelay = 5 * time.Second
	// DownAfter is how many connections in a row that fail make a sender
	// down.
	DownAfter = 3
	// DefaultTenantAssociations and DefaultInstallationAssociations are
	// the bounds used while the settings cannot be read: the settings'
	// built-in defaults.
	DefaultTenantAssociations       = 8
	DefaultInstallationAssociations = 32
	// opTimeout bounds one read or write of a tenant's database the
	// supervisor makes on its own behalf (its senders, its events).
	opTimeout = 10 * time.Second
	// readBuffer is the size of each of the two buffers a checked read of
	// one instance holds.
	readBuffer = 256 << 10
	// metaPrefix is the most bytes of an instance's start read to find its
	// file meta information (PS3.10 7.1), which any tool writes in a few
	// hundred.
	metaPrefix = 64 << 10
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
	// Credential opens a stow sender's sealed credential (as
	// tenant.Senders.SenderCredential answers it) for sending. It never
	// logs it.
	Credential func(ctx context.Context, tenantName, sender string, sealed []byte) (string, error)
	// Reads is the server's count of reads of archived bytes open, shared
	// with the API so one bound covers both. Required.
	Reads *checkedread.Slots
	// TLSDir returns the directory of a tenant's TLS files,
	// <data directory>/tls/<tenant>.
	TLSDir func(tenantName string) string
	// Wake is called with a tenant's name after a write that may have
	// written events of it; nil does nothing.
	Wake func(tenantName string)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Interval is how often each tenant is read again; 0 is
	// DefaultInterval.
	Interval time.Duration
	// PollInterval is the first wait of an idle worker; 0 is
	// DefaultPollInterval.
	PollInterval time.Duration
	// Logger receives the supervisor's lines: names, labels, ids, counts
	// and fixed phrases, never a credential or a value read from a file.
	// Nil discards them.
	Logger *slog.Logger

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets (export_test.go).
type seams struct {
	// lookup resolves a host name; nil is net.DefaultResolver.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// renew is the interval of lease renewal; 0 is RenewInterval.
	renew time.Duration
}

// check fills the defaults of c and refuses one that lacks a port.
func (c Config) check() (Config, error) {
	switch {
	case c.Tenants == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no tenants")
	case c.Installation == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no installation settings")
	case c.TenantSettings == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no tenant settings")
	case c.OpenStorages == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no way to open storages")
	case c.Credential == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no way to open a credential")
	case c.Reads == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no read slots")
	case c.TLSDir == nil:
		return c, fault.New(fault.Internal, "send: the supervisor is given no TLS directory")
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
	if c.seams.lookup == nil {
		c.seams.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if c.seams.renew <= 0 {
		c.seams.renew = RenewInterval
	}
	return c, nil
}
