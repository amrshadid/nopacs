package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// The tests of this file guard that every answer, success or failure,
// describes the database as committed, even though the changes of one
// transaction see each other's writes before the commit.

// labTenant returns a tenant named lab with the id given.
func labTenant(id string) master.Tenant {
	return master.Tenant{ID: id, Name: "lab", Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: mastertest.T0}
}

// endlessInsert is a statement that runs until its context ends and the
// driver interrupts it; an interrupted INSERT inside a transaction makes
// SQLite undo the whole transaction (sqlite.org/c3ref/interrupt.html).
const endlessInsert = `INSERT INTO settings (name, value)
	WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
	SELECT 'endless.' || i, 'v' FROM n`

// TestFailureFromUndoneNeighborRunsAgain guards a failure caused by a
// neighbor's write that is then never committed. In one transaction A
// creates the tenant lab and B, after it, is refused because lab exists;
// then the transaction is undone (C is interrupted) and A's caller has given
// up, so A is not written. B must not be told that lab exists: it runs
// again, and creates lab itself.
func TestFailureFromUndoneNeighborRunsAgain(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	a := submit(t, &m.base, 1, func() error { return m.CreateTenant(ctxA, labTenant("t1"), entry("a-A"), master.LocalOperator()) })
	b := submit(t, &m.base, 2, func() error { return m.CreateTenant(ctx, labTenant("t2"), entry("a-B"), master.LocalOperator()) })
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	c := submit(t, &m.base, 3, func() error {
		return m.write(stopped, "test", func(tx *sql.Tx, s *statements) error {
			cancelA() // A's caller gives up after A ran, before the commit.
			time.AfterFunc(50*time.Millisecond, cancel)
			_, err := tx.ExecContext(stopped, endlessInsert)
			return err
		})
	})
	h.letGo(t)
	ra, rb, rc := answer(t, "A", a), answer(t, "B", b), answer(t, "C", c)
	if fault.KindOf(ra.err) != fault.Unavailable {
		t.Errorf("A, whose caller gave up before the transaction ended: %+v, want Unavailable", ra)
	}
	if fault.KindOf(rc.err) != fault.Unavailable {
		t.Errorf("C, interrupted: %+v, want Unavailable", rc)
	}
	got, err := m.TenantByName(ctx, "lab")
	if rb.err != nil {
		t.Errorf("B was told %v; the tenant named lab: %+v, %v", rb.err, got, err)
	} else if err != nil || got.ID != "t2" {
		t.Errorf("B was told it created lab, but the tenant named lab is %+v, %v", got, err)
	}
}

