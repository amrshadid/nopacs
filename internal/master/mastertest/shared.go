package mastertest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// OpenPair returns two stores on one fresh, empty, migrated database, as two
// processes would each open it, and closes both when tb ends.
type OpenPair func(tb testing.TB) (first, second master.Store)

// RunShared runs the tests that need two instances of the store on one
// database: what one process's guards cannot show, such as a tenant name
// kept unique between two servers. A driver's tests call it as well as Run.
func RunShared(t *testing.T, open OpenPair) {
	tests := []struct {
		name string
		fn   func(e *env, other master.Store)
	}{
		{"TenantNameRaceAcrossInstances", testTenantNameRaceShared},
		{"SetGrantRevokeRaceAcrossInstances", func(e *env, other master.Store) { setGrantRevokeRace(e, e.s, other, 20) }},
		{"ActorRevokeRaceAcrossInstances", func(e *env, other master.Store) { actorRevokeRace(e, e.s, other, 30) }},
		{"WritesSeenAcrossInstances", testWritesSeenShared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, second := open(t)
			tt.fn(&env{T: t, s: first, ctx: context.Background()}, second)
		})
	}
	t.Run("AuditSeal", func(t *testing.T) {
		RunAuditSealShared(t, func(tb testing.TB) (SealLog, SealLog) { return open(tb) })
	})
}

// testTenantNameRaceShared guards the unique tenant name between two
// instances: creations of one name split between them end with exactly one
// tenant and one audit entry, every other creation Conflict.
func testTenantNameRaceShared(e *env, other master.Store) {
	const rounds, racers = 20, 8
	for round := range rounds {
		name := fmt.Sprintf("race%02d", round)
		tenants := make([]master.Tenant, racers)
		entries := make([]master.AuditEntry, racers)
		for i := range racers {
			tenants[i], entries[i] = e.tenant(name), e.audit()
		}
		errs := make([]error, racers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range racers {
			s := e.s
			if i%2 == 1 {
				s = other
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.CreateTenant(e.ctx, tenants[i], entries[i], master.LocalOperator())
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case fault.KindOf(err) != fault.Conflict:
				e.Errorf("round %d: creation %d: %v, want Conflict", round, i, err)
			}
		}
		if winners != 1 {
			e.Fatalf("round %d: %d creations of one name succeeded across two instances, want 1", round, winners)
		}
		ids := e.auditIDs()
		n := 0
		for _, a := range entries {
			if ids[a.ID] {
				n++
			}
		}
		if n != 1 {
			e.Errorf("round %d: %d audit entries for one creation", round, n)
		}
	}
}

// testWritesSeenShared guards that what one instance commits the other reads
// at once, and that a duplicate audit id is caught across instances.
func testWritesSeenShared(e *env, other master.Store) {
	k := e.mustKey()
	if _, err := other.Key(e.ctx, k.ID); err != nil {
		e.Errorf("the other instance does not see a committed key: %v", err)
	}
	a := e.audit()
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	e.wantDuplicateAudit("AppendAudit of the same id on the other instance", other.AppendAudit(e.ctx, a))
	_, err := other.RevokeKey(e.ctx, k.ID, T0.Add(time.Hour), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	got, err := e.s.Key(e.ctx, k.ID)
	if err != nil || got.Revoked.IsZero() {
		e.Errorf("a revocation on the other instance is not seen: %v, %v", got.Revoked, err)
	}
}
