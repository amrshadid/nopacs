package mastertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// This file holds the tests that pin what a second driver could otherwise
// get subtly wrong and still pass: cursors bound to their list, error kinds,
// a close with calls in flight, and both or neither for failures. The values
// a driver must keep exactly (ids, text, times, sizes) are in exact.go.

// wantDuplicateAudit fails unless err is ErrDuplicateAuditID of kind
// Internal.
func (e *env) wantDuplicateAudit(what string, err error) {
	e.Helper()
	if !errors.Is(err, master.ErrDuplicateAuditID) || fault.KindOf(err) != fault.Internal {
		e.Errorf("%s: %v (kind %v), want ErrDuplicateAuditID of kind Internal", what, err, fault.KindOf(err))
	}
}

// altered returns c with one character in its middle changed.
func altered(c string) string {
	i := len(c) / 2
	r := byte('A')
	if c[i] == 'A' {
		r = 'B'
	}
	return c[:i] + string(r) + c[i+1:]
}

// testCursorsBound guards that a cursor is accepted only by the list, and
// the arguments, it was made for: another list, another key or scope,
// another audit filter (any field), an altered cursor and a cut one are all
// Invalid (the API contract's page token rule).
func testCursorsBound(e *env) {
	lab, clinic := e.mustTenant("lab"), e.mustTenant("clinic")
	k1, k2 := e.mustKey(), e.mustKey()
	for _, k := range []master.Key{k1, k2} {
		e.mustGrant(k.ID, master.Installation, master.RoleReader)
		e.mustGrant(k.ID, master.TenantScope(lab.ID), master.RoleReader)
		e.mustGrant(k.ID, master.TenantScope(clinic.ID), master.RoleReader)
	}
	e.mustTenant("third")
	for i := range 3 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("s%d", i), "v", e.audit(), master.LocalOperator()))
	}
	filter := master.AuditFilter{TenantName: "lab", KeyID: k1.ID, Outcome: master.OutcomeDone, Since: T0, Until: T0.Add(time.Hour)}
	for range 3 {
		a := e.audit()
		a.TenantName, a.KeyID, a.LocalUser = "lab", k1.ID, ""
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}

	page := func(c string) master.Page { return master.Page{Limit: 1, Cursor: c} }
	type list struct {
		call func(cursor string) (string, error)
	}
	lists := map[string]list{
		"ListTenants": {func(c string) (string, error) { _, n, err := e.s.ListTenants(e.ctx, page(c)); return n, err }},
		"ListKeys":    {func(c string) (string, error) { _, n, err := e.s.ListKeys(e.ctx, page(c)); return n, err }},
		"GrantsByKey": {func(c string) (string, error) { _, n, err := e.s.GrantsByKey(e.ctx, k1.ID, page(c)); return n, err }},
		"GrantsByScope": {func(c string) (string, error) {
			_, n, err := e.s.GrantsByScope(e.ctx, master.TenantScope(lab.ID), page(c))
			return n, err
		}},
		"ListSettings": {func(c string) (string, error) { _, n, err := e.s.ListSettings(e.ctx, page(c)); return n, err }},
		"ListAudit":    {func(c string) (string, error) { _, n, err := e.s.ListAudit(e.ctx, filter, page(c)); return n, err }},
	}
	cursors := map[string]string{}
	for name, l := range lists {
		c, err := l.call("")
		e.ok(name, err)
		if c == "" {
			e.Fatalf("%s returned no cursor with more to come", name)
		}
		cursors[name] = c
		if _, err := l.call(c); err != nil {
			e.Errorf("%s refuses its own cursor: %v", name, err)
		}
		_, err = l.call(altered(c))
		e.wantKind(name+" with an altered cursor", err, fault.Invalid)
		_, err = l.call(c[:len(c)-1])
		e.wantKind(name+" with a cut cursor", err, fault.Invalid)
	}
	for name, l := range lists {
		for other, c := range cursors {
			if other == name {
				continue
			}
			_, err := l.call(c)
			e.wantKind(name+" with a cursor from "+other, err, fault.Invalid)
		}
	}

	// The same list with other arguments.
	_, _, err := e.s.GrantsByKey(e.ctx, k2.ID, page(cursors["GrantsByKey"]))
	e.wantKind("GrantsByKey cursor for another key", err, fault.Invalid)
	for name, scope := range map[string]master.Scope{"another tenant": master.TenantScope(clinic.ID), "the installation": master.Installation} {
		_, _, err := e.s.GrantsByScope(e.ctx, scope, page(cursors["GrantsByScope"]))
		e.wantKind("GrantsByScope cursor for "+name, err, fault.Invalid)
	}
	changed := map[string]master.AuditFilter{}
	f := filter
	f.TenantName = "clinic"
	changed["tenant"] = f
	f = filter
	f.KeyID = k2.ID
	changed["key"] = f
	f = filter
	f.Outcome = master.OutcomeFailed
	changed["outcome"] = f
	f = filter
	f.Since = T0.Add(-time.Second)
	changed["start time"] = f
	f = filter
	f.Until = T0.Add(2 * time.Hour)
	changed["end time"] = f
	changed["no filter"] = master.AuditFilter{}
	for name, f := range changed {
		_, _, err := e.s.ListAudit(e.ctx, f, page(cursors["ListAudit"]))
		e.wantKind("ListAudit cursor with another "+name, err, fault.Invalid)
	}
}

