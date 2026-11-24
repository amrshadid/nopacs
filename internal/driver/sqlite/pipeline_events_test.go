package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// countEvents returns how many events of a type the feed holds.
func countEvents(t *testing.T, p *Tenant, typ tenant.EventType) int {
	t.Helper()
	var n int
	if err := p.p.read.QueryRow(`SELECT count(*) FROM events WHERE type = ?`, string(typ)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStepEventOnceWithItsBatch guards a step's event when the transaction
// the step was grouped into is undone by a neighbor (one whose statement is
// interrupted) and run again: the step runs again and writes its event
// again in the new transaction, and the feed holds it once, since the first
// write was undone with its transaction.
func TestStepEventOnceWithItsBatch(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	fixtureSetup(t, p, "a")
	r, _, err := p.OpenReception(ctx, "a", "2.25.1.1", fixtureID("rc"), fixtureAt(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ready(ctx); err != nil {
		t.Fatal(err)
	}
	before := countEvents(t, p, tenant.EventReceptionStep)
	h := holdWriter(t, &p.base)
	var calls atomic.Int32
	stepped := submit(t, &p.base, 1, func() error {
		_, err := p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: fixtureAt(70)},
			func(context.Context, tenant.Reception, tenant.Reception) error { calls.Add(1); return nil })
		return err
	})
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted := submit(t, &p.base, 2, func() error {
		return p.writeTx(stopped, "test", func(tx *sql.Tx) error {
			time.AfterFunc(50*time.Millisecond, cancel)
			_, err := tx.ExecContext(stopped, `INSERT INTO reception_associations (reception_id, association)
				WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
				SELECT 'x', 'a' || i FROM n`)
			return err
		})
	})
	h.letGo(t)
	if res := answer(t, "step", stepped); res.err != nil {
		t.Fatal(res.err)
	}
	answer(t, "interrupted", interrupted)
	t.Logf("the step's hooks ran %d times", calls.Load())
	if calls.Load() < 2 {
		t.Fatalf("the hook ran %d times; the batch was not run again, so this test showed nothing", calls.Load())
	}
	if got := countEvents(t, p, tenant.EventReceptionStep) - before; got != 1 {
		t.Errorf("a step run again with its batch left %d events, want 1", got)
	}
}

// TestEventsResumeAcrossRestart guards a cursor across a restart of the
// store: a reader that stops mid-way, with the store closed and opened
// again on the same file, resumes from its cursor with every event after
// it, once, in order.
func TestEventsResumeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tenant.db")
	open := func() *Tenant {
		s, err := OpenTenant(ctx, path, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	appendN := func(s *Tenant, from, n int) {
		batch := make([]tenant.EventAppend, n)
		for i := range batch {
			e, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "store", State: tenant.StorageRecovered, UsedBytes: int64(from + i)},
				fixtureAt(from+i))
			if err != nil {
				t.Fatal(err)
			}
			batch[i] = tenant.EventAppend{Key: fmt.Sprintf("k%d", from+i), Event: e}
		}
		if _, err := s.AppendEvents(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	s := open()
	appendN(s, 0, 30)
	p, err := s.ReadEvents(ctx, "", nil, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer func() { _ = s.Close() }()
	appendN(s, 30, 5)
	rest, err := s.ReadEvents(ctx, p.Cursor, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Events) != 23 {
		t.Fatalf("after a restart the reader read %d events, want 23", len(rest.Events))
	}
	for i, e := range rest.Events {
		if got := e.Payload.(*tenant.StorageSpaceChanged).UsedBytes; got != int64(12+i) {
			t.Fatalf("event %d after the restart is number %d, want %d", i, got, 12+i)
		}
	}
}

// spaceEvent returns a storage space event with used bytes, at a time.
func spaceEvent(t testing.TB, used int64, at time.Time) tenant.EventRecord {
	t.Helper()
	e, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "store", State: tenant.StorageRecovered, UsedBytes: used}, at)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// writeRawEvent writes an event row straight through SQL, as a later
// version or a damaged file might have left it.
func writeRawEvent(t *testing.T, p *Tenant, typ, payload string) {
	t.Helper()
	if err := p.writeTx(context.Background(), "test", func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO events (type, at, payload) VALUES (?, 1, ?)`, typ, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFeedPassesUnreadableRows guards that one stored row never stops the
// feed: a payload of a newer shape (a field this version does not know) is
// read with the fields it knows, and one that cannot be read back at all
// (empty, not JSON, a value outside this version's lists) is answered with
// its type, time and cursor and no payload, counted on its page, filtered
// or not; the reader moves past it.
func TestFeedPassesUnreadableRows(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "k1", Event: spaceEvent(t, 1, fixtureAt(1))}}); err != nil {
		t.Fatal(err)
	}
	start, err := p.EventCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeRawEvent(t, p, "storage_space", `{"storage":"store","state":"recovered","used_bytes":7,"added_later":{"x":1}}`)
	for _, bad := range []string{`{}`, `not json`, `{"storage":"store","state":"half_full","used_bytes":1}`} {
		writeRawEvent(t, p, "storage_space", bad)
	}
	writeRawEvent(t, p, "file_released", `{"anything":true}`)
	if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "k2", Event: spaceEvent(t, 2, fixtureAt(2))}}); err != nil {
		t.Fatal(err)
	}
	for _, types := range [][]tenant.EventType{nil, {tenant.EventStorageSpace}} {
		page, err := p.ReadEvents(ctx, start, types, 100)
		if err != nil {
			t.Fatalf("a read past unreadable rows (filter %v): %v", types, err)
		}
		want := 6
		if types != nil {
			want = 5
		}
		if len(page.Events) != want || page.Unreadable != 3 {
			t.Fatalf("filter %v: %d events, %d unreadable; want %d and 3", types, len(page.Events), page.Unreadable, want)
		}
		newer, ok := page.Events[0].Payload.(*tenant.StorageSpaceChanged)
		if !ok || newer.UsedBytes != 7 {
			t.Errorf("a payload with a field added later reads %+v, want its known fields", page.Events[0].Payload)
		}
		for _, e := range page.Events[1:4] {
			if e.Payload != nil || e.Type != tenant.EventStorageSpace || e.Cursor == "" {
				t.Errorf("an unreadable row is answered as %+v, want its type and cursor and no payload", e)
			}
		}
		if last := page.Events[len(page.Events)-1].Payload.(*tenant.StorageSpaceChanged); last.UsedBytes != 2 {
			t.Errorf("the event after the unreadable rows reads %+v", last)
		}
		if again, err := p.ReadEvents(ctx, page.Cursor, types, 100); err != nil || len(again.Events) != 0 {
			t.Errorf("a read from after them answered %d events (%v)", len(again.Events), err)
		}
	}
}

// TestReadEventsIsOneSnapshot guards that a page and its cursor come from
// one snapshot: an event committed while the read is under way, between its
// reads, is not passed by the page's cursor, so the next read answers it.
// A cursor read in another snapshot than the events skips it for good.
func TestReadEventsIsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "k1", Event: spaceEvent(t, 1, fixtureAt(1))}}); err != nil {
		t.Fatal(err)
	}
	once := false
	p.setBetweenReads(func() {
		if once {
			return
		}
		once = true
		if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "k2", Event: spaceEvent(t, 2, fixtureAt(2))}}); err != nil {
			t.Error(err)
		}
	})
	page, err := p.ReadEvents(ctx, "", nil, 100)
	p.setBetweenReads(nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := p.ReadEvents(ctx, page.Cursor, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events)+len(next.Events) != 2 {
		t.Errorf("the read under way answered %d events and the next %d: the event committed between them was passed", len(page.Events), len(next.Events))
	}
}

// TestStatusSnapshotIsOneSnapshot guards that a status snapshot's cursor and
// counts come from one snapshot: a step committed while the snapshot is
// read, between its reads, is either in the counts and at or before the
// cursor, or in neither: replaying the events after the cursor onto the
// counts gives the receptions in each step as they are.
func TestStatusSnapshotIsOneSnapshot(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	fixtureSetup(t, p, "a")
	r, _, err := p.OpenReception(ctx, "a", "2.25.1.1", fixtureID("rc"), fixtureAt(1))
	if err != nil {
		t.Fatal(err)
	}
	once := false
	p.setBetweenReads(func() {
		if once {
			return
		}
		once = true
		if _, err := p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: fixtureAt(70)}, nil); err != nil {
			t.Error(err)
		}
	})
	s, err := p.StatusSnapshot(ctx)
	p.setBetweenReads(nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[reception.State]int64{}
	for _, c := range s.InProgress {
		steps[c.Step] = c.Receptions
	}
	after, err := p.ReadEvents(ctx, s.EventCursor, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after.Events {
		if st, ok := e.Payload.(*tenant.ReceptionStepReached); ok {
			if st.PreviousStep != "" {
				steps[st.PreviousStep]--
			}
			steps[st.Step]++
		}
	}
	if steps[reception.Receiving] != 0 || steps[reception.Complete] != 1 {
		t.Errorf("the snapshot replayed with the events after its cursor has %d receiving and %d complete, want 0 and 1", steps[reception.Receiving], steps[reception.Complete])
	}
}

// TestPruneHoldsTheWriterBriefly guards that a pruning of many events is
// made in transactions of at most tenant.PruneStep events each, so the one
// writer is never held for the whole pruning: 1,200 events removed in one
// call take at least three commits.
func TestPruneHoldsTheWriterBriefly(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	batch := make([]tenant.EventAppend, 0, tenant.MaxEventBatch)
	for i := range 1200 {
		batch = append(batch, tenant.EventAppend{Key: fmt.Sprintf("k%d", i), Event: spaceEvent(t, int64(i), fixtureAt(i))})
		if len(batch) == cap(batch) || i == 1199 {
			if _, err := p.AppendEvents(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if err := p.CheckpointEvents(ctx, fixtureAt(50000)); err != nil {
		t.Fatal(err)
	}
	before := p.w.commits.Load()
	n, more, err := p.PruneEvents(ctx, fixtureAt(100000), tenant.MaxPruneBatch)
	if err != nil || n != 1200 || more {
		t.Fatalf("pruned %d (more %v): %v", n, more, err)
	}
	if commits := p.w.commits.Load() - before; commits < 3 {
		t.Errorf("a pruning of 1200 events took %d commits, want at least 3 of at most %d events", commits, tenant.PruneStep)
	}
}
