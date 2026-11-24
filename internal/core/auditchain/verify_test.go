package auditchain

import (
	"errors"
	"testing"
	"time"
)

// TestVerifyAcceptsASealedLog guards that a log sealed in several
// checkpoints, signed under two key versions, verifies whole, that Finish
// returns the state after its last checkpoint, and that the next range
// verifies from that state alone, as an export starting mid-log does.
func TestVerifyAcceptsASealedLog(t *testing.T) {
	l := makeLog(t, "inst-1", MasterLog, 3, 1, 6)
	state, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, l.entries, l.checkpoints)
	if err != nil {
		t.Fatal(err)
	}
	if state != l.final || state.Seq != 10 {
		t.Fatalf("Finish = %+v, want %+v", state, l.final)
	}
	if l.checkpoints[0].KeyVersion != 1 || l.checkpoints[2].KeyVersion != 2 {
		t.Fatalf("key versions %d and %d, want 1 and 2", l.checkpoints[0].KeyVersion, l.checkpoints[2].KeyVersion)
	}
	// The third checkpoint alone, from the state after the second.
	mid, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, l.entries[:4], l.checkpoints[:2])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyLog(l.keys, "inst-1", MasterLog, mid, l.entries[4:], l.checkpoints[2:]); err != nil {
		t.Fatalf("the last range does not verify from the state before it: %v", err)
	}
	// An empty range verifies to where it started.
	if s, err := verifyLog(l.keys, "inst-1", MasterLog, mid, nil, nil); err != nil || s != mid {
		t.Fatalf("an empty range: %+v, %v", s, err)
	}
}

// wantProblem fails the test unless err is an *Error with the given problem
// at the given seq.
func wantProblem(t *testing.T, what string, err error, p Problem, seq uint64) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Errorf("%s: err = %v, want %q at seq %d", what, err, p, seq)
		return
	}
	if e.Problem != p || e.Seq != seq {
		t.Errorf("%s: %q at seq %d, want %q at seq %d", what, e.Problem, e.Seq, p, seq)
	}
}

// TestVerifyFindsEachTamper guards that each way of changing a sealed log is
// found, and where: an entry changed, removed, inserted or two swapped; a
// checkpoint removed, its head or time changed, signed by another key, of
// an unknown key version, or naming another installation or log; and
// entries left after the last checkpoint.
func TestVerifyFindsEachTamper(t *testing.T) {
	l := makeLog(t, "inst-1", "tenant-7", 3, 4, 3)
	run := func(entries []Entry, checkpoints []Checkpoint, keys PublicKeys) error {
		_, err := verifyLog(keys, "inst-1", "tenant-7", State{}, entries, checkpoints)
		return err
	}
	clone := func() ([]Entry, []Checkpoint) {
		return append([]Entry(nil), l.entries...), append([]Checkpoint(nil), l.checkpoints...)
	}

	es, cs := clone()
	es[4].Reason = "rewritten"
	wantProblem(t, "entry 5 changed", run(es, cs, l.keys), ProblemHead, 7)

	es, cs = clone()
	es = append(es[:4], es[5:]...)
	wantProblem(t, "entry 5 removed", run(es, cs, l.keys), ProblemOrder, 5)

	es, cs = clone()
	extra := es[4]
	extra.ID = "inserted"
	es = append(es[:5], append([]Entry{extra}, es[5:]...)...)
	wantProblem(t, "an entry inserted", run(es, cs, l.keys), ProblemOrder, 6)

	es, cs = clone()
	es[4], es[5] = es[5], es[4]
	wantProblem(t, "entries 5 and 6 swapped", run(es, cs, l.keys), ProblemOrder, 5)

	es, cs = clone()
	es[4].Seq, es[5].Seq = 6, 5
	es[4], es[5] = es[5], es[4]
	wantProblem(t, "entries 5 and 6 swapped with their seqs", run(es, cs, l.keys), ProblemHead, 7)

	es, cs = clone()
	cs = append(cs[:1], cs[2:]...)
	wantProblem(t, "checkpoint 2 removed", run(es, cs, l.keys), ProblemRange, 10)

	es, cs = clone()
	cs[1].Head[0] ^= 1
	wantProblem(t, "checkpoint head changed", run(es, cs, l.keys), ProblemHead, 7)

	es, cs = clone()
	cs[1].Time = cs[1].Time.Add(time.Second)
	wantProblem(t, "checkpoint time changed", run(es, cs, l.keys), ProblemSignature, 7)

	es, cs = clone()
	forged, err := Sign(newTestSigner(2, 0x66), cs[1])
	if err != nil {
		t.Fatal(err)
	}
	cs[1] = forged
	wantProblem(t, "checkpoint signed by another key", run(es, cs, l.keys), ProblemSignature, 7)

	es, cs = clone()
	wantProblem(t, "key version 2 unknown", run(es, cs, PublicKeys{1: l.keys[1]}), ProblemKey, 7)

	es, cs = clone()
	_, err = verifyLog(l.keys, "inst-2", "tenant-7", State{}, es, cs)
	wantProblem(t, "another installation", err, ProblemLog, 3)
	_, err = verifyLog(l.keys, "inst-1", MasterLog, State{}, es, cs)
	wantProblem(t, "another log", err, ProblemLog, 3)

	es, cs = clone()
	cs[1].Previous = cs[0].Head
	wantProblem(t, "checkpoint 2 names the wrong previous", run(es, cs, l.keys), ProblemPrevious, 7)

	es, cs = clone()
	wantProblem(t, "the last checkpoint removed", run(es, cs[:2], l.keys), ProblemUnsealed, 8)
}

