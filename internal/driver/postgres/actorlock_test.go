package postgres

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The tests in this file put a change and what would undo its actor's right
// in a known order on the database's row locks, which a test of the port
// alone cannot do: a third connection holds a row, the undoing change is
// queued on it first and the change second, and the row is let go. The
// undoing change then commits while the change waits for its lock, the case
// a check that read before it held its locks would get wrong.

// lockRig is one schema with its store, a connection of the test's own, and
// a clock the actors read, which a test moves.
type lockRig struct {
	t       *testing.T
	ctx     context.Context
	address string
	m       *Master
	conn    *pgx.Conn
	now     atomic.Int64
	n       atomic.Int64
}

// newLockRig makes a rig whose store has enough connections for the changes
// a test runs at once.
func newLockRig(t *testing.T) *lockRig {
	t.Helper()
	address := withParams(t, newSchema(t), map[string]string{"pool_max_conns": "8"})
	r := &lockRig{t: t, ctx: context.Background(), address: address, m: openAt(t, address)}
	if err := r.m.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.conn = admin(t, address)
	r.now.Store(lockT0.UnixMicro())
	return r
}

// lockT0 is the rig's clock when it starts.
var lockT0 = time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)

// clock is the rig's clock, which actors read inside their transactions.
func (r *lockRig) clock() time.Time { return time.UnixMicro(r.now.Load()).UTC() }

// id returns a new id with the prefix.
func (r *lockRig) id(prefix string) string { return fmt.Sprintf("%s%06d", prefix, r.n.Add(1)) }

// audit returns a new audit entry.
func (r *lockRig) audit() master.AuditEntry {
	return master.AuditEntry{ID: r.id("a"), Time: lockT0, LocalUser: "501", Method: "Test", Outcome: master.OutcomeDone, RequestID: "r"}
}

// ok fails the test now on an error.
func (r *lockRig) ok(what string, err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// key makes a key that expires an hour after the rig's start, with the
// grants given.
func (r *lockRig) key(grants ...master.Grant) master.Key {
	r.t.Helper()
	k := master.Key{ID: r.id("k"), Hash: make([]byte, master.KeyHashLength), Holder: "h", Created: lockT0, Expires: lockT0.Add(time.Hour)}
	r.ok("CreateKey", r.m.CreateKey(r.ctx, k, r.audit(), master.LocalOperator()))
	for _, g := range grants {
		g.KeyID = k.ID
		_, err := r.m.SetGrant(r.ctx, g, r.audit(), master.LocalOperator())
		r.ok("SetGrant", err)
	}
	return k
}

// tenant makes a tenant.
func (r *lockRig) tenant() master.Tenant {
	r.t.Helper()
	t := master.Tenant{ID: r.id("t"), Name: r.id("n"), Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: lockT0}
	r.ok("CreateTenant", r.m.CreateTenant(r.ctx, t, r.audit(), master.LocalOperator()))
	return t
}

// hold locks one row with query on a connection of its own and returns the
// connection's backend id and the function that lets the row go.
func (r *lockRig) hold(query string, args ...any) (int32, func()) {
	r.t.Helper()
	c := admin(r.t, r.address)
	tx, err := c.Begin(r.ctx)
	r.ok("begin", err)
	_, err = tx.Exec(r.ctx, query, args...)
	r.ok("hold", err)
	var pid int32
	r.ok("pid", tx.QueryRow(r.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid, func() { r.ok("commit", tx.Commit(r.ctx)) }
}

// waitBlocked waits until n sessions wait behind the session pid, directly or
// behind another session that waits for it (Postgres queues a second waiter
// for a row behind the first), and fails the test after ten seconds.
func (r *lockRig) waitBlocked(pid int32, n int) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		r.ok("pg_blocking_pids", r.conn.QueryRow(r.ctx, `WITH RECURSIVE behind(pid) AS (
				SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
				UNION SELECT a.pid FROM pg_stat_activity a JOIN behind b ON b.pid = ANY(pg_blocking_pids(a.pid)))
			SELECT count(*) FROM behind`, pid).Scan(&got))
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("%d sessions wait for the held row, want %d: a change did not wait for its lock", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hasEntry reports whether the audit log holds the entry with id.
func (r *lockRig) hasEntry(id string) bool {
	var one int
	err := r.conn.QueryRow(r.ctx, `SELECT 1 FROM audit WHERE id = $1`, id).Scan(&one)
	return err == nil
}

// lockedChange is one change of the port, made by an actor: prepare sets up
// what it changes and returns the call and a probe of whether it was made.
type lockedChange struct {
	name    string
	prepare func(r *lockRig) (do func(audit master.AuditEntry, by master.Actor) error, made func() bool)
}

// lockedChanges is every change of the port that takes an actor.
func lockedChanges() []lockedChange {
	return []lockedChange{
		{"CreateTenant", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := master.Tenant{ID: r.id("t"), Name: r.id("n"), Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: lockT0}
			return func(a master.AuditEntry, by master.Actor) error { return r.m.CreateTenant(r.ctx, t, a, by) },
				func() bool { _, err := r.m.Tenant(r.ctx, t.ID); return err == nil }
		}},
		{"SetTenantState", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := r.tenant()
			return func(a master.AuditEntry, by master.Actor) error {
					return r.m.SetTenantState(r.ctx, t.ID, master.StateFailed, "a test", a, by)
				}, func() bool {
					got, err := r.m.Tenant(r.ctx, t.ID)
					return err == nil && got.State == master.StateFailed
				}
		}},
		{"RemoveTenant", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			t := r.tenant()
			return func(a master.AuditEntry, by master.Actor) error { return r.m.RemoveTenant(r.ctx, t.ID, a, by) },
				func() bool { _, err := r.m.Tenant(r.ctx, t.ID); return err != nil }
		}},
		{"CreateKey", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := master.Key{ID: r.id("k"), Hash: make([]byte, master.KeyHashLength), Holder: "h", Created: lockT0}
			return func(a master.AuditEntry, by master.Actor) error { return r.m.CreateKey(r.ctx, k, a, by) },
				func() bool { _, err := r.m.Key(r.ctx, k.ID); return err == nil }
		}},
		{"RevokeKey", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := r.key()
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := r.m.RevokeKey(r.ctx, k.ID, lockT0, a, by)
					return err
				}, func() bool {
					got, err := r.m.Key(r.ctx, k.ID)
					return err == nil && !got.Revoked.IsZero()
				}
		}},
		{"SetGrant", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := r.key()
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := r.m.SetGrant(r.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleReader}, a, by)
					return err
				}, func() bool {
					got, err := r.m.RolesFor(r.ctx, k.ID, "")
					return err == nil && got.Installation != ""
				}
		}},
		{"RemoveGrant", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			k := r.key(master.Grant{Scope: master.Installation, Role: master.RoleReader})
			return func(a master.AuditEntry, by master.Actor) error {
					_, err := r.m.RemoveGrant(r.ctx, k.ID, master.Installation, a, by)
					return err
				}, func() bool {
					got, err := r.m.RolesFor(r.ctx, k.ID, "")
					return err == nil && got.Installation == ""
				}
		}},
		{"SetSetting", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			name := r.id("s")
			return func(a master.AuditEntry, by master.Actor) error { return r.m.SetSetting(r.ctx, name, "v", a, by) },
				func() bool { _, err := r.m.Setting(r.ctx, name); return err == nil }
		}},
		{"UnsetSetting", func(r *lockRig) (func(master.AuditEntry, master.Actor) error, func() bool) {
			name := r.id("s")
			r.ok("SetSetting", r.m.SetSetting(r.ctx, name, "v", r.audit(), master.LocalOperator()))
			return func(a master.AuditEntry, by master.Actor) error {
				_, err := r.m.UnsetSetting(r.ctx, name, a, by)
				return err
			}, func() bool { _, err := r.m.Setting(r.ctx, name); return err != nil }
		}},
	}
}

