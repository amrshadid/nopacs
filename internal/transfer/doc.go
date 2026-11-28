// Package transfer runs a transfer job: the archived instances of a subject
// or a session copied into another tenant, and for a move deleted from the
// tenant they came from once every copy is confirmed.
//
// The job lives in the source tenant, in four steps its database saves, so a
// restart goes on from the last item recorded:
//
//  1. Listing reads the catalog into the job's items. The instance list
//     stays in the source: the target never stores a UID of the source.
//  2. Copying reads each item's archived bytes through the checked read
//     (checkedread: from a replica in step when the primary cannot serve
//     them, every byte counted and its SHA-256 compared with the catalog's)
//     and hands them to the target's receiver of type job
//     (receive.Pipeline.AcceptTransfer). The bytes go in through the
//     target's own receiver, so its routing, profile, naming and checks
//     apply, and the labels are the target's own.
//  3. Confirming finds each instance in the target by the UID the receiver
//     gives it (the keyed remapping of the source's UID when the receiver
//     de-identifies: receive.Pipeline.TransferRow, the mapping the check of
//     a landed file applies), compares the checksum the target received it
//     with to the source's archived SHA-256, or its keyed form when the
//     receiver de-identifies, and records where it came from in the
//     target (tenant.TransferProvenance).
//  4. Deleting, for a move only, runs once every item is confirmed and the
//     source is found unchanged: the source's ordinary preview and delete,
//     with the reason "moved to <target>". A hold or a retention rule that
//     stops the delete ends the job partial, and the copy stays.
//
// What leaves the source obeys tenant.SendRefusal, as every path that hands
// archived bytes to someone outside the tenant does: an instance archived as
// received is copied only into a receiver that de-identifies.
package transfer
