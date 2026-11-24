package access

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The defaults of the sizes and times in Config, used when a field is zero.
const (
	// DefaultMarkQueue is how many last-use marks wait for their worker. A
	// mark is a key id and a time, so the queue costs a few kilobytes; it is
	// deep enough to ride out a slow write of the master and shallow enough
	// that a stuck master shows in the drop count, not in memory.
	DefaultMarkQueue = 256
	// DefaultRefusalQueue is how many audit entries of refusals wait for the
	// audit writer. Refusals have their own queue so that a flood of bad
	// keys fills it, not the queue of allowed requests' entries.
	DefaultRefusalQueue = 4096
	// DefaultRecordQueue is how many audit entries of allowed requests that
	// changed nothing (reads, and changes that failed) wait for the writer.
	DefaultRecordQueue = 8192
	// DefaultAuditBatch is the most entries one append writes. One synced
	// commit costs milliseconds whatever its size, so a batch of a thousand
	// costs about what one entry did.
	DefaultAuditBatch = 1000
	// DefaultAuditInterval is the longest an entry waits in a batch that is
	// not full: the window in which a killed server loses entries.
	DefaultAuditInterval = 200 * time.Millisecond
	// DefaultAuditTimeout bounds one append, so a store that hangs holds the
	// writer this long at most for a batch.
	DefaultAuditTimeout = 5 * time.Second
	// DefaultAuditRetries is how many times a batch is tried again after its
	// store answered Unavailable, before it is given up.
	DefaultAuditRetries = 3
	// DefaultAuditBackoff is the wait before the first retry; each next
	// wait is twice the one before, so the defaults wait 0.7 s in all.
	DefaultAuditBackoff = 100 * time.Millisecond
)

// Config is what New needs. Store, Clock and NewID are required; the rest
// have defaults.
type Config struct {
	// Store is the master database.
	Store Store
	// Clock returns the current time, for every audit entry and every
	// expiry. It is given, not read from the system, so a test moves it to
	// expire a key without sleeping.
	Clock func() time.Time
	// NewID returns a fresh id for an audit entry: 1 to 64 of A-Z, a-z,
	// 0-9, "_" and "-", unique among entries. A check given an id that is
	// not valid fails as Internal rather than lose its audit entry.
	NewID func() string
	// MarkQueue is how many last-use marks may wait; zero is
	// DefaultMarkQueue. When it is full, a new mark is dropped.
	MarkQueue int
	// RefusalQueue is how many refusals' audit entries may wait for the
	// audit writer; zero is DefaultRefusalQueue.
	RefusalQueue int
	// RecordQueue is how many allowed requests' audit entries handed to
	// Record or RecordTo may wait; zero is DefaultRecordQueue.
	RecordQueue int
	// AuditBatch is the most entries one append writes, 1 to
	// master.MaxAuditBatch; zero is DefaultAuditBatch.
	AuditBatch int
	// AuditInterval is the longest an entry waits before its batch is
	// written; zero is DefaultAuditInterval.
	AuditInterval time.Duration
	// AuditTimeout bounds one append; zero is DefaultAuditTimeout.
	AuditTimeout time.Duration
	// AuditRetries is how many times a batch whose store answered
	// Unavailable is tried again; zero is DefaultAuditRetries, and a
	// negative value is none.
	AuditRetries int
	// AuditBackoff is the wait before the first retry, doubled for each
	// next one; zero is DefaultAuditBackoff.
	AuditBackoff time.Duration
	// Events receives the access events (Event): refusals by a rule,
	// rotations, keys about to expire. Nil discards them; the audit log
	// records each refusal and rotation whatever it is.
	Events Events
	// TenantNetworks reads a tenant's allowed networks for the check. Nil
	// is no tenant's rule, for a server that has no tenants' databases (a
	// test of this package alone).
	TenantNetworks TenantNetworks
	// ExpiryWarning returns the installation's key.expiry-warning; with it
	// set, New starts a goroutine that raises EventKeyExpiring once a day
	// per key that expires within it, checking every ExpiryInterval. Nil
	// starts none.
	ExpiryWarning func(ctx context.Context) (time.Duration, error)
	// ExpiryInterval is how often the keys are read for EventKeyExpiring;
	// zero is DefaultExpiryInterval.
	ExpiryInterval time.Duration
	// Log receives what the package cannot return to anyone: a last-use
	// write or an audit append that failed, entries dropped. Nil discards.
	// No line carries a secret.
	Log *slog.Logger
}

