package scrub

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/pace"
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

// TenantSettings is what one tenant's settings say that a scrub reads:
// package settingvalue's Tenant is one.
type TenantSettings interface {
	// FailuresKept returns job.failures-kept as it applies on the tenant.
	FailuresKept(ctx context.Context) (int, error)
	// ScrubRate returns scrub.rate as it applies on the tenant: bytes a
	// second.
	ScrubRate(ctx context.Context) (int64, error)
	// ScrubInterval returns scrub.interval as it applies on the tenant,
	// zero for never.
	ScrubInterval(ctx context.Context) (time.Duration, error)
	// ArchiveSample returns scrub.archive-sample as it applies on the
	// tenant: the share, from 0 to 1, of the objects online on a storage of
	// the archive tier whose bytes a scrub reads; the rest are checked by
	// presence (Stat: size, and the store's own SHA-256 where it keeps
	// one). Zero, the default, reads no object's bytes there: a read may
	// bill the site, and a scrub never asks for a recall.
	ArchiveSample(ctx context.Context) (float64, error)
}

// Storages opens the storages a unit reads, each once, and closes them at
// the unit's end: package checkedread's Stores is one.
type Storages interface {
	// Get returns the storage named name, opening it on first use.
	Get(ctx context.Context, name string) (blob.Store, error)
	// Close closes every storage opened.
	Close()
}

// Storage is all a scrub does with a storage: read an object and list
// keys. Every storage a unit gets is held as this type (readStores), so a
// write, a delete or any other change of a storage does not compile in
// this package.
type Storage interface {
	// Read returns length bytes of the object at key from offset, -1 for
	// the rest of it (blob.Store's Read).
	Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// List returns a page of keys (blob.Store's List).
	List(ctx context.Context, opts blob.ListOptions) (blob.Page, error)
	// Stat returns what the storage knows of the object at key without
	// reading it (blob.Store's Stat): what a presence check of an object
	// of the archive tier reads, since a read of one held cold fails, and
	// a recall costs the site.
	Stat(ctx context.Context, key string) (blob.Info, error)
}

// readStores hands out a unit's storages as Storage alone.
type readStores struct{ s Storages }

