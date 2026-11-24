// Package registry holds one open store per tenant and is the only way the
// rest of the server gets a tenant's database. It opens each tenant's
// database from the address sealed in the master, upgrades it, watches it,
// and records the tenant's state in the master, so that one tenant's trouble
// stays its own.
//
// # Getting a store
//
// Store(name) is on the path of every request into a tenant. It reads an
// immutable map behind an atomic pointer and the tenant's current view
// behind another: no lock, no allocation, and no call to the master. A
// tenant whose view is not ready is refused with kind Unavailable; the
// others are served. A caller uses the store for one request and does not
// keep it, which leaves the registry free to replace the store behind a name
// (and, later, to close an idle one and reopen it on demand) without its
// callers changing.
//
// # States
//
// A tenant is upgrading while its database is first opened and migrated,
// ready when it answers at this binary's schema version, unreachable when it
// does not answer (it is retried each Interval, and returns to ready on its
// own), and failed when it cannot be served until a person acts: its address
// does not open with this key file, its database type is unknown, its
// database refuses its address or its file (a file others can read, a path
// that is not a regular file), its schema cannot be brought to this binary's
// version, or its driver panicked. A failed tenant is retried only when the
// registry starts again.
//
// State(name) is the current view, correct the moment a check completes, with
// its reason and the time it was reached: it is what the API answers. The
// master's row is the record of that view. A change of state, or of reason
// within a state, is written to it with an audit entry whose actor is the
// system, but a change between ready and unreachable, or of reason alone, is
// written at most once a minute per tenant (StateChangeInterval), so a
// flapping database cannot fill the master's audit log; the limit bounds the
// writes to the master only, never the view. The master records upgrading
// only for a tenant being created (its row is written that way first).
//
// # What a reason, an error and a log line hold
//
// Only the registry's own fixed phrases, a fault's kind, and the message of a
// fault from a driver (fault.Message), which the Driver interface requires to
// hold no address and no password. A driver's raw error text, its Error(),
// is never used: it may carry the address in any form (quoted, escaped, in a
// URL that does not parse), and no scrubber can be shown to find every one.
//
// # Opening, checking, and their limits
//
// Every open and upgrade runs under the tenant's own OpenTimeout, at most
// MaxOpening at once; a check of a tenant whose store is open is a ping
// under its own PingTimeout, which takes no opening slot, so a ready tenant
// that goes down is seen within one Interval and one PingTimeout however
// many dead tenants are being opened. A timer round starts each tenant's
// check and does not wait for it; a tenant whose check or open is still
// running is skipped. A driver that does not return by its deadline is left
// to finish in a goroutine the registry still owns: the tenant is
// unreachable meanwhile, no second open starts beside it, and a store it
// returns late is closed. A panic in a driver's Open, Ping or Migrate is
// recovered: that tenant is failed and the stack goes to the log.
//
// Each round also reconciles the registry with the master's list of
// tenants: an entry the master no longer has is unregistered and closed, a
// row the registry does not know is opened. So no failure between the two
// (a removal that committed and then reported an error, a creation whose
// row committed and whose call failed) keeps them apart for longer than one
// Interval.
//
// # Open files
//
// Every tenant's store stays open. A SQLite tenant can hold up to
// filesPerTenant files (its database and log for each of five connections,
// and its shared memory file). At start the registry compares the tenants
// times that, plus a margin, with the process's limit on open files, and
// logs one warning with both numbers when they would not fit; it does not
// refuse to start. Beyond the limit an open, or a new connection of an open
// store, fails as the system refuses another file: that tenant is
// unreachable and retried each Interval, the other tenants are served, and
// the server's own sockets may be refused too. Raise the limit (ulimit -n,
// LimitNOFILE) before that.
//
// # Creating and removing
//
// Create is whole or nothing, as the API contract says: it writes the
// tenant's row as upgrading with a system entry saying the creation started
// (carrying the request's id), opens and migrates its database, and records
// ready with the request's own entry, outcome done. When the database cannot
// be opened or upgraded, it removes the row it wrote, in one master
// transaction with the request's entry, outcome failed and the reason, and
// returns the failure; nothing stays registered. So a request's id leads to
// how it ended, and its entry is written once. Only a crash between the two
// leaves an upgrading row, which the next start or round opens like any
// other, recording that it finished the creation that request began; a
// repeated Create with the same id, name, type and address finishes what the
// first began. Remove unregisters the tenant and leaves its data where it is,
// so a mistaken removal loses no data, and says where: the database type and
// a location with no user and no password.
//
// Both take who asks (master.Actor), which the master checks in the
// transaction that writes the row or removes it, so a key that may no longer
// act changes nothing. A creation is checked when its row is written; the
// writes that settle it are the server's own, so a revocation while the
// database is being opened does not leave it half made. Create reports
// whether it wrote the request's entry, so its caller records the request
// exactly when Create did not.
//
// # What this package must never do
//
// It must never start a goroutine that nothing waits for: Close stops the
// timer, waits for every open in flight up to the deadline it is given, and
// closes every store; what it had to abandon it logs and reports. It must
// never import a database driver (a lint rule holds it): drivers are given
// to it, which is also how its tests run a database that can be made to
// fail, hang, panic and recover. It must never put a database address or
// password in a state, an error, a log line or an audit entry. It never reads
// the time of day but through Config.Now, and its interval and deadlines are
// given in Config, never package variables, so a test moves time instead of
// sleeping.
package registry
