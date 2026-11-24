package receive

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestRestartLeavesALiveSendUnit guards the reclaim at a pipeline's start on
// SQLite: a send unit the same server's senders hold live keeps its lease,
// owner and attempt (its fence still renews, so its batch is not sent twice
// and no attempt is spent), while a unit of the pipeline's own kinds that a
// process that is gone left leased is taken up at once.
func TestRestartLeavesALiveSendUnit(t *testing.T) {
	restartLeavesALiveSendUnit(t, "")
}

// TestRestartLeavesALiveSendUnitOnPostgres guards the same on Postgres.
func TestRestartLeavesALiveSendUnitOnPostgres(t *testing.T) {
	address, ok := testkit.PostgresSchema(t)
	if !ok {
		t.Skipf("%s is not set; the Postgres reclaim test is skipped", testkit.PostgresEnv)
	}
	restartLeavesALiveSendUnit(t, address)
}

// restartLeavesALiveSendUnit stops the pipeline, leases a send unit to a
// live sender and a reception unit to a dead worker, reopens the tenant's
// database, starts a new pipeline on it, and checks each unit.
func restartLeavesALiveSendUnit(t *testing.T, postgres string) {
	e := newEnv(t, envOptions{postgres: postgres})
	if err := e.p.Stop(e.ctx); err != nil {
		t.Fatal(err)
	}
	now := e.clock.Now()
	for _, w := range []tenant.Work{
		{ID: "send-unit", Kind: tenant.SendKind("peer"), Subject: "job-1", Due: now},
		{ID: "dead-unit", Kind: kindQuiet, Subject: "rc-gone", Due: now},
	} {
		if _, _, err := e.store.EnqueueWork(e.ctx, w, now); err != nil {
			t.Fatal(err)
		}
	}
	live, err := e.store.ClaimWork(e.ctx, tenant.SendKind("peer"), "sender-peer-0", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := e.store.ClaimWork(e.ctx, kindQuiet, "dead-worker", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	if postgres != "" {
		s, err := (driver.PostgresTenants{}).Open(e.ctx, "T0000000000001", "lab", postgres)
		if err != nil {
			t.Fatal(err)
		}
		e.store = s.(tenant.Pipeline)
		t.Cleanup(func() { _ = s.Close() })
	} else {
		e.openStore()
	}
	e.start()
	e.waitFor("the dead worker's unit to be taken up", 5*time.Second, func() bool {
		w, err := e.store.Work(e.ctx, dead.ID)
		return err == nil && (w.Owner != "dead-worker" || w.Attempt != dead.Attempt)
	})
	w, err := e.store.Work(e.ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != tenant.WorkLeased || w.Owner != "sender-peer-0" || w.Attempt != live.Attempt || !w.Due.Equal(live.Due) {
		t.Fatalf("the live send unit is %s, owner %q, attempt %d, due %v after the restart; want leased by sender-peer-0, attempt %d, due %v",
			w.State, w.Owner, w.Attempt, w.Due, live.Attempt, live.Due)
	}
	if _, err := e.store.RenewWork(context.Background(), tenant.WorkFence{ID: live.ID, Attempt: live.Attempt}, e.clock.Now(), time.Hour); err != nil {
		t.Fatalf("the live send unit's fence no longer renews after the restart: %v", err)
	}
}