// TestKeyLapseWhileChangeWaitsForItsLock guards the in-transaction check of
// a change's key against what commits while the change waits for its locks:
// a revocation, a grant removed, a grant lowered (each queued on the key's
// row before the change), and an expiry (the clock passing it while the
// change waits). For every change of the port, the change is refused and
// writes nothing. Under read committed, a key's grant read in the statement
// that also locked the key's row would be the grant before the change the
// lock waited for, since a grant's change locks the key's row without
// changing it; an expiry judged at a time taken before the transaction
// would let the change through.
func TestKeyLapseWhileChangeWaitsForItsLock(t *testing.T) {
	r := newLockRig(t)
	lapses := []struct {
		name   string
		kind   fault.Kind
		reason string
		// undo takes key k's right away; nil for the expiry, which is the
		// clock moving.
		undo func(k master.Key) error
	}{
		{"revoked", fault.Unauthenticated, master.RefusedRevoked, func(k master.Key) error {
			_, err := r.m.RevokeKey(r.ctx, k.ID, lockT0, r.audit(), master.LocalOperator())
			return err
		}},
		{"expired", fault.Unauthenticated, master.RefusedExpired, nil},
		{"whose grant was removed", fault.Denied, master.RefusedNoRole, func(k master.Key) error {
			_, err := r.m.RemoveGrant(r.ctx, k.ID, master.Installation, r.audit(), master.LocalOperator())
			return err
		}},
		{"whose grant was lowered", fault.Denied, master.RefusedNoRole, func(k master.Key) error {
			_, err := r.m.SetGrant(r.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleOperator}, r.audit(), master.LocalOperator())
			return err
		}},
	}
	for _, c := range lockedChanges() {
		for _, l := range lapses {
			what := c.name + " by a key " + l.name + " while the change waits"
			r.now.Store(lockT0.UnixMicro())
			k := r.key(master.Grant{Scope: master.Installation, Role: master.RoleAdmin})
			do, made := c.prepare(r)
			by := master.ActingKey(k.ID, master.Installation, master.RoleAdmin, r.clock)
			pid, release := r.hold(`SELECT 1 FROM api_keys WHERE id = $1 FOR NO KEY UPDATE`, k.ID)
			waiting := 0
			undone := make(chan error, 1)
			if l.undo != nil {
				go func() { undone <- l.undo(k) }()
				waiting++
				r.waitBlocked(pid, waiting)
			} else {
				undone <- nil
			}
			audit := r.audit()
			changed := make(chan error, 1)
			go func() { changed <- do(audit, by) }()
			waiting++
			r.waitBlocked(pid, waiting)
			if l.undo == nil {
				r.now.Store(k.Expires.Add(time.Millisecond).UnixMicro())
			}
			release()
			r.ok(what+": the undoing change", <-undone)
			err := <-changed
			if fault.KindOf(err) != l.kind {
				t.Errorf("%s: %v (%v), want %v", what, err, fault.KindOf(err), l.kind)
			}
			if rf, ok := master.RefusalOf(err); !ok || rf.Reason != l.reason {
				t.Errorf("%s: %v, want a refusal for %q", what, err, l.reason)
			}
			if made() || r.hasEntry(audit.ID) {
				t.Errorf("%s: the change or its entry was written", what)
			}
		}
	}
}

