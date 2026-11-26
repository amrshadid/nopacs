package scrub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A read that finds an instance's bytes not as archived midway cuts its
// answer (it has sent bytes already, so it cannot turn to a replica), and
// until a scrub reads the instance every later read streams the same wrong
// bytes up to the same point. A request to scrub the instance closes that
// gap: the read asks (Supervisor.RequestInstance), the next unit of the
// tenant's scrubs reads the instance first, before its own work, and
// records what it finds as any scan's unit does, so a confirmed difference
// marks the instance and every later read fails before its first byte, or
// turns to a replica in step.
//
// The requests wait in memory, not in the database: a request lost to a
// restart costs only the time until the next scheduled scrub, which finds
// the same difference. They are bounded three ways, since a reader's key
// must not become a lever on the storage's bandwidth: one entry per
// instance however often it is asked (a read that fails asks on each
// retry), at most Config.MaxRequests waiting per tenant, and at most
// Config.RequestRate accepted per tenant a minute.
//
// A request that cannot be read (its storage or the tenant's database does
// not answer) never holds back the unit that took it: it goes to the end
// of the queue with its attempts counted, and is dropped, and logged, after
// maxRequestAttempts, or at once when its storage is unavailable; the unit
// then goes on with its own work. A dropped request costs what a lost one
// does.
//
// Requests are read by the units of scrubs that run anyway. When none runs,
// the supervisor makes a scrub of the one scan that holds the first request
// waiting, on its storage alone: a scan's read, not a whole storage's, so a
// stream of requests costs what the scans asked for and never becomes a
// stream of whole-storage reads; the rate and the bound of requests above
// bound it. Its unit reads the requests waiting first, as every unit does,
// then the scan. A scrub of the storage already queued or running serves
// instead, and its next unit reads them.

// The bounds of requests to scrub one instance.
const (
	// DefaultRequestRate is how many requests a tenant may make a minute.
	DefaultRequestRate = 30
	// DefaultMaxRequests is how many wait at most per tenant.
	DefaultMaxRequests = 256
	// requestsPerUnit is the most requests one unit reads before its own
	// work, so a unit's lease and its own work are not held back long.
	requestsPerUnit = 16
	// requestWindow is the time the rate is counted over.
	requestWindow = time.Minute
	// maxRequestAttempts is how many times a request is tried before it is
	// dropped.
	maxRequestAttempts = 3
)

// RequestMethod is the method the audit entry of a scrub the server makes
// for a request names.
const RequestMethod = "scrub.request"

// TallyRequestChecked counts the instances a job read for a request.
const TallyRequestChecked = "request|checked"

// InstanceRequest names one instance a read found not as archived.
type InstanceRequest struct {
	// Path names its scan; SOPInstanceUID the instance.
	Path           label.Path
	SOPInstanceUID string
}

// RequestOutcome is what became of a request.
type RequestOutcome int

// The outcomes of a request.
const (
	// RequestQueued: the request waits, to be read by the next unit.
	RequestQueued RequestOutcome = iota
	// RequestDuplicate: the instance waits already, or is being read; the
	// request is that one.
	RequestDuplicate
	// RequestFull: the tenant's requests waiting are at the bound.
	RequestFull
	// RequestLimited: the tenant made its requests of the minute.
	RequestLimited
	// RequestInvalid: the scan's labels or the UID are not well formed.
	RequestInvalid
)

// String returns the outcome's name, as a log shows it.
func (o RequestOutcome) String() string {
	switch o {
	case RequestQueued:
		return "queued"
	case RequestDuplicate:
		return "duplicate"
	case RequestFull:
		return "full"
	case RequestLimited:
		return "limited"
	case RequestInvalid:
		return "invalid"
	}
	return "unknown"
}

// requests holds every tenant's requests waiting.
type requests struct {
	rate, max int
	now       func() time.Time

	mu sync.Mutex
	by map[string]*requestQueue
}

// requestQueue is one tenant's requests: those waiting in order, those
// waiting or being read by instance, and the times of those accepted in
// the last window.
type requestQueue struct {
	waiting  []queuedRequest
	known    map[InstanceRequest]bool
	accepted []time.Time
}

// queuedRequest is a request waiting, with the times it was tried and
// failed.
type queuedRequest struct {
	req      InstanceRequest
	attempts int
}

// forget drops the queue of the tenant named name: a tenant removed or let
// go keeps no requests.
func (r *requests) forget(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.by, name)
}

// queue returns the tenant's queue, made on first use.
func (r *requests) queue(name string) *requestQueue {
	if r.by == nil {
		r.by = map[string]*requestQueue{}
	}
	q := r.by[name]
	if q == nil {
		q = &requestQueue{known: map[InstanceRequest]bool{}}
		r.by[name] = q
	}
	return q
}

