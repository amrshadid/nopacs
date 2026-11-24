package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// waitLimit bounds every wait in these tests for something that happens in
// memory, not on the disk: a change reaching the queue, a caller answered
// while the writer is held. It is long, so a slow runner does not fail a
// test, and only a wrong answer reaches it.
const waitLimit = 20 * time.Second

// openReady opens a fresh, migrated master with its statements prepared.
// They are prepared on the write connection on first use, so a change made
// first while a test holds the writer would wait for it forever.
func openReady(t *testing.T) *Master {
	t.Helper()
	m := openTest(t).(*Master)
	if _, err := m.ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

// held is a change that holds the writer: it runs alone, so the changes
// submitted after it queue, and it commits only when release is closed.
type held struct {
	release chan struct{}
	done    chan error
}

// holdWriter starts a change that holds b's writer until the returned
// release is called, and waits until it runs.
func holdWriter(t *testing.T, b *base) *held {
	t.Helper()
	h := &held{release: make(chan struct{}), done: make(chan error, 1)}
	started := make(chan struct{})
	go func() {
		h.done <- b.writeTx(context.Background(), "hold", func(*sql.Tx) error {
			close(started)
			<-h.release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(waitLimit):
		t.Fatal("the holding change did not start")
	}
	return h
}

// letGo releases the holding change and checks that it committed.
func (h *held) letGo(t *testing.T) {
	t.Helper()
	close(h.release)
	if err := <-h.done; err != nil {
		t.Fatalf("the holding change: %v", err)
	}
}

// queued returns how many changes wait in b's queue.
func queued(b *base) int {
	b.w.mu.Lock()
	defer b.w.mu.Unlock()
	return len(b.w.queue)
}

// waitQueued waits until n changes wait in b's queue.
func waitQueued(t *testing.T, b *base, n int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for queued(b) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d changes queued, want %d", queued(b), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// result is one submitted call's answer: its error, or what it panicked
// with.
type result struct {
	err      error
	panicked any
}

// submit runs call in a goroutine, waits until it is queued as the n-th
// waiting change, and returns where its answer will be.
func submit(t *testing.T, b *base, n int, call func() error) <-chan result {
	t.Helper()
	out := make(chan result, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				out <- result{panicked: v}
			}
		}()
		out <- result{err: call()}
	}()
	waitQueued(t, b, n)
	return out
}

// answer waits for the answer on ch.
func answer(t *testing.T, what string, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitLimit):
		t.Fatalf("%s was not answered", what)
		return result{}
	}
}

// entry returns an audit entry with id.
func entry(id string) master.AuditEntry {
	return master.AuditEntry{ID: id, Time: mastertest.T0, Method: "Test", Outcome: master.OutcomeDone}
}

