package mastertest

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// change is one changing operation of the port as the actor tests make it:
// prepare sets up what it changes (as the local operator) and returns the
// call, made by an actor with an audit entry, and a probe that reports
// whether the call changed anything.
type change struct {
	name    string
	prepare func(e *env) (do func(audit master.AuditEntry, by master.Actor) error, changed func() bool)
}

// changes returns every operation of the port that takes an Actor.
func changes() []change {
	return []change{
		{"CreateTenant", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := e.tenant(e.id("t"))
			return func(a master.AuditEntry, by master.Actor) error {
					return e.s.CreateTenant(e.ctx, t, a, by)
				}, func() bool {
					_, err := e.s.Tenant(e.ctx, t.ID)
					return err == nil
				}
		}},
		{"SetTenantState", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := e.mustTenant(e.id("t"))
			return func(a master.AuditEntry, by master.Actor) error {
					return e.s.SetTenantState(e.ctx, t.ID, master.StateFailed, "a test", a, by)
				}, func() bool {
					got, err := e.s.Tenant(e.ctx, t.ID)
					e.ok("Tenant", err)
					return got.State != master.StateReady
				}
		}},
		{"RemoveTenant", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := e.mustTenant(e.id("t"))
			return func(a master.AuditEntry, by master.Actor) error {
					return e.s.RemoveTenant(e.ctx, t.ID, a, by)
				}, func() bool {
					_, err := e.s.Tenant(e.ctx, t.ID)
					return err != nil
				}
		}},
		{"CreateKey", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := e.key()
			return func(a master.AuditEntry, by master.Actor) error {
					return e.s.CreateKey(e.ctx, k, a, by)
				}, func() bool {
					_, err := e.s.Key(e.ctx, k.ID)
					return err == nil
				}
		}},
		{"RevokeKey", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := e.mustKey()
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := e.s.RevokeKey(e.ctx, k.ID, T0.Add(time.Minute), a, by)
					return err
				}, func() bool {
					got, err := e.s.Key(e.ctx, k.ID)
					e.ok("Key", err)
					return !got.Revoked.IsZero()
				}
		}},
		{"SetGrant", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := e.mustKey()
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleReader}, a, by)
					return err
				}, func() bool {
					r, err := e.s.RolesFor(e.ctx, k.ID, "")
					e.ok("RolesFor", err)
					return r.Installation != ""
				}
		}},
		{"RemoveGrant", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := e.mustKey()
			e.mustGrant(k.ID, master.Installation, master.RoleReader)
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := e.s.RemoveGrant(e.ctx, k.ID, master.Installation, a, by)
					return err
				}, func() bool {
					r, err := e.s.RolesFor(e.ctx, k.ID, "")
					e.ok("RolesFor", err)
					return r.Installation == ""
				}
		}},
		{"SetSetting", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			name := e.id("s")
			return func(a master.AuditEntry, by master.Actor) error {
					return e.s.SetSetting(e.ctx, name, "v", a, by)
				}, func() bool {
					_, err := e.s.Setting(e.ctx, name)
					return err == nil
				}
		}},
		{"UnsetSetting", func(e *env) (func(master.AuditEntry, master.Actor) error, func() bool) {
			name := e.id("s")
			e.ok("SetSetting", e.s.SetSetting(e.ctx, name, "v", e.audit(), master.LocalOperator()))
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := e.s.UnsetSetting(e.ctx, name, a, by)
					return err
				}, func() bool {
					_, err := e.s.Setting(e.ctx, name)
					return err != nil
				}
		}},
	}
}

// at returns a clock that always reads t, for an actor judged at a known
// time.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// adminKey creates a key with admin on the installation and returns it with
// its actor at T0, which may act.
func (e *env) adminKey() (master.Key, master.Actor) {
	e.Helper()
	k := e.mustKey()
	e.mustGrant(k.ID, master.Installation, master.RoleAdmin)
	return k, master.ActingKey(k.ID, master.Installation, master.RoleAdmin, at(T0))
}