// add puts a request in the tenant's queue, under the bounds.
func (r *requests) add(name string, req InstanceRequest) RequestOutcome {
	if tenant.ValidateScanPath(req.Path) != nil || tenant.ValidateInstanceUID(req.SOPInstanceUID) != nil {
		return RequestInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queue(name)
	if q.known[req] {
		return RequestDuplicate
	}
	if len(q.known) >= r.max {
		return RequestFull
	}
	now := r.now()
	kept := q.accepted[:0]
	for _, t := range q.accepted {
		if now.Sub(t) < requestWindow {
			kept = append(kept, t)
		}
	}
	q.accepted = kept
	if len(q.accepted) >= r.rate {
		return RequestLimited
	}
	q.accepted = append(q.accepted, now)
	q.waiting = append(q.waiting, queuedRequest{req: req})
	q.known[req] = true
	return RequestQueued
}

// take takes up to n of the tenant's requests waiting, first come first;
// each stays known, so a request for it meanwhile is a duplicate, until
// done, put back or queued again.
func (r *requests) take(name string, n int) []queuedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queue(name)
	n = min(n, len(q.waiting))
	out := append([]queuedRequest(nil), q.waiting[:n]...)
	q.waiting = q.waiting[n:]
	return out
}

// again puts requests taken and not read at the end of the queue, behind
// those waiting, so one that fails does not come first again. A request of
// a tenant forgotten meanwhile is dropped.
func (r *requests) again(name string, reqs []queuedRequest) {
	if len(reqs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.by[name]
	if q == nil {
		return
	}
	q.waiting = append(q.waiting, reqs...)
}

// done forgets a request read.
func (r *requests) done(name string, req InstanceRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q := r.by[name]; q != nil {
		delete(q.known, req)
	}
}

// putBack puts requests taken and not tried back at the head of the
// queue, as they were: for a unit that stopped before it read them. A
// request of a tenant forgotten meanwhile is dropped.
func (r *requests) putBack(name string, reqs []queuedRequest) {
	if len(reqs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.by[name]
	if q == nil {
		return
	}
	q.waiting = append(append([]queuedRequest(nil), reqs...), q.waiting...)
}

// first returns the tenant's first request waiting, and whether one waits.
func (r *requests) first(name string) (InstanceRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queue(name)
	if len(q.waiting) == 0 {
		return InstanceRequest{}, false
	}
	return q.waiting[0].req, true
}

// RequestInstance asks that the instance a read of the tenant named
// tenantName found not as archived be scrubbed before any other work of
// the tenant's scrubs. It never blocks and never fails the read: what
// became of the request is the answer, for the reader's log.
func (s *Supervisor) RequestInstance(tenantName string, req InstanceRequest) RequestOutcome {
	out := s.requests.add(tenantName, req)
	if out == RequestQueued {
		s.Poke(tenantName)
	}
	return out
}

// requested reads the requests waiting of the unit's tenant, at most
// requestsPerUnit, before the unit's own work, each instance on the
// storage the catalog holds it on, recording what it finds under the
// unit's lease as a scan's unit does. A request is forgotten once read, or
// once its instance is no longer in the catalog. One that fails is queued
// again at the end, or dropped after maxRequestAttempts (at once when its
// storage is unavailable); one not tried for want of a read slot is
// queued again without an attempt counted. No request's failure gives the
// unit back: only the unit's own end (its lease lost, the server
// stopping) stops it here.
func (u *unitRun) requested() outcome {
	name := u.t.name
	reqs := u.sup.requests.take(name, requestsPerUnit)
	if len(reqs) == 0 {
		return outcome{}
	}
	read := int64(0)
	for i, q := range reqs {
		storage, err := u.requestOne(q.req)
		switch {
		case err == nil:
			u.sup.requests.done(name, q.req)
			read++
			continue
		case u.ctx.Err() != nil || errors.Is(err, tenant.ErrLeaseLost) || errors.Is(err, errUnitStopped):
			u.sup.requests.putBack(name, reqs[i:])
			if errors.Is(err, tenant.ErrLeaseLost) {
				u.lost.Store(true)
			}
			return outcome{stopped: true}
		case fault.KindOf(err) == fault.Exhausted:
			// No read slot free: the rest wait for a later unit, none
			// counted as tried.
			u.sup.requests.again(name, reqs[i:])
		default:
			q.attempts++
			kind := fault.KindOf(err)
			if q.attempts < maxRequestAttempts && kind != fault.Unavailable {
				u.log.Info("scrubs: a requested instance could not be read; it is tried again later", "storage", storage,
					"kind", kind.String(), "attempts", q.attempts)
				u.sup.requests.again(name, []queuedRequest{q})
				continue
			}
			u.sup.requests.done(name, q.req)
			u.log.Warn("scrubs: a requested instance could not be read and the request is dropped; a later scrub reads it",
				"storage", storage, "kind", kind.String(), "attempts", q.attempts)
			continue
		}
		break
	}
	if read == 0 {
		return outcome{}
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	if err := u.p.AddScrubTallies(ctx, u.fence, map[string]int64{TallyRequestChecked: read}); err != nil {
		if u.ctx.Err() != nil || errors.Is(err, tenant.ErrLeaseLost) {
			return u.failure(err, reasonDatabase)
		}
		u.log.Warn("scrubs: the count of requested instances read could not be written", "reason", fault.Message(err))
	}
	return outcome{}
}

// errUnitStopped is a requested instance's read cut short by the unit's
// own end.
var errUnitStopped = errors.New("scrub: the unit stopped")

// requestOne reads one requested instance and records what it finds. It
// returns the storage the catalog holds it on (empty when not known yet)
// and why it could not be read: nil once read, or once its instance is
// no longer in the catalog.
func (u *unitRun) requestOne(req InstanceRequest) (string, error) {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	sc, err := u.p.CatalogScan(ctx, req.Path)
	var in tenant.ScanInstance
	if err == nil {
		in, err = u.p.HeldInstance(ctx, req.Path, req.SOPInstanceUID)
	}
	cancel()
	if fault.KindOf(err) == fault.NotFound {
		return "", nil // gone from the catalog: nothing to read
	}
	if err != nil {
		return "", err
	}
	store, err := u.stores.Get(u.ctx, sc.Storage)
	if err != nil {
		return sc.Storage, err
	}
	offset, length, size := ownBytes(in)
	m, err := u.measureTwice(store, sc.Storage, in.Key, offset, length, size, in.ArchivedSHA256)
	if err != nil {
		return sc.Storage, err
	}
	v, out := u.judgeInstance(sc.Storage, req.Path, in, size, m)
	switch {
	case out.stopped:
		return sc.Storage, errUnitStopped
	case out != (outcome{}):
		return sc.Storage, fault.New(fault.Internal, "what a requested instance's read found could not be recorded")
	case v.code != "":
		u.log.Warn("scrubs: an instance a read asked to be scrubbed is not what was archived", "code", string(v.code))
	}
	return sc.Storage, nil
}

// requestJob makes, when requests of the tenant wait and no unit of its
// scrubs was due, a scrub of the scan the first of them names, on the
// storage the catalog holds it on, so they are read now; a scrub of that
// storage already queued or running serves. It reports whether it made
// one.
func (t *tenantRun) requestJob(p tenant.Pipeline) bool {
	req, ok := t.sup.requests.first(t.name)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
	defer cancel()
	sc, err := p.CatalogScan(ctx, req.Path)
	if fault.KindOf(err) == fault.NotFound {
		// Gone from the catalog: forgotten, and the next looked at.
		if taken := t.sup.requests.take(t.name, 1); len(taken) == 1 {
			t.sup.requests.done(t.name, taken[0].req)
		}
		t.wake()
		return false
	}
	if err != nil {
		t.log.Warn("scrubs: a requested instance's scan could not be read; it waits", "reason", fault.Message(err))
		return false
	}
	now := t.sup.cfg.Now()
	id := requestJobID(sc.Storage, req.Path, now)
	audit := master.AuditEntry{ID: t.sup.cfg.NewID(), Time: now, Method: RequestMethod, TenantName: t.name, Object: id,
		Outcome: master.OutcomeDone, Details: map[string]string{"kind": string(tenant.JobScrub), "storage": sc.Storage}}
	target := tenant.JobTarget{SubjectLabel: req.Path.Subject, SessionLabel: req.Path.Session, ScanLabel: req.Path.Scan}
	_, err = p.CreateJob(ctx, tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: sc.Storage, Target: target, Scheduled: true,
		Created: now}, audit, nil)
	switch fault.KindOf(err) {
	case fault.Conflict:
		return false // made at this instant already, by this server or another
	case fault.Precondition:
		return false // a scrub of the storage runs: its next unit reads the requests
	default:
		if err != nil {
			t.log.Warn("scrubs: a scrub for requested instances could not be made", "storage", sc.Storage, "reason", fault.Message(err))
			return false
		}
	}
	t.log.Info("scrubs: a scrub of one scan was made to read instances reads asked for", "storage", sc.Storage, "job", id)
	t.sup.cfg.Wake(t.name)
	return true
}

// requestJobID returns the id of the scrub made at now for requests of the
// scan at path on storage.
func requestJobID(storage string, path label.Path, now time.Time) string {
	sum := sha256.Sum256([]byte(storage + "\x00" + path.Subject + "\x00" + path.Session + "\x00" + path.Scan + "\x00" +
		strconv.FormatInt(now.UnixNano(), 10)))
	return "scrub-request-" + hex.EncodeToString(sum[:10])
}
