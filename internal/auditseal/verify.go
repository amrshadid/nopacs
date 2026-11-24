package auditseal

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The bounds of one verification (audit_seal.proto, VerifyAudit).
const (
	// MaxVerifyEntries is the most entries one Verify recomputes; a longer
	// log is verified in several calls, from NextSeq.
	MaxVerifyEntries = 1_000_000
	// MaxVerifyCheckpoints is the most checkpoints one Verify checks, pruned
	// ones included: a log of many small or pruned checkpoints recomputes
	// few entries but reads and checks the signature of each checkpoint, so
	// a bound on entries alone would let one call run past its deadline.
	MaxVerifyCheckpoints = 10_000
	// VerifyDeadlineMargin is the time left before the context's deadline
	// at which Verify stops after the checkpoint it is on and answers
	// NextSeq, so a slow database ends a call with where to go on rather
	// than with a deadline exceeded and nothing.
	VerifyDeadlineMargin = 2 * time.Second
	// MaxProblems is the most problems one Verify answers.
	MaxProblems = 1000
)

// ProblemKind is what a verification found wrong (audit_seal.proto,
// AuditVerifyProblemKind).
type ProblemKind int

// The kinds.
const (
	// ChainBroken: the chain recomputed from the entries does not reach a
	// checkpoint's head, or a sealed entry lies past every checkpoint.
	ChainBroken ProblemKind = iota + 1
	// EntryMissing: a seq is missing in a range a checkpoint covers and
	// retention has not pruned.
	EntryMissing
	// BadSignature: a checkpoint's signature does not check, or names a key
	// version no public key is given for.
	BadSignature
	// OutOfLine: a checkpoint does not follow the one before (range,
	// previous digest, log, key version or time going back), or the digest
	// kept with it is not its signed text's.
	OutOfLine
	// AnchorDisagrees: the checkpoint at the seq an anchor another log keeps
	// of this one names is there, but its digest is not the anchor's.
	AnchorDisagrees
	// RolledBack: the log does not reach what was kept outside the
	// installation, or no longer holds the checkpoint an anchor another log
	// keeps of it names: it went back past it, and may have grown again.
	RolledBack
	// SealingStalled: an entry not yet sealed has waited more than two seal
	// intervals since it was written and since the log was last sealed:
	// the passes cannot seal the log (an entry that cannot be encoded holds
	// it back), and what waits can be removed without a trace.
	SealingStalled
)

// Problem is one thing a verification found wrong, where it shows.
type Problem struct {
	// Kind is what it is.
	Kind ProblemKind
	// Seq is where it shows: the first entry whose fold differs, the
	// missing seq, or the To of the checkpoint concerned; 0 for a problem of
	// the entries not yet sealed, which have no seq.
	Seq int64
	// Time is, for SealingStalled, when the oldest entry not yet sealed was
	// written: where it is in time, never what it holds. Zero otherwise.
	Time time.Time
	// Detail says it in words from a fixed set of phrases; never an entry's
	// content.
	Detail string
}

// VerifyOptions names the log and range to verify and what to check it
// against.
type VerifyOptions struct {
	// Installation is the installation whose checkpoints the log holds.
	Installation string
	// Log is the log's name in its checkpoints: master.MasterAuditLog or the
	// tenant's id.
	Log string
	// Keys are the public keys to check signatures with. They must come
	// from outside the database checked (PublicKeys derives them from the
	// key file).
	Keys []PublicKey
	// From is the first seq to verify, 0 or 1 for the first; it is moved
	// back to the From of the checkpoint that covers it.
	From int64
	// To is the last seq to verify, 0 for the last sealed.
	To int64
	// MaxEntries bounds the entries recomputed; 0 is MaxVerifyEntries.
	MaxEntries int64
	// MaxCheckpoints bounds the checkpoints checked, pruned ones included;
	// 0 is MaxVerifyCheckpoints.
	MaxCheckpoints int64
	// ExpectedMinSeq, when not 0, is a seq the log is known from outside to
	// have reached. A log taken back and grown again past it passes this
	// check; only ExpectedDigest finds that.
	ExpectedMinSeq int64
	// ExpectedDigest, when not zero, is the digest of a checkpoint known
	// from outside to be in the log.
	ExpectedDigest master.AuditDigest
	// Anchors are the anchors other logs keep of this one: the master's of
	// a tenant, a tenant's of the master.
	Anchors []master.AuditAnchor
	// Now is the time the verification is made at, which the age of a
	// pruned checkpoint and the wait of an unsealed entry are measured
	// from; zero is the clock's time.
	Now time.Time
	// SealInterval is audit.seal-interval as it is now, which says when an
	// unsealed entry has waited too long; 0 is DefaultInterval.
	SealInterval time.Duration
}