// lapse is one way a key that could act when its request was checked can no
// longer act when the change is written.
type lapse struct {
	name   string
	kind   fault.Kind
	reason string
	// apply takes the right away from k, whose actor by could act, and
	// returns the actor the change is made by.
	apply func(e *env, k master.Key, by master.Actor) master.Actor
}

// lapses are the four ways the issue names, and a key that is gone.
func lapses() []lapse {
	return []lapse{
		{"revoked", fault.Unauthenticated, master.RefusedRevoked, func(e *env, k master.Key, by master.Actor) master.Actor {
			_, err := e.s.RevokeKey(e.ctx, k.ID, T0, e.audit(), master.LocalOperator())
			e.ok("RevokeKey", err)
			return by
		}},
		{"expired", fault.Unauthenticated, master.RefusedExpired, func(_ *env, k master.Key, by master.Actor) master.Actor {
			// The change is written at the key's expiry: an expiry is the
			// first moment the key no longer works.
			return master.ActingKey(k.ID, by.Scope(), by.Role(), at(k.Expires))
		}},
		{"whose grant was removed", fault.Denied, master.RefusedNoRole, func(e *env, k master.Key, by master.Actor) master.Actor {
			_, err := e.s.RemoveGrant(e.ctx, k.ID, master.Installation, e.audit(), master.LocalOperator())
			e.ok("RemoveGrant", err)
			return by
		}},
		{"whose grant was lowered", fault.Denied, master.RefusedNoRole, func(e *env, k master.Key, by master.Actor) master.Actor {
			_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleOperator}, e.audit(), master.LocalOperator())
			e.ok("SetGrant", err)
			return by
		}},
		{"gone", fault.Unauthenticated, master.RefusedUnknownKey, func(_ *env, _ master.Key, by master.Actor) master.Actor {
			// A key the master never had: what an id left in a Decision
			// whose key is no longer there looks like to the store.
			return master.ActingKey("never-made", by.Scope(), by.Role(), at(T0))
		}},
	}
}

// wantRefused fails unless err is a refusal of the change's actor, of kind
// want and with the reason given, and the change wrote neither itself nor its
// audit entry.
func (e *env) wantRefused(what string, err error, kind fault.Kind, reason string, changed func() bool, audit master.AuditEntry) {
	e.Helper()
	e.wantKind(what, err, kind)
	if r, ok := master.RefusalOf(err); !ok || r.Reason != reason {
		e.Errorf("%s: %v, want a refusal for %q", what, err, reason)
	}
	if changed() {
		e.Errorf("%s: the refused change was made", what)
	}
	if e.auditIDs()[audit.ID] {
		e.Errorf("%s: the refused change wrote its audit entry", what)
	}
}

