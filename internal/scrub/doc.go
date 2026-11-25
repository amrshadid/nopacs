// Package scrub runs every tenant's scrub jobs: it reads every stored object
// of a storage again and compares it with what the catalog, the scan's
// manifest and its sidecar say it holds, and makes a scrub of each storage
// by itself every scrub.interval.
//
// A storage is the archive's truth, and a byte can change on it without
// anyone writing it (a failing disk, an operator's mistake, a bucket edited
// by hand). A scrub finds such a change before a reader does, and marks the
// instance so every read of it fails as data loss before its first byte
// instead of streaming bytes that are not what was archived.
//
// # What a unit does
//
// A scrub job is a job of the tenant's leased queue (tenant.JobWork), its
// units of the kind tenant.ScrubWorkKind, each write fenced by the unit's
// attempt. The listing unit first looks for orphans outside every scan of
// its storages (walk.go), passing over and tallying a storage that does
// not answer, then lists the job's scans, one unit each. A scan's
// unit reads each instance's object whole, its length and SHA-256 (FIPS
// 180-4) against the catalog's (scan.go), then the scan's manifest against
// the checksum the catalog holds and its lines against the catalog's rows,
// its sidecar's lines likewise, and the objects under the scan's keys that
// nothing names (files.go).
//
// # Believing a difference
//
// A difference is read a second time before it is believed: an instance's
// object at once, a scan's files and the orphans SettleDelay later with the
// catalog read again, since a reception writes a scan's objects, manifest
// and sidecar before it commits their catalog rows. An instance a reception
// claims and has not committed is in flight, never an orphan or a
// difference. A believed difference is recorded as a finding
// (tenant.Scrubbing), before the progress write that moves the unit's
// cursor past its instance, so a crash between the two reads the instance
// again and records the same finding. An instance the catalog answers
// marked whose object reads whole has its findings cleared, and its mark
// removed, by that read, whatever the unit's cursor; a finding of a scan's
// files that held when the unit started and that the unit read good again
// is cleared at the scan's end. A scan's manifest put by a reception that
// has not committed its rows (every line the catalog lacks claimed, every
// row listed), and a new scan's objects put so, are in flight, not
// findings.
//
// What a correction not yet purged is doing (corrections.go) is expected,
// not a finding: a delete's hidden objects kept for its grace (not read
// for damage, since the catalog no longer serves them), a relabel's copies
// and its old tree, a replace's new revision with the manifest and
// metadata naming it before its publish and the revision it superseded
// until its purge. The tenant's database says which corrections are
// pending; a correction record on a storage with no row is tallied and
// explains only objects its scans' own manifests name.
//
// An object the storage answers as not online (blob.ErrNotOnline) is
// tallied, never a finding and never a mark: not readable now is not
// missing.
//
// # Replicas and the archive tier
//
// A scrub of the whole tenant reads each scan on its storage and on each
// replica of that storage that holds it in step (copies.go); a scrub of
// one storage reads that storage. What is wrong on a replica is recorded
// as the replica's damage, never as a mark on the instance, and an object
// no manifest names there is recorded as an orphan on the replica's name.
// A finding a switch moved to a replica's name with its place is cleared
// when a read of the replica shows it gone. A replica that fails a read is
// dropped for the unit and tallied; the scan is read on its other copies.
// An object of the archive tier held cold is checked by presence, never
// read or recalled.
//
// # Requests and coverage
//
// A read that finds an instance not as archived midway asks for it to be
// scrubbed (requests.go): the next unit of the tenant's scrubs reads it
// first. A request that fails is tried again behind the others and dropped
// after a few attempts; it never holds back the unit. When no scrub runs,
// the supervisor makes one for requests, at most one per storage per
// scrub.interval; later requests wait for the next scrub. Each scan read
// whole on a storage (every object read, or checked by presence on the
// archive tier) is recorded with its time, from which a storage's coverage
// is computed (verified.go).
//
// # What a scrub never does
//
// It never writes to a storage: it opens storages for reading and holds
// each as a Storage, which reads and lists only, so no write compiles
// here. It never deletes, moves, rewrites or repairs an object; repair is
// a replace from a file sent again.
//
// # Scheduled scrubs
//
// A storage is due a scrub one scrub.interval after its last scrub ended
// (one never scrubbed, at once), so a change of the interval moves the
// next scrub from that end and never starts one by itself.
//
// # Pace
//
// Every read holds one of the server's read slots, under the owner
// ReadOwner, so a scrub's reads count in the bound on reads of archived
// bytes, and is paced so a storage is read at most at scrub.rate bytes a
// second, whichever unit reads it.
package scrub
