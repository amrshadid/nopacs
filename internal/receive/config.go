package receive

import (
	"context"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Clock is the only source of time the pipeline has: Now for every time it
// gives the tenant's database, After for every wait. A test gives one it
// moves by hand.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the clock of the machine.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// OpenStorage opens a tenant's storage by its name. The store it returns is
// the caller's: the pipeline uses it and never closes it, so the caller may
// hand the same store to every step.
type OpenStorage func(ctx context.Context, name string) (blob.Store, error)

// Limit reads a bound that may change while the pipeline runs, such as the
// setting receiver.header-limit, in bytes.
type Limit func(ctx context.Context) (int64, error)

// Check says whether something holds now, for a refusal the pipeline makes
// without reading settings itself: the tenant over its limit of incoming
// bytes, or the disk below its reserve.
type Check func(ctx context.Context) (bool, error)

// The defaults of a Config.
const (
	DefaultWorkers       = 4
	DefaultLease         = 2 * time.Minute
	DefaultPollInterval  = time.Second
	DefaultBatchSize     = 500
	DefaultBatchInterval = 200 * time.Millisecond
	DefaultRetryInterval = 5 * time.Second
)

// HeaderLimitInterval is how long a header limit read is used before it
// is read again: a setting read once a second at most, not once a file.
const HeaderLimitInterval = time.Second

// DefaultPixelLimit is the pixel bound when none can be read: the setting
// deid.pixel-limit's default.
const DefaultPixelLimit = 1 << 30

// Config is what one tenant's pipeline is handed. Every port is given, none
// is opened here.
type Config struct {
	// Tenant is the tenant's name: its directory in the incoming area.
	Tenant string
	// Store is the tenant's database.
	Store tenant.Pipeline
	// OpenStorage opens one of the tenant's storages by name.
	OpenStorage OpenStorage
	// Hasher is the tenant's keyed hash for naming rules ({#...}).
	Hasher naming.Hasher
	// DeidKeys reads the tenant's keys of de-identification, for a
	// receiver that de-identifies (tenant.Receiver.Profile): asked once,
	// when such a receiver's first file comes. Nil, a file at such a
	// receiver waits unrecorded, and is never recorded as received.
	DeidKeys DeidKeys
	// Check returns the check after de-identification for files of at
	// most maxFileBytes (the receiver's largest file, with room for what
	// the engine adds), run on every de-identified file before it is put
	// on a storage. Nil, a de-identified file is never archived: its step
	// fails and is tried again.
	Check func(maxFileBytes int64) Checker
	// Wake is called after every commit that wrote an event of the tenant
	// (events.Broadcaster.Wake with the tenant's name). Nil does nothing.
	Wake func()
	// Forwarded is called after a reception's step reached done, the step
	// that makes a send job for each sender its receiver forwards to
	// (tenant.Sending), so the senders look for that work at once rather
	// than at their next poll. Nil does nothing.
	Forwarded func()
	// Clock is the pipeline's clock; nil is SystemClock.
	Clock Clock
	// Logger receives the pipeline's log lines: ids, labels, counts and
	// fault messages only. Nil discards them.
	Logger *slog.Logger
	// IncomingRoot is the root of the incoming area; the tenant's files are
	// in the directory of its name below it.
	IncomingRoot string
	// OverLimit says the tenant holds more incoming bytes than it may; a
	// new file is then refused as a temporary failure. Nil never refuses.
	OverLimit Check
	// LowDisk says the incoming area's disk is below its reserve; a new
	// file is then refused as a temporary failure. Nil never refuses.
	LowDisk Check
	// HeaderLimit reads the most bytes of a file read as its header,
	// everything before its pixel data (the setting
	// receiver.header-limit); it is read again at most every
	// HeaderLimitInterval, so a raised bound applies without a restart. Nil,
	// or a read that fails before any has succeeded, is header.MaxHeader.
	HeaderLimit Limit
	// PixelLimit reads the most bytes of an image decoded that
	// de-identification works on the pixels of (the setting
	// deid.pixel-limit, as it applies on the tenant); read again at most
	// every HeaderLimitInterval, so a raised bound applies without a
	// restart. Nil, or a read that fails before any has succeeded, is
	// DefaultPixelLimit.
	PixelLimit Limit
	// Workers is the most steps run at once; 0 is DefaultWorkers.
	Workers int
	// Lease is how long a worker holds a unit between renewals; 0 is
	// DefaultLease. It is renewed every third of it.
	Lease time.Duration
	// PollInterval is how often an idle worker asks for work; 0 is
	// DefaultPollInterval. A unit the pipeline queues itself wakes a
	// worker at once.
	PollInterval time.Duration
	// BatchSize is the most received files or holds written at once; 0 is
	// DefaultBatchSize, and it is at most tenant.MaxReceivedBatch.
	BatchSize int
	// BatchInterval is the longest a received file waits for its row; 0 is
	// DefaultBatchInterval.
	BatchInterval time.Duration
	// RetryInterval is how long a file whose row could not be written waits
	// before it is checked again; 0 is DefaultRetryInterval.
	RetryInterval time.Duration
	// Owner names this process's workers in their leases; empty is a
	// random name.
	Owner string
	// StepHook, when not nil, is run inside the transaction of every step
	// the pipeline takes (tenant.Pipeline.StepReception). It is for a store
	// whose step does not write its own event: the server passes the hook
	// that writes it (tenant.StepEventHook), so a step and its event commit
	// together. On a store whose StepReception writes the step's event
	// itself it is nil, and passes through as nil.
	StepHook tenant.StepHook
	// ScanLocks, when not nil, are the locks a scan's manifest and sidecar
	// are rewritten under (LockScan), shared with the tenant's other
	// writers of them (a correction's purge or replace) and with the
	// pipelines that follow this one: a lock of the pipeline's own would
	// end with it, and a correction holding one across a restart would no
	// longer keep the next pipeline from the same scan. Nil gives the
	// pipeline locks of its own.
	ScanLocks *ScanLocks
}

// withDefaults returns c with every zero field given its default, or an
// error naming the first port that is missing.
func (c Config) withDefaults() (Config, error) {
	switch {
	case c.Store == nil:
		return c, fault.New(fault.Internal, "the reception pipeline is given no tenant database")
	case c.OpenStorage == nil:
		return c, fault.New(fault.Internal, "the reception pipeline is given no way to open a storage")
	case c.IncomingRoot == "":
		return c, fault.New(fault.Internal, "the reception pipeline is given no incoming area")
	}
	if err := master.ValidateTenantName(c.Tenant); err != nil {
		return c, err
	}
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Wake == nil {
		c.Wake = func() {}
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Lease == 0 {
		c.Lease = DefaultLease
	}
	c.Lease = min(max(c.Lease, tenant.MinLease), tenant.MaxLease)
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultBatchSize
	}
	c.BatchSize = min(c.BatchSize, tenant.MaxReceivedBatch, tenant.MaxQuarantineBatch, tenant.MaxDuplicateBatch)
	if c.BatchInterval <= 0 {
		c.BatchInterval = DefaultBatchInterval
	}
	if c.RetryInterval <= 0 {
		c.RetryInterval = DefaultRetryInterval
	}
	if c.Owner == "" {
		c.Owner = "receive-" + newID()[:12]
	}
	return c, nil
}
