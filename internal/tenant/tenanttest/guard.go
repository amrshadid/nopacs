package tenanttest

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// guardKey is the context key the guard tests use to see that a Guard gets
// the context its change was called with.
type guardKey struct{}

// testGuards guards the Guard every change takes (tenant.Store): a Guard
// that refuses stops the change, so neither the change nor its entry is
// written, and its error comes back as it is, kind and chain; a Guard that
// allows lets the change be written, once. A Guard runs with the change's
// context, inside the change's transaction, at least once (a driver that
// runs a change again runs its Guard again, so the effect is asserted, not
// the number of calls): what it reads of the
// store through another connection is what was there before the change,
// since the change is not committed yet. A nil Guard is no check.
func testGuards(e *env) {
	refusal := fault.Wrap(fault.Unauthenticated, &master.Refusal{Reason: master.RefusedRevoked}, "a test's refusal")
	ctx := context.WithValue(e.ctx, guardKey{}, "the change's")
	type op struct {
		name string
		// do makes the change with guard; seen returns whether it was made.
		do   func(guard tenant.Guard, audit master.AuditEntry) error
		seen func() bool
	}
	ops := func() []op {
		set, unset := e.id("s"), e.id("s")
		e.ok("SetSetting", e.s.SetSetting(e.ctx, set, "before", e.audit(), nil))
		e.ok("SetSetting", e.s.SetSetting(e.ctx, unset, "before", e.audit(), nil))
		value := func(name string) string {
			v, err := e.s.Setting(e.ctx, name)
			if fault.KindOf(err) == fault.NotFound {
				return "(none)"
			}
			e.ok("Setting", err)
			return v
		}
		return []op{
			{"SetSetting", func(g tenant.Guard, a master.AuditEntry) error {
				return e.s.SetSetting(ctx, set, "after", a, g)
			}, func() bool { return value(set) == "after" }},
			{"UnsetSetting", func(g tenant.Guard, a master.AuditEntry) error {
				_, err := e.s.UnsetSetting(ctx, unset, a, g)
				return err
			}, func() bool { return value(unset) == "(none)" }},
		}
	}

	for _, o := range ops() {
		var calls atomic.Int32
		audit := e.audit()
		err := o.do(func(context.Context) error {
			calls.Add(1)
			return refusal
		}, audit)
		e.wantKind(o.name+" with a Guard that refuses", err, fault.Unauthenticated)
		if !errors.Is(err, refusal) {
			e.Errorf("%s with a Guard that refuses: %v, want the Guard's error in the chain", o.name, err)
		}
		if r, ok := master.RefusalOf(err); !ok || r.Reason != master.RefusedRevoked {
			e.Errorf("%s with a Guard that refuses: %v, want its master.Refusal kept", o.name, err)
		}
		if o.seen() {
			e.Errorf("%s with a Guard that refuses: the change was made", o.name)
		}
		if e.auditIDs()[audit.ID] {
			e.Errorf("%s with a Guard that refuses: its audit entry was written", o.name)
		}
		if n := calls.Load(); n < 1 {
			e.Errorf("%s: the Guard did not run", o.name)
		}
	}

	for _, o := range ops() {
		var calls atomic.Int32
		audit := e.audit()
		var gotCtx any
		var seenInside bool
		err := o.do(func(gctx context.Context) error {
			calls.Add(1)
			gotCtx = gctx.Value(guardKey{})
			seenInside = o.seen()
			return nil
		}, audit)
		e.ok(o.name+" with a Guard that allows", err)
		if !o.seen() || !e.auditIDs()[audit.ID] {
			e.Errorf("%s with a Guard that allows: the change or its entry is missing", o.name)
		}
		if n := calls.Load(); n < 1 {
			e.Errorf("%s: the Guard did not run", o.name)
		}
		if gotCtx != "the change's" {
			e.Errorf("%s: the Guard got another context than the change's (value %v)", o.name, gotCtx)
		}
		if seenInside {
			e.Errorf("%s: the change was visible to another reader while its Guard ran: the Guard ran after the commit", o.name)
		}
	}

	for _, o := range ops() {
		audit := e.audit()
		e.ok(o.name+" with no Guard", o.do(nil, audit))
		if !o.seen() || !e.auditIDs()[audit.ID] {
			e.Errorf("%s with no Guard: the change or its entry is missing", o.name)
		}
	}
}

// testGuardRunsLast guards where a Guard runs in its change: last, after the
// change and its audit entry are written and before the commit. A change
// whose entry cannot be written (its id taken) fails before the Guard's
// turn, so the Guard never runs. And a second change of the same setting,
// made while the first one's Guard is running, cannot run its own Guard
// until the first has committed: its write waits for the first one's write
// lock or row, and its Guard comes after its write. A Guard run before the
// writes would run at once.
func testGuardRunsLast(e *env) {
	name := e.id("s")
	taken := e.audit()
	e.ok("SetSetting", e.s.SetSetting(e.ctx, name, "first", taken, nil))
	var ran atomic.Bool
	err := e.s.SetSetting(e.ctx, name, "second", taken, func(context.Context) error {
		ran.Store(true)
		return nil
	})
	e.wantDuplicateAudit("SetSetting with an entry id taken", err)
	if ran.Load() {
		e.Error("a change whose audit entry could not be written ran its Guard: the Guard ran before the entry was written")
	}
	ran.Store(false)
	_, err = e.s.UnsetSetting(e.ctx, name, taken, func(context.Context) error {
		ran.Store(true)
		return nil
	})
	e.wantDuplicateAudit("UnsetSetting with an entry id taken", err)
	if ran.Load() {
		e.Error("an unset whose audit entry could not be written ran its Guard")
	}

	inFirst, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- e.s.SetSetting(e.ctx, name, "a", e.audit(), func(context.Context) error {
			close(inFirst)
			<-release
			return nil
		})
	}()
	<-inFirst
	var secondRan atomic.Bool
	second := make(chan error, 1)
	audit := e.audit()
	go func() {
		second <- e.s.SetSetting(e.ctx, name, "b", audit, func(context.Context) error {
			secondRan.Store(true)
			return nil
		})
	}()
	time.Sleep(200 * time.Millisecond)
	early := secondRan.Load()
	close(release)
	e.ok("the first SetSetting", <-first)
	e.ok("the second SetSetting", <-second)
	if early {
		e.Error("a change's Guard ran while another change of the same setting was not yet committed: the Guard ran before the change's write")
	}
	if v, err := e.s.Setting(e.ctx, name); err != nil || v != "b" {
		e.Errorf("after the two changes the setting is %q (%v), want the second's", v, err)
	}
}
