// Package tenant states what one tenant's own database holds and what can be
// done with it: the tenant's overrides of the installation's settings and its
// audit log (Store), and what the reception pipeline keeps (Pipeline): the
// tenant's storages and receivers, its receptions and the files they
// received, its quarantine, and the leased units of work that carry a
// reception through its steps, with the catalog of what it archived, and
// its senders and the jobs that send archived data to them (Sending); and
// the tenant's event feed (Events): what happened, written with the change
// it reports and read back in commit order from a cursor.
//
// Each tenant has its own database, reached only through Store, an interface
// stated as operations, never as SQL. A database type is one driver package
// that implements Store and passes the contract suite in package tenanttest;
// nothing outside the driver knows which database it is, so each database can
// do its best behind the same operations. Package registry opens, upgrades
// and watches each tenant's store and is the only way the rest of the server
// gets one.
//
// # Rules every driver keeps
//
// They are the master port's rules (package master), and the values they
// apply to are the master's own types: a setting has the master's name and
// value bounds, and an audit entry is a master.AuditEntry listed with a
// master.AuditFilter and paged with a master.Page. One type for both logs
// means an entry reads the same wherever it was written, and a bound changed
// in one place changes in both.
//
// Every operation that changes the tenant's database takes the audit entry
// for that change and writes both or neither, so no change goes unrecorded.
// Ids and times are given by the caller. Lists are paged by an opaque cursor
// bound to its list, its filter and its tenant, never by an offset. A failure
// is an error from package fault with the kind a caller acts on: Invalid,
// NotFound, Unavailable when the database cannot be reached or the store is
// closed, Internal for the rest, a duplicate audit id included
// (master.ErrDuplicateAuditID). Store's documentation says each rule.
//
// # One instance per UID in a tenant
//
// A SOP Instance UID names at most one instance in a tenant, whatever its
// study, series or bytes: not one per scan. The catalog holds one row per
// UID, claimed by one reception at a time and then held; a file under a
// UID held in another study or series is UIDElsewhere, one under the same
// series with other bytes is DifferentFile, and neither is ever archived
// beside the held instance, which reception never replaces (CompareHeld,
// DecideClaim). Each driver also holds the rule in its schema, the UID
// being the key of the instances' table, so neither a race between two
// claims nor a fault in a driver's code can hold two.
//
// Per tenant, because a UID is unique by the standard's own rule (PS3.5
// 9.1), so two files under one UID in two series are a fault of a sender,
// or of a de-identification that kept a UID it should have replaced, which
// a person decides from the quarantine; and because an instance must be
// found by its UID alone: DICOMweb's retrieval of an instance (PS3.18 10.4)
// and a file's place in the catalog both name it so. Per scan would let
// one UID answer with two files.
//
// # Who makes a change
//
// A change in the master takes the acting key (master.Actor) and checks it
// inside its own transaction, so a key revoked after its request was checked
// makes no change there. A tenant's database cannot do the same: it holds no
// keys or grants (below), and the two databases share no transaction; they
// may not even be of one type. A change here takes a Guard instead, the
// check of its key that the caller builds on the master
// (master.Store.CheckActor), and the store runs it last in the change's
// transaction: after the change and its entry are written, so after any wait
// for the write lock or for the rows, and just before the commit. A Guard
// that refuses undoes both. The key the change is made by is in its audit
// entry (master.AuditEntry.KeyID), as it is for every entry.
//
// What that leaves is stated, not hidden: a revocation, an expiry or a grant
// removed or lowered that commits in the master after the Guard read it and
// before the tenant's commit is not seen by that one change. The window is
// one commit of the tenant's database; where a driver commits several
// waiting changes together (the SQLite driver does), it also holds the
// changes written after this one in the same transaction, a bounded few.
// Closing it would need the master to hold the key's row locked while
// another database commits, which on a SQLite master would stop every write
// of the installation for the length of a tenant's commit, or a commit
// protocol across two databases of different types; neither is worth a
// window of one commit for a change an admin was allowed to make a moment
// before.
//
// # What this package must never do
//
// It must never import a database driver, database/sql or package sqlkit: the
// port must stay implementable by a database with no SQL at all, and a lint
// rule (.golangci.yml, depguard "tenant") holds it to that. It holds no
// access rules and no secrets: who may enter a tenant lives only in the
// master, so revoking is one write in one place, a tenant's database restored
// from a backup cannot bring access back, and it can be copied or handed over
// without carrying them.
package tenant
