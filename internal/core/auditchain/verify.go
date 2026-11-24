package auditchain

import (
	"crypto/ed25519"
	"strconv"
	"time"
)

// Problem names what a verification found wrong.
type Problem string

// The problems a Verifier reports.
const (
	// ProblemOrder: an entry's seq is not the one after the last; one was
	// removed, repeated or moved.
	ProblemOrder Problem = "an entry is out of order or missing"
	// ProblemEncoding: an entry cannot be encoded.
	ProblemEncoding Problem = "an entry cannot be encoded"
	// ProblemRange: a checkpoint does not cover exactly the entries since the
	// one before.
	ProblemRange Problem = "a checkpoint does not cover the entries since the one before"
	// ProblemLog: a checkpoint names another installation or log.
	ProblemLog Problem = "a checkpoint names another installation or log"
	// ProblemHead: a checkpoint's head is not the chain's value: an entry
	// was changed, or the checkpoint was.
	ProblemHead Problem = "the chain does not reach the checkpoint's head"
	// ProblemPrevious: a checkpoint does not name the one before it.
	ProblemPrevious Problem = "a checkpoint does not follow the one before"
	// ProblemKey: no public key is known for a checkpoint's key version.
	ProblemKey Problem = "no public key for the checkpoint's key version"
	// ProblemSignature: a checkpoint's signature does not verify.
	ProblemSignature Problem = "a checkpoint's signature does not verify"
	// ProblemUnsealed: entries follow the last checkpoint.
	ProblemUnsealed Problem = "entries follow the last checkpoint"
	// ProblemRegress: a checkpoint's key version or time is lower than the
	// one before's, as when a retired key signs after its successor.
	ProblemRegress Problem = "a checkpoint's key version or time goes back"
	// ProblemShort: the log ends before a checkpoint known from outside it,
	// as when its tail was deleted.
	ProblemShort Problem = "the log ends before the checkpoint known from outside it"
	// ProblemAnchor: the checkpoint at the seq known from outside the log is
	// not the one known.
	ProblemAnchor Problem = "the log does not hold the checkpoint known from outside it"
)

// Error is a verification failure: what was wrong, at which seq. For a
// checkpoint, Seq is its To.
type Error struct {
	Seq     uint64
	Problem Problem
}

// Error returns the problem and its seq.
func (e *Error) Error() string {
	return "auditchain: at seq " + strconv.FormatUint(e.Seq, 10) + ": " + string(e.Problem)
}

// State is where a verification starts: the chain after the last entry of a
// checkpoint already trusted. The zero State is the start of a log.
type State struct {
	// Seq is the To of that checkpoint, 0 at the start.
	Seq uint64
	// Head is the chain's value at Seq, zero at the start.
	Head Digest
	// Checkpoint is that checkpoint's digest, zero at the start.
	Checkpoint Digest
	// KeyVersion is that checkpoint's key version, 0 at the start. The next
	// checkpoint's may be the same or higher, never lower: once a key
	// version has signed, an older one, which may have been retired because
	// it leaked, no longer can.
	KeyVersion uint32
	// Time is that checkpoint's time, to the microsecond it is signed at, in
	// UTC; the zero time at the start. The next checkpoint's may be the same
	// or later, never earlier.
	Time time.Time
}

// sealedState returns the state after c, whose digest is d.
func sealedState(c Checkpoint, d Digest) State {
	return State{Seq: c.To, Head: c.Head, Checkpoint: d, KeyVersion: c.KeyVersion, Time: signedTime(c.Time)}
}

// signedTime returns t as the signed text holds it: whole microseconds, in
// UTC, so two States compare equal with == when their checkpoints do.
func signedTime(t time.Time) time.Time {
	return time.UnixMicro(t.UnixMicro()).UTC()
}

// before reports whether a is earlier than b at the microsecond the signed
// text holds. The zero time, which starts a log, is before every time a
// checkpoint holds.
func before(a, b time.Time) bool {
	if b.IsZero() {
		return false
	}
	return a.UnixMicro() < b.UnixMicro()
}

// PublicKeys maps a key version to its Ed25519 public key.
type PublicKeys map[uint32]ed25519.PublicKey

// Anchor is what is known of a log from outside the databases that hold it,
// such as a checkpoint copied to another system when it was made. Without
// one, a log whose tail was deleted verifies as a shorter log: every
// checkpoint left is still signed and still chained.
type Anchor struct {
	// Seq is the To of a checkpoint known to have been made: the log must
	// reach at least it.
	Seq uint64
	// Checkpoint, when not zero, is that checkpoint's digest: the log must
	// hold exactly it, ending at Seq.
	Checkpoint Digest
}

// Verifier checks a range of a log, fed in seq order: entries, each run of
// them followed by the checkpoint that covers it. It holds the chain's
// current value and nothing else, so a log of any length is checked in
// constant memory. After the first failure every call returns it.
type Verifier struct {
	keys         PublicKeys
	installation string
	log          string
	seq          uint64
	head         Digest
	sealed       State
	anchor       Anchor
	err          error
}

// NewVerifier returns a Verifier of one log of one installation, starting at
// from.
func NewVerifier(keys PublicKeys, installation, log string, from State) *Verifier {
	if !from.Time.IsZero() {
		from.Time = signedTime(from.Time)
	}
	return &Verifier{keys: keys, installation: installation, log: log, seq: from.Seq, head: from.Head, sealed: from}
}

