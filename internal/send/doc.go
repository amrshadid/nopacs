// Package send carries out a tenant's send jobs: it passes archived
// instances to the peers of the tenant's senders, by C-STORE (PS3.4 Annex B)
// or by STOW-RS (PS3.18 10.5), and records in the tenant's database how far
// each peer answered.
//
// # Who works what
//
// State lives in each tenant's database, never here. A job's work is units
// of the tenant's leased queue, of the kind tenant.SendKind(sender), and
// this package only claims them. A Supervisor reads every tenant's senders
// every Interval, and at once when poked (Poke), and keeps for each enabled
// sender a pool of at most its max_associations workers, each claiming that
// sender's units one at a time. A worker holds one of the tenant's
// associations (sender.tenant-max-associations) and one of the
// installation's (sender.max-associations) for as long as it works a unit,
// so the bounds count associations open, never units queued.
//
// # What a unit does
//
// A listing unit lists the job's scans, a page per call, which queues one
// unit per scan. A scan unit sends the scan's instances in the catalog's
// order after its cursor. Each instance's bytes are read through package
// checkedread: one read slot (checkedread.Slots, the same value the API
// counts its reads in) for each instance open, and every byte checked
// against the length and SHA-256 the catalog holds, the last piece held back
// until both match. A DIMSE sender sends the data set after the file meta
// information, as stored, on a presentation context with exactly the
// syntax the file names (go-dicom's SCU.StoreStream); a stow sender sends
// the whole file as one part of a request (go-dicom's Client.StoreStream).
// Nothing is decoded or re-encoded. A mismatch aborts the instance before
// its last byte (an A-ABORT, or a request cut before its part ends), so the
// peer never holds it whole; it is refused as damaged_in_storage.
//
// The peer's answers are recorded (tenant.JobWork.RecordJobProgress) in
// batches of at most MaxBatch instances or BatchInterval, whichever comes
// first, each write fenced by the unit's attempt: a worker whose lease was
// taken over writes nothing and stops. A crash, a lost lease or a stop
// therefore sends again at most the instances answered since the last write,
// one batch per unit, and a cancel is seen at the next write. The lease is
// renewed every RenewInterval.
//
// # When a unit is given back
//
// A temporary failure of the unit (a connection that fails, an A7xx status,
// a timeout, a stow answer 408, 429 or 5xx) spends an attempt (FailWork),
// and the store waits before the next one. A cause that is not the unit's
// spends none (ReleaseJobUnit): its sender disabled or down, no read slot
// free, or the server stopping.
//
// # Stalls
//
// A send is cut when the peer stops taking bytes, never for being slow: no
// write of a C-STORE's fragments, or of a STOW-RS request's body, may wait
// longer than the sender's response timeout, each bound renewed by the
// progress before it, and the answer to a request must come whole within
// the response timeout once its body is sent. A store response is read up
// to MaxStowAnswer. A change of where a sender connects, or as whom, gives
// its units in flight back after the instance in flight, so nothing more
// reaches the old peer or goes with the old credential.
//
// # A sender's health
//
// Kept in memory only, as the contract's SenderHealth: unknown after the
// server starts, after the sender is added or its destination changes; up
// after a connection that succeeded; down after three connections in a row
// failed, with the fault of the last. While a sender is down none of its
// units is claimed, so its jobs use none of their attempts; one worker tries
// the peer again (C-ECHO, or a QIDO-RS search of the studies resource) after
// a wait that doubles from 10 seconds to an hour (tenant.RetryDelay). Each
// change is a sender_health event the supervisor appends to the tenant's
// feed.
//
// # Where a sender may connect
//
// Every connection resolves the sender's host once and connects only to a
// resolved address the installation's sender.destinations allows for that
// host and port, so a name that later resolves elsewhere cannot carry a
// connection into the server's own network; plain http needs
// sender.allow-plain-http. Proxies named by the environment are not used,
// and a redirect is never followed.
package send
