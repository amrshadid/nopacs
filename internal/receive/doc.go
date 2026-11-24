// Package receive is the reception pipeline of one tenant: from a file handed
// in by a source to its reception done, with every file archived, held in
// quarantine with its reason, or counted as a duplicate, and nothing lost or
// archived twice whatever happens to the process.
//
// It joins ports and is not a driver: it is handed the tenant's database
// (tenant.Pipeline), a way to open the tenant's storages (blob.Store), the
// tenant's keyed hash for naming, the live feed's wake, a clock, a logger
// and the root of the incoming area, and it imports no database or storage
// driver. Each part is small and is tested alone with fakes of the ports.
//
// # The parts
//
//   - A source hands in one file at a time through Sink.Accept and tells
//     its sender the Outcome. DirSource is one; a DICOM listener is another,
//     added apart.
//   - The incoming area (package incoming) keeps each file whole and synced
//     before its sender is told it was received.
//   - The receiving side checks each file's header (package header: is it
//     DICOM, is its SOP class taken, are its UIDs well formed) and records
//     it as received or holds it, in batches a few times a second (batcher).
//   - Workers, a bounded pool, claim leased units of work and run each step
//     of a reception: quiet, identify, archive (with the scan's metadata
//     sidecar, package sidecar, and its manifest) and verify, which also
//     finishes. Each step's order of effects is written at its top, so that
//     running it twice, or after a crash at any line, changes nothing more.
//   - Recover, at start, reconciles the incoming area with the database.
//   - Release, Reject, Retry and Discard are the store's decisions with what
//     the pipeline does around them to the bytes and the work queue.
//
// # De-identification at reception
//
// A receiver may name a profile version (tenant.Receiver.Profile). Its
// files are then recorded at landing with their placing UIDs remapped by
// the tenant's UID key, the engine's own remapping, and a received
// checksum keyed by it (check, protect), so receptions, received files,
// claims, quarantine records and the catalog hold nothing an original file
// could be matched by. The archive step de-identifies each file it writes
// (prepare, deidentify): the engine runs over the original's data set, the
// result is written beside the original in the incoming area, the check
// after (Checker) reads it, and only then is it put, read back and
// committed with its profile version, its sidecar line read from the same
// bytes. The engine judges the pixels too (engine.ApplyImage): an image
// at risk of burned-in text no pixel rule matches is held as
// burned_in_risk; a rule's rectangles are filled, and a compressed image
// decoded, a frame at a time as the writer copies the pixels, so memory
// does not follow the image; JPEG segments are removed; an image whose
// decoded size passes deid.pixel-limit is held (deid_failed,
// pixels_too_large) and released once the setting is raised. A file that
// cannot be is held with deid_failed, residual_value, profile_refused or
// burned_in_risk, its original kept. Which way a file goes is decided by
// its row, never by the receiver's choice now: each received file and
// quarantine record says whether it was recorded de-identified
// (tenant.ReceivedFile.Deidentified). One recorded as received at a
// receiver that de-identifies now is held (recorded_as_received), and a
// release of such a record through a receiver that de-identifies lands it
// again, so it is recorded remapped and keyed and goes through the engine
// and the check; one recorded de-identified at a receiver that no longer
// names a profile is held (no_profile). A record made de-identified is
// released only through a receiver that de-identifies (the store's rule),
// and one that lands again at a receiver changed meanwhile to archive as
// received is held (no_profile), never archived identified. A placing UID
// under the standard's own root is held as bad_uid when it lands at such a
// receiver. The check after's copy of each file is taken from the server's
// bound on de-identification's memory (Config.Check's checker, deid.memory):
// a file that does not fit beside the others waits, and one larger than the
// whole bound is held (deid_failed, check_too_large) and released once the
// setting is raised. Every byte
// put is first compared with the row's checksum, or the copy's, through
// the handle it is put from, so wrong bytes never reach the storage. The
// de-identified copy is made again whenever it is needed and
// is never owed: a step run again writes the same bytes, and Recover
// removes one a crash left.
//
// # What makes a crash safe
//
// Every state the pipeline must not lose is in the tenant's database or in
// the name of a file in the incoming area; nothing is kept only in memory.
// Every write of a worker carries its unit's fence (tenant.WorkFence), so a
// worker whose lease was taken over changes nothing. Every step that holds a
// file gives the record an id made from the reception and the file, so a
// step run again writes it once. Every Put replaces an object whole, so a
// Put run again leaves the same object. A step queues the next unit in the
// transaction that completes its own (CompleteWork with next), so a
// reception past receiving always has its unit; a receiving one is given
// its quiet unit when it opens, and Start queues what a crash left missing.
//
// # One process per installation
//
// A scan's manifest is made from the catalog's list of the scan
// (tenant.ScanContents), whichever reception confirmed each instance, and
// the verify step checks that it lists every one; the sidecar, which holds
// headers the catalog does not, is merged from what the storage holds. Both
// are written, and the scan committed, under a lock this process holds per
// storage and scan, so receptions of one process never lose each other's
// entries. That holds while one process serves an installation, as the
// server's lock on its data directory ensures; with two, a manifest can
// miss an instance the other confirmed meanwhile (never the catalog), which
// verify finds and the scan's next manifest lists.
//
// # Routing
//
// A study is routed once, for its session. The identify step of the first
// reception of a new session evaluates the receiver's routes
// (tenant.Routing) over the file its naming rule reads first, as received
// (route): the first route whose conditions hold gives the storage, and may
// give a profile version and senders in place of the receiver's. What was
// chosen is kept with the identify step (tenant.ReceptionStep.Route) and,
// at its first commit, with the session (tenant.SessionLabels.Route); a
// later arrival, and a reception of a study another reception in flight
// was routed for (routedInFlight), go by it. So the archive step
// de-identifies with the chosen version (receiverFor) and the done step
// forwards to the chosen senders, whatever the routes say by then. The
// archive step holds the files of a reception whose session another made
// on another storage meanwhile (session_elsewhere) before any put. A study
// the routing does not route is held as unrouted, and a run again finishes
// that hold; a release naming a storage (tenant.Release.Storage) chooses
// the storage, the routes still the profile and senders; a release naming
// a profile version (tenant.Release.Profile) chooses the version for its
// reception's files, before the route's and the receiver's.
//
// # What this package must never do
//
// It must never import a database or storage driver, parse DICOM with its
// own code, read a setting itself (a bound that may change, such as the
// header limit, is handed in as a function to call), read the clock but through Clock, start a
// goroutine that no one waits for, or keep a global. It must never put a
// value read from a file's header in a log line, an error's text, an event
// or a metric: labels, ids, counts and fixed phrases only. It must never
// remove a file from the incoming area before the database has recorded why
// (a reception verified, a reception discarded, a record rejected), and
// never delete anything from a storage.
package receive
