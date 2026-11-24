// Package access decides who is calling and what they may do, records each
// request in the audit log, and manages API keys and their grants.
//
// It knows nothing about gRPC, HTTP or the command line. A server hands it a
// Credential (a presented key, or the local operator the socket reported), a
// Requirement (only to be authenticated, a role on the installation, a role
// on a tenant named by the request, or the right to read a tenant's
// registration) and the facts of the request it needs for the audit entry,
// and gets back a Decision or an error from package fault.
//
// # The one way in
//
// Access.Check is the only function that makes a Decision, and so a
// Principal. The server calls it once for every request, on the one path
// every request takes, and hands the Decision to the handler. Principal and
// Decision have no exported fields and no exported constructor, so no other
// package can build or alter one that carries rights; their zero values
// carry none. LocalOperator, the credential that is admin everywhere, may be
// called only by internal/server, and a test holds every other package to
// that.
//
// A Decision is for one request. The operations of this package that act
// for a caller (CreateKey, GetKey, ListKeys, RevokeKey, SetGrant,
// ListGrants, RemoveGrant, WhoAmI) take that Decision, not a credential, so
// a request is checked once. Each refuses the zero Decision as Denied, and
// each but WhoAmI refuses as Denied, reading nothing, a Decision whose
// principal the check did not find admin on the installation, so one made
// for a lesser requirement by a wrong entry in the server's table of methods
// opens nothing.
//
// The checks run in the order the API contract states (access.proto, "The
// order of checks"): the credential first, answered Unauthenticated when it
// is no good for any reason; then the role, answered Denied, or NotFound for
// a tenant that does not exist when the caller may list every tenant, so an
// operator who mistypes a name is told so while a caller with no right to
// know learns nothing. Validating the request itself comes after, in the
// handler, or in this package for the operations it implements.
//
// # A key after its check
//
// A key revoked, expired, or stripped of its grant after its request was
// checked makes no change, and a Decision kept past its request opens
// nothing. Every change made for a key carries the key, with the role the
// change needs on its scope, as the change's actor (Decision.Actor,
// master.Actor), and the master checks it inside the change's transaction,
// so the check and the change are one step, with no moment between them. When
// the key may no longer act, nothing is written, and the caller is answered
// exactly as Check answers such a key, Unauthenticated or Denied with the
// same fixed message, and recorded as refused in the installation's log,
// with the real cause and "since the check" (RefusedAtChange). This package
// makes no read of its own for it: the one implementation of the check of a
// key after Check is the master's, in each driver.
//
// Before anything else, a change (and the handler that makes it, through
// Confirm) asks the master for the same check on its own
// (master.Store.CheckActor), so the contract's order of checks holds for a
// key that lapsed after Check: it is refused before it is told whether what
// it names exists, or whether its request is valid, and the request is
// recorded as refused. A read (GetKey, ListKeys, ListGrants, WhoAmI) has no
// transaction to check in, so that first check is its only one. A change in a tenant's own database cannot
// check the key in its transaction either, since the tenant's database holds
// no keys: the API hands the tenant's store a Guard that asks the master, and
// the store runs it last in its transaction, just before its commit (package
// tenant). The window each leaves is stated under Known limits.
//
// # Refusals
//
// Every way a credential can be no good (none, malformed, unknown id, wrong
// secret, revoked, expired) is one kind and one fixed message, and an unknown
// id costs the same comparison of secrets as a wrong secret. A good key with
// no sufficient grant and a good key naming a tenant that does not exist are
// one kind and one fixed message too, and both make the same two calls to the
// store in the same order: Key, then RolesFor by the name as given, which
// reads the roles and the tenant in one statement that runs the same lookups
// in both cases. Neither the answer nor the calls tell a caller which tenants
// exist. Their times were measured close but not equal (the change that
// introduced RolesFor reports the numbers): a tenant that exists is read
// from its row, one that does not is not. The audit entry keeps the real
// cause in its reason; the caller never sees it.
//
// # Where a key may be used from
//
// A key may list the networks it may be used from and the TLS client
// certificate it must come with (master.KeyRules), and a tenant may list the
// networks every key used on it must come from (TenantNetworks, read through
// the server, since the value lives in the tenant's database). The check
// judges a key's own rules once its secret and state have passed, and a
// tenant's networks once the role check has allowed the caller, so a caller
// with no grant learns nothing of them; the installation's networks are the
// server's to judge, before any key is read. A tenant's registration read
// by a caller its role on the installation allows (GetTenant) is a read of
// the master and is judged by no tenant's networks, so a tenant whose
// database stopped is still seen as unreachable; a tenant's networks that
// cannot be read refuse the request as the tenant's database not answering. A refusal by any of them is
// answered as a key that is no good, recorded with the rule's name
// (RuleReason), and raised as an Event. A request on the local socket
// (Request.Local) is judged by none of them.
//
// A key may also be rotated (RotateKey): a successor with its grants and
// rules, while the rotated key keeps working for an overlap, all in one
// transaction of the master. A key may rotate itself.
//
// # Where an entry goes
//
// A refusal is always written to the installation's log (the master), with
// the tenant's name when the request named one: a caller who was refused must
// not cause a write in a tenant's database, and keys and grants are the
// installation's. An allowed request that concerns a tenant is written to
// that tenant's log, one that concerns the installation to the master's: a
// change writes Decision.Entry in its own transaction in that log; a read
// hands its entry to Record (the master) or RecordTo (a tenant's log, a
// Sink). When a tenant's log does not answer, its entries are written to the
// master's log instead, with the tenant's name and the detail
// DetailRerouted. A handler that learns what a request was about only after
// the check (an object, a tenant's name, details) passes them as Facts.
//
// # What is audited, and when
//
// A change is never unaudited. Its entry is written in the same transaction
// as the change: if the entry cannot be written, the change is not made.
//
// A refusal or a read is audited unless the server is overwhelmed, a log does
// not answer for longer than the retries, or the server is killed within the
// flush interval; each case is counted or bounded. Its entry costs no synced
// commit on the request's path. It is handed to the audit writer, one
// goroutine of this package with two bounded queues, one for refusals and
// one for allowed requests, so a flood of bad keys never pushes out the
// entries of reads. The writer appends in batches per log, when a batch is
// full (Config.AuditBatch) or when Config.AuditInterval passes. In detail:
//
//   - Overwhelmed: when a queue is full, the entry is dropped and counted
//     (AuditStats.DroppedRefusals, DroppedRecords). The request is neither
//     failed nor delayed.
//   - A log that does not answer: a batch the store fails as Unavailable is
//     tried again Config.AuditRetries times, waiting Config.AuditBackoff,
//     doubled each time; an append that runs out of Config.AuditTimeout is
//     not tried again. A tenant's batch then goes to the master's log; the
//     master's is dropped and counted (DroppedWrites). So a store that hangs
//     holds the writer one timeout per batch, and one that is down costs one
//     call and the retries per batch, never one call per entry.
//   - One bad entry: a batch refused for one of its entries (an entry not
//     valid, an id already taken) is appended again one entry at a time,
//     once each, so the bad entry is Failed and only it is lost.
//   - Killed: what is queued and the batches not yet written are lost: at
//     most the two queues (Config.RefusalQueue and RecordQueue entries) and
//     one interval's batches. That is the window a crash can cost.
//   - Closed: Close writes what the writer holds and what is queued until its
//     deadline, and returns by its deadline even if a store ignores its
//     context; what is left is dropped and counted. Every entry handed in is
//     counted exactly once, as written, failed or dropped, and so is every
//     last-use mark.
//
// Drops are reported in the master's log by an entry of method
// "access.AuditWriter" with the number dropped since the last report: at most
// one a minute, and a last one when Close runs, so every drop before Close is
// reported. The time of the last report moves only once a report is written.
//
// The reason for the split: on SQLite a synced commit costs milliseconds,
// so one per refusal would cap the whole server at a few hundred requests a
// second, and a flood of bad keys would use up the master's one writer for
// everyone. Changes keep the strict rule, since a change nobody can see in
// the log is the failure that matters.
//
// Every text of an entry that a caller controls is escaped (control
// characters, line breaks, escape, the bidirectional controls) and bounded
// by one function, so an entry can neither forge a line of a log nor be
// refused by the master for its size.
//
// # Known limits
//
// These are known and accepted, each by a decision, not oversights:
//
//   - The two Denied cases make the same calls but do not take exactly the
//     same time: on SQLite a tenant that does not exist is answered about
//     0.3 microseconds sooner (about 4.5%) than one without a grant, because
//     a tenant that exists is read from its row. A covering index on the
//     tenants' name and id would close it, and is not worth a migration now.
//   - A change in a tenant's own database checks its key by a read of the
//     master made inside the tenant's transaction, just before the tenant's
//     commit, not in a transaction the two databases share, which they do
//     not have. A revocation that commits between that read and the
//     tenant's commit, one commit of the tenant's database, is not seen by
//     that one change. A change in the master has no such window.
//   - A read checks its key once, before it reads, so a revocation that
//     commits while the read runs is not seen by that one read.
//   - Under a load the audit writer cannot keep up with, the entries of
//     refusals and reads are dropped, counted and reported, not written.
//
// # What it must never do
//
// It must never cache a key or a grant: every check reads both, so a revoked
// key is refused on its very next request. It must never put a key's secret
// in an audit entry, an error, a log line or anything it returns, except the
// one secret.NewAPIKey that CreateKey returns, which prints only its id. It
// must never read the wall clock for an entry's time or an expiry, nor make
// ids itself: the clock and the id source are given to New. It must never
// let recording a key's last use or an entry fail or delay a request: both
// go to bounded workers that drop when they fall behind, and a key marked
// used within the minute is not marked again. It must never leave a
// goroutine running after Close returns, unless a store ignores its context,
// and then it counts what that goroutine held as dropped.
package access
