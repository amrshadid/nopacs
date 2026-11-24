package reception

import (
	"errors"
	"testing"
	"time"
)

// TestStepWhole guards the state table whole: every pair of state and event
// is listed here, written out by hand rather than derived from the table,
// with the state it leads to or the kind of its refusal. A pair added to or
// dropped from the table, or a refusal of another kind, fails this test.
func TestStepWhole(t *testing.T) {
	type want struct {
		to   State // set when allowed
		kind Kind  // set when refused
	}
	ok := func(s State) want { return want{to: s} }
	no := func(k Kind) want { return want{kind: k} }
	table := map[State]map[Event]want{
		Receiving: {
			File: ok(Receiving), Quiet: ok(Complete), Identify: no(OutOfOrder), Archive: no(OutOfOrder),
			Verify: no(OutOfOrder), Finish: no(OutOfOrder), Quarantine: ok(Quarantined), Fail: ok(Failed),
		},
		Complete: {
			File: no(Late), Quiet: no(Already), Identify: ok(Identified), Archive: no(OutOfOrder),
			Verify: no(OutOfOrder), Finish: no(OutOfOrder), Quarantine: ok(Quarantined), Fail: ok(Failed),
		},
		Identified: {
			File: no(Late), Quiet: no(Already), Identify: no(Already), Archive: ok(Archived),
			Verify: no(OutOfOrder), Finish: no(OutOfOrder), Quarantine: ok(Quarantined), Fail: ok(Failed),
		},
		Archived: {
			File: no(Late), Quiet: no(Already), Identify: no(Already), Archive: no(Already),
			Verify: ok(Verified), Finish: no(OutOfOrder), Quarantine: ok(Quarantined), Fail: ok(Failed),
		},
		Verified: {
			File: no(Late), Quiet: no(Already), Identify: no(Already), Archive: no(Already),
			Verify: no(Already), Finish: ok(Done), Quarantine: ok(Quarantined), Fail: ok(Failed),
		},
		Done: {
			File: no(Late), Quiet: no(Ended), Identify: no(Ended), Archive: no(Ended),
			Verify: no(Ended), Finish: no(Ended), Quarantine: no(Ended), Fail: no(Ended),
		},
		Quarantined: {
			File: no(Late), Quiet: no(Ended), Identify: no(Ended), Archive: no(Ended),
			Verify: no(Ended), Finish: no(Ended), Quarantine: no(Ended), Fail: no(Ended),
		},
		Failed: {
			File: no(Late), Quiet: no(Ended), Identify: no(Ended), Archive: no(Ended),
			Verify: no(Ended), Finish: no(Ended), Quarantine: no(Ended), Fail: no(Ended),
		},
	}
	if len(table) != len(States()) {
		t.Fatalf("the test lists %d states, the package %d", len(table), len(States()))
	}
	n := 0
	for _, s := range States() {
		row, ok := table[s]
		if !ok || len(row) != len(Events()) {
			t.Fatalf("state %s: the test lists %d events, the package %d", s, len(row), len(Events()))
		}
		for _, e := range Events() {
			w, ok := row[e]
			if !ok {
				t.Fatalf("%s/%s not listed", s, e)
			}
			n++
			got, err := Step(s, e)
			if w.to != "" {
				if err != nil || got != w.to {
					t.Errorf("%s on %s: got %s %v, want %s", s, e, got, err, w.to)
				}
				continue
			}
			var r *Refusal
			if !errors.As(err, &r) || r.Kind != w.kind || r.State != s || r.Event != e || got != s {
				t.Errorf("%s on %s: got %s %v, want refusal %v", s, e, got, err, w.kind)
			}
		}
	}
	if n != 64 {
		t.Fatalf("%d pairs checked", n)
	}
}

// TestStepUnknown guards that a state or an event this package does not
// know is refused as Unknown, never stepped.
func TestStepUnknown(t *testing.T) {
	for _, c := range []struct {
		s State
		e Event
	}{{"paused", Quiet}, {Receiving, "pause"}, {"", ""}, {"Receiving", File}} {
		_, err := Step(c.s, c.e)
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != Unknown {
			t.Errorf("%q/%q: %v", c.s, c.e, err)
		}
	}
}

// TestEnded guards which states are ends.
func TestEnded(t *testing.T) {
	for _, s := range States() {
		want := s == Done || s == Quarantined || s == Failed
		if s.Ended() != want {
			t.Errorf("%s: ended %v", s, s.Ended())
		}
	}
	if State("x").Ended() {
		t.Error("an unknown state is an end")
	}
}

