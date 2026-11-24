package master

import (
	"context"
	"strconv"
	"time"
)

// The audit seal: what a store keeps so that its audit log can be shown not
// to have changed since it was written. A sealing pass, outside the store,
// reads the entries not yet sealed (UnsealedAudit), gives them their order
// and folds them into a hash chain, signs a checkpoint of the chain's head,
// and hands both back (SealAudit), which gives each entry its seq and keeps
// the checkpoint in one transaction. The master and every tenant keep these
// in tables of the same shape, so one interface states both; the encoding,
// the chain and the signature are the business of the code that seals, not
// of a store.
//
// An entry's seq is given at sealing, never at writing: entries written at
// once by many requests commit in no fixed order (on Postgres a transaction
// begun first may commit last), so an order given at writing would need a
// lock on every audited request. An entry that commits after a pass has
// read the unsealed entries is simply not in that pass, and the next one
// seals it.

// The bounds of the audit seal.
const (
	// AuditDigestSize is the length of a chain head and a checkpoint's
	// digest: a SHA-256.
	AuditDigestSize = 32
	// AuditSignatureSize is the length of a checkpoint's signature: an
	// Ed25519 signature (RFC 8032 5.1.6).
	AuditSignatureSize = 64
	// MaxAuditSealBatch bounds the entries one SealAudit seals, and so one
	// checkpoint covers: the same bound as an AppendAudit batch, so one seal
	// holds a database's write lock no longer than one append.
	MaxAuditSealBatch = MaxAuditBatch
	// MaxAuditPruneBatch bounds the checkpoints one PruneAudit prunes, so one
	// pruning removes at most MaxAuditPruneBatch * MaxAuditSealBatch entries
	// in its transaction.
	MaxAuditPruneBatch = 10
	// MaxAuditKeyVersion is the highest key version a checkpoint names: the
	// contract answers it as a 32-bit signed number.
	MaxAuditKeyVersion = 1<<31 - 1
)

// MasterAuditLog names the master's audit log in a checkpoint and in an
// anchor; a tenant's log is named by the tenant's id. A tenant's id is made
// by the server, 26 characters, so it never spells this six-letter name.
const MasterAuditLog = "master"

// The details PruneAudit adds to its audit entry, read inside the pruning.
const (
	// DetailPrunedCheckpoints is how many checkpoints the pruning reached.
	DetailPrunedCheckpoints = "pruned_checkpoints"
	// DetailPrunedEntries is how many entries it removed.
	DetailPrunedEntries = "pruned_entries"
	// DetailPrunedFromSeq and DetailPrunedToSeq are the first and last seq
	// of the checkpoints it reached.
	DetailPrunedFromSeq = "pruned_from_seq"
	// DetailPrunedToSeq is the last seq of the checkpoints it reached.
	DetailPrunedToSeq = "pruned_to_seq"
)

// AuditDigest is a SHA-256 value: a chain head or a checkpoint's digest.
type AuditDigest [AuditDigestSize]byte

// AuditCheckpoint is a signed record of a log's chain head after a range of
// its entries, as a store keeps it. The store keeps what it is given; the
// code that seals made the signature and the digest, and the code that
// verifies checks them.
type AuditCheckpoint struct {
	// Installation is the id of the installation whose key signed it.
	Installation string
	// Log is MasterAuditLog or the tenant's id: the log its signed text
	// names.
	Log string
	// From and To are the first and last seq it covers, both included.
	From, To int64
	// Head is the chain's value after entry To.
	Head AuditDigest
	// Previous is the digest of the checkpoint before it in the log, zero for
	// the log's first.
	Previous AuditDigest
	// Digest is the SHA-256 of its signed text: what the next checkpoint
	// names as Previous and what the other log keeps as its anchor.
	Digest AuditDigest
	// Time is when it was made, kept to the microsecond.
	Time time.Time
	// KeyVersion is the version of the installation key whose derived key
	// signed it, from 1 to MaxAuditKeyVersion.
	KeyVersion uint32
	// Signature is the Ed25519 signature of its signed text.
	Signature []byte
	// Pruned reports that retention removed the entries it covers; the
	// checkpoint itself is kept, so the chain is still checked across them.
	Pruned bool
}

// Entries returns how many entries it covers: To - From + 1, also after
// they were pruned.
func (c AuditCheckpoint) Entries() int64 { return c.To - c.From + 1 }

