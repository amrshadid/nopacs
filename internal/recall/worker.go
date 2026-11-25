package recall

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Queue is the part of the tenant's database the worker uses: the rows of
// recalls due, and the record of what a store said of one.
type Queue interface {
	DueRecalls(ctx context.Context, now time.Time, limit int) ([]tenant.Recall, error)
	RecordRecall(ctx context.Context, a tenant.RecallAnswer) (tenant.Recall, error)
}

// OpenFunc opens the tenant's storage of this name. The worker closes what
// it opens, once per pass for each storage.
type OpenFunc func(ctx context.Context, storage string) (blob.Store, error)

// The defaults of a Config's bounds.
const (
	// DefaultBatch is how many due recalls one pass takes.
	DefaultBatch = 100
	// DefaultPoll is how long a recall the store is working on waits before
	// its next Stat: a restore of a cold object takes minutes to hours, so a
	// short poll only spends requests.
	DefaultPoll = time.Minute
	// DefaultIdle is how long Run waits after a pass that found nothing.
	DefaultIdle = 5 * time.Second
	// DefaultRetryMin and DefaultRetryMax bound the wait after a temporary
	// failure of the store: it doubles with each failure in a row of the
	// object, from the first to the second.
	DefaultRetryMin = 30 * time.Second
	DefaultRetryMax = 30 * time.Minute
)

// The reasons a recall is recorded failed with. They are fixed phrases: a
// store's own text never reaches the row, since a hostile or misconfigured
// server could put anything there, and the row is shown to operators.
const (
	// ReasonNotRecaller: the storage cannot be asked to bring objects back.
	ReasonNotRecaller = "the storage cannot recall objects"
	// ReasonRefused: the store would not take the recall as asked.
	ReasonRefused = "the storage would not take the recall as asked"
	// ReasonMissing: the storage holds no object at the key.
	ReasonMissing = "the storage holds no object at the key"
	// ReasonRejected: the request or the storage's configuration was
	// refused as invalid.
	ReasonRejected = "the recall request was refused as invalid"
	// ReasonUnopenable: the storage is not configured any more.
	ReasonUnopenable = "the storage is not configured"
)

// opTimeout bounds one write to the database and one call to a store.
const opTimeout = 30 * time.Second

// Config is what a Worker is made with.
type Config struct {
	// Queue is the tenant's database.
	Queue Queue
	// Open opens a storage by name.
	Open OpenFunc
	// Now is the clock every time the worker records comes from.
	Now func() time.Time
	// Batch, Poll, Idle, RetryMin and RetryMax are the bounds above; zero
	// takes the default.
	Batch    int
	Poll     time.Duration
	Idle     time.Duration
	RetryMin time.Duration
	RetryMax time.Duration
	// Log receives what the worker does; nil discards it. It never
	// receives an object's key.
	Log *slog.Logger
}

// Worker asks stores to bring objects back from their cold class and polls
// them until they read. It is safe for concurrent use; Run is called once.
type Worker struct {
	cfg  Config
	log  *slog.Logger
	wake chan struct{}

	mu sync.Mutex
	// failures counts the temporary failures in a row per object, for the
	// wait before the next try. It is pacing only: a restart that forgets it
	// starts again from RetryMin.
	failures map[objectID]int
}

// objectID names one recall's object.
type objectID struct{ storage, key string }