// wantSetting checks that the setting name is set, with its audit entry
// auditID, or that neither is there.
func wantSetting(t *testing.T, m *Master, name, auditID string, there bool) {
	t.Helper()
	ctx := context.Background()
	_, err := m.Setting(ctx, name)
	switch {
	case there && err != nil:
		t.Errorf("setting %s: %v, want it set", name, err)
	case !there && fault.KindOf(err) != fault.NotFound:
		t.Errorf("setting %s: %v, want it absent", name, err)
	}
	var n int
	if err := m.p.read.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE id = ?`, auditID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if want := map[bool]int{true: 1, false: 0}[there]; n != want {
		t.Errorf("audit entry %s: %d rows, want %d", auditID, n, want)
	}
}

// TestFailingChangeInBatchAlone guards the isolation of a change inside a
// shared transaction: changes that fail in the middle of a batch (a
// duplicate audit id after the setting was written, a duplicate tenant
// name, a panic after a write) fail for their callers only, leave nothing of
// themselves, and the changes before and after them in the same commit are
// written with their audit entries.
func TestFailingChangeInBatchAlone(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	if err := m.SetSetting(ctx, "first", "v", entry("taken"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateTenant(ctx, master.Tenant{ID: "t1", Name: "lab", Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: mastertest.T0}, entry("tenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	commits := m.w.commits.Load()

	h := holdWriter(t, &m.base)
	before := submit(t, &m.base, 1, func() error { return m.SetSetting(ctx, "before", "v", entry("a-before"), master.LocalOperator()) })
	dupAudit := submit(t, &m.base, 2, func() error { return m.SetSetting(ctx, "dup-audit", "v", entry("taken"), master.LocalOperator()) })
	dupName := submit(t, &m.base, 3, func() error {
		return m.CreateTenant(ctx, master.Tenant{ID: "t2", Name: "lab", Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: mastertest.T0}, entry("a-dup-name"), master.LocalOperator())
	})
	panics := submit(t, &m.base, 4, func() error {
		return m.write(ctx, "test", func(tx *sql.Tx, s *statements) error {
			if _, err := tx.StmtContext(ctx, s.upsertSetting).ExecContext(ctx, "half", "v"); err != nil {
				return err
			}
			panic("inside a change")
		})
	})
	after := submit(t, &m.base, 5, func() error { return m.SetSetting(ctx, "after", "v", entry("a-after"), master.LocalOperator()) })
	h.letGo(t)

	if r := answer(t, "the change before", before); r.err != nil || r.panicked != nil {
		t.Errorf("the change before the failures: %+v", r)
	}
	if r := answer(t, "the duplicate audit id", dupAudit); !errors.Is(r.err, master.ErrDuplicateAuditID) {
		t.Errorf("the duplicate audit id: %+v, want master.ErrDuplicateAuditID", r)
	}
	if r := answer(t, "the duplicate name", dupName); fault.KindOf(r.err) != fault.Conflict {
		t.Errorf("the duplicate tenant name: %+v, want Conflict", r)
	}
	if r := answer(t, "the panic", panics); r.panicked != "inside a change" {
		t.Errorf("the panicking change: %+v, want its panic in its own goroutine", r)
	}
	if r := answer(t, "the change after", after); r.err != nil || r.panicked != nil {
		t.Errorf("the change after the failures: %+v", r)
	}
	if got := m.w.commits.Load() - commits; got != 2 {
		t.Errorf("%d commits, want 2: the holding change, then one for the five queued", got)
	}

	wantSetting(t, m, "before", "a-before", true)
	wantSetting(t, m, "after", "a-after", true)
	wantSetting(t, m, "dup-audit", "a-dup-audit", false)
	wantSetting(t, m, "half", "a-half", false)
	if _, err := m.Tenant(ctx, "t2"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the tenant with a taken name: %v, want NotFound", err)
	}
	wantSetting(t, m, "x", "a-dup-name", false)
}

// TestAnsweredOnlyAfterCommit guards durability as callers see it: no
// change of a batch is answered before the commit that holds it, which with
// synchronous=FULL is the write-ahead log synced to the disk.
func TestAnsweredOnlyAfterCommit(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	var early []string
	m.w.beforeCommit = func(written []*change) {
		for _, c := range written {
			if len(c.done) != 0 {
				early = append(early, c.message)
			}
		}
	}
	h := holdWriter(t, &m.base)
	var answers []<-chan result
	for i := range 3 {
		answers = append(answers, submit(t, &m.base, i+1, func() error {
			return m.SetSetting(ctx, fmt.Sprintf("s%d", i), "v", entry(fmt.Sprintf("a%d", i)), master.LocalOperator())
		}))
	}
	h.letGo(t)
	for i, ch := range answers {
		if r := answer(t, "a change", ch); r.err != nil {
			t.Errorf("change %d: %+v", i, r)
		}
	}
	if len(early) != 0 {
		t.Errorf("answered before their commit: %v", early)
	}
}

// TestCanceledCallerAnsweredAtOnce guards a caller whose context ends while
// its change waits: it is answered at once, while the writer is still busy,
// with Unavailable; its change is never written; and the changes queued
// around it are.
func TestCanceledCallerAnsweredAtOnce(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	before := submit(t, &m.base, 1, func() error { return m.SetSetting(ctx, "before", "v", entry("a-before"), master.LocalOperator()) })
	gone, cancel := context.WithCancel(ctx)
	canceled := submit(t, &m.base, 2, func() error { return m.SetSetting(gone, "canceled", "v", entry("a-canceled"), master.LocalOperator()) })
	after := submit(t, &m.base, 3, func() error { return m.SetSetting(ctx, "after", "v", entry("a-after"), master.LocalOperator()) })
	cancel()
	// The writer is still held: an answer now is an answer at once.
	r := answer(t, "the canceled caller", canceled)
	if fault.KindOf(r.err) != fault.Unavailable || !errors.Is(r.err, context.Canceled) {
		t.Errorf("the canceled caller: %+v, want Unavailable with context.Canceled", r)
	}
	h.letGo(t)
	for what, ch := range map[string]<-chan result{"before": before, "after": after} {
		if r := answer(t, what, ch); r.err != nil {
			t.Errorf("the change %s the canceled one: %+v", what, r)
		}
	}
	wantSetting(t, m, "before", "a-before", true)
	wantSetting(t, m, "after", "a-after", true)
	wantSetting(t, m, "canceled", "a-canceled", false)
}

// TestInterruptedChangeInBatch guards a change whose context ends while one
// of its statements runs, which the driver stops with sqlite3_interrupt. An
// interrupted INSERT inside a transaction makes SQLite roll the whole
// transaction back (sqlite.org/c3ref/interrupt.html), taking with it the
// changes before it in the batch: they must still be written, in a new
// transaction, and answered only then, and nothing of the interrupted change
// may stay.
func TestInterruptedChangeInBatch(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	h := holdWriter(t, &m.base)
	before := submit(t, &m.base, 1, func() error { return m.SetSetting(ctx, "before", "v", entry("a-before"), master.LocalOperator()) })
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted := submit(t, &m.base, 2, func() error {
		return m.write(stopped, "test", func(tx *sql.Tx, s *statements) error {
			if err := insertAudit(stopped, tx, s, entry("a-interrupted")); err != nil {
				return err
			}
			// Rows without end, until the context ends and the driver
			// interrupts the statement.
			time.AfterFunc(50*time.Millisecond, cancel)
			_, err := tx.ExecContext(stopped, `INSERT INTO settings (name, value)
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
				SELECT 'endless.' || i, 'v' FROM n`)
			return err
		})
	})
	after := submit(t, &m.base, 3, func() error { return m.SetSetting(ctx, "after", "v", entry("a-after"), master.LocalOperator()) })
	h.letGo(t)

	if r := answer(t, "the interrupted change", interrupted); fault.KindOf(r.err) != fault.Unavailable {
		t.Errorf("the interrupted change: %+v, want Unavailable", r)
	}
	for what, ch := range map[string]<-chan result{"before": before, "after": after} {
		if r := answer(t, what, ch); r.err != nil {
			t.Errorf("the change %s the interrupted one: %+v", what, r)
		}
	}
	wantSetting(t, m, "before", "a-before", true)
	wantSetting(t, m, "after", "a-after", true)
	wantSetting(t, m, "endless.1", "a-interrupted", false)
}

// TestBatchBounded guards the bound on a batch: changes queued past
// maxBatch wait for the next commit rather than lengthen this one.
func TestBatchBounded(t *testing.T) {
	ctx := context.Background()
	m := openReady(t)
	commits := m.w.commits.Load()
	h := holdWriter(t, &m.base)
	n := 2*maxBatch + 1
	var answers []<-chan result
	for i := range n {
		answers = append(answers, submit(t, &m.base, i+1, func() error {
			return m.SetSetting(ctx, fmt.Sprintf("s%d", i), "v", entry(fmt.Sprintf("a%d", i)), master.LocalOperator())
		}))
	}
	h.letGo(t)
	for _, ch := range answers {
		if r := answer(t, "a change", ch); r.err != nil {
			t.Fatal(r)
		}
	}
	if got := m.w.commits.Load() - commits; got != 4 {
		t.Errorf("%d commits for the holding change and %d queued, want 4 (1 + %d, %d, 1)", got, n, maxBatch, maxBatch)
	}
}

// TestTenantChangesGrouped guards that a tenant's changes go through the
// same writer: changes queued together share one commit and keep their own
// results.
func TestTenantChangesGrouped(t *testing.T) {
	ctx := context.Background()
	s := openTenantTest(t, "t1").(*Tenant)
	if err := s.SetSetting(ctx, "first", "v", entry("taken"), nil); err != nil {
		t.Fatal(err)
	}
	commits := s.w.commits.Load()
	h := holdWriter(t, &s.base)
	ok := submit(t, &s.base, 1, func() error { return s.SetSetting(ctx, "ok", "v", entry("a-ok"), nil) })
	dup := submit(t, &s.base, 2, func() error { return s.SetSetting(ctx, "dup", "v", entry("taken"), nil) })
	h.letGo(t)
	if r := answer(t, "the good change", ok); r.err != nil {
		t.Errorf("the good change: %+v", r)
	}
	if r := answer(t, "the duplicate audit id", dup); !errors.Is(r.err, master.ErrDuplicateAuditID) {
		t.Errorf("the duplicate audit id: %+v", r)
	}
	if got := s.w.commits.Load() - commits; got != 2 {
		t.Errorf("%d commits, want 2", got)
	}
	if _, err := s.Setting(ctx, "dup"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("the setting whose audit entry failed: %v, want NotFound", err)
	}
}
