package replicate

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/pace"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Queue is the part of the tenant's database the worker writes: the claims
// of replicas' scans, their leases, their ends and failures, and a copy's
// damage found on read back; and the row of a scan it reads, for the
// manifest digest the replica holds verified (tenant.Replication).
type Queue interface {
	ReplicaScan(ctx context.Context, replica, seriesUID string) (tenant.ReplicaScan, error)
	ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error)
	RenewReplicaScan(ctx context.Context, fence tenant.ReplicaFence, now time.Time, lease time.Duration) error
	EndReplicaScan(ctx context.Context, fence tenant.ReplicaFence, manifest tenant.SHA256, now time.Time) (tenant.ReplicaScanState, error)
	FailReplicaScan(ctx context.Context, f tenant.ReplicaFailure) error
	RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error)
}

// Catalog is the part of the catalog the worker reads: the instances a scan
// holds now, which a delete's hidden instances are not among, each of
// whose manifest lines must agree with its row (tenant.InstanceReads).
type Catalog interface {
	ListHeldInstances(ctx context.Context, path label.Path, page master.Page) ([]tenant.ScanInstance, string, error)
}

// OpenFunc opens the tenant's storage of this name for reading and writing.
// The worker closes what it opens.
type OpenFunc func(ctx context.Context, storage string) (blob.Store, error)

// The defaults of a Config's bounds.
const (
	// DefaultLease is how long a claim holds a scan; the worker renews it
	// every third of it. Each put and each read back has five sixths of
	// it, so the lease must be longer than the largest object takes to put
	// on the slowest replica.
	DefaultLease = time.Minute
	// DefaultBatch is how many scans one claim leases.
	DefaultBatch = 16
	// DefaultScans is how many claimed scans are copied at once.
	DefaultScans = 4
	// DefaultParallel is how many objects are copied at once to one
	// replica, across every scan: it bounds the memory the streams hold and
	// the load on the replica.
	DefaultParallel = 4
	// DefaultIdle is how long Run waits after a claim that found nothing.
	DefaultIdle = 5 * time.Second
	// DefaultRetryMin and DefaultRetryMax bound the wait before a failed
	// scan is due again: it doubles with each failure in a row on its
	// replica, from the first to the second.
	DefaultRetryMin = 30 * time.Second
	DefaultRetryMax = 30 * time.Minute
	// DefaultChangedDelay is how long a scan whose primary changed under
	// the copy waits before it is due again: a reception in flight has
	// written its next manifest and commits within seconds.
	DefaultChangedDelay = 5 * time.Second
)

// opTimeout bounds one write to the database.
const opTimeout = 30 * time.Second

// Config is what a Worker is made with.
type Config struct {
	// Queue and Catalog are the tenant's database.
	Queue   Queue
	Catalog Catalog
	// Open opens a storage by name.
	Open OpenFunc
	// Now is the clock every time the worker records comes from.
	Now func() time.Time
	// Owner names this worker in its claims.
	Owner string
	// Lease, Batch, Scans, Parallel, Idle, RetryMin, RetryMax and
	// ChangedDelay are the bounds above; zero takes the default.
	Lease        time.Duration
	Batch        int
	Scans        int
	Parallel     int
	Idle         time.Duration
	RetryMin     time.Duration
	RetryMax     time.Duration
	ChangedDelay time.Duration
	// Rate, when set, returns replica.rate as it applies on the tenant now:
	// the most bytes a second put on one replica, 0 for no bound. It is
	// read at each claim; nil bounds nothing.
	Rate func(ctx context.Context) (int64, error)
	// Log receives what the worker does; nil discards it.
	Log *slog.Logger

	// seams are what a test changes.
	seams seams
}

