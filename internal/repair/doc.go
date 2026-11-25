// Package repair is the repair job (kind tenant.JobRepair): it puts back on
// a primary storage the objects scrubs found missing or damaged there, each
// read from a replica in step for the object's scan.
//
// # One object
//
//  1. The finding is checked (tenant.CheckRepair): it holds, is on the
//     storage, and is of a kind a copy mends; the storage is a primary and
//     the source a replica of it. The bytes expected are the catalog's now:
//     the instance's row (its key, size and SHA-256, FIPS 180-4) or the
//     scan's manifest digest, never the finding's copy of them, so a key
//     the catalog no longer names is left alone, and so is a key that is
//     not the layout's own key of its instance (an object other files may
//     share, of which the catalog names no checksum whole).
//  2. A replica in step for the scan at the catalog's manifest digest
//     (tenant.Replication.InStepReplicas) is read whole and its bytes
//     hashed. A replica whose object is missing or not the catalog's bytes
//     is recorded as that replica's damage (the replicator copies it again
//     from the primary) and the next one is tried. A replica not in step is
//     never read.
//  3. The primary's object is read as it is now. One that reads whole with
//     the catalog's bytes needs no put: its finding is ended as below.
//  4. The repair is recorded (tenant.Repairs.RecordRepair) with the damaged
//     object's size and SHA-256 as found, before anything is written, so
//     what was there is known whatever happens next. The record checks the
//     catalog again in its transaction and holds the scan still until the
//     repair ends: while a correction of the scan is under way, or a switch
//     of the storage, it is refused and the object skipped; while it
//     holds, a correction's steps and a reception's commit to the scan are
//     refused as retryable (tenant.ErrScanRepairing). A put cannot share a
//     transaction with the catalog, so this is what keeps a delete's object
//     from being put back after its purge, and an old manifest from being
//     put over a new one. The hold runs out by itself a grace
//     (tenant.RepairHoldGrace) after the due the unit's lease had at the
//     record; a record refused because the job was canceled meanwhile
//     ends the walk.
//  5. The replica's object is streamed to the same key on the primary
//     through blob.PutConfirmed against the catalog's size and SHA-256:
//     exactly the bytes the catalog names, never a new key, and nothing
//     kept when a byte changed on the way. The put enters the storage's
//     write gate (Config.Pause) and releases it after, so a switch never
//     swaps the storage's place under it. It is cut Config.PutMargin before
//     the lease's due, so a worker that lost the database has ended its put
//     before its hold runs out. Unless the primary confirmed the checksum
//     itself, the object is read back.
//  6. The repair is ended (tenant.Repairs.CompleteRepair): the finding is
//     cleared by the repair job, the instance's mark lifted, the job's
//     counts and tallies moved, an audit entry written by the server, in
//     one transaction, and the hold ends. A finding a switch moved to
//     another storage since step 4 is not cleared; nor one whose bytes the
//     catalog no longer names (the hold ran out meanwhile), whose key is
//     then recorded as an orphan for a scrub to read again. Every other way
//     out of the object after step 4 (a put refused or failed, a read-back
//     that differs, an end refused) lets the hold go at once
//     (tenant.Repairs.ReleaseRepair).
//
// A replica's own damage is not this job's: the replicator copies it again
// from the primary, unasked.
//
// # The job's unit
//
// A job is a row of the jobs table (kind tenant.JobRepair, made by
// tenant.Jobs.CreateJob from Job.NewJob), whose one unit of work, of the
// kind tenant.RepairWorkKind, is queued with it, as every job's first unit
// is; a Run claims the next one due, so a give-up ends the job failed and a
// cancel ends its queued unit as for every other job. The unit is leased
// and fenced by its attempt: every write of a repair carries the fence, so
// a worker that lost its lease writes nothing more, and a lease that could
// not be renewed for its whole length stops the run. A job canceled while
// it runs stops at its next object's record. A stop leaves the unit leased;
// once its lease ends another Run takes it over. A temporary failure (a
// storage or the database not answering) records a failed attempt, with
// the first reason and the error's kind (never its text), and the unit is
// due again later. A job naming findings reads those alone; one of all, or
// automatic, walks the findings that hold on the storage page by page, so
// its memory does not grow with their number. A run taken over walks them
// again: a finding repaired already no longer holds, the repair of a
// finding by a job has one id (tenant.RepairID), and the job counts each
// finding once, so nothing is recorded, cleared or counted twice.
//
// # Manual and automatic
//
// Mode is the tenant's setting replica.repair. A job made by a person names
// the findings it mends (a caller that has instances looks up the findings
// their marks name, tenant.Scrubbing.DamagedInstances), or all of the
// storage's; a job the server makes by itself (ModeAuto) mends every finding
// that marks an instance (an object missing or damaged), the ones every read
// of the instance fails on. When a job is made is its caller's decision.
package repair