// TestEveryStateReachesAnEnd guards that the table has no dead loop: from
// every state an end is reachable, and only File steps to the same state.
func TestEveryStateReachesAnEnd(t *testing.T) {
	for _, s := range States() {
		seen := map[State]bool{s: true}
		queue := []State{s}
		reached := false
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			if c.Ended() {
				reached = true
			}
			for _, e := range Events() {
				next, err := Step(c, e)
				if err != nil {
					continue
				}
				if next == c && e != File {
					t.Errorf("%s on %s steps to itself", c, e)
				}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		if !reached {
			t.Errorf("no end reachable from %s", s)
		}
	}
}

// TestCompletion guards the completion decision at the edges of the quiet
// period and the longest open time, which wins when both hold, and when to
// ask again.
func TestCompletion(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tm := Timing{Opened: t0, LastFile: t0.Add(10 * time.Second), Quiet: time.Minute, MaxOpen: time.Hour}
	for _, c := range []struct {
		name   string
		timing Timing
		now    time.Time
		reason Reason
		due    time.Time
	}{
		{"just after a file", tm, t0.Add(11 * time.Second), NotYet, t0.Add(70 * time.Second)},
		{"one ns before quiet", tm, t0.Add(70*time.Second - 1), NotYet, t0.Add(70 * time.Second)},
		{"quiet exactly", tm, t0.Add(70 * time.Second), Quieted, t0.Add(70 * time.Second)},
		{"long quiet", tm, t0.Add(48 * time.Hour), Quieted, t0.Add(70 * time.Second)},
		{
			"files keep coming", Timing{Opened: t0, LastFile: t0.Add(time.Hour - time.Second), Quiet: time.Minute, MaxOpen: time.Hour},
			t0.Add(time.Hour), TimedOut, t0.Add(time.Hour),
		},
		{
			"before max open", Timing{Opened: t0, LastFile: t0.Add(time.Hour - time.Second), Quiet: time.Minute, MaxOpen: time.Hour},
			t0.Add(time.Hour - 1), NotYet, t0.Add(time.Hour),
		},
		{
			"both hold, quiet wins", Timing{Opened: t0, LastFile: t0, Quiet: time.Minute, MaxOpen: time.Second},
			t0.Add(time.Minute), Quieted, t0.Add(time.Second),
		},
		{"no last file", Timing{Opened: t0, Quiet: time.Minute, MaxOpen: time.Hour}, t0.Add(time.Minute), Quieted, t0.Add(time.Minute)},
		{"last before opened", Timing{Opened: t0, LastFile: t0.Add(-time.Hour), Quiet: time.Minute, MaxOpen: time.Hour}, t0.Add(time.Second), NotYet, t0.Add(time.Minute)},
	} {
		d, err := Completion(c.timing, c.now)
		if err != nil {
			t.Fatal(err)
		}
		if d.Complete != (c.reason != NotYet) || d.Reason != c.reason || !d.Due.Equal(c.due) || d.ClockBack {
			t.Errorf("%s: got %+v, want reason %v due %v", c.name, d, c.reason, c.due)
		}
	}
}

// TestCompletionClockBack guards the clock set back: a now before the last
// file, or before the reception opened, counts from now, says so, and does
// not hold the reception open until the clock catches up.
func TestCompletionClockBack(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tm := Timing{Opened: t0, LastFile: t0.Add(time.Minute), Quiet: time.Minute, MaxOpen: time.Hour}

	// Set back 30 s, between Opened and LastFile.
	now := t0.Add(30 * time.Second)
	d, _ := Completion(tm, now)
	if d.Complete || !d.ClockBack || !d.LastFile.Equal(now) || !d.Opened.Equal(t0) || !d.Due.Equal(now.Add(time.Minute)) {
		t.Errorf("between: %+v", d)
	}
	// Set back an hour, before Opened: due a quiet period from now, not
	// an hour and a minute from now.
	now = t0.Add(-time.Hour)
	d, _ = Completion(tm, now)
	if d.Complete || !d.ClockBack || !d.Opened.Equal(now) || !d.LastFile.Equal(now) || !d.Due.Equal(now.Add(time.Minute)) {
		t.Errorf("before opened: %+v", d)
	}
	// Storing the times it returned and asking a quiet period later
	// completes the reception.
	d, _ = Completion(Timing{Opened: d.Opened, LastFile: d.LastFile, Quiet: tm.Quiet, MaxOpen: tm.MaxOpen}, now.Add(time.Minute))
	if !d.Complete || d.Reason != Quieted || d.ClockBack {
		t.Errorf("after storing: %+v", d)
	}
}

// TestCompletionRefuses guards that a quiet period or longest open time that
// is not positive is refused, not taken as "complete at once".
func TestCompletionRefuses(t *testing.T) {
	t0 := time.Now()
	for _, tm := range []Timing{
		{Opened: t0, Quiet: 0, MaxOpen: time.Hour},
		{Opened: t0, Quiet: time.Minute, MaxOpen: 0},
		{Opened: t0, Quiet: -time.Minute, MaxOpen: time.Hour},
	} {
		if _, err := Completion(tm, t0); err == nil {
			t.Errorf("%+v accepted", tm)
		}
	}
}