// Access is the check and the management of keys and grants over one master
// database. It owns two goroutines, the last-use worker and the audit
// writer, which Close stops. Its methods are safe for concurrent use.
type Access struct {
	store Store
	clock func() time.Time
	newID func() string
	log   *slog.Logger
	seams accessSeams

	events         Events
	tenantNetworks TenantNetworks
	expiry         expiryWatch

	// sendMu makes a hand-off to a worker's queue and the start of Close
	// exclude each other: a sender holds it for reading while it checks
	// closed and sends without waiting; Close holds it for writing while it
	// sets closed. So no entry or mark reaches a queue after Close has
	// begun, and every one is counted, as written, failed or dropped.
	sendMu sync.RWMutex
	closed bool
	once   sync.Once
	stop   chan struct{}
	// workCtx is the context of every write the two workers make; Close
	// cancels it when its own deadline passes.
	workCtx    context.Context
	workCancel context.CancelFunc

	marks     chan mark
	marksDone chan struct{}
	markAcct  accounts

	refusals      chan master.AuditEntry
	records       chan auditItem
	auditFlush    chan chan struct{}
	auditDone     chan struct{}
	auditBatch    int
	auditInterval time.Duration
	auditTimeout  time.Duration
	auditRetries  int
	auditBackoff  time.Duration
	auditAcct     accounts
}

// accessSeams are the points at which a test of this package stands in for
// what it cannot otherwise observe or order. Each is nil in a server, and
// set only through export_test.go.
type accessSeams struct {
	// verify replaces the comparison of a presented secret with a stored
	// hash (secret.VerifyAPISecret), so a test can count the comparisons.
	verify func(sec string, stored []byte) bool
	// afterCancel runs in Close right after the workers' writes are
	// canceled at its deadline, so a test lets a worker finish what the
	// cancel cut short before Close counts anything, the order in which a
	// miscount would show.
	afterCancel func()
}

// verify reports whether the presented secret sec hashes to stored, in
// constant time.
func (a *Access) verify(sec string, stored []byte) bool {
	if a.seams.verify != nil {
		return a.seams.verify(sec, stored)
	}
	return secret.VerifyAPISecret(sec, stored)
}

// accounts counts what became of the items handed to one worker, and what
// the worker holds, under one lock, so that when Close gives up on a worker
// at its deadline, what it held is counted as dropped exactly once: the
// worker's later results for those items, if it ever returns, are ignored.
type accounts struct {
	mu        sync.Mutex
	abandoned bool
	held      int
	counts    map[string]uint64
}

// add counts n items of kind, unless the worker was given up on.
func (c *accounts) add(kind string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]uint64{}
	}
	c.counts[kind] += uint64(n) //nolint:gosec // G115: n counts items, never negative.
}

// take records that the worker took one item from its queue. It reports
// false when the worker was given up on: the item is then counted as
// dropped by Close's accounting and the worker must leave it.
func (c *accounts) take(dropKind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abandoned {
		if c.counts == nil {
			c.counts = map[string]uint64{}
		}
		c.counts[dropKind]++
		return false
	}
	c.held++
	return true
}

// resolve records that the worker settled n items it held, as kind. After
// the worker was given up on, it changes nothing: Close counted them.
func (c *accounts) resolve(kind string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abandoned {
		return
	}
	c.held -= n
	if c.counts == nil {
		c.counts = map[string]uint64{}
	}
	c.counts[kind] += uint64(n) //nolint:gosec // G115: n counts items, never negative.
}

// abandon gives up on the worker: what it holds is counted as dropKind, and
// so is everything taken after. It returns how many it held.
func (c *accounts) abandon(dropKind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abandoned {
		return 0
	}
	c.abandoned = true
	held := c.held
	if c.counts == nil {
		c.counts = map[string]uint64{}
	}
	c.counts[dropKind] += uint64(held) //nolint:gosec // G115: held counts items, never negative.
	c.held = 0
	return held
}

// get returns the count of kind.
func (c *accounts) get(kind string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[kind]
}

