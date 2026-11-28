package tenanttest

// The status snapshot, and the order of the feed against commits.

import (
	"context"
	"fmt"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testStatusSnapshot guards the snapshot: its counts equal the running
// totals and the receptions in each step, the incoming bytes count what the
// receptions hold and the quarantine keeps, and its cursor divides the feed
// exactly: a read from it answers the changes made after it and nothing
// else.
func testStatusSnapshot(e *eenv) {
	e.addReceiver("a", "store", 11112)
	empty, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	if empty.EventCursor == "" || len(empty.InProgress) != 0 || empty.IncomingBytes != 0 || empty.Catalog != (tenant.CatalogCounts{}) || len(empty.StorageBytes) != 0 {
		e.Errorf("an empty tenant's snapshot = %+v", empty)
	}
	r1 := e.openReception("a", 1, at(1))
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{file(r1, 1, 1)}, at(2), nil)
	e.ok("AddReceivedFiles", err)
	r2, _ := e.identified("a", 2, 2, 3)
	e.commit(r2, file(r2, 2, 2))
	done := e.stepTo(e.openReception("a", 3, at(3)), reception.Done, "store")
	failed := e.failReception(e.openReception("a", 4, at(4)))
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{e.heldFile("a", 40, 5)}, nil)
	e.ok("HoldFiles", err)
	s, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	cat, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	files, bytes, err := e.p.QuarantineTotals(e.ctx)
	e.ok("QuarantineTotals", err)
	wantSteps := []tenant.StepCount{{Step: reception.Receiving, Receptions: 1}, {Step: reception.Identified, Receptions: 1}}
	if s.Catalog != cat || s.QuarantinedFiles != files || s.QuarantinedBytes != bytes || fmt.Sprint(s.InProgress) != fmt.Sprint(wantSteps) {
		e.Errorf("the snapshot = %+v, want catalog %+v, quarantine %d files %d bytes, steps %v", s, cat, files, bytes, wantSteps)
	}
	// Received bytes of the receptions in progress (1001; 1002 and 1003)
	// and of the failed one (none), and the quarantine's 40.
	if want := int64(1001 + 1002 + 1003 + 40); s.IncomingBytes != want {
		e.Errorf("the snapshot's incoming bytes = %d, want %d", s.IncomingBytes, want)
	}
	// The one instance committed, of 1002 bytes, on its storage; the same
	// file committed again adds nothing, and another file adds its size.
	if fmt.Sprint(s.StorageBytes) != "map[store:1002]" {
		e.Errorf("the snapshot's bytes by storage = %v, want store at 1002", s.StorageBytes)
	}
	e.commit(r2, file(r2, 2, 2), file(r2, 2, 3))
	if again, err := e.ev.StatusSnapshot(e.ctx); err != nil || fmt.Sprint(again.StorageBytes) != "map[store:2005]" {
		e.Errorf("after one more instance the bytes by storage = %v (%v), want store at 2005", again.StorageBytes, err)
	}
	_ = done
	_, err = e.p.DiscardReception(e.ctx, failed.ID, "gone", at(6), e.audit(), nil)
	e.ok("DiscardReception", err)
	_, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r1.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70)},
		nil)
	e.ok("StepReception", err)
	after := e.read(s.EventCursor, 100)
	if len(after.Events) != 1 || e.stepOf(after.Events[0]).Reception != r1.ID {
		e.Errorf("a read from the snapshot's cursor answered %v, want the one step after it", types(after.Events))
	}
	s2, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	if got := e.read(s2.EventCursor, 100); len(got.Events) != 0 {
		e.Errorf("a read from the newest snapshot's cursor answered %d events", len(got.Events))
	}
	wantSteps = []tenant.StepCount{{Step: reception.Complete, Receptions: 1}, {Step: reception.Identified, Receptions: 1}}
	if fmt.Sprint(s2.InProgress) != fmt.Sprint(wantSteps) {
		e.Errorf("after a step the snapshot's steps = %v, want %v", s2.InProgress, wantSteps)
	}
}

