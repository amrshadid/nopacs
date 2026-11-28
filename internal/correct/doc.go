// Package correct makes corrections to archived data on the storage side,
// in the order that keeps every reader, a rebuild and a restore right: a
// delete, a relabel or a replace, its purge, its undo, and what a start
// takes up after a stop.
//
// The tenant's database is the authority while it lives
// (tenant.Corrections); the correction records under meta/.corrections/
// (package record) are what a rebuild or a restore reads without it. So
// every correction goes:
//
//  1. its row committed, prepared, with its operation id, before anything
//     is written to a storage (PrepareCorrection);
//  2. its record put, prepared, on every storage its target touches;
//  3. for a relabel, every object copied to its key under the new labels,
//     read back and compared, and new manifests and metadata written; for a
//     replace, the new revision put at a key of its own, read back, and the
//     scan's manifest and metadata written to name it. No key that readers
//     use is ever written again;
//  4. the publishing transaction (CommitCorrection), which makes the change
//     visible to every read at once;
//  5. its records rewritten committed; and after the grace period
//  6. the purge: the row purging and its records rewritten; each object the
//     publish fixed as the correction's own (tenant.Corrections.PurgeTargets)
//     removed through the storage's Purge, recorded with its size, SHA-256
//     and what the storage said it did, a page at a time; an object a live
//     row resolves to, or a manifest or metadata whose bytes are no longer
//     the ones read before the publish, kept; a delete's metadata and
//     manifest last; what else names a delete's data cleared; then the row
//     purged and its records rewritten. Nothing a storage holds now decides
//     what is removed.
//
// While a delete or relabel has not ended, the labels and UIDs it holds are
// taken for arrivals: the reception pipeline holds a file that would land
// on them, before any put. A relabel an admin stops while it copies writes
// nothing after the stop is seen, and its copies are removed after its
// last write (or by Recover). A purge, and a replace, that rewrite a scan's
// manifest and metadata in place hold the reception pipeline's lock of that
// scan (Config.LockScan). The purge runs only while the server's clock is
// one it trusts (tenant.Corrections.CheckPurgeClock).
//
// A step that fails before the publish abandons the correction: what it
// wrote is removed (a replace's new revision, a relabel's copies) or
// written back (a replace's manifest and metadata), the row abandoned and
// the records rewritten so. Recover, at start, takes up what a stop left:
// a delete or replace left prepared is abandoned (its request was never
// answered), a relabel left prepared goes on with its copy, a purge left
// purging goes on, and records that lag their row are rewritten.
//
// What a purge removes is answered per storage and never more strongly
// than the storage said it (blob.Removal): removed, hidden behind older
// versions, retained by object lock, or unknown.
//
// A correction reaches every copy (copies.go): its record goes on each
// replica of its scans' storages too, owed through the tenant's database
// to a replica that does not take it and written later (WriteOwedRecords);
// a relabel copies to the new keys on each replica in step before the
// publish; the purge removes each object from the primary, then from each
// replica, records what each did, and waits, purging, while a replica does
// not answer, is owed the record, or a switch of its storages runs; before
// it ends, every key it removed is checked absent on every copy attached
// then, and one found again is removed. A replica detached with abandon_purges
// is no longer reached.
//
// A move (move.go) files a session under another subject, or merges a
// subject into another, through the relabel's copy: its scans' new paths
// are their old ones under the destination's label (ContinueRelabel copies
// and publishes both). It carries a preview's token, as a delete does, and
// is undone after its publish until its purge: the catalog names the old
// tree again and the copies are swept. Its identity rewrite, when asked, is
// an edit of each moved session made once it is published
// (RewriteIdentity), its values read from the destination's own archived
// files.
package correct
