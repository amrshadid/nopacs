package auditseal

import (
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// A checkpoint's pruned flag is a column anyone with the database can set,
// and its entries are then not looked for: so a pruned checkpoint is taken
// as retention's work only when it is what retention leaves, which a hand
// with the database alone cannot fake.
//
//   - Retention prunes the oldest checkpoints first, so the pruned ones are
//     a prefix of the log: one after a checkpoint not pruned was marked by
//     hand.
//   - audit.retention is never below settings.MinAuditRetention, so a
//     pruned checkpoint is older than that, by its signed time.
//   - Each pruning writes an entry, made by the server (ServerUser,
//     RetentionMethod), with the range of seq it pruned
//     (master.DetailPrunedFromSeq, DetailPrunedToSeq); the pass seals it at
//     once, so it is in the chain, which nobody without the key can extend.
//     A pruned checkpoint must lie in the range of such an entry sealed
//     after it. A pruning's entry is itself pruned in its turn, once it is
//     older than the retention, by a pruning whose entry then covers it: so
//     the pruned checkpoints before the first that an entry still in the
//     log covers are those of prunings whose entries are gone, and are
//     accepted; from that one on, each must be covered.

// retentionSpan is the range of seq one pruning's entry names.
type retentionSpan struct{ from, to int64 }

// retentionCheck is what a verification gathers about pruned checkpoints.
type retentionCheck struct {
	// unprunedBefore reports that a checkpoint before the one being checked
	// (walked, or the one just before the range) was not pruned.
	unprunedBefore bool
	// before is the checkpoint just before the range when it is pruned, nil
	// otherwise: a call that starts in the middle of the pruned checkpoints
	// (the bounds of one verification split them) needs it to know whether
	// the covered ones began before it.
	before *master.AuditCheckpoint
	// pruned are the pruned checkpoints walked that are in a prefix and old
	// enough, which still need a pruning's entry to cover them.
	pruned []master.AuditCheckpoint
	// spans are the ranges of the prunings' entries seen in the chain.
	spans []retentionSpan
	// walkedTo is the To of the last checkpoint walked, and noted the seq of
	// the last entry seen.
	walkedTo, noted int64
}

// note records s when it is a pruning's entry made by the server whose
// range ends before it.
func (r *retentionCheck) note(s master.SealedAuditEntry) {
	if s.Seq > r.noted {
		r.noted = s.Seq
	}
	e := s.Entry
	if e.LocalUser != ServerUser || e.Method != RetentionMethod || e.Outcome != master.OutcomeDone {
		return
	}
	from, err := strconv.ParseInt(e.Details[master.DetailPrunedFromSeq], 10, 64)
	if err != nil {
		return
	}
	to, err := strconv.ParseInt(e.Details[master.DetailPrunedToSeq], 10, 64)
	if err != nil || from < 1 || to < from || s.Seq <= to {
		return
	}
	r.spans = append(r.spans, retentionSpan{from: from, to: to})
}

// covers reports whether a pruning's entry covers c.
func (r *retentionCheck) covers(c master.AuditCheckpoint) bool {
	for _, s := range r.spans {
		if s.from <= c.From && c.To <= s.to {
			return true
		}
	}
	return false
}

// coveredAfter reports whether a pruning's entry names a range that starts
// past seq.
func (r *retentionCheck) coveredAfter(seq int64) bool {
	for _, s := range r.spans {
		if s.from > seq {
			return true
		}
	}
	return false
}

// prunedByRetention checks the place and age of c, a pruned checkpoint
// whose signature checked, and keeps it to be matched with a pruning's
// entry when both hold.
func (v *verification) prunedByRetention(c master.AuditCheckpoint) {
	switch {
	case v.retention.unprunedBefore:
		v.problem(EntryMissing, c.To, "a checkpoint marked pruned follows one that was not; retention prunes the oldest first")
	case !c.Time.Before(v.o.Now.Add(-settings.MinAuditRetention)):
		v.problem(EntryMissing, c.To, "a checkpoint marked pruned is younger than the shortest retention")
	default:
		v.retention.pruned = append(v.retention.pruned, c)
	}
}

// prunedCovered matches the pruned checkpoints walked with the prunings'
// entries. Those entries lie anywhere after them up to the log's end, so
// the entries past what the walk read are read too (not recomputed: a
// verification of their range checks them).
func (v *verification) prunedCovered(last master.AuditCheckpoint) error {
	r := &v.retention
	if len(r.pruned) == 0 {
		return nil
	}
	for next := max(r.noted, r.walkedTo) + 1; next <= last.To; {
		page, err := v.st.SealedAudit(v.ctx, next, master.MaxAuditSealBatch)
		if err != nil {
			return err
		}
		for _, s := range page {
			if s.Seq > last.To {
				break
			}
			r.note(s)
		}
		if len(page) < master.MaxAuditSealBatch {
			break
		}
		next = page[len(page)-1].Seq + 1
	}
	first := len(r.pruned)
	for i, c := range r.pruned {
		if r.covers(c) {
			first = i
			break
		}
	}
	// The checkpoints a pruning's entry still in the log covers are the last
	// of the pruned ones, since those entries are pruned oldest first. A
	// range that holds only part of the pruned checkpoints (the bounds of
	// one call, or a range asked for) is judged as the whole log would be:
	// when the checkpoint just before it is covered, every pruned one in it
	// must be; when none in it is covered but a pruning's entry names seq
	// past it, its pruned ones come before the first covered one and are
	// accepted.
	switch {
	case r.before != nil && r.covers(*r.before):
		first = 0
	case first == len(r.pruned) && r.coveredAfter(r.pruned[len(r.pruned)-1].To):
		return nil
	}
	for i, c := range r.pruned {
		if first < len(r.pruned) && i < first {
			continue
		}
		if !r.covers(c) {
			v.problem(EntryMissing, c.To, "a checkpoint marked pruned is in the range of no pruning the server sealed")
		}
	}
	return nil
}
