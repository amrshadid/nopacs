package tenanttest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// OpenPair returns two stores for the tenant whose id is tenantID on one
// fresh, empty, migrated database, as two processes would each open it, and
// closes both when tb ends.
type OpenPair func(tb testing.TB, tenantID string) (first, second tenant.Store)

// runStoreShared runs the tests that need two instances of the store on one
// database. Run runs it.
func runStoreShared(t *testing.T, open OpenPair) {
	tests := []struct {
		name string
		fn   func(e *env, other tenant.Store)
	}{
		{"WritesSeenAcrossInstances", testWritesSeenShared},
		{"ConcurrentChangesAcrossInstances", testConcurrentShared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, second := open(t, SuiteTenant)
			tt.fn(&env{T: t, s: first, ctx: context.Background()}, second)
		})
	}
	// The audit seal's rules are the master's, so the master's tests run
	// here unchanged.
	t.Run("AuditSeal", func(t *testing.T) {
		mastertest.RunAuditSealShared(t, func(tb testing.TB) (mastertest.SealLog, mastertest.SealLog) { return open(tb, SuiteTenant) })
	})
}

// testWritesSeenShared guards that what one instance commits the other reads
// at once, and that a duplicate audit id is caught across instances.
func testWritesSeenShared(e *env, other tenant.Store) {
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "seen", "yes", e.audit(), nil))
	if v, err := other.Setting(e.ctx, "seen"); err != nil || v != "yes" {
		e.Errorf("the other instance reads %q, %v; want the committed value", v, err)
	}
	a := e.audit()
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	e.wantDuplicateAudit("AppendAudit of the same id on the other instance", other.AppendAudit(e.ctx, a))
	removed, err := other.UnsetSetting(e.ctx, "seen", e.audit(), nil)
	if err != nil || !removed {
		e.Errorf("UnsetSetting on the other instance = %v, %v", removed, err)
	}
	if _, err := e.s.Setting(e.ctx, "seen"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a removal on the other instance is not seen (%v)", err)
	}
}

// testConcurrentShared guards that changes made at once through two
// instances, as two processes would make them, are each written with their
// audit entry or not at all: none lost, none half written, every failure
// leaving nothing behind. In each round the two instances write a short burst
// of changes at the same moment, the last change of each burst with an audit
// id already taken, which must fail and leave no setting; at the end the
// settings and the audit entries are counted exactly.
//
// It pins correctness across instances, not fairness, which the port does
// not promise: on SQLite a writer in one process waits for as long as
// another keeps writing (package sqlite's documentation), so a test of two
// sustained writers would measure how SQLite's busy wait happens to
// interleave them on the machine at hand. The bursts are short, so neither
// instance waits for more than a few of the other's commits, and they are
// concurrent, so a store that is not atomic across instances still fails.
func testConcurrentShared(e *env, other tenant.Store) {
	const rounds, burst = 15, 3
	taken := e.audit()
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, taken))
	stores := []tenant.Store{e.s, other}
	for round := range rounds {
		errs := make([][]error, len(stores))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, s := range stores {
			errs[i] = make([]error, burst+1)
			wg.Go(func() {
				<-start
				for j := range burst + 1 {
					a := e.audit()
					if j == burst {
						a.ID = taken.ID
					}
					errs[i][j] = s.SetSetting(e.ctx, fmt.Sprintf("r%02d.i%d.c%d", round, i, j), "v", a, nil)
				}
			})
		}
		close(start)
		wg.Wait()
		for i := range stores {
			for j, err := range errs[i] {
				switch {
				case j < burst && err != nil:
					e.Errorf("round %d, instance %d, change %d: %v", round, i, j, err)
				case j == burst:
					e.wantDuplicateAudit(fmt.Sprintf("round %d, instance %d, a change with a taken audit id", round, i), err)
				}
			}
		}
	}
	all, next, err := other.ListSettings(e.ctx, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListSettings", err)
	if next != "" {
		e.Fatal("more settings than one page holds")
	}
	for _, s := range all {
		if strings.HasSuffix(s.Name, fmt.Sprintf(".c%d", burst)) {
			e.Errorf("the setting %s of a change whose audit entry failed was written", s.Name)
		}
	}
	if want := rounds * len(stores) * burst; len(all) != want {
		e.Errorf("%d settings after %d changes that succeeded across two instances", len(all), want)
	}
	if want := 1 + rounds*len(stores)*burst; len(e.auditIDs()) != want {
		e.Errorf("%d audit entries, want %d: one for each change that succeeded, and the taken one", len(e.auditIDs()), want)
	}
}

// testTenantsApart guards that two tenants' databases stay apart: each reads
// only its own settings and audit entries, and a page token from one
// tenant's list is refused by the other's, for the settings list and for the
// audit list with the same filter (the API contract's page token rule, with
// the tenant as one of the list's arguments).
func testTenantsApart(e *env) {
	other := e.open(e.T, OtherTenant)
	for i := range 3 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("mine.%d", i), "v", e.audit(), nil))
		e.ok("SetSetting", other.SetSetting(e.ctx, fmt.Sprintf("theirs.%d", i), "v", e.audit(), nil))
	}
	mine, _, err := e.s.ListSettings(e.ctx, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListSettings", err)
	for _, s := range mine {
		if !strings.HasPrefix(s.Name, "mine.") {
			e.Errorf("a tenant reads another tenant's setting %q", s.Name)
		}
	}
	_, settingsCursor, err := e.s.ListSettings(e.ctx, master.Page{Limit: 1})
	e.ok("ListSettings", err)
	_, _, err = other.ListSettings(e.ctx, master.Page{Limit: 1, Cursor: settingsCursor})
	e.wantKind("another tenant's ListSettings with this tenant's cursor", err, fault.Invalid)

	_, auditCursor, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: 1})
	e.ok("ListAudit", err)
	if auditCursor == "" {
		e.Fatal("ListAudit returned no cursor with more to come")
	}
	_, _, err = other.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: 1, Cursor: auditCursor})
	e.wantKind("another tenant's ListAudit with this tenant's cursor", err, fault.Invalid)
	if _, _, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: 1, Cursor: auditCursor}); err != nil {
		e.Errorf("the tenant refuses its own cursor: %v", err)
	}
}
