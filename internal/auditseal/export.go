package auditseal

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// EncodingVersion is the version of the entry encoding and the signed text
// an export carries (audit_seal.proto, AuditExportHeader.encoding_version).
const EncodingVersion = 1

// ExportOptions names the log and range to export.
type ExportOptions struct {
	// Installation is the installation's id.
	Installation string
	// Log is the log's name in its checkpoints: master.MasterAuditLog or the
	// tenant's id.
	Log string
	// Keys are the public keys that check its checkpoints (PublicKeys).
	Keys []PublicKey
	// From is the first seq, 0 or 1 for the first; it is moved back to the
	// From of the checkpoint that covers it.
	From int64
	// To is the last seq, 0 for the last sealed; the export ends with the
	// checkpoint that covers it.
	To int64
	// OtherAnchors are the anchors other logs keep of this one, carried in
	// the header beside the log's own.
	OtherAnchors []master.AuditAnchor
}

// ExportHeader opens an export: what checks the rest.
type ExportHeader struct {
	// Installation is the installation's id.
	Installation string
	// Log is the log's name in its checkpoints.
	Log string
	// Keys are the public keys.
	Keys []PublicKey
	// ChainBefore is the chain's value before the first entry exported.
	ChainBefore master.AuditDigest
	// From is the first seq exported, after moving back to a checkpoint's
	// start; 0 when nothing is exported.
	From int64
	// Anchors are the anchors this log keeps of the others, then those the
	// others keep of it.
	Anchors []master.AuditAnchor
}

// ExportEntry is one sealed entry of an export.
type ExportEntry struct {
	// Seq is its place in the chain.
	Seq int64
	// Entry is the entry.
	Entry master.AuditEntry
	// Encoded is its encoded form, the bytes folded into the chain.
	Encoded []byte
	// Chain is the chain's value after it.
	Chain master.AuditDigest
}

// ExportItem is one message of an export: exactly one field is set.
type ExportItem struct {
	// Header is the first message.
	Header *ExportHeader
	// Entry is a sealed entry.
	Entry *ExportEntry
	// Checkpoint is a checkpoint, after the last entry it covers.
	Checkpoint *master.AuditCheckpoint
}

// Export writes a range of a log to emit: the header, then each checkpoint's
// entries in seq order followed by the checkpoint, the pruned checkpoints
// with no entries. Entries not sealed yet are not exported. It returns how
// many entries it wrote. An error from emit stops it and is returned as it
// is.
//
// The chain value after each entry is folded here from the entries read, so
// an entry changed in the database shows in the export as a chain that does
// not reach its checkpoint's head, which the auditor's own fold also finds.
func Export(ctx context.Context, st master.AuditSealReads, o ExportOptions, emit func(ExportItem) error) (int64, error) {
	if o.From < 0 || o.To < 0 || (o.To > 0 && o.To < max(o.From, 1)) {
		return 0, fault.New(fault.Invalid, "a range of seq is from a first seq to a last one not below it")
	}
	anchors, err := st.AuditAnchors(ctx)
	if err != nil {
		return 0, err
	}
	h := ExportHeader{Installation: o.Installation, Log: o.Log, Keys: o.Keys, Anchors: append(anchors, o.OtherAnchors...)}
	first, err := st.AuditCheckpoints(ctx, max(o.From, 1), 1)
	if err != nil {
		return 0, err
	}
	if len(first) > 0 {
		h.From = first[0].From
		if h.From > 1 {
			prev, err := st.RecentAuditCheckpoints(ctx, h.From, 1)
			if err != nil {
				return 0, err
			}
			if len(prev) > 0 {
				h.ChainBefore = prev[0].Head
			}
		}
	}
	if err := emit(ExportItem{Header: &h}); err != nil {
		return 0, err
	}
	if len(first) == 0 {
		return 0, nil
	}
	chain := auditchain.Digest(h.ChainBefore)
	var count int64
	seq := h.From
	for {
		page, err := st.AuditCheckpoints(ctx, seq, master.MaxPageLimit)
		if err != nil {
			return count, err
		}
		for _, c := range page {
			if o.To > 0 && c.From > o.To {
				return count, nil
			}
			if c.Pruned {
				chain = auditchain.Digest(c.Head)
			} else {
				n, next, err := exportEntries(ctx, st, c, chain, emit)
				count += n
				if err != nil {
					return count, err
				}
				chain = next
			}
			cp := c
			if err := emit(ExportItem{Checkpoint: &cp}); err != nil {
				return count, err
			}
			if o.To > 0 && c.To >= o.To {
				return count, nil
			}
		}
		if len(page) < master.MaxPageLimit {
			return count, nil
		}
		seq = page[len(page)-1].To + 1
	}
}

// exportEntries emits the entries c covers, folding the chain from chain,
// and returns how many it emitted and the chain after them.
func exportEntries(ctx context.Context, st master.AuditSealReads, c master.AuditCheckpoint, chain auditchain.Digest, emit func(ExportItem) error) (int64, auditchain.Digest, error) {
	var n int64
	next := c.From
	for next <= c.To {
		page, err := st.SealedAudit(ctx, next, int(min(int64(master.MaxAuditSealBatch), c.To-next+1)))
		if err != nil {
			return n, chain, err
		}
		for _, s := range page {
			if s.Seq > c.To {
				return n, chain, nil
			}
			ce := chainEntry(s.Seq, s.Entry)
			enc, err := auditchain.Encode(ce)
			if err != nil {
				return n, chain, fault.Wrap(fault.Internal, err, "auditseal: a sealed entry cannot be encoded")
			}
			if chain, err = auditchain.Next(chain, ce); err != nil {
				return n, chain, fault.Wrap(fault.Internal, err, "auditseal: a sealed entry cannot be encoded")
			}
			if err := emit(ExportItem{Entry: &ExportEntry{Seq: s.Seq, Entry: s.Entry, Encoded: enc, Chain: master.AuditDigest(chain)}}); err != nil {
				return n, chain, err
			}
			n++
		}
		if len(page) == 0 {
			return n, chain, nil
		}
		next = page[len(page)-1].Seq + 1
	}
	return n, chain, nil
}
