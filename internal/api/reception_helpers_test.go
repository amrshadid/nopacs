package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pace of a test's reception. The fake clock moves only for what waits
// on time (a reception's quiet period, a failed step's retry delay); a step
// that is running is waited for, however long it takes, so the clock never
// outruns a step's lease and a slow, loaded machine only makes the wait
// longer.
const (
	// settleTick is the longest the wait sleeps between two looks when the
	// pipeline signals nothing.
	settleTick = 10 * time.Millisecond
	// receivingStep is how far the clock moves per tick while a reception
	// is still receiving: its quiet period (tenant.DefaultQuietPeriod) ends
	// within a second of real time.
	receivingStep = time.Second
	// idleAfter is how long nothing visible changes before the clock is
	// moved: a step queued for later (a retry after tenant.RetryBase)
	// becomes due, while a step that is running renews its lease many
	// times (every third of it, on the fake clock's short waits) between
	// two moves.
	idleAfter = 250 * time.Millisecond
	// idleStep is how far the clock moves after idleAfter of nothing.
	idleStep = 10 * time.Second
	// stallAfter is how long nothing visible changes before the wait
	// fails: no step committed, no file was recorded, no instance was
	// archived. It bounds a stalled pipeline, not a slow one: each of those
	// changes restarts it.
	stallAfter = 2 * time.Minute
)

// settle waits for a test's reception pipeline: its Wake is given to
// receive.Config.Wake, so every step that commits an event wakes the wait at
// once rather than at its next tick.
type settle struct {
	clock *fakeClock
	woke  chan struct{}
}

// newSettle returns a settle with a fake clock at a fixed time.
func newSettle() *settle {
	return &settle{
		clock: &fakeClock{now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		woke:  make(chan struct{}, 1),
	}
}

// Wake is receive.Config.Wake: it never blocks the pipeline's commit.
func (s *settle) Wake() {
	select {
	case s.woke <- struct{}{}:
	default:
	}
}

// progress is what the wait sees of the pipeline at one look; two looks that
// differ mean the pipeline moved.
type progress struct {
	instances, done, failed, recorded, held int64
	// receiving says a reception is still receiving; going, that one has
	// not ended.
	receiving, going bool
}

// failedIDs returns the ids of lab1's receptions that failed.
func failedIDs(tb testing.TB, p tenant.Pipeline) map[string]bool {
	tb.Helper()
	rs, _, err := p.ListReceptions(context.Background(), tenant.ReceptionFilter{States: []reception.State{reception.Failed}}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		tb.Fatal(err)
	}
	out := make(map[string]bool, len(rs))
	for _, r := range rs {
		out[r.ID] = true
	}
	return out
}

// look reads the pipeline's counters, the receptions that have not ended
// and the catalog's count of instances, in that order, so a count read
// after every reception ended holds all they archived; it fails the test
// at once on a reception that failed and is not among before.
func (s *settle) look(tb testing.TB, pipe *receive.Pipeline, p tenant.Pipeline, before map[string]bool) progress {
	tb.Helper()
	st := pipe.Stats()
	g := progress{done: st.StepsDone.Load(), failed: st.StepsFailed.Load(), recorded: st.Recorded.Load(), held: st.Held.Load()}
	rs, _, err := p.ListReceptions(context.Background(), tenant.ReceptionFilter{States: []reception.State{reception.Failed}}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		tb.Fatal(err)
	}
	for _, r := range rs {
		if !before[r.ID] {
			tb.Fatalf("a reception failed: %s", r.Failure)
		}
	}
	open, _, err := p.ListReceptions(context.Background(), tenant.ReceptionFilter{States: []reception.State{reception.Receiving}}, master.Page{Limit: 1})
	if err != nil {
		tb.Fatal(err)
	}
	going, _, err := p.ListReceptions(context.Background(), tenant.ReceptionFilter{InProgress: true}, master.Page{Limit: 1})
	if err != nil {
		tb.Fatal(err)
	}
	g.receiving, g.going = len(open) > 0, len(going) > 0
	totals, err := p.CatalogTotals(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	g.instances = totals.Instances
	return g
}

// ended says every file was recorded or held and every reception has
// ended: nothing more can be archived, so a count still short of want is
// final.
func (g progress) ended(want int) bool {
	return !g.going && g.recorded+g.held >= int64(want)
}

// await waits until lab1's catalog holds want instances, as pipe archives
// them into p. The clock moves fast only while a reception is receiving,
// and by a retry's worth only after the pipeline has been idle; the wait
// fails when a reception failed, when every reception ended short of want,
// or when nothing moved for stallAfter, with what the quarantine and the
// receptions say.
func (s *settle) await(tb testing.TB, pipe *receive.Pipeline, p tenant.Pipeline, want int) {
	tb.Helper()
	before := failedIDs(tb, p)
	last := s.look(tb, pipe, p, before)
	moved, idleSince := time.Now(), time.Now()
	for last.instances != int64(want) {
		switch {
		case last.receiving:
			s.clock.advance(receivingStep)
		case time.Since(idleSince) >= idleAfter:
			s.clock.advance(idleStep)
			idleSince = time.Now()
		}
		if last.ended(want) || time.Since(moved) > stallAfter {
			s.stalled(tb, p, last, want)
		}
		select {
		case <-s.woke:
		case <-time.After(settleTick):
		}
		now := s.look(tb, pipe, p, before)
		if now != last {
			moved, idleSince = time.Now(), time.Now()
		}
		last = now
	}
}

// stalled fails the test with the state of a pipeline that stopped moving.
func (s *settle) stalled(tb testing.TB, p tenant.Pipeline, last progress, want int) {
	tb.Helper()
	var why []string
	held, _, _ := p.ListQuarantined(context.Background(), tenant.QuarantineFilter{}, master.Page{Limit: 20})
	for _, q := range held {
		why = append(why, string(q.Reason)+" "+q.TransferSyntaxUID)
	}
	rs, _, _ := p.ListReceptions(context.Background(), tenant.ReceptionFilter{}, master.Page{Limit: 20})
	for _, r := range rs {
		why = append(why, "reception "+string(r.State)+" "+r.Failure)
	}
	tb.Fatalf("lab1 holds %d of %d instances, its receptions ended or nothing moved for %v (%+v); quarantined and receptions: %v",
		last.instances, want, stallAfter, last, why)
}