// TestVerifyRefusesAKeyOrTimeGoingBack guards that once a checkpoint is
// signed with key version 2, one signed with version 1 (a retired key, whose
// signature is valid) is refused, dated earlier or not, and so is a
// checkpoint of the same key dated before the one it follows; and that Seal
// refuses such a signer and raises a time earlier than the last to it.
func TestVerifyRefusesAKeyOrTimeGoingBack(t *testing.T) {
	l := makeLog(t, "inst-1", MasterLog, 3, 4, 3)
	mid, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, l.entries[:7], l.checkpoints[:2])
	if err != nil {
		t.Fatal(err)
	}
	if mid.KeyVersion != 2 || !mid.Time.Equal(l.checkpoints[1].Time) {
		t.Fatalf("the state after checkpoint 2 holds key version %d and time %v", mid.KeyVersion, mid.Time)
	}
	// Seal itself refuses the older signer, so the forgery is made from a
	// state that has forgotten which key and time came last, as a holder of
	// the retired key would make it.
	forget := mid
	forget.KeyVersion, forget.Time = 0, time.Time{}
	seal := func(s Signer, at time.Time) Checkpoint {
		c, _, err := Seal(s, "inst-1", MasterLog, forget, l.entries[7:], at)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	old, earlier, later := newTestSigner(1, 0x41), mid.Time.Add(-time.Minute), mid.Time.Add(time.Minute)
	for name, c := range map[string]Checkpoint{
		"key version 1, dated earlier":    seal(old, earlier),
		"key version 1, dated later":      seal(old, later),
		"key version 2, dated earlier":    seal(newTestSigner(2, 0x42), earlier),
		"key version 1, at the same time": seal(old, mid.Time),
	} {
		cs := append(append([]Checkpoint(nil), l.checkpoints[:2]...), c)
		_, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, l.entries, cs)
		wantProblem(t, name, err, ProblemRegress, 10)
		_, err = verifyLog(l.keys, "inst-1", MasterLog, mid, l.entries[7:], cs[2:])
		wantProblem(t, name+", from the state after checkpoint 2", err, ProblemRegress, 10)
	}

	_, _, err = Seal(old, "inst-1", MasterLog, mid, l.entries[7:], later)
	wantProblem(t, "Seal with key version 1 after 2", err, ProblemRegress, 8)
	c, next, err := Seal(newTestSigner(2, 0x42), "inst-1", MasterLog, mid, l.entries[7:], earlier)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Time.Equal(mid.Time) || !next.Time.Equal(mid.Time) {
		t.Errorf("a time a minute before the last was sealed as %v, want %v", c.Time, mid.Time)
	}
	if _, err := verifyLog(l.keys, "inst-1", MasterLog, mid, l.entries[7:], []Checkpoint{c}); err != nil {
		t.Errorf("the checkpoint sealed at the raised time: %v", err)
	}
}

