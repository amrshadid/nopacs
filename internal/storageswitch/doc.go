// Package storageswitch runs the switch that makes a replica the primary:
// the last step of moving a tenant off a storage, after the replica was
// copied, scrubbed and compared from its own bytes.
//
// # Preview and token
//
// Validate answers whether a switch could start now and what stands in
// its way: the blockers the tenant's database knows (tenant.SwitchBlockers)
// and the two of verification it reads from the jobs, a clean scrub of the
// replica since its backfill ended and a rebuild compare from the replica
// alone that found no difference since then, which skip_verification with
// its reason lifts. It answers a confirm token: the time it was issued and
// an HMAC-SHA-256 (RFC 2104), under the server's confirm key, of the
// tenant's id, both storages' places, the primary's switch generation and
// that time. Begin takes the token within an hour of its issue, made again
// from what the database holds now, so a place or a generation changed
// since does not match, nor does a token another tenant's preview gave; a
// token is used once, and sent again within its life answers the switch it
// began, through the database, which checks the caller's guard and records
// its audit entry as for the first request.
//
// # The writes gated
//
// Gate.Enter is asked by the writes that land new bytes on a primary's
// place in the course of the server's work: the reception pipeline's
// archive step (every reception, release and import hand-off archives
// through it), and the passes a start runs that rewrite older sidecars,
// once for each rewrite, each opening the storage by name once admitted.
// The other writes are held otherwise. A correction's purge and a
// relabel's copies are not begun while a switch runs, since the tenant's
// database refuses a correction then; a purge already running when a
// switch begins is not refused by the database, so its caller asks the
// gate before each object it removes (the server's wiring does), and a
// purge that does not would remove objects from the place the swap is
// leaving after the drain has counted them. A new writer of a primary's
// place asks the gate, or it may land bytes on the old place after the
// swap.
//
// The owner record step is not serialized with the advance of an owner
// record a backup's reclaim makes: both take the record of a place one
// generation further, and two at once on the same place leave one of them
// with a generation the database does not hold. A caller does not run a
// reclaim of a tenant's storages while a switch of that tenant runs
// (tenant.Switching.LastSwitch answers whether one is accepted).
//
// # The run
//
// Once accepted, a switch runs on a goroutine of the Switcher, not of the
// request, to its end or its refusal:
//
//  1. Pause: every write that would land bytes on the primary's place is
//     held at its one gate (Gate.Enter): a writer that has entered finishes,
//     one that comes later waits, and is refused with UNAVAILABLE and the
//     reason STORAGE_SWITCHING once its wait runs out. Reads go on.
//  2. The writers in flight leave, then the replicator drains, woken,
//     until every scan of the primary is in step on the replica and no
//     finding holds on it, within the drain timeout.
//  3. The owner record of the replica's place is taken one generation
//     further, under the replica's name, the database first.
//  4. Each S3 place's secret is sealed again for the name it comes to
//     (place.SwapSecrets), and one transaction swaps the places
//     (tenant.Switching.SwapStorages), which checks every rule again.
//  5. Writes resume, on the new place under the same name, and the switch
//     is finished.
//
// A failure before the swap refuses the switch with its fixed phrase and
// resumes every write on the old place; after the swap commits it stands.
// At start (Recover), a switch left accepted by a server that stopped is
// refused ("server restarted") and one left swapped is finished. The old
// place stays a replica: nothing on it is deleted.
package storageswitch