// Validate returns an Invalid error when c cannot be kept.
func (c AuditCheckpoint) Validate() error {
	if err := ValidateID("installation id", c.Installation); err != nil {
		return err
	}
	if err := ValidateAuditLog(c.Log); err != nil {
		return err
	}
	if c.From < 1 || c.To < c.From {
		return invalid("a checkpoint covers seq from to to, with 1 <= from <= to")
	}
	if err := ValidateTime("checkpoint time", c.Time); err != nil {
		return err
	}
	if c.KeyVersion < 1 || c.KeyVersion > MaxAuditKeyVersion {
		return invalid("a checkpoint's key version is 1 to " + strconv.Itoa(MaxAuditKeyVersion))
	}
	if len(c.Signature) != AuditSignatureSize {
		return invalid("a checkpoint's signature is " + strconv.Itoa(AuditSignatureSize) + " bytes")
	}
	if c.Pruned {
		return invalid("a checkpoint is made with its entries; only PruneAudit marks it pruned")
	}
	return nil
}

// ValidateAuditLog returns an Invalid error unless log names an audit log:
// MasterAuditLog, or a tenant's id (ValidateID).
func ValidateAuditLog(log string) error {
	if log == MasterAuditLog {
		return nil
	}
	return ValidateID("audit log (master or a tenant's id)", log)
}

// SealedAuditEntry is an audit entry with its place in its log's chain.
type SealedAuditEntry struct {
	// Seq is its place, from 1.
	Seq int64
	// Entry is the entry.
	Entry AuditEntry
}

// AuditSeal is what SealAudit keeps: the entries sealed, in the order of
// their seq, and the checkpoint that covers them.
type AuditSeal struct {
	// IDs are the ids of the entries sealed: IDs[i] is given seq
	// Checkpoint.From + i.
	IDs []string
	// Checkpoint covers exactly them.
	Checkpoint AuditCheckpoint
}

// Validate returns an Invalid error when s cannot be kept: its checkpoint is
// not valid, it seals no entry or more than MaxAuditSealBatch, its ids are
// not as many as the checkpoint covers, or one is not valid or given twice.
func (s AuditSeal) Validate() error {
	if err := s.Checkpoint.Validate(); err != nil {
		return err
	}
	if len(s.IDs) == 0 || len(s.IDs) > MaxAuditSealBatch {
		return invalid("a seal covers 1 to " + strconv.Itoa(MaxAuditSealBatch) + " entries")
	}
	if int64(len(s.IDs)) != s.Checkpoint.Entries() {
		return invalid("a seal names as many entries as its checkpoint covers")
	}
	seen := make(map[string]bool, len(s.IDs))
	for _, id := range s.IDs {
		if err := ValidateID("audit entry id", id); err != nil {
			return err
		}
		if seen[id] {
			return invalid("a seal names an entry twice")
		}
		seen[id] = true
	}
	return nil
}

// AuditAnchor is the latest checkpoint digest one log keeps of another: the
// master of each tenant's log, a tenant of the master's. A log rewritten
// whole, its checkpoints included, then disagrees with the anchor the other
// database keeps of it.
type AuditAnchor struct {
	// Log is the other log: MasterAuditLog or a tenant's id.
	Log string
	// To is the To of that log's checkpoint.
	To int64
	// Digest is that checkpoint's digest.
	Digest AuditDigest
}

// Validate returns an Invalid error when a cannot be kept.
func (a AuditAnchor) Validate() error {
	if err := ValidateAuditLog(a.Log); err != nil {
		return err
	}
	if a.To < 1 {
		return invalid("an anchor names a checkpoint's last seq, from 1")
	}
	return nil
}

// AuditPosition is where a reading of the unsealed entries goes on from: the
// time and id of the last entry read. The zero AuditPosition is the start.
type AuditPosition struct {
	// Time is the last entry's time.
	Time time.Time
	// ID is the last entry's id.
	ID string
}

// AuditPruned is what one PruneAudit removed.
type AuditPruned struct {
	// Checkpoints is how many checkpoints it marked pruned.
	Checkpoints int
	// Entries is how many entries it removed.
	Entries int64
	// More reports that it stopped at its limit with more checkpoints due.
	More bool
}

