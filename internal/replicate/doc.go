// Package replicate is the replicator: the worker that copies each scan of a
// primary storage to the primary's replicas, behind the reception.
//
// # The queue
//
// The tenant's database keeps, for each replica, a row per scan of its
// primary: pending, copying, in step or damaged (tenant.Replication). The
// transaction that publishes a change of a scan makes it pending again, so
// the queue is those rows, and a crash between a commit and its copy
// forgets nothing. The worker claims a batch of rows, each under a lease
// fenced by its attempt, copies each claimed scan, and ends it with the
// claim's fence and the manifest digest it copied. The database records it
// in step only when no change of the scan was published since the claim
// and that digest is still the catalog's.
//
// # One scan's copy
//
//  1. The primary's manifest is read whole (it is bounded,
//     manifest.MaxBytes) and its SHA-256 compared with the claim's digest.
//     A manifest that is not the claim's is a change in flight or a damaged
//     primary: nothing is copied, and the scan is tried again.
//  2. Every object the manifest names is copied, an instance a delete hid
//     among them: a replica is a faithful copy of its storage, and a hidden
//     instance's object stays on the primary until its purge. A manifest
//     line of an instance the catalog holds must agree with the catalog's
//     row of it, or the scan changed under the copy.
//  3. Each object the replica does not hold verified, every object the
//     claim names damaged, and every object when the claim says to copy
//     all, is streamed from the primary to the replica through
//     blob.PutConfirmed against the size and SHA-256 (FIPS 180-4) its
//     manifest lines give: a byte changed on the way fails the put before
//     the replica keeps anything. When the replica did not itself confirm
//     the checksum (Written.Confirmed), the object is read back and hashed
//     before it counts as copied; a read back that differs is recorded as
//     the replica's damage, so the next claim copies it again. An object
//     already on the replica counts as held only when the replica's own
//     manifest, held verified by its row, lists it with the same lines, or
//     when the replica's SHA-256 of it, or its bytes read whole, are the
//     manifest's; its size alone never counts.
//  4. The scan's sidecar of headers, then its manifest, are put last, so a
//     replica's scan is whole once its manifest is there and a rebuild from
//     the replica alone never sees a manifest naming an object not yet
//     copied. After the sidecar's put the primary's manifest and sidecar
//     are read again: a manifest no longer the claim's, or a sidecar no
//     longer the bytes put, is a reception in flight, and the copy fails
//     as a change.
//
// A timer renews the lease every third of it while a scan is claimed,
// whatever the copy is doing, and each put and read back is bounded by a
// time shorter than the lease. A renewal that fails cancels the scan's
// copy: a lease lost (tenant.ErrLeaseLost) or a database that did not
// answer stops it at once, no further object, sidecar or manifest is put,
// and nothing is written to the row, which is another worker's or left to
// its lease. A stop of the context abandons the scans in flight the same
// way; their leases run out and the next claim takes them.
//
// Every instance is streamed, never held whole; a bound on the copies that
// run at once on each replica bounds the memory and the load on the
// replica. Every time the worker records comes from the clock function it
// is given.
package replicate