// testErrorKinds guards the kinds of the failures a caller acts on, and that
// a call that returned an error committed nothing: a context already
// canceled, a deadline already passed, and a context canceled while calls
// are in flight.
func testErrorKinds(e *env) {
	canceled, cancel := context.WithCancel(e.ctx)
	cancel()
	k, a := e.key(), e.audit()
	err := e.s.CreateKey(canceled, k, a, master.LocalOperator())
	e.wantKind("CreateKey with a canceled context", err, fault.Unavailable)
	if !errors.Is(err, context.Canceled) {
		e.Errorf("CreateKey with a canceled context: %v does not keep context.Canceled", err)
	}
	_, err = e.s.Key(canceled, "anything")
	e.wantKind("Key with a canceled context", err, fault.Unavailable)
	_, _, err = e.s.ListAudit(canceled, master.AuditFilter{}, master.Page{Limit: 5})
	e.wantKind("ListAudit with a canceled context", err, fault.Unavailable)

	past, cancelPast := context.WithDeadline(e.ctx, time.Now().Add(-time.Second))
	defer cancelPast()
	a2 := e.audit()
	err = e.s.SetSetting(past, "late", "v", a2, master.LocalOperator())
	e.wantKind("SetSetting past its deadline", err, fault.Unavailable)
	if !errors.Is(err, context.DeadlineExceeded) {
		e.Errorf("SetSetting past its deadline: %v does not keep context.DeadlineExceeded", err)
	}
	if _, err := e.s.Key(e.ctx, k.ID); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the key of the canceled CreateKey exists (%v)", err)
	}
	if _, err := e.s.Setting(e.ctx, "late"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the setting of the late SetSetting exists (%v)", err)
	}
	ids := e.auditIDs()
	if ids[a.ID] || ids[a2.ID] {
		e.Error("an audit entry of a call that failed was written")
	}

	// Contexts canceled at varied moments: whatever each call returned,
	// it committed exactly when it returned nil.
	type result struct {
		key   master.Key
		entry master.AuditEntry
		err   error
	}
	results := make([]result, 60)
	var wg sync.WaitGroup
	for i := range results {
		results[i].key, results[i].entry = e.key(), e.audit()
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(e.ctx)
			go func() { time.Sleep(time.Duration(i%6) * 300 * time.Microsecond); cancel() }()
			results[i].err = e.s.CreateKey(ctx, results[i].key, results[i].entry, master.LocalOperator())
			cancel()
		}()
	}
	wg.Wait()
	ids = e.auditIDs()
	failed := 0
	for _, r := range results {
		_, kerr := e.s.Key(e.ctx, r.key.ID)
		committed := kerr == nil
		if r.err != nil {
			failed++
			if k := fault.KindOf(r.err); k != fault.Unavailable {
				e.Errorf("CreateKey under a canceled context: kind %v (%v), want Unavailable", k, r.err)
			}
		}
		if committed != (r.err == nil) || ids[r.entry.ID] != (r.err == nil) {
			e.Errorf("CreateKey returned %v but the key present = %v, its audit entry present = %v", r.err, committed, ids[r.entry.ID])
		}
	}
	e.Logf("%d of %d calls failed under a canceled context", failed, len(results))
}

// testCloseInFlight guards that calls in flight while the store is closed
// fail as Unavailable, never with another kind or a panic.
func testCloseInFlight(e *env) {
	k := e.mustKey()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	stop := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				if i%2 == 0 {
					_, err = e.s.Key(e.ctx, k.ID)
				} else {
					err = e.s.SetSetting(e.ctx, "busy", "v", e.auditAt(i), master.LocalOperator())
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	e.ok("Close", e.s.Close())
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		if fault.KindOf(err) != fault.Unavailable {
			e.Errorf("a call in flight during Close: kind %v (%v), want Unavailable", fault.KindOf(err), err)
		}
	}
}