// TestFailureRunsAgainWhenCommitFails guards a failure caused by a
// neighbor's write when the COMMIT itself fails. A creates the tenant lab, B
// is refused because lab exists, and X writes a grant to a key that does not
// exist with the foreign key check deferred to the commit, so the COMMIT
// fails (sqlite.org/foreignkeys.html#fk_deferred). A and X are answered with
// the commit's failure and are absent; B, whose refusal came from A's write,
// runs again and creates lab.
func TestFailureRunsAgainWhenCommitFails(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	a := submit(t, &m.base, 1, func() error { return m.CreateTenant(ctx, labTenant("t1"), entry("a-A"), master.LocalOperator()) })
	b := submit(t, &m.base, 2, func() error { return m.CreateTenant(ctx, labTenant("t2"), entry("a-B"), master.LocalOperator()) })
	x := submit(t, &m.base, 3, func() error {
		return m.writeTx(ctx, "test", func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO grants (key_id, scope, tenant_id, role) VALUES ('NoSuchKey', '', NULL, 'reader')`)
			return err
		})
	})
	h.letGo(t)
	ra, rb, rx := answer(t, "A", a), answer(t, "B", b), answer(t, "X", x)
	if ra.err == nil || rx.err == nil {
		t.Errorf("A and X, whose commit failed: %+v, %+v; want errors", ra, rx)
	}
	got, err := m.TenantByName(ctx, "lab")
	if rb.err != nil {
		t.Errorf("B was told %v; the tenant named lab: %+v, %v", rb.err, got, err)
	} else if err != nil || got.ID != "t2" {
		t.Errorf("B was told it created lab, but the tenant named lab is %+v, %v", got, err)
	}
	if _, err := m.Tenant(ctx, "t1"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("A's tenant after its commit failed: %v, want NotFound", err)
	}
}

// TestConflictAnsweredAfterWinnerCommits guards the order of answers inside
// one transaction: B, refused because A created the same name just before
// it, is not answered before A's commit, so B never reads "no such tenant"
// right after being told the tenant exists.
func TestConflictAnsweredAfterWinnerCommits(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	gate := make(chan struct{})
	atCommit := make(chan []*change, 1)
	m.w.beforeCommit = func(waiting []*change) {
		if waiting[0].message != "hold" {
			atCommit <- waiting
			<-gate
		}
	}
	h := holdWriter(t, &m.base)
	a := submit(t, &m.base, 1, func() error { return m.CreateTenant(ctx, labTenant("t1"), entry("a-A"), master.LocalOperator()) })
	b := submit(t, &m.base, 2, func() error { return m.CreateTenant(ctx, labTenant("t2"), entry("a-B"), master.LocalOperator()) })
	close(h.release)
	var waiting []*change
	select {
	case waiting = <-atCommit:
	case <-time.After(waitLimit):
		t.Fatal("the batch did not reach its commit")
	}
	if len(waiting) != 2 {
		t.Errorf("%d changes wait for the commit, want 2 (A written, B refused)", len(waiting))
	}
	var rb result
	select {
	case rb = <-b:
		t.Errorf("B was answered before A's commit: %+v", rb)
		close(gate)
	default:
		close(gate)
		rb = answer(t, "B", b)
	}
	if fault.KindOf(rb.err) != fault.Conflict {
		t.Errorf("B: %+v, want Conflict", rb)
	}
	if got, err := m.TenantByName(ctx, "lab"); err != nil || got.ID != "t1" {
		t.Errorf("after B's Conflict, the tenant named lab: %+v, %v", got, err)
	}
	if r := answer(t, "A", a); r.err != nil {
		t.Errorf("A: %+v", r)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
}

// TestConflictThenReadFindsWinner guards the same order without any hook:
// eight callers create one name at once, 200 times over, and every caller
// told Conflict reads the name right away and must find it.
func TestConflictThenReadFindsWinner(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	var mu sync.Mutex
	bad, conflicts := 0, 0
	for i := range 200 {
		name := "lab" + strconv.Itoa(i)
		var wg sync.WaitGroup
		for j := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := strconv.Itoa(i) + "x" + strconv.Itoa(j)
				tn := labTenant("t" + id)
				tn.Name = name
				err := m.CreateTenant(ctx, tn, entry("a"+id), master.LocalOperator())
				if fault.KindOf(err) != fault.Conflict {
					return
				}
				_, rerr := m.TenantByName(ctx, name)
				mu.Lock()
				defer mu.Unlock()
				conflicts++
				if rerr != nil {
					bad++
				}
			}()
		}
		wg.Wait()
	}
	t.Logf("%d conflicts", conflicts)
	if bad > 0 {
		t.Errorf("%d of %d callers told Conflict then could not read the tenant", bad, conflicts)
	}
}

// TestCanceledAfterTakenToldTruth guards a caller who gives up after its
// change has run, while a later change of the same transaction still runs:
// its change is committed, so it is answered nil, not with its context's
// error.
func TestCanceledAfterTakenToldTruth(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	a := submit(t, &m.base, 1, func() error { return m.SetSetting(ctxA, "a", "v", entry("a-A"), master.LocalOperator()) })
	inB, releaseB := make(chan struct{}), make(chan struct{})
	b := submit(t, &m.base, 2, func() error {
		return m.writeTx(ctx, "b", func(*sql.Tx) error { close(inB); <-releaseB; return nil })
	})
	close(h.release)
	<-inB
	cancelA()
	// Time for A's caller to see its context end; an answer given now
	// would be a guess, since the commit has not happened.
	time.Sleep(20 * time.Millisecond)
	close(releaseB)
	ra := answer(t, "A", a)
	if r := answer(t, "B", b); r.err != nil {
		t.Errorf("B: %+v", r)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	_, err := m.Setting(ctx, "a")
	if ra.err != nil || err != nil {
		t.Errorf("A was answered %v and its setting reads %v; want nil and its change written", ra.err, err)
	}
}

// TestBatchOutlivesFirstCallersContext guards that a shared transaction
// belongs to no caller: the first caller of a batch giving up after its
// change ran undoes neither its neighbors nor itself.
func TestBatchOutlivesFirstCallersContext(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	a := submit(t, &m.base, 1, func() error { return m.SetSetting(ctxA, "a", "v", entry("a-A"), master.LocalOperator()) })
	inB, releaseB := make(chan struct{}), make(chan struct{})
	b := submit(t, &m.base, 2, func() error {
		return m.writeTx(ctx, "b", func(*sql.Tx) error { close(inB); <-releaseB; return nil })
	})
	c := submit(t, &m.base, 3, func() error { return m.SetSetting(ctx, "c", "v", entry("a-C"), master.LocalOperator()) })
	close(h.release)
	<-inB
	cancelA()
	// Time for anything bound to A's context to act on its end.
	time.Sleep(20 * time.Millisecond)
	close(releaseB)
	ra, rb, rc := answer(t, "A", a), answer(t, "B", b), answer(t, "C", c)
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	if rb.err != nil || rc.err != nil {
		t.Errorf("neighbors of a first caller that gave up: B %v, C %v", rb.err, rc.err)
	}
	wantSetting(t, m, "c", "a-C", true)
	_, err := m.Setting(ctx, "a")
	if (ra.err == nil) != (err == nil) {
		t.Errorf("A was answered %v and its setting reads %v", ra.err, err)
	}
}

// TestTwoStoresAnswersMatchFile guards answers against the file when two
// stores on one file each group their callers' changes, some with taken
// audit ids and some with contexts that end at varied moments: a change is
// in the file exactly when its caller was answered nil.
func TestTwoStoresAnswersMatchFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	var stores [2]*Master
	for i := range stores {
		m, err := OpenMaster(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		if err := m.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		stores[i] = m
	}
	type res struct {
		n   int
		err error
	}
	out := make(chan res, 64*30)
	var wg sync.WaitGroup
	for g := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 30 {
				n := g*100 + k
				id := "s-" + strconv.Itoa(n)
				if n%5 == 0 {
					id = "s-dup" // Written by the first, refused after.
				}
				cctx, cancel := ctx, context.CancelFunc(func() {})
				if n%3 == 0 {
					cctx, cancel = context.WithTimeout(ctx, time.Duration(n%7+1)*time.Millisecond)
				}
				err := stores[n%2].SetSetting(cctx, "n."+strconv.Itoa(n), "v", entry(id), master.LocalOperator())
				cancel()
				out <- res{n, err}
			}
		}()
	}
	wg.Wait()
	close(out)
	okN, errN := 0, 0
	for r := range out {
		_, gerr := stores[0].Setting(ctx, "n."+strconv.Itoa(r.n))
		if r.err == nil {
			okN++
		} else {
			errN++
		}
		if (r.err == nil) != (gerr == nil) {
			t.Errorf("change %d answered %v, but reading it gives %v", r.n, r.err, gerr)
		}
	}
	t.Logf("%d written, %d refused or given up", okN, errN)
}