// Get returns the storage named name, to read and list only.
func (r readStores) Get(ctx context.Context, name string) (Storage, error) {
	st, err := r.s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Close closes every storage opened.
func (r readStores) Close() { r.s.Close() }

// The fixed bounds of scrubs.
const (
	// MaxUnits is the most units of scrubs the server works at once, and
	// MaxTenantUnits the most of one tenant's: a scrub reads in the
	// background, behind reception and reads, and each of its reads holds
	// one of the server's read slots.
	MaxUnits       = 2
	MaxTenantUnits = 1
	// ReadOwner is the owner a scrub's reads are counted under in the
	// server's read slots.
	ReadOwner = "scrub"
	// MaxBatch is the most instances one progress write records, and
	// BatchInterval the longest a batch is held before it is written.
	MaxBatch      = 64
	BatchInterval = time.Second
	// Lease is how long a claim holds a unit; RenewInterval how often a
	// worker renews it.
	Lease         = time.Minute
	RenewInterval = 20 * time.Second
	// SlotDelay is how long a unit given back for want of a read slot
	// waits before it is due again.
	SlotDelay = 5 * time.Second
	// SettleDelay is how long a scan's unit waits before it reads a
	// manifest, a sidecar or the catalog's rows a second time, so a
	// reception that wrote them between two reads has committed.
	SettleDelay = 2 * time.Second
	// DefaultInterval is how often the list of tenants is read again.
	DefaultInterval = 30 * time.Second
	// DefaultPollInterval is how long a tenant with no unit due waits
	// before it looks again; each empty look in a row doubles it, up to
	// MaxPollInterval.
	DefaultPollInterval = 2 * time.Second
	MaxPollInterval     = 30 * time.Second
	// DefaultScheduleInterval is how often a tenant's scheduled scrubs are
	// looked at: well under the shortest scrub.interval, a day.
	DefaultScheduleInterval = time.Hour
	// opTimeout bounds one read or write of a tenant's database.
	opTimeout = 10 * time.Second
	// readBuffer is the size of each piece of a read.
	readBuffer = 256 << 10
	// instancePage is how many instances one read of a unit's stream, or of
	// a scan's catalog rows, answers.
	instancePage = 256
	// maxListPages bounds the pages of one listing a unit reads, so a
	// storage that never ends a listing cannot hold a unit for ever.
	maxListPages = 1 << 20
)

// Config is what a Supervisor is handed.
type Config struct {
	// Tenants lists the tenants and opens their databases.
	Tenants Tenants
	// TenantSettings returns the reader of the settings that apply on the
	// tenant whose database is st.
	TenantSettings func(st tenant.Store) TenantSettings
	// OpenStorages returns the storages of the tenant named name, none open
	// yet, for one unit, opened for reading only: a scrub never writes to
	// a storage.
	OpenStorages func(name string, p tenant.Pipeline) Storages
	// Reads is the server's count of reads of archived bytes open, shared
	// with the API, the senders and the deid jobs. Required.
	Reads *checkedread.Slots
	// NewID makes the id of the audit entry of a scrub the server makes by
	// itself. Required.
	NewID func() string
	// Keys checks the MAC of each correction record a scrub reads
	// (record.DecodeCorrection): the installation keys. Required, so no
	// record explains an object unchecked; one that does not verify
	// explains nothing and is counted (tenant.TallyCorrectionUnauthenticated).
	Keys record.Keyring
	// TenantID returns the id of the tenant named name, which its records
	// are authenticated for. Required.
	TenantID func(ctx context.Context, name string) (string, error)
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
	// ScheduleInterval is how often a tenant's scheduled scrubs are looked
	// at; 0 is DefaultScheduleInterval.
	ScheduleInterval time.Duration
	// RequestRate is how many requests to scrub one instance a tenant may
	// make a minute (Supervisor.RequestInstance); 0 is
	// DefaultRequestRate. MaxRequests is how many wait at most per
	// tenant; 0 is DefaultMaxRequests.
	RequestRate int
	MaxRequests int
	// Verifications, when set, records when each scan was last read whole
	// on each storage, the ground of a storage's coverage (Coverage). Nil
	// records nothing.
	Verifications func(st tenant.Store) tenant.VerificationRecorder
	// Logger receives the supervisor's lines: names, labels, ids, counts
	// and fixed phrases, never a key or a UID. Nil discards them.
	Logger *slog.Logger

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets (export_test.go).
type seams struct {
	// renew is the interval of lease renewal; 0 is RenewInterval.
	renew time.Duration
	// settle is the wait before a second read of a scan's files; 0 is
	// SettleDelay, negative none.
	settle time.Duration
	// sleep waits d or until ctx ends; nil is a timer.
	sleep func(ctx context.Context, d time.Duration) error
	// read, when set, is called before each read of an object, so a test
	// can change the object between a read and the second read of it.
	read func(key string, attempt int)
}

// check fills the defaults of c and refuses one that lacks a port.
func (c Config) check() (Config, error) {
	switch {
	case c.Tenants == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no tenants")
	case c.TenantSettings == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no tenant settings")
	case c.OpenStorages == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no way to open storages")
	case c.Reads == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no read slots")
	case c.NewID == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no way to make ids")
	case c.Keys == nil || c.Keys.SealVersion() == 0 || c.TenantID == nil:
		return c, fault.New(fault.Internal, "scrub: the supervisor is given no installation keys or tenant ids to check correction records with")
	case c.RequestRate < 0 || c.MaxRequests < 0:
		return c, fault.New(fault.Internal, "scrub: the bounds of requests to scrub an instance are not negative")
	}
	if c.RequestRate == 0 {
		c.RequestRate = DefaultRequestRate
	}
	if c.MaxRequests == 0 {
		c.MaxRequests = DefaultMaxRequests
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
	if c.ScheduleInterval <= 0 {
		c.ScheduleInterval = DefaultScheduleInterval
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.seams.renew <= 0 {
		c.seams.renew = RenewInterval
	}
	if c.seams.settle == 0 {
		c.seams.settle = SettleDelay
	}
	if c.seams.sleep == nil {
		c.seams.sleep = pace.Sleep
	}
	return c, nil
}