// testLateCommit guards commit order where a driver has several writers:
// transaction A writes its event first and commits last, transaction B
// writes after it and commits first (other, the store B runs on, may be
// another instance on the same database). A reader that reads while A is
// open answers nothing it will later contradict: once both commit, a read
// from the cursor the first read gave answers every event the first did
// not, so the two reads hold both events, each once. A driver that reads
// by id alone answers B, moves its cursor past A's id, and never answers A.
// On SQLite it passes by serialization: B cannot begin until A commits, so
// the reader between them sees neither.
func testLateCommit(e *eenv, other EventStore) {
	e.addReceiver("a", "store", 11112)
	ra := e.openReception("a", 1, at(1))
	rb := e.openReception("a", 2, at(2))
	start := e.now()
	ctx, cancel := context.WithTimeout(e.ctx, time.Minute)
	defer cancel()
	wrote, release := make(chan struct{}), make(chan struct{})
	first := true
	hold := func(context.Context, tenant.Reception, tenant.Reception) error {
		// A run again after the batch was undone does not wait a second
		// time: the test waits for the first only.
		if first {
			first = false
			close(wrote)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	aDone := make(chan error, 1)
	go func() {
		_, err := e.ev.StepReception(ctx, tenant.ReceptionStep{ID: ra.ID, From: reception.Receiving, Event: reception.Quiet, At: at(70)},
			hold)
		aDone <- err
	}()
	select {
	case <-wrote:
	case <-ctx.Done():
		e.Fatal("transaction A never wrote its event")
	}
	bDone := make(chan error, 1)
	go func() {
		_, err := other.StepReception(ctx, tenant.ReceptionStep{ID: rb.ID, From: reception.Receiving, Event: reception.Quiet, At: at(71)},
			nil)
		bDone <- err
	}()
	// B commits now on a database with several writers; on one with a
	// single writer it waits for A, and the wait is all this costs.
	bFirst := false
	select {
	case err := <-bDone:
		e.ok("transaction B", err)
		bFirst = true
	case <-time.After(500 * time.Millisecond):
	}
	mid := e.read(start, 100)
	close(release)
	e.ok("transaction A", <-aDone)
	if !bFirst {
		e.ok("transaction B", <-bDone)
	}
	rest, _ := e.readAll(mid.Cursor, 100)
	got := map[string]int{}
	for _, ev := range append(mid.Events, rest...) {
		got[e.stepOf(ev).Reception]++
	}
	if got[ra.ID] != 1 || got[rb.ID] != 1 || len(got) != 2 {
		e.Errorf("a reader between the transactions read %d events and then %d; by reception %v, want each of A and B once (B committed first: %v)",
			len(mid.Events), len(rest), got, bFirst)
	}
}

// testSnapshotDividesTheFeed guards the status snapshot's promise under
// writers working at once: the events after a snapshot's cursor, replayed
// onto its counts, give the counts of a snapshot taken once the writers
// stopped. Each step's event names the step left and the step reached,
// and each quarantine event the totals after it, so both the receptions in
// progress by step and the quarantine's totals can be replayed. A store
// whose cursor and counts come from two moments fails it.
func testSnapshotDividesTheFeed(e *eenv) {
	e.addReceiver("a", "store", 11112)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	done := make(chan error, 4)
	for w := range 4 {
		go func() {
			for i := range 15 {
				if ctx.Err() != nil {
					break
				}
				r, _, err := e.p.OpenReception(ctx, "a", uidOf(1, 1000*w+i), e.id("rc"), at(i))
				if err != nil {
					done <- err
					return
				}
				if i%2 == 0 {
					if _, err := e.p.StepReception(ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet, At: at(i + 60)}, nil); err != nil {
						done <- err
						return
					}
				}
				q := e.heldFile("a", int64(10+i), i)
				if _, err := e.p.HoldFiles(ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
					done <- err
					return
				}
				if i%3 == 0 {
					if _, err := e.p.RejectQuarantined(ctx, q.ID, "no", at(i+1), e.audit(), nil); err != nil {
						done <- err
						return
					}
				}
			}
			done <- nil
		}()
	}
	var snaps []tenant.StatusSnapshot
	for range 10 {
		s, err := e.ev.StatusSnapshot(e.ctx)
		e.ok("StatusSnapshot", err)
		snaps = append(snaps, s)
	}
	for range 4 {
		e.ok("a writer", <-done)
	}
	final, err := e.ev.StatusSnapshot(e.ctx)
	e.ok("StatusSnapshot", err)
	for k, s := range snaps {
		steps := map[reception.State]int64{}
		for _, c := range s.InProgress {
			steps[c.Step] = c.Receptions
		}
		files, bytes := s.QuarantinedFiles, s.QuarantinedBytes
		events, _ := e.readAll(s.EventCursor, 100)
		for _, ev := range events {
			switch p := ev.Payload.(type) {
			case *tenant.ReceptionStepReached:
				if p.PreviousStep != "" {
					steps[p.PreviousStep]--
				}
				steps[p.Step]++
			case *tenant.FileQuarantined:
				files, bytes = p.HeldFiles, p.HeldBytes
			case *tenant.QuarantineDecided:
				files, bytes = p.HeldFiles, p.HeldBytes
			}
		}
		var replayed []tenant.StepCount
		for _, st := range reception.States() {
			if n := steps[st]; n != 0 && !st.Ended() {
				replayed = append(replayed, tenant.StepCount{Step: st, Receptions: n})
			}
		}
		if fmt.Sprint(replayed) != fmt.Sprint(final.InProgress) || files != final.QuarantinedFiles || bytes != final.QuarantinedBytes {
			e.Errorf("snapshot %d replayed with the %d events after its cursor gives steps %v and %d files of %d bytes held; the final snapshot has %v and %d of %d",
				k, len(events), replayed, files, bytes, final.InProgress, final.QuarantinedFiles, final.QuarantinedBytes)
		}
	}
}
