package registry

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The reasons the registry gives. A reason is one of these, followed for the
// first two by the driver's safe message (fault.Message), and nothing else.
const (
	reasonUnreachable = "the database does not answer"
	reasonNotOpened   = "the database cannot be opened or brought to this version's schema"
	reasonDeadline    = "the database did not answer within its deadline"
	reasonAddress     = "the database address did not open with this key file; is it the key file the tenant was created with?"
	reasonNoDriver    = "this server has no driver for the database type "
	reasonPanic       = "the database driver failed unexpectedly; the server's log has the details"
)

// addressContext is the context a tenant's address is sealed under (package
// secret), which binds it to the tenant's row: a sealed address copied into
// another tenant's row does not open there.
func addressContext(id string) string {
	return "tenant/" + id + "/database"
}

// AddressContext is the context the address of the tenant whose id is id is
// sealed under, for a local command that opens a tenant's database with no
// registry: a backup made with the server stopped, a restore.
func AddressContext(id string) string { return addressContext(id) }

// refresh brings the entry's view, and the state the master holds, to what
// its database does now: it opens a tenant that has no store and pings one
// that has. On a timer check (first false) a failed tenant is left alone,
// since it waits for a person. The caller holds e.op.
func (r *Registry) refresh(e *entry, first bool) {
	if e.removed {
		return
	}
	if !first && e.view.Load().state == master.StateFailed {
		return
	}
	if e.opening.Load() {
		// An earlier driver call outlived its deadline and is still
		// running; another is not started beside it.
		r.settle(e, master.StateUnreachable, reasonDeadline)
		return
	}
	if e.store != nil {
		r.ping(e)
		return
	}
	r.open(e, first)
}

// stepError says which step of an open failed, which decides the kind a
// failed creation answers.
type stepError struct {
	step string // "open", "ping", "migrate" or "secret"
	err  error
}

// Error returns the step and the driver's safe message, never its cause.
func (s *stepError) Error() string { return s.step + ": " + fault.Message(s.err) }

// Unwrap returns the driver's error.
func (s *stepError) Unwrap() error { return s.err }

// open opens, pings and migrates the entry's database under its deadline,
// taking one of the opening slots, and settles the entry in the state that
// follows. The caller holds e.op. On a first open the view shows upgrading
// meanwhile; a retry keeps the state it had, so a tenant that stays down is
// not shown changing on every round.
func (r *Registry) open(e *entry, first bool) {
	drv, ok := r.cfg.Drivers[e.driver]
	if !ok {
		e.lastKind = fault.Invalid
		r.settle(e, master.StateFailed, reasonNoDriver+e.driver)
		return
	}
	address, err := r.cfg.Keys.Open(addressContext(e.id), e.sealed)
	if err != nil {
		e.lastKind = fault.Internal
		r.settle(e, master.StateFailed, reasonAddress)
		return
	}
	// The string is made here, before the open may outlive its deadline,
	// so clearing the bytes cannot change what a late open reads.
	plain := string(address)
	clear(address)

	select {
	case r.slots <- struct{}{}:
	case <-e.ctx.Done():
		return
	}
	defer func() { <-r.slots }()
	if first {
		e.view.Store(&view{state: master.StateUpgrading, since: r.cfg.Now()})
	}
	s, err := r.attempt(e, r.cfg.OpenTimeout, func(ctx context.Context) (tenant.Store, error) {
		s, err := drv.Open(ctx, e.id, e.name, plain)
		if err != nil {
			return nil, &stepError{"open", err}
		}
		if err := s.Ping(ctx); err != nil {
			_ = s.Close() // The ping failure is the one reported.
			return nil, &stepError{"ping", err}
		}
		if err := s.Migrate(ctx); err != nil {
			_ = s.Close() // The migration failure is the one reported.
			return nil, &stepError{"migrate", err}
		}
		if err := r.ensureSecrets(ctx, e.id, s); err != nil {
			_ = s.Close() // The failure to keep the key is the one reported.
			return nil, &stepError{"secret", err}
		}
		return s, nil
	})
	if err != nil {
		// The state follows the driver's kind (a failed migration waits for
		// a person); the kind a creation answers follows the API contract.
		e.lastKind = failureKind(err)
		r.logFailure(e, "open", err)
		if unavailable(err) {
			r.settle(e, master.StateUnreachable, because(reasonUnreachable, err))
		} else {
			r.settle(e, master.StateFailed, because(reasonNotOpened, err))
		}
		return
	}
	e.store = s
	r.fence(e)
	r.settle(e, master.StateReady, "")
}