// TestTenantRoleLapseWhileChangeWaitsForItsLock guards the lock a change's
// check takes on the tenant its actor's role is on: RemoveTenant removes
// that tenant's grants without locking their key, so it is the tenant's row
// that orders it with the change. A removal queued on the tenant's row
// before the change commits while the change waits, and the change, whose
// key's role was on that tenant, is refused.
func TestTenantRoleLapseWhileChangeWaitsForItsLock(t *testing.T) {
	r := newLockRig(t)
	for _, c := range lockedChanges() {
		if c.name == "SetTenantState" || c.name == "RemoveTenant" {
			continue // They change the tenant itself; TestTenantChangedTwiceByItsKeysAtOnce covers them.
		}
		what := c.name + " by a key whose tenant is removed while the change waits"
		ten := r.tenant()
		k := r.key(master.Grant{Scope: master.TenantScope(ten.ID), Role: master.RoleAdmin})
		do, made := c.prepare(r)
		by := master.ActingKey(k.ID, master.TenantScope(ten.ID), master.RoleAdmin, r.clock)
		pid, release := r.hold(`SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, ten.ID)
		removed := make(chan error, 1)
		go func() { removed <- r.m.RemoveTenant(r.ctx, ten.ID, r.audit(), master.LocalOperator()) }()
		r.waitBlocked(pid, 1)
		audit := r.audit()
		changed := make(chan error, 1)
		go func() { changed <- do(audit, by) }()
		r.waitBlocked(pid, 2)
		release()
		r.ok(what+": RemoveTenant", <-removed)
		err := <-changed
		if rf, ok := master.RefusalOf(err); !ok || rf.Reason != master.RefusedNoRole || fault.KindOf(err) != fault.Denied {
			t.Errorf("%s: %v, want a refusal for %q", what, err, master.RefusedNoRole)
		}
		if made() || r.hasEntry(audit.ID) {
			t.Errorf("%s: the change or its entry was written", what)
		}
	}
}

// TestTenantChangedTwiceByItsKeysAtOnce guards the rule that a change takes
// the strongest lock it needs on a row the first time it touches it: two
// keys whose role is on one tenant each remove it, or each set its state, at
// the same moment. Each change's check locks the tenant's row; were that a
// weaker lock than the change then asks for, each would hold what the other
// waits for, and Postgres would fail one of them as a deadlock. Every change
// here succeeds, or finds the tenant gone (NotFound, or no role on a tenant
// that no longer exists).
func TestTenantChangedTwiceByItsKeysAtOnce(t *testing.T) {
	r := newLockRig(t)
	for _, change := range []string{"RemoveTenant", "SetTenantState"} {
		for round := range 20 {
			ten := r.tenant()
			var bys []master.Actor
			for range 2 {
				k := r.key(master.Grant{Scope: master.TenantScope(ten.ID), Role: master.RoleAdmin})
				bys = append(bys, master.ActingKey(k.ID, master.TenantScope(ten.ID), master.RoleAdmin, r.clock))
			}
			errs := make([]error, 2)
			audits := []master.AuditEntry{r.audit(), r.audit()}
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if change == "RemoveTenant" {
						errs[i] = r.m.RemoveTenant(r.ctx, ten.ID, audits[i], bys[i])
					} else {
						errs[i] = r.m.SetTenantState(r.ctx, ten.ID, master.StateFailed, "a test", audits[i], bys[i])
					}
				}()
			}
			close(start)
			wg.Wait()
			for _, err := range errs {
				if err != nil && fault.KindOf(err) != fault.NotFound && fault.KindOf(err) != fault.Denied {
					t.Errorf("%s round %d: %v", change, round, err)
				}
			}
		}
	}
}