// AuditSealReads are the reads of an audit log's seal: what a verification
// and an export need, and nothing that changes the log.
type AuditSealReads interface {
	// CountUnsealedAudit returns how many entries have no seq yet.
	CountUnsealedAudit(ctx context.Context) (int64, error)
	// SealedAudit returns up to limit sealed entries, from seq fromSeq on, in
	// the order of their seq. A seq that was pruned, or never sealed, is
	// absent: the caller sees the gap. A limit outside 1 to
	// MaxAuditSealBatch, or a fromSeq below 1, is Invalid.
	SealedAudit(ctx context.Context, fromSeq int64, limit int) ([]SealedAuditEntry, error)
	// AuditCheckpoints returns up to limit checkpoints whose To is at least
	// fromSeq, oldest first, pruned ones included. A limit outside 1 to
	// MaxPageLimit, or a fromSeq below 1, is Invalid.
	AuditCheckpoints(ctx context.Context, fromSeq int64, limit int) ([]AuditCheckpoint, error)
	// RecentAuditCheckpoints returns up to limit checkpoints whose To is
	// below beforeSeq, newest first; a beforeSeq of 0 has no bound, so a
	// limit of 1 returns the log's latest. A limit outside 1 to
	// MaxPageLimit, or a negative beforeSeq, is Invalid.
	RecentAuditCheckpoints(ctx context.Context, beforeSeq int64, limit int) ([]AuditCheckpoint, error)
	// AuditAnchors returns every anchor this log keeps of another, in order
	// of the other log's name: one for each tenant in the master, one for
	// the master in a tenant.
	AuditAnchors(ctx context.Context) ([]AuditAnchor, error)
}

// AuditSealWrites are the changes of an audit log's seal, made only by the
// server's own sealing and retention, never by a request: they take no
// Actor or Guard.
type AuditSealWrites interface {
	// UnsealedAudit returns up to limit entries that have no seq, in order of
	// time then id, after the position given. A limit outside 1 to
	// MaxAuditSealBatch is Invalid.
	UnsealedAudit(ctx context.Context, after AuditPosition, limit int) ([]AuditEntry, error)
	// SealAudit keeps a seal, all or nothing, in one transaction: each entry
	// named is given its seq, which never changes afterward, and the
	// checkpoint is kept. It is Conflict, keeping nothing, unless the
	// checkpoint follows the log's latest (its From is the latest's To + 1,
	// or 1 for the first, and its Previous is the latest's Digest, or zero)
	// and every entry named exists and has no seq: so a pass that read the
	// log before another sealed it, as after a restart in the middle of one,
	// changes nothing and starts again.
	SealAudit(ctx context.Context, s AuditSeal) error
	// SetAuditAnchor keeps the anchor this log holds of another log,
	// replacing the one it held. The same anchor again changes nothing. One
	// whose To is below the one held, or equal to it with another digest, is
	// Conflict and changes nothing: the other log went back, which is what
	// the anchor is there to show.
	SetAuditAnchor(ctx context.Context, a AuditAnchor) error
	// PruneAudit removes the entries of whole checkpoints made before the
	// time given, oldest first, at most limit checkpoints (1 to
	// MaxAuditPruneBatch) a call, and marks those checkpoints pruned; every
	// checkpoint row stays. When it prunes anything it writes audit, with
	// the counts and range in its details (DetailPrunedCheckpoints and the
	// others), in the same transaction; when nothing is due it writes
	// nothing, the entry included. Entries without a seq are never removed.
	PruneAudit(ctx context.Context, before time.Time, limit int, audit AuditEntry) (AuditPruned, error)
}

// AuditSeals is the whole of an audit log's seal, which the master's store
// and every tenant's store implement (Store, tenant.Store).
type AuditSeals interface {
	AuditSealReads
	AuditSealWrites
}

// ValidateAuditSealLimit returns an Invalid error unless limit is 1 to max.
func ValidateAuditSealLimit(limit, max int) error {
	if limit < 1 || limit > max {
		return invalid("a limit is 1 to " + strconv.Itoa(max))
	}
	return nil
}

// WithPruned returns a copy of e whose details say what a pruning removed,
// read inside it.
func (e AuditEntry) WithPruned(checkpoints int, entries, fromSeq, toSeq int64) AuditEntry {
	return e.WithDetails(
		DetailPrunedCheckpoints, strconv.Itoa(checkpoints),
		DetailPrunedEntries, strconv.FormatInt(entries, 10),
		DetailPrunedFromSeq, strconv.FormatInt(fromSeq, 10),
		DetailPrunedToSeq, strconv.FormatInt(toSeq, 10),
	)
}

// ValidatePruned returns an Invalid error when e would be past its bounds
// once WithPruned has added its details, so a driver refuses it before it
// changes anything.
func (e AuditEntry) ValidatePruned() error {
	return e.WithPruned(MaxAuditPruneBatch, 1<<62, 1<<62, 1<<62).Validate()
}