// auditMu guards env.seq for helpers called from several goroutines.
var auditMu sync.Mutex

// auditAt returns a new audit entry; safe to call from several goroutines.
func (e *env) auditAt(int) master.AuditEntry {
	auditMu.Lock()
	defer auditMu.Unlock()
	return e.audit()
}

// testFailuresWriteNoAudit guards both or neither in the other direction:
// for every failure of every changing operation, the audit entry it was
// handed is absent afterwards.
func testFailuresWriteNoAudit(e *env) {
	t := e.mustTenant("lab")
	k := e.mustKey()
	gone := e.mustKey()
	_, err := e.s.RevokeKey(e.ctx, gone.ID, T0.Add(time.Hour), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "kept", "v", e.audit(), master.LocalOperator()))

	type call struct {
		name string
		kind fault.Kind
		run  func(a master.AuditEntry) error
	}
	sameName := e.tenant("lab")
	sameID := e.tenant("other")
	sameID.ID = t.ID
	dupKey := e.key()
	dupKey.ID = k.ID
	badTenant := e.tenant("Bad")
	calls := []call{
		{"CreateTenant, name taken", fault.Conflict, func(a master.AuditEntry) error { return e.s.CreateTenant(e.ctx, sameName, a, master.LocalOperator()) }},
		{"CreateTenant, id taken", fault.Conflict, func(a master.AuditEntry) error { return e.s.CreateTenant(e.ctx, sameID, a, master.LocalOperator()) }},
		{"CreateTenant, invalid", fault.Invalid, func(a master.AuditEntry) error { return e.s.CreateTenant(e.ctx, badTenant, a, master.LocalOperator()) }},
		{"SetTenantState, unknown", fault.NotFound, func(a master.AuditEntry) error {
			return e.s.SetTenantState(e.ctx, "nope", master.StateFailed, "", a, master.LocalOperator())
		}},
		{"SetTenantState, invalid", fault.Invalid, func(a master.AuditEntry) error {
			return e.s.SetTenantState(e.ctx, t.ID, "gone", "", a, master.LocalOperator())
		}},
		{"RemoveTenant, unknown", fault.NotFound, func(a master.AuditEntry) error { return e.s.RemoveTenant(e.ctx, "nope", a, master.LocalOperator()) }},
		{"CreateKey, id taken", fault.Conflict, func(a master.AuditEntry) error { return e.s.CreateKey(e.ctx, dupKey, a, master.LocalOperator()) }},
		{"RevokeKey, unknown", fault.NotFound, func(a master.AuditEntry) error {
			_, err := e.s.RevokeKey(e.ctx, "nope", T0, a, master.LocalOperator())
			return err
		}},
		{"SetGrant, unknown key", fault.NotFound, func(a master.AuditEntry) error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: "nope", Scope: master.Installation, Role: master.RoleReader}, a, master.LocalOperator())
			return err
		}},
		{"SetGrant, unknown tenant", fault.NotFound, func(a master.AuditEntry) error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.TenantScope("nope"), Role: master.RoleReader}, a, master.LocalOperator())
			return err
		}},
		{"SetGrant, revoked key", fault.Conflict, func(a master.AuditEntry) error {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: gone.ID, Scope: master.Installation, Role: master.RoleReader}, a, master.LocalOperator())
			return err
		}},
		{"RemoveGrant, unknown key", fault.NotFound, func(a master.AuditEntry) error {
			_, err := e.s.RemoveGrant(e.ctx, "nope", master.Installation, a, master.LocalOperator())
			return err
		}},
		{"RemoveGrant, unknown tenant", fault.NotFound, func(a master.AuditEntry) error {
			_, err := e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope("nope"), a, master.LocalOperator())
			return err
		}},
		{"SetSetting, invalid", fault.Invalid, func(a master.AuditEntry) error {
			return e.s.SetSetting(e.ctx, "Bad Name", "v", a, master.LocalOperator())
		}},
		{"UnsetSetting, invalid", fault.Invalid, func(a master.AuditEntry) error {
			_, err := e.s.UnsetSetting(e.ctx, "Bad Name", a, master.LocalOperator())
			return err
		}},
		{"AppendAudit, an invalid entry in the batch", fault.Invalid, func(a master.AuditEntry) error {
			bad := e.audit()
			bad.Outcome = "maybe"
			return e.s.AppendAudit(e.ctx, a, bad)
		}},
	}
	for _, c := range calls {
		a := e.audit()
		e.wantKind(c.name, c.run(a), c.kind)
		if e.auditIDs()[a.ID] {
			e.Errorf("%s failed but wrote its audit entry", c.name)
		}
	}
}