// verifyAnchored is verifyLog with a.
func verifyAnchored(keys PublicKeys, from State, a Anchor, entries []Entry, checkpoints []Checkpoint) (State, error) {
	v := NewVerifier(keys, "inst-1", MasterLog, from)
	v.Expect(a)
	next := 0
	for _, e := range entries {
		if err := v.Entry(e); err != nil {
			return State{}, err
		}
		for next < len(checkpoints) && checkpoints[next].To <= e.Seq {
			if err := v.Checkpoint(checkpoints[next]); err != nil {
				return State{}, err
			}
			next++
		}
	}
	return v.Finish()
}

// TestVerifyFindsATailRolledBack guards the Anchor: a log whose last
// checkpoint and the entries it covers were deleted together still verifies
// on its own (it is a valid shorter log), and is refused once the Verifier
// expects the seq or the digest of the last checkpoint, as kept outside the
// databases; a log holding another checkpoint where the anchor names one is
// refused too.
func TestVerifyFindsATailRolledBack(t *testing.T) {
	l := makeLog(t, "inst-1", MasterLog, 3, 4, 3)
	cut, cutCPs := l.entries[:7], l.checkpoints[:2]
	if _, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, cut, cutCPs); err != nil {
		t.Fatalf("the rolled-back log alone: %v; it is expected to verify", err)
	}
	last := Anchor{Seq: l.final.Seq, Checkpoint: l.final.Checkpoint}
	_, err := verifyAnchored(l.keys, State{}, Anchor{Seq: l.final.Seq}, cut, cutCPs)
	wantProblem(t, "rolled back, the last seq expected", err, ProblemShort, 10)
	_, err = verifyAnchored(l.keys, State{}, last, cut, cutCPs)
	wantProblem(t, "rolled back, the last checkpoint expected", err, ProblemShort, 10)

	if s, err := verifyAnchored(l.keys, State{}, last, l.entries, l.checkpoints); err != nil || s != l.final {
		t.Errorf("the whole log, the last checkpoint expected: %+v, %v", s, err)
	}
	mid, err := verifyLog(l.keys, "inst-1", MasterLog, State{}, cut, cutCPs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyAnchored(l.keys, State{}, Anchor{Seq: 7, Checkpoint: mid.Checkpoint}, l.entries, l.checkpoints); err != nil {
		t.Errorf("an earlier checkpoint expected: %v", err)
	}
	if _, err := verifyAnchored(l.keys, mid, Anchor{Seq: 7, Checkpoint: mid.Checkpoint}, l.entries[7:], l.checkpoints[2:]); err != nil {
		t.Errorf("starting at the expected checkpoint: %v", err)
	}

	wrong := last
	wrong.Checkpoint[0] ^= 1
	_, err = verifyAnchored(l.keys, State{}, wrong, l.entries, l.checkpoints)
	wantProblem(t, "another checkpoint at the expected seq", err, ProblemAnchor, 10)
	_, err = verifyAnchored(l.keys, State{}, Anchor{Seq: 5, Checkpoint: mid.Checkpoint}, l.entries, l.checkpoints)
	wantProblem(t, "no checkpoint ends at the expected seq", err, ProblemAnchor, 7)
	_, err = verifyAnchored(l.keys, mid, Anchor{Seq: 7, Checkpoint: wrong.Checkpoint}, l.entries[7:], l.checkpoints[2:])
	wantProblem(t, "starting at another checkpoint than expected", err, ProblemAnchor, 7)
}

// TestVerifierKeepsItsFirstFailure guards that after a failure every call,
// Finish included, returns the same error, so a caller that goes on feeding
// a log cannot end with a pass.
func TestVerifierKeepsItsFirstFailure(t *testing.T) {
	l := makeLog(t, "inst-1", MasterLog, 3)
	v := NewVerifier(l.keys, "inst-1", MasterLog, State{})
	first := v.Entry(l.entries[1])
	wantProblem(t, "seq 2 first", first, ProblemOrder, 1)
	if err := v.Entry(l.entries[0]); !errors.Is(err, first) {
		t.Errorf("Entry after a failure = %v", err)
	}
	if err := v.Checkpoint(l.checkpoints[0]); !errors.Is(err, first) {
		t.Errorf("Checkpoint after a failure = %v", err)
	}
	if _, err := v.Finish(); !errors.Is(err, first) {
		t.Errorf("Finish after a failure = %v", err)
	}
}