// testActorChecked guards the check every change makes of its actor inside
// its transaction: a key that could act when its request was checked, and
// is then revoked, expires, has its grant removed or lowered, or is gone,
// makes no change and writes no entry, and gets Unauthenticated or Denied
// with a master.Refusal saying why; the same change by a key that may still
// act, by the local operator and by the server is made; a refusal comes
// before what the change would otherwise meet (NotFound); and a zero Actor
// is Invalid.
func testActorChecked(e *env) {
	for _, c := range changes() {
		for _, l := range lapses() {
			what := c.name + " by a key " + l.name + " since the check"
			k, by := e.adminKey()
			if err := e.s.CheckActor(e.ctx, by); err != nil {
				e.Fatalf("%s: the key could not act at the check: %v", what, err)
			}
			do, changed := c.prepare(e)
			by = l.apply(e, k, by)
			audit := e.audit()
			e.wantRefused(what, do(audit, by), l.kind, l.reason, changed, audit)
		}

		_, by := e.adminKey()
		for _, actor := range []master.Actor{by, master.LocalOperator(), master.Server()} {
			do, changed := c.prepare(e)
			audit := e.audit()
			if err := do(audit, actor); err != nil {
				e.Errorf("%s by %v: %v", c.name, actor, err)
				continue
			}
			if !changed() || !e.auditIDs()[audit.ID] {
				e.Errorf("%s by %v: succeeded without the change and its entry", c.name, actor)
			}
		}

		do, changed := c.prepare(e)
		audit := e.audit()
		e.wantKind(c.name+" by the zero Actor", do(audit, master.Actor{}), fault.Invalid)
		if changed() || e.auditIDs()[audit.ID] {
			e.Errorf("%s by the zero Actor wrote something", c.name)
		}
	}

	// A refused key learns nothing of what it named: removing a tenant that
	// does not exist is the refusal, not NotFound.
	k, by := e.adminKey()
	_, err := e.s.RevokeKey(e.ctx, k.ID, T0, e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	e.wantKind("RemoveTenant of no tenant by a revoked key", e.s.RemoveTenant(e.ctx, "no-such-tenant", e.audit(), by), fault.Unauthenticated)
}

// testActorOnTenant guards a role on a tenant, inside a change: a key that is
// operator on a tenant acts by that role, the same key is refused a role it
// does not hold, a grant on the installation is not a role on the tenant,
// and when the tenant is removed, its grants with it, the key's role there
// is gone for the next change.
func testActorOnTenant(e *env) {
	t := e.mustTenant(e.id("t"))
	k := e.mustKey()
	e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleOperator)
	e.mustGrant(k.ID, master.Installation, master.RoleAdmin)
	on := func(role master.Role) master.Actor {
		return master.ActingKey(k.ID, master.TenantScope(t.ID), role, at(T0))
	}

	e.ok("SetSetting by the tenant's operator", e.s.SetSetting(e.ctx, e.id("s"), "v", e.audit(), on(master.RoleOperator)))
	e.ok("SetSetting needing writer, by the tenant's operator", e.s.SetSetting(e.ctx, e.id("s"), "v", e.audit(), on(master.RoleWriter)))
	audit := e.audit()
	err := e.s.SetSetting(e.ctx, e.id("s"), "v", audit, on(master.RoleAdmin))
	e.wantRefused("SetSetting needing admin on the tenant, by its operator who is admin on the installation", err,
		fault.Denied, master.RefusedNoRole, func() bool { return false }, audit)

	e.ok("RemoveTenant", e.s.RemoveTenant(e.ctx, t.ID, e.audit(), master.LocalOperator()))
	name := e.id("s")
	audit = e.audit()
	err = e.s.SetSetting(e.ctx, name, "v", audit, on(master.RoleOperator))
	e.wantRefused("SetSetting by the operator of a tenant since removed", err, fault.Denied, master.RefusedNoRole, func() bool {
		_, err := e.s.Setting(e.ctx, name)
		return err == nil
	}, audit)
}

