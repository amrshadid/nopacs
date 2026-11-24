package auditseal

import (
	"context"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// heldAnchors returns the anchors st keeps of other logs, by the other
// log's name, and whether they could be read; a pass that cannot read them
// keeps no anchor in st this time, since it could not check what it would
// replace.
func (s *Sealer) heldAnchors(ctx context.Context, st master.AuditSealReads, tenantName string) (map[string]master.AuditAnchor, bool) {
	as, err := st.AuditAnchors(ctx)
	if err != nil {
		s.log.Warn("auditseal: the anchors a log keeps could not be read; they are kept next pass", "tenant", tenantName, "reason", fault.Message(err))
		return nil, false
	}
	out := make(map[string]master.AuditAnchor, len(as))
	for _, a := range as {
		out[a.Log] = a
	}
	return out, true
}

// anchor keeps c, the latest checkpoint of the log in anchored, in holder,
// whose current anchors of other logs are held, after checking that c
// descends from the anchor holder keeps of that log now (Descends). One
// that does not is the log taken back past the checkpoint the anchor names
// and sealed again from there, as a restore, or someone with its database
// alone, would leave it: the anchor is kept as it is, so Verify of that log
// goes on reporting it rolled back, and the pass logs an error naming the
// log and the seq the anchor holds, and counts it. held is updated with
// the anchor kept.
//
// Accepting a log restored on purpose is not the pass's to decide: it is
// an operator's step, audited, that replaces the anchor after checking the
// restore, and comes with the restore of a database. Descends is the check
// it starts from.
func (s *Sealer) anchor(ctx context.Context, holder master.AuditSeals, held map[string]master.AuditAnchor,
	anchored master.AuditSealReads, tenantName string, c master.AuditCheckpoint) {
	log := s.logger(c.Log, tenantName)
	if h, ok := held[c.Log]; ok {
		err := Descends(ctx, anchored, h, c)
		switch {
		case fault.KindOf(err) == fault.Conflict:
			s.stats.rolledBack.Add(1)
			log.Error("auditseal: a log no longer descends from the checkpoint the other log keeps of it; the anchor is kept as it was",
				"anchored_log", c.Log, "held_to_seq", h.To)
			return
		case err != nil:
			s.stats.failed.Add(1)
			log.Warn("auditseal: the anchor could not be checked; it is tried again next pass", "reason", fault.Message(err))
			return
		}
	}
	a := master.AuditAnchor{Log: c.Log, To: c.To, Digest: c.Digest}
	err := holder.SetAuditAnchor(ctx, a)
	switch {
	case err == nil:
		held[c.Log] = a
	case fault.KindOf(err) == fault.Conflict:
		s.stats.rolledBack.Add(1)
		log.Error("auditseal: a log went back from the checkpoint the other log keeps of it",
			"anchored_log", c.Log, "to_seq", c.To, "reason", fault.Message(err))
	default:
		s.stats.failed.Add(1)
		log.Warn("auditseal: the anchor could not be kept; it is tried again next pass", "reason", fault.Message(err))
	}
}

// Descends reports whether next, a log's checkpoint, descends from held,
// the anchor another log keeps of it: the log still holds the checkpoint
// held names, its digest computed from its signed text (not read from the
// row) is held's, and every checkpoint after it up to next follows the one
// before (its range starts after the one before's, its previous digest is
// the one before's computed digest), ending at next with next's digest. A
// nil error means an anchor may move from held to next.
//
// It is Conflict when the log does not descend: it was taken back past the
// held checkpoint and grew again, or a checkpoint on the way was changed.
// A store's failure is returned as it is.
func Descends(ctx context.Context, st master.AuditSealReads, held master.AuditAnchor, next master.AuditCheckpoint) error {
	if next.To < held.To {
		return discontinuity(held, "the log ends before the checkpoint the other log keeps of it")
	}
	page, err := st.AuditCheckpoints(ctx, held.To, master.MaxPageLimit)
	if err != nil {
		return err
	}
	if len(page) == 0 || page[0].To != held.To {
		return discontinuity(held, "the log no longer holds the checkpoint the other log keeps of it")
	}
	prev := page[0]
	prevDigest, ok := computedDigest(prev)
	if !ok || prevDigest != held.Digest {
		return discontinuity(held, "the log's checkpoint is not the one the other log keeps of it")
	}
	i := 1
	for prev.To < next.To {
		if i == len(page) {
			if len(page) < master.MaxPageLimit {
				return discontinuity(held, "the log's checkpoints stop before the checkpoint to anchor")
			}
			if page, err = st.AuditCheckpoints(ctx, prev.To+1, master.MaxPageLimit); err != nil {
				return err
			}
			if len(page) == 0 {
				return discontinuity(held, "the log's checkpoints stop before the checkpoint to anchor")
			}
			i = 0
		}
		c := page[i]
		i++
		if c.From != prev.To+1 || c.Previous != prevDigest {
			return discontinuity(held, "a checkpoint does not follow the one before it")
		}
		if prevDigest, ok = computedDigest(c); !ok {
			return discontinuity(held, "a checkpoint cannot be digested")
		}
		prev = c
	}
	if prev.To != next.To || prevDigest != next.Digest {
		return discontinuity(held, "the checkpoint to anchor is not the one the log holds")
	}
	return nil
}

// discontinuity is Descends' Conflict, naming the seq the anchor holds.
func discontinuity(held master.AuditAnchor, why string) error {
	return fault.New(fault.Conflict, "auditseal: "+why+" (anchor at seq "+strconv.FormatInt(held.To, 10)+")")
}

// computedDigest returns the digest of c's signed text, computed, and
// whether it could be.
func computedDigest(c master.AuditCheckpoint) (master.AuditDigest, bool) {
	d, err := chainCheckpoint(c).Digest()
	if err != nil {
		return master.AuditDigest{}, false
	}
	return master.AuditDigest(d), true
}
