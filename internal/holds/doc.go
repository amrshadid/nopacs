// Package holds writes and reads the records of a tenant's holds on its
// storages, so a hold outlives a lost database.
//
// A hold (tenant.Holds) stops every delete, purge and disposal of what it
// covers. Its row is in the tenant's database; its record,
// meta/.holds/<id>.json (package record, sealed with a MAC under a key
// derived from the installation key for hold records alone), is on every
// storage its
// target touches and on each of their replicas. A rebuild of the catalog
// from the storages alone, a restore, and every start read the records, and
// one found active with no row makes an active hold again
// (tenant.Holds.AdoptHold): keeping data longer is the lesser error.
//
// # Placing and releasing
//
// Writer.Place puts the new hold's record on every primary its target
// touches before its row commits (tenant.Holds.PlaceHold with Written): a
// primary that does not take it refuses the hold, and no row is made. A
// replica that does not take it is owed it. Writer.Release first reserves
// the release's count in the database (tenant.Holds.BeginHoldRelease), then
// rewrites the record released at that count on every primary, then
// commits the release at it. A primary that does not take the record, or a
// release that does not commit, whether refused or unanswered, ends the
// release: the hold is active at the count after, and its active records,
// written again there, outrank any released record written or kept from
// the release, so no copy of one can suppress the hold after a database is
// lost. A record written for a hold whose row was then
// refused for certain is rewritten released, so no later reading adopts a
// hold no one placed; one whose row's fate is unknown (the database did not
// answer) is left active, and is adopted if no row was made.
//
// # The records owed
//
// Writer.Run is the tenant's worker: once at its start it reads every
// storage's records and adopts the holds found active with no row
// (Writer.AdoptFound); then it writes, every Config.Interval, each record a
// storage is owed (tenant.Holds.HoldRecordsOwed): a replica that did not
// answer, a storage the target spread to, a hold a relabel moved or a
// release changed, a record sealed under a key version the installation no
// longer seals with. Each record is written as the hold stands, then marked
// written at the hold's count (tenant.Holds.MarkHoldRecord), under a lock
// of the hold its other writers share, so a hold changed in between is
// written again. The records owed are paged by a cursor, and a storage
// that does not answer is left out for the rest of the pass, so it holds
// back no other storage's records. A release left under way by a writer
// that stopped is ended once stale (StaleRelease).
//
// Every write asks the tenant's switch gate first (Config.Pause), as the
// reception pipeline's do, so no record lands on a place a switch is
// leaving.
//
// # What is refused
//
// A record whose MAC is missing or does not verify for the tenant, or that
// is not in the record's form, makes no hold: anyone who can write to a
// storage can put one there. It is not passed over silently: it is counted
// and named by its key (Found.Refused), which a rebuild tallies
// (tenant.TallyUnauthenticatedHold), a restore reports, and a start logs.
package holds
