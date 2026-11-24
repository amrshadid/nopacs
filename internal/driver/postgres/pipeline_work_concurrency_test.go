package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests in this file need several writers at once, which only a
// database such as Postgres has: many connections, through several stores
// on one schema as several processes would be, calling at the same moment.
// Each runs its race many rounds and then checks the rows themselves, read
// with a connection of its own.

// TestWorkersTakeEachUnitOnce guards leased work with many writers. Sixteen
// workers through two stores claim 400 units, each until none is due: every
// unit is claimed exactly once. Then 200 more units are leased for a
// second, and eight workers finish them (completing or failing each) while
// eight others, at once, claim them two seconds later, taking over every
// lease that ended: a unit whose takeover succeeded was never completed or
// failed by its first worker, whose write was refused as a lease lost and
// changed nothing.
func TestWorkersTakeEachUnitOnce(t *testing.T) {
	e := newCenv(t, 2, 10)
	for i := range 400 {
		_, _, err := e.p().EnqueueWork(e.ctx, tenant.Work{ID: fmt.Sprintf("w%04d", i), Kind: "archive", Due: cT0.Add(time.Duration(i%7) * time.Millisecond)}, cT0)
		e.ok("EnqueueWork", err)
	}
	const workers = 16
	claimed := make([][]string, workers)
	errs := make([]error, workers)
	atOnce(workers, func(k int) {
		for {
			w, err := e.stores[k%2].ClaimWork(e.ctx, "archive", fmt.Sprintf("worker-%d", k), cT0.Add(time.Second), time.Minute)
			if fault.KindOf(err) == fault.NotFound {
				return
			}
			if err != nil {
				errs[k] = err
				return
			}
			claimed[k] = append(claimed[k], w.ID)
		}
	})
	seen := map[string]int{}
	for k := range workers {
		e.ok("ClaimWork", errs[k])
		for _, id := range claimed[k] {
			seen[id]++
		}
	}
	if len(seen) != 400 {
		t.Errorf("%d units claimed, want 400", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s claimed %d times", id, n)
		}
	}

	for i := range 200 {
		_, _, err := e.p().EnqueueWork(e.ctx, tenant.Work{ID: fmt.Sprintf("x%04d", i), Kind: "lease", Due: cT0}, cT0)
		e.ok("EnqueueWork", err)
	}
	// The first workers lease every unit for a second.
	var first []tenant.Work
	for {
		w, err := e.stores[len(first)%2].ClaimWork(e.ctx, "lease", fmt.Sprintf("first-%d", len(first)%8), cT0, time.Second)
		if fault.KindOf(err) == fault.NotFound {
			break
		}
		e.ok("ClaimWork", err)
		first = append(first, w)
	}
	// Then, at once, each first worker finishes its units (completing some,
	// failing others) while the second workers take over every lease that
	// ended, two seconds later.
	var mu sync.Mutex
	finished := map[string]bool{} // units a first worker's write was accepted for
	takenOver := map[string]bool{}
	var failures []string
	atOnce(workers, func(k int) {
		store := e.stores[k%2]
		if k < workers/2 {
			for i := k; i < len(first); i += workers / 2 {
				w := first[i]
				var err error
				if i%2 == 0 {
					err = store.CompleteWork(e.ctx, w.Fence(), cT0.Add(500*time.Millisecond), nil)
				} else {
					_, err = store.FailWork(e.ctx, w.Fence(), "storage unreachable", cT0.Add(500*time.Millisecond))
				}
				mu.Lock()
				switch {
				case err == nil:
					finished[w.ID] = true
				case !errors.Is(err, tenant.ErrLeaseLost):
					failures = append(failures, "first write: "+err.Error())
				}
				mu.Unlock()
			}
			return
		}
		for {
			w, err := store.ClaimWork(e.ctx, "lease", fmt.Sprintf("second-%d", k), cT0.Add(2*time.Second), time.Hour)
			if fault.KindOf(err) == fault.NotFound {
				return
			}
			mu.Lock()
			if err != nil {
				failures = append(failures, "takeover: "+err.Error())
			} else {
				takenOver[w.ID] = true
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	})
	for _, f := range failures {
		t.Error(f)
	}
	for id := range finished {
		if takenOver[id] {
			w, _ := e.p().Work(e.ctx, id)
			t.Errorf("%s: its first worker's write was accepted and it was taken over too (%+v)", id, w)
		}
	}
	// How the race fell is the machine's; both outcomes are logged.
	t.Logf("%d units finished by their first worker, %d taken over", len(finished), len(takenOver))
	// Every unit taken over is leased by its second worker, unchanged by the
	// first.
	if n := e.scalar(`SELECT count(*) FROM work WHERE kind = 'lease' AND attempt >= 2 AND (state <> 'leased' OR owner NOT LIKE 'second-%')`); n != 0 {
		t.Errorf("%d units taken over were changed by a write under a lost lease", n)
	}
}

// TestClaimWorkSkipsHeldUnit guards that a claim never waits for a unit
// another transaction holds: with the earliest due unit's row locked by a
// transaction that stays open, a claim returns the next unit at once.
func TestClaimWorkSkipsHeldUnit(t *testing.T) {
	e := newCenv(t, 1, 4)
	for _, id := range []string{"early", "late"} {
		due := cT0
		if id == "late" {
			due = cT0.Add(time.Second)
		}
		_, _, err := e.p().EnqueueWork(e.ctx, tenant.Work{ID: id, Kind: "archive", Due: due}, cT0)
		e.ok("EnqueueWork", err)
	}
	tx, err := e.db.Begin(e.ctx)
	e.ok("Begin", err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM work WHERE id = 'early' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	w, err := e.p().ClaimWork(ctx, "archive", "worker-1", cT0.Add(time.Minute), time.Minute)
	if err != nil || w.ID != "late" {
		t.Fatalf("a claim with the earliest unit held = %s, %v; want the next unit at once", w.ID, err)
	}
}

// TestEnqueueOneKeyAtOnce guards EnqueueWork with many writers on one
// unique key, thirty rounds: sixteen calls at once through two stores queue
// one unit, every call is answered with that unit, and none fails.
func TestEnqueueOneKeyAtOnce(t *testing.T) {
	e := newCenv(t, 2, 8)
	for round := range 30 {
		const n = 16
		got := make([]tenant.Work, n)
		created := make([]bool, n)
		errs := make([]error, n)
		key := fmt.Sprintf("key-%d", round)
		atOnce(n, func(i int) {
			got[i], created[i], errs[i] = e.stores[i%2].EnqueueWork(e.ctx, tenant.Work{ID: fmt.Sprintf("w%02d-%02d", round, i),
				Kind: "reception", UniqueKey: key, Due: cT0}, cT0)
		})
		made := 0
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("round %d: enqueue %d: %v (kind %v)", round, i, errs[i], fault.KindOf(errs[i]))
			}
			if created[i] {
				made++
			}
			if got[i].ID != got[0].ID {
				t.Fatalf("round %d: calls answered %s and %s", round, got[0].ID, got[i].ID)
			}
		}
		if made != 1 {
			t.Fatalf("round %d: %d units created; want 1", round, made)
		}
		if c := e.scalar(`SELECT count(*) FROM work WHERE unique_key = $1`, key); c != 1 {
			t.Fatalf("round %d: %d units under the key; want 1", round, c)
		}
	}
}
