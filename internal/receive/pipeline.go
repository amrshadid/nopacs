package receive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Pipeline is one tenant's reception pipeline: it takes files from sources
// (Accept), records or holds each, and carries each reception through its
// steps with leased work. Make one with New, run it with Start, end it with
// Stop.
type Pipeline struct {
	cfg   Config
	store tenant.Pipeline
	area  *incoming.Area
	log   *slog.Logger

	// started is when this process's pipeline started: a reception that
	// was receiving across a restart counts its quiet period from it.
	started time.Time

	receivers  *receiverCache
	ruleCache  ruleCache
	receptions *receptionCache
	batch      *batcher
	scanLocks  keyedMutex
	// deid keeps the keys and engines of de-identification (deid.go).
	deid deid
	// quota admits new files against the tenant's quota (quota.go).
	quota quotaGate

	// mu guards stopping and accepting, so Stop's wait cannot pass an
	// Accept, or a decision (enterDecision), that has just begun.
	mu        sync.Mutex
	stopping  bool
	accepting sync.WaitGroup

	// wg holds every goroutine the pipeline starts; Stop waits for it.
	wg sync.WaitGroup
	// stopWork ends the workers' loops; abort ends the steps in flight.
	stopWork, abort  context.CancelFunc
	workCtx, stepCtx context.Context
	poke             chan struct{}
	batchStop        chan struct{}
	running          atomic.Bool

	stats Stats
	// meter measures the throughput a status reports.
	meter meter
	// stepping counts the steps in flight, and landedSeen the files landed
	// when the pass of frame tables last asked (busy).
	stepping, landedSeen atomic.Int64

	// limit is the header bound last read and when (headerLimit).
	limit struct {
		mu    sync.Mutex
		bytes int64
		read  time.Time
	}

	// hook, when not nil, runs at each durable effect of each step, for a
	// test that stops the process there or watches the steps
	// (export_test.go).
	hook func(point string)
}

// Stats are the pipeline's running counts since it started.
type Stats struct {
	// Landed counts files kept in the incoming area and acknowledged.
	Landed atomic.Int64
	// Recorded counts files recorded as received.
	Recorded atomic.Int64
	// Held counts files held in quarantine by the receiving side.
	Held atomic.Int64
	// EarlyDuplicates counts files the catalog already held, whose bytes
	// were let go when they were recorded.
	EarlyDuplicates atomic.Int64
	// Refused counts files refused as a temporary failure.
	Refused atomic.Int64
	// StepsDone counts steps that completed; StepsFailed those that failed
	// an attempt.
	StepsDone, StepsFailed atomic.Int64
	// MissingFiles counts files the database says are in the incoming
	// area and that Recover did not find there.
	MissingFiles atomic.Int64
}

// New returns a pipeline for the tenant cfg names. It opens the tenant's
// incoming area and does nothing else until Start.
func New(cfg Config) (*Pipeline, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	area, err := incoming.Open(cfg.IncomingRoot, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	p := &Pipeline{
		cfg:        cfg,
		store:      cfg.Store,
		area:       area,
		log:        cfg.Logger.With("tenant", cfg.Tenant),
		receivers:  newReceiverCache(cfg.Store, cfg.Clock),
		receptions: newReceptionCache(),
		poke:       make(chan struct{}, 1),
	}
	p.batch = newBatcher(p)
	return p, nil
}

// Stats returns the pipeline's running counts.
func (p *Pipeline) Stats() *Stats { return &p.stats }

// now returns the clock's time, in UTC and to the microsecond a store keeps,
// so a time read back compares equal to the one written.
func (p *Pipeline) now() time.Time {
	return p.cfg.Clock.Now().UTC().Truncate(time.Microsecond)
}

// headerLimit returns the bound a file's header is read within: the one
// read from Config.HeaderLimit, again once HeaderLimitInterval has passed.
// A read that fails keeps the last bound, or header.MaxHeader before any,
// and is logged: a setting that cannot be read never stops a reception.
func (p *Pipeline) headerLimit(ctx context.Context) int64 {
	if p.cfg.HeaderLimit == nil {
		return header.MaxHeader
	}
	now := p.cfg.Clock.Now()
	p.limit.mu.Lock()
	defer p.limit.mu.Unlock()
	if p.limit.bytes > 0 && now.Sub(p.limit.read) < HeaderLimitInterval && !now.Before(p.limit.read) {
		return p.limit.bytes
	}
	n, err := p.cfg.HeaderLimit(ctx)
	switch {
	case err != nil:
		p.log.Warn("receive: the header limit could not be read; the last one is kept", "reason", fault.Message(err))
	case n > 0:
		p.limit.bytes = n
	}
	p.limit.read = now
	if p.limit.bytes <= 0 {
		return header.MaxHeader
	}
	return p.limit.bytes
}

// at runs the test hook at a named point.
func (p *Pipeline) at(point string) {
	if p.hook != nil {
		p.hook(point)
	}
}

// wake tells the live feed that a commit wrote events.
func (p *Pipeline) wake() { p.cfg.Wake() }

// wakeWorkers makes an idle worker look for work at once.
func (p *Pipeline) wakeWorkers() {
	select {
	case p.poke <- struct{}{}:
	default:
	}
}

// newID returns a random id in the form master.ValidateID takes.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return hex.EncodeToString(b[:])
}

// derivedID returns an id made from parts, the same each time for the same
// parts: a record written again after a crash takes the id it took the first
// time, so the store skips it (HoldFiles skips an id it holds).
func derivedID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:40]
}

// safe returns the message of err that may be logged and kept: a fault's
// message, written to be shown, never a driver's raw text.
func safe(err error) string { return fault.Message(err) }

// reasonText returns err's safe message cut to what a unit's last error
// holds.
func reasonText(err error) string {
	s := safe(err)
	if s == "" {
		s = "the step failed"
	}
	if len(s) > tenant.MaxWorkTextLength {
		// A rune cut in two at the end is invalid UTF-8, which
		// ToValidUTF8 drops.
		s = s[:tenant.MaxWorkTextLength]
	}
	return strings.ToValidUTF8(s, "")
}

// keyedMutex is a set of mutexes by key, each made on first use and kept
// while held.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

// lock locks key and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedEntry{}
	}
	e := k.locks[key]
	if e == nil {
		e = &keyedEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// ScanLocks are the locks of a tenant's scans, by storage and manifest key,
// shared by every writer of a scan's manifest and sidecar in the process
// (Config.ScanLocks). Its zero value is ready to use.
type ScanLocks struct {
	k keyedMutex
}

// Lock takes the lock of the scan whose manifest is at manifestKey on the
// storage named storage, and returns its release.
func (s *ScanLocks) Lock(storage, manifestKey string) func() {
	return s.k.lock(storage + "\x00" + manifestKey)
}

// itoa returns n in decimal.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// validUID reports whether s is a well-formed UID (PS3.5 9.1).
func validUID(s string) bool { return uid.Valid(s) }