// testCheckActor guards CheckActor, the same check on its own: what it
// answers for each kind of actor and each state of a key, the expiry's exact
// boundary, the scope of a role, and an Actor that is not valid.
func testCheckActor(e *env) {
	t := e.mustTenant(e.id("t"))
	admin := e.mustKey()
	e.mustGrant(admin.ID, master.Installation, master.RoleAdmin)
	op := e.mustKey()
	e.mustGrant(op.ID, master.TenantScope(t.ID), master.RoleOperator)
	revoked := e.mustKey()
	e.mustGrant(revoked.ID, master.Installation, master.RoleAdmin)
	_, err := e.s.RevokeKey(e.ctx, revoked.ID, T0, e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)

	inst, ten := master.Installation, master.TenantScope(t.ID)
	for _, c := range []struct {
		name   string
		by     master.Actor
		kind   fault.Kind // fault.Internal stands for no error
		reason string
	}{
		{"the local operator", master.LocalOperator(), fault.Internal, ""},
		{"the server", master.Server(), fault.Internal, ""},
		{"admin on the installation", master.ActingKey(admin.ID, inst, master.RoleAdmin, at(T0)), fault.Internal, ""},
		{"a valid key, no role asked", master.ActingKey(admin.ID, inst, "", at(T0)), fault.Internal, ""},
		{"a microsecond before the expiry", master.ActingKey(admin.ID, inst, master.RoleAdmin, at(admin.Expires.Add(-time.Microsecond))), fault.Internal, ""},
		{"at the expiry", master.ActingKey(admin.ID, inst, master.RoleAdmin, at(admin.Expires)), fault.Unauthenticated, master.RefusedExpired},
		{"admin on the installation, asked on a tenant", master.ActingKey(admin.ID, ten, master.RoleReader, at(T0)), fault.Denied, master.RefusedNoRole},
		{"operator on the tenant", master.ActingKey(op.ID, ten, master.RoleOperator, at(T0)), fault.Internal, ""},
		{"operator on the tenant, asked for writer", master.ActingKey(op.ID, ten, master.RoleWriter, at(T0)), fault.Internal, ""},
		{"operator on the tenant, asked for admin", master.ActingKey(op.ID, ten, master.RoleAdmin, at(T0)), fault.Denied, master.RefusedNoRole},
		{"operator on the tenant, asked on the installation", master.ActingKey(op.ID, inst, master.RoleReader, at(T0)), fault.Denied, master.RefusedNoRole},
		{"revoked", master.ActingKey(revoked.ID, inst, master.RoleAdmin, at(T0)), fault.Unauthenticated, master.RefusedRevoked},
		{"revoked, no role asked", master.ActingKey(revoked.ID, inst, "", at(T0)), fault.Unauthenticated, master.RefusedRevoked},
		{"unknown", master.ActingKey("never-made", inst, master.RoleReader, at(T0)), fault.Unauthenticated, master.RefusedUnknownKey},
	} {
		err := e.s.CheckActor(e.ctx, c.by)
		if c.kind == fault.Internal {
			if err != nil {
				e.Errorf("CheckActor of %s: %v, want nil", c.name, err)
			}
			continue
		}
		e.wantKind("CheckActor of "+c.name, err, c.kind)
		if r, ok := master.RefusalOf(err); !ok || r.Reason != c.reason {
			e.Errorf("CheckActor of %s: %v, want a refusal for %q", c.name, err, c.reason)
		}
	}
	for name, by := range map[string]master.Actor{
		"the zero Actor":      {},
		"a key with no id":    master.ActingKey("", inst, master.RoleAdmin, at(T0)),
		"a key's bad role":    master.ActingKey(admin.ID, inst, "owner", at(T0)),
		"a key's bad scope":   master.ActingKey(admin.ID, master.TenantScope("not an id"), master.RoleAdmin, at(T0)),
		"a key with no clock": master.ActingKey(admin.ID, inst, master.RoleAdmin, nil),
	} {
		e.wantKind("CheckActor of "+name, e.s.CheckActor(e.ctx, by), fault.Invalid)
	}
}

// testActorRevokeRace guards that a change and a revocation of its key are
// ordered: see actorRevokeRace.
func testActorRevokeRace(e *env) {
	actorRevokeRace(e, e.s, e.s, 60)
}