// failureKind is the kind a creation that failed this way answers (the API
// contract's CreateTenant): Unavailable when the database does not answer or
// its tables cannot be made, Invalid when it refuses the address or its file,
// Internal for the rest, a driver's panic included.
func failureKind(err error) fault.Kind {
	if errors.Is(err, errPanic) {
		return fault.Internal
	}
	if unavailable(err) {
		return fault.Unavailable
	}
	var se *stepError
	if errors.As(err, &se) {
		switch {
		case se.step == "migrate":
			return fault.Unavailable
		case fault.KindOf(se.err) == fault.Invalid:
			return fault.Invalid
		}
	}
	return fault.Internal
}

// ping checks an open store under PingTimeout, without an opening slot, and
// settles the entry ready or unreachable. A panicking ping fails the tenant.
func (r *Registry) ping(e *entry) {
	s := e.store
	_, err := r.attempt(e, r.cfg.PingTimeout, func(ctx context.Context) (tenant.Store, error) {
		return nil, s.Ping(ctx)
	})
	switch {
	case err == nil:
		r.fence(e)
		r.settle(e, master.StateReady, "")
	case errors.Is(err, errPanic):
		r.logFailure(e, "ping", err)
		r.settle(e, master.StateFailed, reasonPanic)
	default:
		r.logFailure(e, "ping", err)
		r.settle(e, master.StateUnreachable, because(reasonUnreachable, err))
	}
}

// errDeadline is the cause of a driver call that outlived its deadline.
var errDeadline = errors.New("deadline passed")

// errPanic is the cause of a driver call that panicked.
var errPanic = errors.New("the driver panicked")

// attempt runs fn in a goroutine of its own under the entry's context and
// timeout, and returns what it returned, or Unavailable when the deadline
// passes first. A driver that does not return by then is left running,
// still counted by the registry (Close waits for it, up to its own deadline),
// with e.opening set until it returns; a store it returns late is closed,
// since nobody holds it. A panic in fn is recovered: the call fails with
// errPanic, and the stack goes to the log without the panic's value.
func (r *Registry) attempt(e *entry, timeout time.Duration, fn func(ctx context.Context) (tenant.Store, error)) (tenant.Store, error) {
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	type result struct {
		s   tenant.Store
		err error
	}
	var mu sync.Mutex
	gaveUp := false
	done := make(chan result, 1)
	r.inflight.Add(1)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.inflight.Add(-1)
		defer cancel()
		var res result
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.log.Error("a database driver panicked", "tenant", e.name, "stack", string(debug.Stack()))
					res = result{nil, fault.Wrap(fault.Internal, errPanic, reasonPanic)}
				}
			}()
			res.s, res.err = fn(ctx)
		}()
		mu.Lock()
		defer mu.Unlock()
		if gaveUp {
			if res.s != nil {
				_ = res.s.Close() // Nobody holds it; its error has no one to tell.
			}
			e.opening.Store(false)
			return
		}
		done <- res
	}()
	select {
	case res := <-done:
		return res.s, res.err
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	select {
	case res := <-done:
		// It returned as the deadline passed: what it returned is used.
		return res.s, res.err
	default:
	}
	gaveUp = true
	e.opening.Store(true)
	return nil, fault.Wrap(fault.Unavailable, errDeadline, reasonDeadline)
}