// New returns an Access over cfg.Store and starts its two workers. The
// caller must call Close when done.
func New(cfg Config) (*Access, error) {
	if cfg.Store == nil || cfg.Clock == nil || cfg.NewID == nil {
		return nil, fault.New(fault.Internal, "access: a store, a clock and an id source are required")
	}
	if cfg.MarkQueue < 0 || cfg.RefusalQueue < 0 || cfg.RecordQueue < 0 || cfg.AuditInterval < 0 ||
		cfg.AuditTimeout < 0 || cfg.AuditBackoff < 0 || cfg.AuditBatch < 0 || cfg.AuditBatch > master.MaxAuditBatch {
		return nil, fault.New(fault.Internal, "access: queue sizes and times are not negative, and a batch is at most "+strconv.Itoa(master.MaxAuditBatch))
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	retries := orDefault(cfg.AuditRetries, DefaultAuditRetries)
	if retries < 0 {
		retries = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Access{
		store:         cfg.Store,
		clock:         cfg.Clock,
		newID:         cfg.NewID,
		log:           log,
		stop:          make(chan struct{}),
		workCtx:       ctx,
		workCancel:    cancel,
		marks:         make(chan mark, orDefault(cfg.MarkQueue, DefaultMarkQueue)),
		marksDone:     make(chan struct{}),
		refusals:      make(chan master.AuditEntry, orDefault(cfg.RefusalQueue, DefaultRefusalQueue)),
		records:       make(chan auditItem, orDefault(cfg.RecordQueue, DefaultRecordQueue)),
		auditFlush:    make(chan chan struct{}),
		auditDone:     make(chan struct{}),
		auditBatch:    orDefault(cfg.AuditBatch, DefaultAuditBatch),
		auditInterval: orDefault(cfg.AuditInterval, DefaultAuditInterval),
		auditTimeout:  orDefault(cfg.AuditTimeout, DefaultAuditTimeout),
		auditRetries:  retries,
		auditBackoff:  orDefault(cfg.AuditBackoff, DefaultAuditBackoff),

		events:         cfg.Events,
		tenantNetworks: cfg.TenantNetworks,
		expiry: expiryWatch{
			warning:  cfg.ExpiryWarning,
			interval: orDefault(cfg.ExpiryInterval, DefaultExpiryInterval),
			done:     make(chan struct{}),
			raised:   map[string]time.Time{},
		},
	}
	go a.markWorker()
	go a.auditWriter()
	if a.expiry.warning != nil {
		go a.expiryWorker()
	} else {
		close(a.expiry.done)
	}
	return a, nil
}

// orDefault returns v, or def when v is zero.
func orDefault[T int | time.Duration](v, def T) T {
	if v == 0 {
		return def
	}
	return v
}

// Close stops both workers. Each first writes what it already holds and what
// is queued, until ctx is done. Then Close stops waiting, even for a store
// that ignores its context: the writes in flight are canceled, everything
// the workers held or had queued is counted as dropped, one line is logged,
// and Close returns ctx's error. A worker stuck in such a store returns when
// the store does, and changes no count then. Every entry and mark handed in
// is counted once, as written, failed or dropped. Checks made after Close
// still work, but their marks and audit entries are dropped and counted.
// Close does not close the store, which belongs to the caller.
func (a *Access) Close(ctx context.Context) error {
	a.once.Do(func() {
		a.sendMu.Lock()
		a.closed = true
		a.sendMu.Unlock()
		close(a.stop)
	})
	marksOK := waitDone(ctx, a.marksDone)
	auditOK := marksOK && waitDone(ctx, a.auditDone)
	// The expiry worker writes nothing that must be counted; it is waited
	// for so no goroutine outlives Close, and given up at the deadline.
	_ = waitDone(ctx, a.expiry.done)
	if marksOK && auditOK {
		a.workCancel()
		return nil
	}
	// The workers are given up on before their writes are canceled: what
	// they hold is then counted as dropped, and a write the cancel cuts
	// short changes no count when it returns. Canceled first, a worker
	// could count such a write as failed before Close counted it as
	// dropped, so the same mark fell in one count or the other by timing.
	marks := a.markAcct.abandon(markDropped)
	entries := a.auditAcct.abandon(auditDroppedWrites)
	a.workCancel()
	if a.seams.afterCancel != nil {
		a.seams.afterCancel()
	}
	queuedMarks := drainCount(a.marks)
	a.markAcct.add(markDropped, queuedMarks)
	marks += queuedMarks
	queued := drainCount(a.refusals) + drainCount(a.records)
	a.auditAcct.add(auditDroppedWrites, queued)
	a.log.Error("access: Close's deadline passed before the workers finished; what they held is dropped",
		"audit_entries", entries+queued, "marks", marks)
	return ctx.Err()
}

// waitDone waits for done or ctx, and reports whether done came first.
func waitDone(ctx context.Context, done chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// drainCount takes everything queued on ch now and returns how many.
func drainCount[T any](ch chan T) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}