// Expect makes Finish fail unless the log reaches a, and Checkpoint fail at
// the checkpoint that ends at a.Seq unless its digest is a.Checkpoint (when
// that is not zero). It is called before the first entry. A start State past
// a.Seq was verified before, and is taken as having reached it.
func (v *Verifier) Expect(a Anchor) {
	v.anchor = a
	if a.Checkpoint != (Digest{}) && v.sealed.Seq == a.Seq && v.sealed.Checkpoint != a.Checkpoint {
		_ = v.fail(a.Seq, ProblemAnchor)
	}
}

// fail records the first failure and returns it.
func (v *Verifier) fail(seq uint64, p Problem) error {
	if v.err == nil {
		v.err = &Error{Seq: seq, Problem: p}
	}
	return v.err
}

// Entry folds e into the chain; its seq must be the one after the last.
func (v *Verifier) Entry(e Entry) error {
	if v.err != nil {
		return v.err
	}
	if e.Seq != v.seq+1 {
		return v.fail(v.seq+1, ProblemOrder)
	}
	head, err := Next(v.head, e)
	if err != nil {
		return v.fail(e.Seq, ProblemEncoding)
	}
	v.seq, v.head = e.Seq, head
	return nil
}

// Checkpoint checks c against the entries folded since the last checkpoint:
// it covers exactly them, names this installation and log and the checkpoint
// before, holds the chain's head, is signed by the key of its version, and
// neither its key version nor its time is lower than the checkpoint
// before's.
func (v *Verifier) Checkpoint(c Checkpoint) error {
	if v.err != nil {
		return v.err
	}
	switch {
	case c.From != v.sealed.Seq+1 || c.To != v.seq || c.To < c.From:
		return v.fail(c.To, ProblemRange)
	case c.Installation != v.installation || c.Log != v.log:
		return v.fail(c.To, ProblemLog)
	case c.Head != v.head:
		return v.fail(c.To, ProblemHead)
	case c.Previous != v.sealed.Checkpoint:
		return v.fail(c.To, ProblemPrevious)
	}
	pub, ok := v.keys[c.KeyVersion]
	if !ok {
		return v.fail(c.To, ProblemKey)
	}
	if !c.VerifySignature(pub) {
		return v.fail(c.To, ProblemSignature)
	}
	// A valid signature under an older key version proves only that the
	// older key signed; after a newer one has, the older may be retired
	// because it leaked, and whoever holds it must not be able to append.
	if c.KeyVersion < v.sealed.KeyVersion || before(c.Time, v.sealed.Time) {
		return v.fail(c.To, ProblemRegress)
	}
	d, err := c.Digest()
	if err != nil {
		return v.fail(c.To, ProblemRange)
	}
	if a := v.anchor; a.Checkpoint != (Digest{}) && c.From <= a.Seq && a.Seq <= c.To && (c.To != a.Seq || d != a.Checkpoint) {
		return v.fail(c.To, ProblemAnchor)
	}
	v.sealed = sealedState(c, d)
	return nil
}

// Finish ends the range and returns the state after its last checkpoint,
// from which the next range is verified. Entries after the last checkpoint
// are a failure: nothing vouches for them. So is ending before the Anchor
// given to Expect.
func (v *Verifier) Finish() (State, error) {
	if v.err != nil {
		return State{}, v.err
	}
	if v.seq != v.sealed.Seq {
		return State{}, v.fail(v.sealed.Seq+1, ProblemUnsealed)
	}
	if v.sealed.Seq < v.anchor.Seq {
		return State{}, v.fail(v.anchor.Seq, ProblemShort)
	}
	return v.sealed, nil
}

// Seal folds entries, which must hold seq from.Seq+1 onward in order, into
// the chain at from, and returns the signed checkpoint that covers them,
// made at time at, and the state after it, from which the next is sealed.
//
// A signer whose key version is lower than from's is refused, as a Verifier
// would refuse its checkpoint. A time earlier than from's, as after the
// clock was stepped back, is raised to from's, so the audit is still sealed
// and the checkpoint still verifies.
func Seal(s Signer, installation, log string, from State, entries []Entry, at time.Time) (Checkpoint, State, error) {
	if len(entries) == 0 {
		return Checkpoint{}, State{}, &Error{Seq: from.Seq + 1, Problem: ProblemRange}
	}
	if s.KeyVersion() < from.KeyVersion {
		return Checkpoint{}, State{}, &Error{Seq: from.Seq + 1, Problem: ProblemRegress}
	}
	if before(at, from.Time) {
		at = from.Time
	}
	head := from.Head
	next := from.Seq + 1
	for _, e := range entries {
		if e.Seq != next {
			return Checkpoint{}, State{}, &Error{Seq: next, Problem: ProblemOrder}
		}
		var err error
		if head, err = Next(head, e); err != nil {
			return Checkpoint{}, State{}, &Error{Seq: e.Seq, Problem: ProblemEncoding}
		}
		next++
	}
	c, err := Sign(s, Checkpoint{
		Installation: installation,
		Log:          log,
		From:         from.Seq + 1,
		To:           next - 1,
		Head:         head,
		Previous:     from.Checkpoint,
		Time:         at,
	})
	if err != nil {
		return Checkpoint{}, State{}, err
	}
	d, err := c.Digest()
	if err != nil {
		return Checkpoint{}, State{}, err
	}
	return c, sealedState(c, d), nil
}