// VerifyResult is what a verification found.
type VerifyResult struct {
	// Intact reports that nothing verified was wrong.
	Intact bool
	// EntriesChecked and CheckpointsChecked count what this call checked.
	EntriesChecked, CheckpointsChecked int64
	// Unsealed is how many entries no checkpoint covers yet.
	Unsealed int64
	// NextSeq is where the next call starts; 0 when this one reached the
	// end of the range.
	NextSeq int64
	// Problems are what was found, in order of seq within each step, at
	// most MaxProblems.
	Problems []Problem
	// Last is the log's latest checkpoint, or nil for a log never sealed.
	Last *master.AuditCheckpoint
}

// verification is one Verify's state.
type verification struct {
	ctx  context.Context
	st   master.AuditSealReads
	o    VerifyOptions
	keys auditchain.PublicKeys
	r    VerifyResult
	// retention is what the pruned checkpoints walked need (retention.go).
	retention retentionCheck
}

// problem records a problem, up to MaxProblems.
func (v *verification) problem(kind ProblemKind, seq int64, detail string) {
	if len(v.r.Problems) < MaxProblems {
		v.r.Problems = append(v.r.Problems, Problem{Kind: kind, Seq: seq, Detail: detail})
	}
}

// Verify recomputes a range of a log's chain and checks its checkpoints,
// the anchors other logs keep of it, and what is known of it from outside,
// as VerifyOptions says. It changes nothing. A problem found is in the
// result; an error is a failure to read the log, or options out of bounds
// (Invalid).
func Verify(ctx context.Context, st master.AuditSealReads, o VerifyOptions) (VerifyResult, error) {
	switch {
	case o.From < 0 || o.To < 0:
		return VerifyResult{}, fault.New(fault.Invalid, "a seq is not negative")
	case o.To > 0 && o.To < max(o.From, 1):
		return VerifyResult{}, fault.New(fault.Invalid, "the last seq is not below the first")
	case o.ExpectedMinSeq < 0:
		return VerifyResult{}, fault.New(fault.Invalid, "an expected seq is from 1")
	case o.MaxEntries < 0 || o.MaxCheckpoints < 0:
		return VerifyResult{}, fault.New(fault.Invalid, "a bound of entries or checkpoints is not negative")
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = MaxVerifyEntries
	}
	if o.MaxCheckpoints == 0 {
		o.MaxCheckpoints = MaxVerifyCheckpoints
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.SealInterval <= 0 {
		o.SealInterval = DefaultInterval
	}
	v := &verification{ctx: ctx, st: st, o: o, keys: keyMap(o.Keys, o.Installation)}
	last, ok, err := latest(ctx, st)
	if err != nil {
		return VerifyResult{}, err
	}
	if ok {
		v.r.Last = &last
	}
	if v.r.Unsealed, err = st.CountUnsealedAudit(ctx); err != nil {
		return VerifyResult{}, err
	}
	if err := v.outside(last, ok); err != nil {
		return VerifyResult{}, err
	}
	if err := v.stalled(last, ok); err != nil {
		return VerifyResult{}, err
	}
	if err := v.walk(last, ok); err != nil {
		return VerifyResult{}, err
	}
	if err := v.prunedCovered(last); err != nil {
		return VerifyResult{}, err
	}
	v.r.Intact = len(v.r.Problems) == 0
	return v.r, nil
}

// unsealedReader is the read of the entries not yet sealed, which every
// store has (master.AuditSealWrites) though a verification is given only
// the reads.
type unsealedReader interface {
	UnsealedAudit(ctx context.Context, after master.AuditPosition, limit int) ([]master.AuditEntry, error)
}

// stalled reports the entries not yet sealed when the oldest of them has
// waited more than two seal intervals since it was written and since the
// log's last seal: a pass every interval would have sealed it, so the
// passes are not sealing this log, and what waits can be removed without a
// trace. A log sealed pass after pass through a long backlog is not
// stalled, since its last seal keeps moving. It names the oldest entry's
// time alone.
func (v *verification) stalled(last master.AuditCheckpoint, ok bool) error {
	if v.r.Unsealed == 0 {
		return nil
	}
	u, can := v.st.(unsealedReader)
	if !can {
		return nil
	}
	es, err := u.UnsealedAudit(v.ctx, master.AuditPosition{}, 1)
	if err != nil || len(es) == 0 {
		return err
	}
	since := es[0].Time
	if ok && last.Time.After(since) {
		since = last.Time
	}
	if v.o.Now.Sub(since) > 2*v.o.SealInterval {
		if len(v.r.Problems) < MaxProblems {
			v.r.Problems = append(v.r.Problems, Problem{Kind: SealingStalled, Time: es[0].Time.UTC(),
				Detail: "an entry not yet sealed has waited more than two seal intervals"})
		}
	}
	return nil
}

// outside checks what the caller knows from outside the log: the values
// kept outside the installation and the anchors other logs keep.
func (v *verification) outside(last master.AuditCheckpoint, ok bool) error {
	if m := v.o.ExpectedMinSeq; m > 0 && (!ok || last.To < m) {
		v.problem(RolledBack, m, "the log ends before the seq known from outside it")
	}
	if d := v.o.ExpectedDigest; d != (master.AuditDigest{}) {
		found, err := v.findDigest(d)
		if err != nil {
			return err
		}
		if !found {
			v.problem(RolledBack, last.To, "no checkpoint of the log has the digest known from outside it")
		}
	}
	for _, a := range v.o.Anchors {
		cs, err := v.st.AuditCheckpoints(v.ctx, a.To, 1)
		if err != nil {
			return err
		}
		switch {
		case len(cs) == 0 || cs[0].To != a.To:
			// The anchor is never moved to a checkpoint the log does not
			// descend to (Descends), so a log that no longer holds it went
			// back past it, whether or not it grew again since.
			v.problem(RolledBack, a.To, "the log no longer holds the checkpoint another log keeps of it")
		case !v.digestIs(cs[0], a.Digest):
			v.problem(AnchorDisagrees, a.To, "the anchor another log keeps is not the digest of this log's checkpoint")
		}
	}
	return nil
}

// digestIs reports whether c's signed text has the digest d: computed, not
// read from the row, so a row whose digest column alone was rewritten does
// not pass.
func (v *verification) digestIs(c master.AuditCheckpoint, d master.AuditDigest) bool {
	got, err := chainCheckpoint(c).Digest()
	return err == nil && master.AuditDigest(got) == d
}

// findDigest reports whether a checkpoint of the log has the digest d.
func (v *verification) findDigest(d master.AuditDigest) (bool, error) {
	from := int64(1)
	for {
		cs, err := v.st.AuditCheckpoints(v.ctx, from, master.MaxPageLimit)
		if err != nil {
			return false, err
		}
		for _, c := range cs {
			if v.digestIs(c, d) {
				return true, nil
			}
		}
		if len(cs) < master.MaxPageLimit {
			return false, nil
		}
		from = cs[len(cs)-1].To + 1
	}
}

// walk verifies the range, checkpoint by checkpoint.
func (v *verification) walk(last master.AuditCheckpoint, ok bool) error {
	if !ok {
		return v.tail(0)
	}
	first, err := v.st.AuditCheckpoints(v.ctx, max(v.o.From, 1), 1)
	if err != nil {
		return err
	}
	if len(first) == 0 {
		return nil
	}
	state, err := v.start(first[0])
	if err != nil {
		return err
	}
	seq := first[0].From
	for {
		page, err := v.st.AuditCheckpoints(v.ctx, seq, master.MaxPageLimit)
		if err != nil {
			return err
		}
		for _, c := range page {
			if v.o.To > 0 && c.From > v.o.To {
				return nil
			}
			if state, err = v.checkpoint(c, state); err != nil {
				return err
			}
			v.retention.walkedTo = c.To
			v.r.CheckpointsChecked++
			if v.o.To > 0 && c.To >= v.o.To {
				return nil
			}
			if c.To < last.To && v.bounded() {
				v.r.NextSeq = c.To + 1
				return nil
			}
		}
		if len(page) < master.MaxPageLimit {
			return v.tail(last.To)
		}
		seq = page[len(page)-1].To + 1
	}
}

// bounded reports whether this call has done its share and stops at the
// end of the checkpoint it is on: the entries or the checkpoints reached
// their bound, or the context's deadline is nearer than
// VerifyDeadlineMargin. It is asked only after a checkpoint was checked, so
// every call goes forward by one checkpoint at least.
func (v *verification) bounded() bool {
	if v.r.EntriesChecked >= v.o.MaxEntries || v.r.CheckpointsChecked >= v.o.MaxCheckpoints {
		return true
	}
	dl, ok := v.ctx.Deadline()
	return ok && time.Until(dl) < VerifyDeadlineMargin
}

// start returns the chain's state before c: the start of the log, or the
// state after the checkpoint before c, whose signature is checked; a
// verification that starts in the middle trusts that checkpoint's head,
// which the verification of the range before it checked.
func (v *verification) start(c master.AuditCheckpoint) (auditchain.State, error) {
	if c.From == 1 {
		return auditchain.State{}, nil
	}
	prev, err := v.st.RecentAuditCheckpoints(v.ctx, c.From, 1)
	if err != nil {
		return auditchain.State{}, err
	}
	if len(prev) == 0 || prev[0].To != c.From-1 {
		v.problem(OutOfLine, c.To, string(auditchain.ProblemRange))
		return auditchain.State{Seq: uint64(c.From - 1)}, nil //nolint:gosec // G115: From is above 1 here.
	}
	p := prev[0]
	v.retention.unprunedBefore = !p.Pruned
	if p.Pruned {
		v.retention.before = &p
	}
	pc := chainCheckpoint(p)
	if pub, ok := v.keys[p.KeyVersion]; !ok || !pc.VerifySignature(pub) {
		v.problem(BadSignature, p.To, string(auditchain.ProblemSignature))
	}
	return v.after(p), nil
}

// after returns the state after c, with c's digest computed from its signed
// text.
func (v *verification) after(c master.AuditCheckpoint) auditchain.State {
	s := stateAfter(c)
	if d, err := chainCheckpoint(c).Digest(); err == nil {
		s.Checkpoint = d
	}
	return s
}

// checkpoint checks c and the entries it covers from the state before it,
// and returns the state from which the next is checked, chosen so that one
// thing changed shows as one problem and not as a problem at every later
// checkpoint too. When c's signature checks, its signed text is genuine:
// the next goes on from its head and the digest of its signed text, so
// entries changed under c do not spill into the next. When it does not, c
// itself was changed: the next goes on from the chain recomputed from c's
// entries (when they are all there) and the digest kept with c, which is
// what the next checkpoint was signed after.
func (v *verification) checkpoint(c master.AuditCheckpoint, from auditchain.State) (auditchain.State, error) {
	cc := chainCheckpoint(c)
	before := len(v.r.Problems)
	recomputed, complete := auditchain.Digest{}, false
	switch {
	case cc.From != from.Seq+1 || cc.To < cc.From:
		v.problem(OutOfLine, c.To, string(auditchain.ProblemRange))
	case c.Pruned:
		v.pruned(c, cc, from)
		if len(v.r.Problems) == before {
			v.prunedByRetention(c)
		}
	default:
		v.retention.unprunedBefore = true
		var err error
		if recomputed, complete, err = v.covered(c, cc, from); err != nil {
			return auditchain.State{}, err
		}
	}
	if len(v.r.Problems) == before && !v.digestIs(c, c.Digest) {
		v.problem(OutOfLine, c.To, "the digest kept with the checkpoint is not its signed text's")
	}
	pub, ok := v.keys[c.KeyVersion]
	if ok && cc.VerifySignature(pub) {
		return v.after(c), nil
	}
	s := stateAfter(c)
	if complete {
		s.Head = recomputed
	}
	return s, nil
}

// pruned checks a checkpoint whose entries retention removed: everything
// but its head, which nothing is left to recompute.
func (v *verification) pruned(c master.AuditCheckpoint, cc auditchain.Checkpoint, from auditchain.State) {
	switch {
	case cc.Installation != v.o.Installation || cc.Log != v.o.Log:
		v.problem(OutOfLine, c.To, string(auditchain.ProblemLog))
	case cc.Previous != from.Checkpoint:
		v.problem(OutOfLine, c.To, string(auditchain.ProblemPrevious))
	default:
		pub, ok := v.keys[c.KeyVersion]
		switch {
		case !ok:
			v.problem(BadSignature, c.To, string(auditchain.ProblemKey))
		case !cc.VerifySignature(pub):
			v.problem(BadSignature, c.To, string(auditchain.ProblemSignature))
		case c.KeyVersion < from.KeyVersion || (!from.Time.IsZero() && c.Time.UnixMicro() < from.Time.UnixMicro()):
			v.problem(OutOfLine, c.To, string(auditchain.ProblemRegress))
		}
	}
}

// covered folds the entries c covers and checks c against them. It returns
// the chain recomputed after them, and whether every one of them was there
// to fold.
func (v *verification) covered(c master.AuditCheckpoint, cc auditchain.Checkpoint, from auditchain.State) (auditchain.Digest, bool, error) {
	ver := auditchain.NewVerifier(v.keys, v.o.Installation, v.o.Log, from)
	head := from.Head
	fed := c.From - 1
	var failed error
	next := c.From
	for next <= c.To && failed == nil {
		page, err := v.st.SealedAudit(v.ctx, next, int(min(int64(master.MaxAuditSealBatch), c.To-next+1)))
		if err != nil {
			return head, false, err
		}
		for _, s := range page {
			if s.Seq > c.To {
				break
			}
			e := chainEntry(s.Seq, s.Entry)
			if failed = ver.Entry(e); failed != nil {
				break
			}
			head, _ = auditchain.Next(head, e) // ver.Entry has just encoded e
			v.retention.note(s)
			fed = s.Seq
			v.r.EntriesChecked++
		}
		if len(page) == 0 || page[len(page)-1].Seq >= c.To {
			break
		}
		next = page[len(page)-1].Seq + 1
	}
	if failed == nil && fed < c.To {
		v.problem(EntryMissing, fed+1, string(auditchain.ProblemOrder))
		return head, false, nil
	}
	if failed != nil {
		v.chainProblem(failed)
		return head, false, nil
	}
	if failed = ver.Checkpoint(cc); failed != nil {
		v.chainProblem(failed)
	}
	return head, true, nil
}

// chainProblem records what a Verifier refused, by the contract's kinds.
func (v *verification) chainProblem(err error) {
	var ce *auditchain.Error
	if !errors.As(err, &ce) {
		v.problem(ChainBroken, 0, "the chain could not be checked")
		return
	}
	seq := int64(ce.Seq) //nolint:gosec // G115: a seq read from a store.
	kind := OutOfLine
	switch ce.Problem {
	case auditchain.ProblemOrder:
		kind = EntryMissing
	case auditchain.ProblemEncoding, auditchain.ProblemHead:
		kind = ChainBroken
	case auditchain.ProblemKey, auditchain.ProblemSignature:
		kind = BadSignature
	case auditchain.ProblemShort, auditchain.ProblemAnchor:
		kind = RolledBack
	}
	v.problem(kind, seq, string(ce.Problem))
}

// tail checks that no sealed entry lies past the last checkpoint: one put
// there by hand, which no checkpoint vouches for.
func (v *verification) tail(lastTo int64) error {
	extra, err := v.st.SealedAudit(v.ctx, lastTo+1, 1)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		v.problem(ChainBroken, extra[0].Seq, "a sealed entry lies past the last checkpoint")
	}
	return nil
}
