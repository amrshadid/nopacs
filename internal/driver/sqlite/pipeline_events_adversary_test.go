package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// pageRec is one page a reader of TestEventFeedUnderAdversaries read: the
// cursor it read from (-1 for the empty cursor) and the one it was given,
// its filter, and the events it got.
type pageRec struct {
	reader   int
	from, to int64 // from -1: the empty cursor
	types    []tenant.EventType
	ids      []int64
	typesGot []tenant.EventType
	source   string
}

// TestEventFeedUnderAdversaries guards that a reader never misses or
// repeats an event whatever happens at once: two stores on one file, writers
// through every path that writes an event, grouped batches undone by an
// interrupted neighbor and run again, a pruner, and readers with small
// limits and random filters resuming from every kind of cursor (a page's,
// EventCursor's, a status snapshot's, the empty one). A trigger logs every
// committed event, and each page read must hold exactly the committed
// events between its two cursors that its filter keeps. The ordinary run is
// bounded and skipped under the race detector; NOPACS_SCALE runs the long
// one (make test-scale).
func TestEventFeedUnderAdversaries(t *testing.T) {
	ctx := context.Background()
	if raceDetector {
		// Under the detector it measures the detector for many seconds of
		// the package's race run; the run without it checks every page.
		t.Skip("run without the race detector")
	}
	ops := 60
	if os.Getenv("NOPACS_SCALE") != "" {
		ops = 400
	}
	a0, b0 := openTenantPair(t, tenanttest.SuiteTenant)
	a, b := a0.(*Tenant), b0.(*Tenant)
	fixtureSetup(t, a, "a", "b")
	if _, err := a.ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ready(ctx); err != nil {
		t.Fatal(err)
	}
	// A log of every committed event, written in the event's transaction.
	if err := a.writeTx(ctx, "log", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE events_log (id INTEGER PRIMARY KEY, type TEXT NOT NULL);
			CREATE TRIGGER events_log_ins AFTER INSERT ON events BEGIN INSERT INTO events_log (id, type) VALUES (NEW.id, NEW.type); END;`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stores := []*Tenant{a, b}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var rollbacks atomic.Int32
	// Interrupters: a neighbor whose statement is interrupted undoes its batch.
	for i := range 2 {
		wg.Add(1)
		go func(p *Tenant) {
			defer wg.Done()
			for !stop.Load() {
				stopped, cancel := context.WithCancel(ctx)
				time.AfterFunc(2*time.Millisecond, cancel)
				err := p.writeTx(stopped, "test", func(tx *sql.Tx) error {
					_, err := tx.ExecContext(stopped, `INSERT INTO reception_associations (reception_id, association)
						WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
						SELECT 'x', 'a' || i FROM n`)
					return err
				})
				if err != nil {
					rollbacks.Add(1)
				}
				cancel()
				time.Sleep(time.Millisecond)
			}
		}(stores[i])
	}
	// Pruner.
	var pruned atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		k := 0
		for !stop.Load() {
			k++
			// A checkpoint at each turn, its time only growing, so the
			// pruning, which runs half as far back, always has one to
			// measure by.
			if err := stores[k%2].CheckpointEvents(ctx, fixtureAt(k)); err != nil {
				t.Errorf("checkpoint: %v", err)
				return
			}
			n, _, err := stores[k%2].PruneEvents(ctx, fixtureAt(k/2), 1+k%7)
			if err != nil {
				t.Errorf("prune: %v", err)
				return
			}
			pruned.Add(int64(n))
			time.Sleep(3 * time.Millisecond)
		}
	}()
	// Writers.
	const workers = 8
	var writers sync.WaitGroup
	for w := range workers {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			p := stores[w%2]
			rng := rand.New(rand.NewPCG(uint64(w), 9)) //nolint:gosec // G404, G115: the test's own choice of operations, not a secret; w is 0 to 7.
			var qids []string
			for i := range ops {
				study := rng.IntN(5)
				recv := []string{"a", "b"}[rng.IntN(2)]
				switch rng.IntN(8) {
				case 0:
					r, _, err := p.OpenReception(ctx, recv, fmt.Sprintf("2.25.1.%d", study+1), fixtureID("rc"), fixtureAt(i))
					if err == nil {
						_, _ = p.AddReceivedFiles(ctx, []tenant.ReceivedFile{fixtureFile(r, study, study*1000+rng.IntN(20))}, fixtureAt(i), nil)
					}
				case 1:
					r, _, err := p.OpenReception(ctx, recv, fmt.Sprintf("2.25.1.%d", study+1), fixtureID("rc"), fixtureAt(i))
					if err != nil {
						continue
					}
					id := fixtureID("q")
					q := tenant.QuarantinedFile{ID: id, Reason: tenant.QuarantineNotDICOM, Receiver: recv, ReceptionID: r.ID,
						Size: int64(rng.IntN(1000)), SHA256: fixtureSum(rng.IntN(1000)), Incoming: "quarantine/" + id, ContentKept: true, Quarantined: fixtureAt(i)}
					if _, err := p.HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil); err == nil {
						qids = append(qids, id)
					}
				case 2:
					if len(qids) > 0 {
						_, _ = p.ReleaseQuarantined(ctx, qids[rng.IntN(len(qids))], tenant.ReleaseAsIs, "", fixtureID("rc"), fixtureAt(i), fixtureAudit(), nil)
					}
				case 3:
					if len(qids) > 0 {
						_, _ = p.RejectQuarantined(ctx, qids[rng.IntN(len(qids))], "no", fixtureAt(i), fixtureAudit(), nil)
					}
				case 4, 5:
					rs, _, err := p.ListReceptions(ctx, tenant.ReceptionFilter{InProgress: true}, master.Page{Limit: 5})
					if err != nil {
						continue
					}
					for _, r := range rs {
						switch {
						case r.State == reception.Receiving:
							_, _ = p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quiet, At: fixtureAt(i)}, nil)
						case r.State == reception.Complete && rng.IntN(2) == 0:
							_, _ = p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Fail, At: fixtureAt(i),
								Failure: tenant.FailureWorkGaveUp}, nil)
						}
					}
					fs, _, _ := p.ListReceptions(ctx, tenant.ReceptionFilter{States: []reception.State{reception.Failed}}, master.Page{Limit: 3})
					for _, f := range fs {
						if rng.IntN(2) == 0 {
							_, _, _ = p.RetryReception(ctx, f.ID, fixtureID("rc"), "", fixtureAt(i), fixtureAudit(), nil)
						}
					}
				case 6, 7:
					n := 1 + rng.IntN(4)
					batch := make([]tenant.EventAppend, n)
					for k := range batch {
						// A key always names the same event, as a server
						// appending a batch again does, so keys repeat
						// across writers and batches without a conflict.
						key := rng.IntN(workers * ops)
						e, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "store", State: tenant.StorageRecovered, UsedBytes: int64(key)}, fixtureAt(key))
						if err != nil {
							t.Error(err)
							return
						}
						batch[k] = tenant.EventAppend{Key: fmt.Sprintf("k%d", key), Event: e}
					}
					if _, err := p.AppendEvents(ctx, batch); err != nil {
						t.Errorf("append: %v", err)
					}
				}
			}
		}(w)
	}
	// Readers.
	var mu sync.Mutex
	var recs []pageRec
	var expired atomic.Int32
	all := tenant.EventTypes()
	var readers sync.WaitGroup
	var writersDone atomic.Bool
	for r := range 4 {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			p := stores[r%2]
			rng := rand.New(rand.NewPCG(uint64(r), 77)) //nolint:gosec // G404, G115: the test's own choice of reads, not a secret; r is 0 to 3.
			cursor, source := "", "empty"
			idle := 0
			for {
				var types []tenant.EventType
				if rng.IntN(2) == 0 {
					for _, ty := range all {
						if rng.IntN(3) == 0 {
							types = append(types, ty)
						}
					}
				}
				limit := 1 + rng.IntN(5)
				from := int64(-1)
				if cursor != "" {
					f, _, err := p.eventPosition(cursor)
					if err != nil {
						t.Errorf("position: %v", err)
						return
					}
					from = f
				}
				page, err := p.ReadEvents(ctx, cursor, types, limit)
				if errors.Is(err, tenant.ErrCursorExpired) {
					expired.Add(1)
					s, err := p.StatusSnapshot(ctx)
					if err != nil {
						t.Errorf("snapshot: %v", err)
						return
					}
					cursor, source = s.EventCursor, "snapshot"
					continue
				}
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
				to, _, err := p.eventPosition(page.Cursor)
				if err != nil {
					t.Errorf("position: %v", err)
					return
				}
				rec := pageRec{reader: r, from: from, to: to, types: types, source: source}
				for _, e := range page.Events {
					id, _, _ := p.eventPosition(e.Cursor)
					rec.ids = append(rec.ids, id)
					rec.typesGot = append(rec.typesGot, e.Type)
				}
				mu.Lock()
				recs = append(recs, rec)
				mu.Unlock()
				cursor, source = page.Cursor, "page"
				switch rng.IntN(40) {
				case 0:
					c, err := p.EventCursor(ctx)
					if err == nil {
						cursor, source = c, "eventcursor"
					}
				case 1:
					s, err := p.StatusSnapshot(ctx)
					if err == nil {
						cursor, source = s.EventCursor, "snapshot"
					}
				case 2:
					cursor, source = "", "empty"
				}
				if writersDone.Load() && len(page.Events) == 0 {
					idle++
					if idle > 20 {
						return
					}
				}
			}
		}(r)
	}
	writers.Wait()
	writersDone.Store(true)
	stop.Store(true)
	wg.Wait()
	readers.Wait()

	// Every committed event.
	logged := map[int64]tenant.EventType{}
	rows, err := a.p.read.Query(`SELECT id, type FROM events_log`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var ty string
		if err := rows.Scan(&id, &ty); err != nil {
			t.Fatal(err)
		}
		logged[id] = tenant.EventType(ty)
		ids = append(ids, id)
	}
	_ = rows.Close()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	bad := 0
	for _, rec := range recs {
		keep := func(ty tenant.EventType) bool {
			if len(rec.types) == 0 {
				return true
			}
			for _, x := range rec.types {
				if x == ty {
					return true
				}
			}
			return false
		}
		lo := rec.from
		if lo < 0 {
			if len(rec.ids) == 0 {
				continue
			}
			lo = rec.ids[0] - 1
		}
		var want []int64
		for _, id := range ids {
			if id > lo && id <= rec.to && keep(logged[id]) {
				want = append(want, id)
			}
		}
		if fmt.Sprint(want) != fmt.Sprint(rec.ids) {
			bad++
			if bad < 10 {
				t.Errorf("reader %d page from %d (%s) to %d types %v: got %v, committed %v", rec.reader, rec.from, rec.source, rec.to, rec.types, rec.ids, want)
			}
		}
		for i, id := range rec.ids {
			if logged[id] != rec.typesGot[i] {
				t.Errorf("event %d type %s, logged %s", id, rec.typesGot[i], logged[id])
			}
		}
	}
	var marks int64
	_ = a.p.read.QueryRow(`SELECT pruned_through FROM event_marks`).Scan(&marks)
	t.Logf("%d events committed, %d pages read, %d bad, %d expired cursors, %d pruned (mark %d), %d interrupted neighbors",
		len(ids), len(recs), bad, expired.Load(), pruned.Load(), marks, rollbacks.Load())
	check := func(what, q string) {
		var n int64
		if err := a.p.read.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d", what, n)
		}
	}
	check("reception_steps equals count", `SELECT count(*) FROM (SELECT state, count(*) AS n FROM receptions GROUP BY state) AS c
		LEFT JOIN reception_steps AS s USING (state) WHERE s.receptions IS NOT c.n`)
	check("steps with no reception", `SELECT count(*) FROM reception_steps AS s WHERE s.receptions <> (SELECT count(*) FROM receptions WHERE state = s.state)`)
	if bad != 0 {
		t.Errorf("%d pages of %d skipped or repeated a committed event", bad, len(recs))
	}
}