// actorRevokeRace races a change by a key, on s1, against the revocation of
// that key, on s2, round after round, and checks each outcome: either the
// change was made, with its entry, and that entry is dated before the
// revocation had committed, or the change was refused as made by a revoked
// key and neither it nor its entry is there. Times come from one counter, so
// "before" is the order the calls took them in: the change's entry takes its
// time as the change begins, the revocation's end takes one after RevokeKey
// returned. In every third round the change begins only after the revocation
// returned, so it must be refused; the other rounds overlap, one side or the
// other starting a moment late, so both orders happen.
func actorRevokeRace(e *env, s1, s2 master.Store, rounds int) {
	var tick atomic.Int64
	now := func() time.Time { return T0.Add(time.Duration(tick.Add(1)) * time.Microsecond) }
	made, refused := 0, 0
	for round := range rounds {
		k := e.key()
		e.ok("CreateKey", s1.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
		_, err := s1.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleAdmin}, e.audit(), master.LocalOperator())
		e.ok("SetGrant", err)
		name, change, revocation := e.id("s"), e.audit(), e.audit()
		after := round%3 == 0
		revoked := make(chan struct{})
		var revokedAt time.Time
		var cerr, rerr error
		// The revocation's lag grows over the rounds, up to a few
		// milliseconds, so the change wins some rounds on a database whose
		// round trips are slow as well as on one whose are fast.
		lag := func(mine int) {
			if !after && round%3 == mine {
				time.Sleep(time.Duration(1+(mine*(round/3))%8) * 200 * time.Microsecond)
			}
		}
		race(func() {
			lag(1)
			_, rerr = s2.RevokeKey(e.ctx, k.ID, now(), revocation, master.LocalOperator())
			revokedAt = now()
			close(revoked)
		}, func() {
			lag(2)
			if after {
				<-revoked
			}
			change.Time = now()
			cerr = s1.SetSetting(e.ctx, name, "v", change, master.ActingKey(k.ID, master.Installation, master.RoleAdmin, at(change.Time)))
		})
		e.ok("RevokeKey", rerr)
		what := fmt.Sprintf("round %d", round)
		_, serr := s1.Setting(e.ctx, name)
		var entry *master.AuditEntry
		for _, a := range e.allAudit(master.AuditFilter{Since: change.Time, Until: change.Time.Add(time.Microsecond)}) {
			if a.ID == change.ID {
				entry = &a
			}
		}
		if cerr == nil {
			made++
			if after {
				e.Errorf("%s: a change begun after its key's revocation returned was made", what)
			}
			if serr != nil || entry == nil {
				e.Errorf("%s: the change succeeded without the setting (%v) or its entry (%v)", what, serr, entry != nil)
			} else if !entry.Time.Before(revokedAt) {
				e.Errorf("%s: the change's entry is dated %v, not before the revocation committed at %v", what, entry.Time, revokedAt)
			}
		} else {
			refused++
			e.wantKind(what+": the refused change", cerr, fault.Unauthenticated)
			if r, ok := master.RefusalOf(cerr); !ok || r.Reason != master.RefusedRevoked {
				e.Errorf("%s: %v, want a refusal for a revoked key", what, cerr)
			}
			if serr == nil || entry != nil {
				e.Errorf("%s: the refused change left the setting (%v) or its entry (%v)", what, serr == nil, entry != nil)
			}
		}
	}
	e.Logf("a change racing its key's revocation: %d made first, %d refused", made, refused)
}

// testCrossedKeyChanges guards changes of keys made by keys, at once, each on
// the other or on itself: key A setting B's grant while B sets A's, and A
// setting its own grant twice at once. Each such change locks its actor's
// key and the key it changes; taken in two orders, those locks would wait on
// each other until the database broke the tie by failing one change. Every
// change here must succeed.
func testCrossedKeyChanges(e *env) {
	a, byA := e.adminKey()
	b, byB := e.adminKey()
	t := e.mustTenant(e.id("t"))
	set := func(target string, by master.Actor, role master.Role, audit master.AuditEntry) error {
		_, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: target, Scope: master.TenantScope(t.ID), Role: role}, audit, by)
		return err
	}
	for round := range 40 {
		role := master.RoleReader
		if round%2 == 1 {
			role = master.RoleWriter
		}
		a1, a2, a3, a4 := e.audit(), e.audit(), e.audit(), e.audit()
		var err1, err2 error
		race(func() { err1 = set(b.ID, byA, role, a1) }, func() { err2 = set(a.ID, byB, role, a2) })
		if err1 != nil || err2 != nil {
			e.Errorf("round %d: A on B and B on A at once: %v, %v", round, err1, err2)
		}
		race(func() { err1 = set(a.ID, byA, role, a3) }, func() { err2 = set(a.ID, byA, role, a4) })
		if err1 != nil || err2 != nil {
			e.Errorf("round %d: A on itself twice at once: %v, %v", round, err1, err2)
		}
	}
}
