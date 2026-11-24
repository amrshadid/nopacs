package mastertest

import (
	"fmt"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// race runs a and b at the same moment and waits for both.
func race(a, b func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range []func(){a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			f()
		}()
	}
	close(start)
	wg.Wait()
}

// testConcurrentSameRow guards that two concurrent calls on one row are
// ordered and that each one's audit entry records what that call saw:
// RemoveGrant twice (one removes the role, the other finds none), RevokeKey
// twice (one revokes, the other finds it revoked and returns the first
// time), SetTenantState twice (one replaces the first state, the other the
// first one's).
func testConcurrentSameRow(e *env) {
	t := e.mustTenant("lab")
	for round := range 15 {
		k := e.mustKey()
		e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleWriter)
		a, b := e.audit(), e.audit()
		var ra, rb master.Role
		var ea, eb error
		race(func() { ra, ea = e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope(t.ID), a, master.LocalOperator()) },
			func() { rb, eb = e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope(t.ID), b, master.LocalOperator()) })
		if ea != nil || eb != nil {
			e.Fatalf("round %d: RemoveGrant: %v, %v", round, ea, eb)
		}
		if (ra == master.RoleWriter) == (rb == master.RoleWriter) || ra+rb != master.RoleWriter {
			e.Errorf("round %d: RemoveGrant returned %q and %q, want writer once and none once", round, ra, rb)
		}
		da, db := e.detailsOf(a.ID), e.detailsOf(b.ID)
		if da[master.DetailOldRole] != roleDetail(ra) || db[master.DetailOldRole] != roleDetail(rb) {
			e.Errorf("round %d: removals' audit old roles %q and %q, returned %q and %q",
				round, da[master.DetailOldRole], db[master.DetailOldRole], ra, rb)
		}

		at1, at2 := T0.Add(time.Hour), T0.Add(2*time.Hour)
		a, b = e.audit(), e.audit()
		var ta, tb time.Time
		race(func() { ta, ea = e.s.RevokeKey(e.ctx, k.ID, at1, a, master.LocalOperator()) },
			func() { tb, eb = e.s.RevokeKey(e.ctx, k.ID, at2, b, master.LocalOperator()) })
		if ea != nil || eb != nil {
			e.Fatalf("round %d: RevokeKey: %v, %v", round, ea, eb)
		}
		if !ta.Equal(tb) || (!ta.Equal(at1) && !ta.Equal(at2)) {
			e.Errorf("round %d: RevokeKey returned %v and %v, want both the first one's time", round, ta, tb)
		}
		da, db = e.detailsOf(a.ID), e.detailsOf(b.ID)
		firstIsA := ta.Equal(at1)
		if (da[master.DetailAlreadyRevoked] == "false") != firstIsA || (db[master.DetailAlreadyRevoked] == "false") == firstIsA {
			e.Errorf("round %d: revocations' audit already_revoked %q and %q, first was a: %v",
				round, da[master.DetailAlreadyRevoked], db[master.DetailAlreadyRevoked], firstIsA)
		}

		e.ok("SetTenantState", e.s.SetTenantState(e.ctx, t.ID, master.StateReady, "", e.audit(), master.LocalOperator()))
		a, b = e.audit(), e.audit()
		race(func() { ea = e.s.SetTenantState(e.ctx, t.ID, master.StateFailed, "a", a, master.LocalOperator()) },
			func() { eb = e.s.SetTenantState(e.ctx, t.ID, master.StateUpgrading, "b", b, master.LocalOperator()) })
		if ea != nil || eb != nil {
			e.Fatalf("round %d: SetTenantState: %v, %v", round, ea, eb)
		}
		da, db = e.detailsOf(a.ID), e.detailsOf(b.ID)
		aFirst := da[master.DetailOldState] == "ready" && db[master.DetailOldState] == "failed"
		bFirst := db[master.DetailOldState] == "ready" && da[master.DetailOldState] == "upgrading"
		if aFirst == bFirst {
			e.Errorf("round %d: state changes' audit old states %q and %q, want one ready and the other the first one's new state",
				round, da[master.DetailOldState], db[master.DetailOldState])
		}
		got, err := e.s.Tenant(e.ctx, t.ID)
		e.ok("Tenant", err)
		if (aFirst && got.State != master.StateUpgrading) || (bFirst && got.State != master.StateFailed) {
			e.Errorf("round %d: the state is %s, not the last change's", round, got.State)
		}
	}
}

// testSetGrantRevokeRace guards that a grant is never written after its key
// is revoked: SetGrant racing RevokeKey on one key either succeeds, and the
// grant exists, or is Conflict, and there is no grant; and a SetGrant after
// the revocation is always Conflict.
func testSetGrantRevokeRace(e *env) {
	setGrantRevokeRace(e, e.s, e.s, 30)
}

// setGrantRevokeRace is testSetGrantRevokeRace on two stores, which may be
// two instances on one database.
func setGrantRevokeRace(e *env, s1, s2 master.Store, rounds int) {
	granted, refused := 0, 0
	for round := range rounds {
		k := e.key()
		e.ok("CreateKey", s1.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
		ga, ra := e.audit(), e.audit()
		var gerr, rerr error
		// Each side in turn starts a moment late, so both orders happen
		// and both outcomes are checked, while the calls still overlap.
		lag := func(mine int) {
			if round%3 == mine {
				time.Sleep(200 * time.Microsecond)
			}
		}
		race(func() {
			lag(0)
			_, gerr = s1.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleAdmin}, ga, master.LocalOperator())
		}, func() {
			lag(1)
			_, rerr = s2.RevokeKey(e.ctx, k.ID, T0.Add(time.Hour), ra, master.LocalOperator())
		})
		e.ok("RevokeKey", rerr)
		grants, _, err := s1.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
		e.ok("GrantsByKey", err)
		switch {
		case gerr == nil:
			granted++
			if len(grants) != 1 {
				e.Errorf("round %d: SetGrant succeeded but the key has %d grants", round, len(grants))
			}
		case fault.KindOf(gerr) == fault.Conflict:
			refused++
			if len(grants) != 0 {
				e.Errorf("round %d: SetGrant was refused but the key has %d grants: a grant written after revocation", round, len(grants))
			}
			if e.auditIDs()[ga.ID] {
				e.Errorf("round %d: the refused SetGrant wrote its audit entry", round)
			}
		default:
			e.Errorf("round %d: SetGrant racing RevokeKey: %v, want success or Conflict", round, gerr)
		}
		_, err = s2.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleReader}, e.audit(), master.LocalOperator())
		e.wantKind(fmt.Sprintf("round %d: SetGrant after RevokeKey", round), err, fault.Conflict)
	}
	e.Logf("SetGrant racing RevokeKey: %d granted first, %d refused", granted, refused)
}