// New returns a worker over cfg, its zero bounds given their defaults.
func New(cfg Config) (*Worker, error) {
	if cfg.Queue == nil || cfg.Open == nil || cfg.Now == nil {
		return nil, fault.New(fault.Invalid, "recall: the queue, the opener and the clock are required")
	}
	if cfg.Batch < 0 || cfg.Batch > tenant.MaxRecallBatch || cfg.Poll < 0 || cfg.Idle < 0 || cfg.RetryMin < 0 || cfg.RetryMax < 0 {
		return nil, fault.New(fault.Invalid, "recall: a bound is negative, or the batch is above the most a pass reads")
	}
	if cfg.Batch == 0 {
		cfg.Batch = DefaultBatch
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&cfg.Poll, DefaultPoll)
	def(&cfg.Idle, DefaultIdle)
	def(&cfg.RetryMin, DefaultRetryMin)
	def(&cfg.RetryMax, DefaultRetryMax)
	if cfg.RetryMax < cfg.RetryMin {
		cfg.RetryMax = cfg.RetryMin
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Worker{cfg: cfg, log: log, wake: make(chan struct{}, 1), failures: map[objectID]int{}}, nil
}

// Wake asks a waiting Run to read the due recalls at once: a request that
// made a recall calls it so the store is asked without waiting for the
// next poll.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run reads and answers due recalls until ctx ends, and returns nil then. A
// pass that found fewer than a batch waits Idle (or a Wake); a full batch
// reads again at once.
func (w *Worker) Run(ctx context.Context) error {
	for {
		pass, err := w.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.log.Warn("recalls: the pass failed; it is tried again", "kind", fault.KindOf(err).String())
		}
		if err == nil && pass.Due >= w.cfg.Batch {
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
	// Due counts the recalls read.
	Due int
	// Asked counts the recalls asked of a store; Polled those whose object
	// was looked at.
	Asked, Polled int
	// Online counts those that now read; Failed those recorded failed;
	// Retried those the store could not answer now and are due later;
	// Stopped those left for the next pass because ctx ended.
	Online, Failed, Retried, Stopped int
}

// RunOnce reads one batch of due recalls and answers each, in the order the
// database gave them, and returns what it did. An error is a read of the
// due rows that failed; a recall's own failure is recorded on its row and
// counted in the Pass.
func (w *Worker) RunOnce(ctx context.Context) (Pass, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	due, err := w.cfg.Queue.DueRecalls(cctx, w.cfg.Now(), w.cfg.Batch)
	cancel()
	if err != nil {
		return Pass{}, err
	}
	pass := Pass{Due: len(due)}
	stores := map[string]blob.Store{}
	defer func() {
		for _, s := range stores {
			_ = s.Close()
		}
	}()
	for i, r := range due {
		if ctx.Err() != nil {
			pass.Stopped += len(due) - i
			break
		}
		w.step(ctx, r, stores, &pass)
	}
	return pass, nil
}

// step answers one due recall: asks the store when it was not asked yet,
// looks at the object otherwise, and records what the store said.
func (w *Worker) step(ctx context.Context, r tenant.Recall, stores map[string]blob.Store, pass *Pass) {
	id := objectID{r.Storage, r.Key}
	st, ok := stores[r.Storage]
	if !ok {
		var err error
		octx, cancel := context.WithTimeout(ctx, opTimeout)
		st, err = w.cfg.Open(octx, r.Storage)
		cancel()
		if err != nil {
			w.answerError(ctx, r, err, ReasonUnopenable, pass)
			return
		}
		stores[r.Storage] = st
	}
	rc, ok := st.(blob.Recaller)
	if !ok {
		w.fail(ctx, r, ReasonNotRecaller, pass)
		return
	}
	now := w.cfg.Now
	if r.State == tenant.RecallRequested {
		// The store has not been asked: ask once. The answer decides
		// whether the object reads already or a restore is running.
		pass.Asked++
		cctx, cancel := context.WithTimeout(ctx, opTimeout)
		ans, err := rc.Recall(cctx, r.Key, r.Request())
		cancel()
		if err != nil {
			w.answerError(ctx, r, err, "", pass)
			return
		}
		w.clear(id)
		a := tenant.RecallAnswer{Storage: r.Storage, Key: r.Key, State: tenant.RecallStateOf(ans.State), Now: now()}
		if a.State == tenant.RecallOnline {
			a.OnlineUntil = ans.OnlineUntil
			pass.Online++
		} else {
			a.Next = a.Now.Add(w.cfg.Poll)
		}
		w.record(ctx, a)
		return
	}
	// In progress: the store was asked; look at the object.
	pass.Polled++
	sctx, cancel := context.WithTimeout(ctx, opTimeout)
	info, err := st.Stat(sctx, r.Key)
	cancel()
	if err != nil {
		w.answerError(ctx, r, err, "", pass)
		return
	}
	w.clear(id)
	a := tenant.RecallAnswer{Storage: r.Storage, Key: r.Key, Now: now()}
	switch info.State {
	case blob.Online:
		a.State, a.OnlineUntil = tenant.RecallOnline, info.OnlineUntil
		pass.Online++
	case blob.Archived:
		// A restore the store lost, or one that came back and went again,
		// or an earlier ask that failed for a reason that passed: the
		// store answers with the restore it already runs when there is
		// one, so asking again never starts a second.
		pass.Asked++
		cctx, cancel := context.WithTimeout(ctx, opTimeout)
		ans, rerr := rc.Recall(cctx, r.Key, r.Request())
		cancel()
		if rerr != nil {
			w.answerError(ctx, r, rerr, "", pass)
			return
		}
		a.State = tenant.RecallStateOf(ans.State)
		if a.State == tenant.RecallOnline {
			a.OnlineUntil = ans.OnlineUntil
			pass.Online++
		} else {
			a.Next = a.Now.Add(w.cfg.Poll)
		}
	default:
		a.State, a.Next = tenant.RecallInProgress, a.Now.Add(w.cfg.Poll)
	}
	w.record(ctx, a)
}

// answerError records what an error from the store or the opener means for
// r: a refusal that will not pass is a failed recall with a fixed reason;
// anything else (the store unreachable, slow, or overloaded) puts the
// recall due again after a wait that grows with each failure in a row. An
// ended ctx records nothing: the row stays due for the next pass.
// openReason, when set, is the reason of a NotFound that came from opening
// the storage rather than from asking for the object.
func (w *Worker) answerError(ctx context.Context, r tenant.Recall, err error, openReason string, pass *Pass) {
	if ctx.Err() != nil {
		pass.Stopped++
		return
	}
	switch {
	case errors.Is(err, blob.ErrRecallRefused):
		w.fail(ctx, r, ReasonRefused, pass)
		return
	case openReason != "" && fault.KindOf(err) == fault.NotFound:
		w.fail(ctx, r, openReason, pass)
		return
	case fault.KindOf(err) == fault.NotFound:
		w.fail(ctx, r, ReasonMissing, pass)
		return
	case fault.KindOf(err) == fault.Invalid:
		w.fail(ctx, r, ReasonRejected, pass)
		return
	}
	pass.Retried++
	now := w.cfg.Now()
	w.log.Warn("recalls: the storage could not answer; the recall is tried again later", "storage", r.Storage, "kind", fault.KindOf(err).String())
	// A recall not yet asked is recorded in progress: the next pass looks
	// at the object and asks again if it is still cold, which the store
	// answers without a second restore.
	w.record(ctx, tenant.RecallAnswer{Storage: r.Storage, Key: r.Key, State: tenant.RecallInProgress, Next: now.Add(w.backoff(objectID{r.Storage, r.Key})), Now: now})
}

// backoff counts one more failure in a row of id and returns the wait that
// follows it.
func (w *Worker) backoff(id objectID) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.failures[id]
	w.failures[id] = n + 1
	d := w.cfg.RetryMin
	for ; n > 0 && d < w.cfg.RetryMax; n-- {
		d *= 2
	}
	return min(d, w.cfg.RetryMax)
}

// clear forgets the failures in a row of id.
func (w *Worker) clear(id objectID) {
	w.mu.Lock()
	delete(w.failures, id)
	w.mu.Unlock()
}

// fail records r failed with a fixed reason.
func (w *Worker) fail(ctx context.Context, r tenant.Recall, reason string, pass *Pass) {
	w.clear(objectID{r.Storage, r.Key})
	pass.Failed++
	w.log.Warn("recalls: a recall failed", "storage", r.Storage, "reason", reason)
	w.record(ctx, tenant.RecallAnswer{Storage: r.Storage, Key: r.Key, State: tenant.RecallFailed, Reason: reason, Now: w.cfg.Now()})
}

// record writes an answer. It is not cut short by a stop: the store has
// answered, and an answer lost is a second ask of the store after a restart.
// A failed write leaves the row as it was, due now, for the next pass.
func (w *Worker) record(ctx context.Context, a tenant.RecallAnswer) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if _, err := w.cfg.Queue.RecordRecall(rctx, a); err != nil {
		w.log.Warn("recalls: an answer could not be recorded; it is read again", "storage", a.Storage, "kind", fault.KindOf(err).String())
	}
}