// seams are the parts of a Config only a test sets.
type seams struct {
	// now and sleep are the clock and the wait of the pacing of puts; nil
	// are Config.Now and a timer.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Worker copies the scans of primaries to their replicas. It is safe for
// concurrent use; Run is called once.
type Worker struct {
	cfg  Config
	log  *slog.Logger
	wake chan struct{}

	mu sync.Mutex
	// slots bound the copies at once per replica.
	slots map[string]chan struct{}
	// failures counts the failed copies in a row per replica, for the wait
	// before the next try; a copy ended in step resets it. It is pacing
	// only, so a restart that forgets it starts again from RetryMin.
	failures map[string]int
	// pacers hold the bytes put on each replica to replica.rate, across
	// every copy to it at once.
	pacers map[string]*pace.Pacer
	// rate is replica.rate as the last claim read it, 0 for no bound.
	rate atomic.Int64
}

// New returns a worker over cfg, its zero bounds given their defaults.
func New(cfg Config) (*Worker, error) {
	if cfg.Queue == nil || cfg.Catalog == nil || cfg.Open == nil || cfg.Now == nil {
		return nil, fault.New(fault.Invalid, "replicate: the queue, the catalog, the opener and the clock are required")
	}
	if cfg.Owner == "" {
		return nil, fault.New(fault.Invalid, "replicate: the worker needs an owner name for its claims")
	}
	if cfg.Lease < 0 || cfg.Batch < 0 || cfg.Batch > tenant.MaxReplicaClaim || cfg.Scans < 0 || cfg.Parallel < 0 || cfg.Idle < 0 ||
		cfg.RetryMin < 0 || cfg.RetryMax < 0 || cfg.ChangedDelay < 0 {
		return nil, fault.New(fault.Invalid, "replicate: a bound is negative, or the batch is above the most a claim leases")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	defInt := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	def(&cfg.Lease, DefaultLease)
	def(&cfg.Idle, DefaultIdle)
	def(&cfg.RetryMin, DefaultRetryMin)
	def(&cfg.RetryMax, DefaultRetryMax)
	def(&cfg.ChangedDelay, DefaultChangedDelay)
	defInt(&cfg.Batch, DefaultBatch)
	defInt(&cfg.Scans, DefaultScans)
	defInt(&cfg.Parallel, DefaultParallel)
	if cfg.RetryMax < cfg.RetryMin {
		cfg.RetryMax = cfg.RetryMin
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Worker{cfg: cfg, log: log, wake: make(chan struct{}, 1), slots: map[string]chan struct{}{}, failures: map[string]int{},
		pacers: map[string]*pace.Pacer{}}, nil
}

// readRate reads replica.rate for the claim about to be made. A read that
// fails keeps the rate the last claim read, so a database that does not
// answer for a moment neither lifts a bound nor stops the copies.
func (w *Worker) readRate(ctx context.Context) {
	if w.cfg.Rate == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	n, err := w.cfg.Rate(cctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("replicas: replica.rate could not be read; the last rate read holds", "kind", fault.KindOf(err).String())
		}
		return
	}
	w.rate.Store(max(n, 0))
}

// pacing returns the replica's pacer and the rate read last, bytes a
// second; a rate of 0 bounds nothing and returns no pacer.
func (w *Worker) pacing(replica string) (*pace.Pacer, int64) {
	rate := w.rate.Load()
	if rate <= 0 {
		return nil, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pacers[replica]
	if p == nil {
		now := w.cfg.seams.now
		if now == nil {
			now = w.cfg.Now
		}
		p = pace.New(now, w.cfg.seams.sleep)
		w.pacers[replica] = p
	}
	return p, rate
}

// Wake asks a waiting Run to claim again at once: a commit that made scans
// pending calls it so a copy need not wait for the next poll.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run claims and copies until ctx ends, and returns nil then. A claim that
// found nothing, or failed, waits Idle (or a Wake) before the next; a full
// batch claims again at once.
func (w *Worker) Run(ctx context.Context) error {
	for {
		pass, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.log.Warn("replicas: the claim of scans to copy failed; it is tried again", "kind", fault.KindOf(err).String())
		}
		if err == nil && pass.Claimed >= w.cfg.Batch {
			continue
		}
		t := time.NewTimer(w.cfg.Idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-w.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Pass is what one RunOnce did.
type Pass struct {
	// Claimed counts the scans claimed.
	Claimed int
	// InStep counts those ended in step; Again those ended but not in
	// step, a change of the scan published while it copied; Failed those
	// whose failure was recorded; Stopped those left to their lease (a
	// lease lost or a stop).
	InStep, Again, Failed, Stopped int
	// Bytes counts the bytes put on replicas.
	Bytes int64
}

// RunOnce claims one batch of scans and copies them, at most Scans at once,
// and returns what it did. An error is a claim that failed; a scan's own
// failure is recorded on its row and counted in the Pass.
func (w *Worker) RunOnce(ctx context.Context) (Pass, error) {
	w.readRate(ctx)
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	claims, err := w.cfg.Queue.ClaimReplicaScans(cctx, w.cfg.Owner, w.cfg.Now(), w.cfg.Lease, w.cfg.Batch)
	cancel()
	if err != nil {
		return Pass{}, err
	}
	pass := Pass{Claimed: len(claims)}
	if len(claims) == 0 {
		return pass, nil
	}
	stores := &storeSet{open: w.cfg.Open, held: map[string]blob.Store{}}
	defer stores.close()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, w.cfg.Scans)
	for _, c := range claims {
		sem <- struct{}{}
		wg.Add(1)
		go func(c tenant.ReplicaClaim) {
			defer func() { <-sem; wg.Done() }()
			out, n := w.copyScan(ctx, c, stores)
			mu.Lock()
			defer mu.Unlock()
			pass.Bytes += n
			switch out {
			case endedInStep:
				pass.InStep++
			case endedAgain:
				pass.Again++
			case failed:
				pass.Failed++
			default:
				pass.Stopped++
			}
		}(c)
	}
	wg.Wait()
	return pass, nil
}

// putTimeout bounds one put on a replica, and one read back: shorter than
// the lease, so a storage that hangs fails the put while the lease still
// holds, rather than keeping the scan claimed by renewals for ever.
func (w *Worker) putTimeout() time.Duration { return w.cfg.Lease - w.cfg.Lease/6 }

// slot takes one of the replica's copy slots, waiting for one, and returns
// its release; ctx ending first is its error.
func (w *Worker) slot(ctx context.Context, replica string) (func(), error) {
	w.mu.Lock()
	ch, ok := w.slots[replica]
	if !ok {
		ch = make(chan struct{}, w.cfg.Parallel)
		w.slots[replica] = ch
	}
	w.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// retryAfter returns when a scan of the replica that just failed is due
// again, and counts the failure: RetryMin doubled for each failure in a
// row before it, at most RetryMax.
func (w *Worker) retryAfter(replica string, now time.Time) time.Time {
	w.mu.Lock()
	n := w.failures[replica]
	w.failures[replica] = n + 1
	w.mu.Unlock()
	d := w.cfg.RetryMin
	for range min(n, 32) {
		if d >= w.cfg.RetryMax/2 {
			d = w.cfg.RetryMax
			break
		}
		d *= 2
	}
	return now.Add(min(d, w.cfg.RetryMax))
}

// succeeded resets the replica's failures in a row.
func (w *Worker) succeeded(replica string) {
	w.mu.Lock()
	delete(w.failures, replica)
	w.mu.Unlock()
}

// storeSet opens each storage a pass reaches once and closes them at its
// end.
type storeSet struct {
	open OpenFunc
	mu   sync.Mutex
	held map[string]blob.Store
}

// get returns the storage named name, opening it on first use.
func (s *storeSet) get(ctx context.Context, name string) (blob.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.held[name]; ok {
		return st, nil
	}
	st, err := s.open(ctx, name)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fault.New(fault.Internal, "replicate: the opener answered no storage")
	}
	s.held[name] = st
	return st, nil
}

// close closes every storage opened. A close after puts that each returned
// already loses nothing: every put was kept or failed when it returned.
func (s *storeSet) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.held {
		_ = st.Close() // Every put returned before this; nothing is left to keep.
	}
	s.held = map[string]blob.Store{}
}