// unavailable reports whether err may pass if tried again: kind Unavailable
// or a context that ended.
func unavailable(err error) bool {
	var se *stepError
	if errors.As(err, &se) {
		err = se.err
	}
	return fault.KindOf(err) == fault.Unavailable ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// because returns a reason: what happened, then the driver's safe message
// (fault.Message), never its Error() text, which may hold the address in a
// form no scrubber finds. A deadline and a panic have their own phrases.
func because(what string, err error) string {
	switch {
	case errors.Is(err, errPanic):
		return reasonPanic
	case errors.Is(err, errDeadline) || errors.Is(err, context.DeadlineExceeded):
		return reasonDeadline
	}
	var se *stepError
	if errors.As(err, &se) {
		err = se.err
	}
	return fitReason(what + ": " + fault.Message(err))
}

// fitReason cuts a reason to the length the master keeps, on a UTF-8
// boundary, so the master never refuses it.
func fitReason(s string) string {
	if len(s) <= master.MaxStateReasonLength {
		return s
	}
	cut := master.MaxStateReasonLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// logFailure logs a failed driver call with the tenant's name, the step, the
// kind and the driver's safe message: never the error's text.
func (r *Registry) logFailure(e *entry, step string, err error) {
	inner := err
	var se *stepError
	if errors.As(err, &se) {
		step, inner = se.step, se.err
	}
	r.log.Warn("a tenant's database call failed", "tenant", e.name, "step", step,
		"kind", fault.KindOf(inner).String(), "message", fault.Message(inner))
}

// settle sets the entry's view to state and reason, with the entry's store
// when ready, and writes them to the master when they differ from what it
// holds. The caller holds e.op.
//
// The view changes at once, and its time is when the state or reason last
// changed. A change between ready and unreachable, or of reason within a
// state, is written at most once per StateChangeInterval: a change not
// written is written by a later check once the interval has passed, if it
// still holds. A change to or from upgrading or failed is written at once:
// those come from an open, not from a ping, and cannot flap. A master that
// no longer has the tenant makes the registry retire it.
func (r *Registry) settle(e *entry, state master.TenantState, reason string) {
	if state == master.StateReady && e.readOnly != "" {
		// A tenant held read only whose database answers shows the state
		// it is held in, with its store, for its reads.
		state, reason = e.readOnly, e.readOnlyReason
	}
	var s tenant.Store
	if state == master.StateReady || state.ReadOnly() {
		s = e.store
	}
	now := r.cfg.Now()
	old := e.view.Load()
	since := old.since
	if old.state != state || old.reason != reason || since.IsZero() {
		since = now
		r.log.Info("tenant state", "tenant", e.name, "state", string(state), "reason", reason)
	}
	e.view.Store(&view{state: state, reason: reason, since: since, store: s})
	if e.removed || e.holdWrite || (state == e.written && reason == e.writtenReason) {
		return
	}
	if e.readOnly != "" && !state.ReadOnly() {
		// The master keeps the read-only state while the database is down
		// (master.CheckStateChange refuses the server any other): what a
		// restart reads back must still be read only.
		return
	}
	limited := flapping(e.written, state) || state == e.written
	if limited && !e.lastWrite.IsZero() && now.Sub(e.lastWrite) < StateChangeInterval {
		return
	}
	audit := master.AuditEntry{
		ID:         r.cfg.NewID(),
		Time:       now,
		Method:     SystemMethod,
		TenantName: e.name,
		Object:     "tenant/" + e.name,
		Outcome:    master.OutcomeDone,
		Reason:     reason,
		Details: map[string]string{
			// The master adds the state before and after, read inside its
			// change (master.AuditEntry.WithStateChange).
			DetailActor: ActorSystem,
		},
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.OpenTimeout)
	defer cancel()
	if e.written == master.StateUpgrading && state == master.StateReady {
		// This finishes a creation a crash interrupted: the entry names the
		// request that began it, so the request's id leads to the end of it.
		if id := r.creationRequest(ctx, e); id != "" {
			audit.RequestID = id
			audit.Details[DetailCreationRequest] = id
		}
	}
	err := r.cfg.Master.SetTenantState(ctx, e.id, state, reason, audit, master.Server())
	switch {
	case err == nil:
		e.written, e.writtenReason, e.lastWrite = state, reason, now
	case fault.KindOf(err) == fault.NotFound:
		// The master no longer has this tenant: a removal committed that
		// the registry did not hear of. It is retired here, not served.
		r.log.Info("tenant unregistered: the master no longer has it", "tenant", e.name)
		r.unregister(e)
		r.retireLocked(e)
	default:
		// The master did not take it; a later check tries again, since what
		// it holds still differs.
		r.log.Warn("tenant state not recorded", "tenant", e.name, "state", string(state),
			"kind", fault.KindOf(err).String(), "message", fault.Message(err))
	}
}

// creationRequest returns the request id of the creation that wrote the
// tenant's row, from the latest StartMethod entry for the tenant in the
// master's log, or "" when none is found among the tenant's latest entries.
func (r *Registry) creationRequest(ctx context.Context, e *entry) string {
	entries, _, err := r.cfg.Master.ListAudit(ctx, master.AuditFilter{TenantName: e.name}, master.Page{Limit: 100})
	if err != nil {
		return ""
	}
	for _, a := range entries {
		if a.Method == StartMethod {
			return a.RequestID
		}
	}
	return ""
}

// flapping reports whether a change from one state to another is between
// ready and unreachable, which a database that comes and goes repeats.
func flapping(from, to master.TenantState) bool {
	pair := func(s master.TenantState) bool { return s == master.StateReady || s == master.StateUnreachable }
	return pair(from) && pair(to)
}
