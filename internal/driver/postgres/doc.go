// Package postgres is the Postgres database type: it implements master.Store
// and tenant.Store, with the reception pipeline's operations (tenant.Pipeline),
// on a Postgres database through github.com/jackc/pgx/v5 and its pool, pure
// Go, so the binary stays static (CGO_ENABLED=0) on every target it ships
// for.
//
// # What it needs from the server
//
// The address is a postgres:// (or postgresql://) URL. The driver creates
// and upgrades its tables in the schema the connection's search_path starts
// with, which is "public" unless the URL says otherwise
// (?search_path=nopacs). It never creates a database, a schema or a role,
// and works with a login that owns that schema and nothing else. It is
// tested on Postgres 16 and 18; pgx supports 14 and later.
//
// The driver is closed by default. pgx's default, sslmode=prefer, encrypts
// when the server offers it and falls back to plain text when it does not, so
// an address whose host is not loopback and not a Unix socket path is refused
// at open, as Invalid, unless a person wrote its sslmode down as verify-full,
// verify-ca, require or disable (the error says to add sslmode=verify-full
// with sslrootcert, or sslmode=disable if the network is trusted). Loopback
// and socket addresses keep pgx's default.
//
// # How it reaches the database
//
// One pgxpool. Every changing operation and its audit entry are one
// transaction (read committed). A statement is prepared on a connection the
// first time it is used there and kept in pgx's statement cache
// (QueryExecModeCacheStatement, the default, 512 statements per connection),
// so the reads of every request are one round trip each after the first. A
// proxy that pools by transaction, such as PgBouncer before its prepared
// statement support, cannot keep a prepared statement from one call to the
// next; for it, give the URL default_query_exec_mode=exec, which pgx reads.
//
// # One database for each tenant
//
// A tenant has the address of a database of its own. When two tenants are
// pointed at one database and schema they would share one set of settings and
// one audit log, each seeing and changing the other's, so the driver refuses
// it: the tenant's schema records the id of the tenant it was made for
// (table tenant_identity, written by the first Migrate and compared by every
// open and every Migrate), and a tenant whose id differs is refused as
// Invalid. That also catches an address pasted for the wrong tenant. Two
// tenants may share a database if each has a schema of its own
// (?search_path=lab); a master and a tenant may not share a schema, and their
// migrations differ, so that is refused as well.
//
// A tenant's pool is small, four connections by default with idle ones let go
// after a minute (DefaultTenantConnections, TenantIdleTime), because a server
// holds many tenants and each connection is a process on the database server;
// pool_max_conns and pool_max_conn_idle_time in the address change them.
//
// # The migration lock
//
// Migrate takes a session advisory lock, named for the schema, on one
// dedicated connection and holds it until the migration ends, so a second
// process that starts migrating waits for the first and then finds the files
// applied. The lock is released on every path; when its release cannot be
// confirmed the connection is closed, which ends the session and with it the
// lock. The pool therefore needs two connections.
//
// # Row locks
//
// A change of the master locks the rows it depends on, under read committed,
// in one order: keys' rows first, several in the order of their ids
// (lockKeys), then tenants' rows, then the row it changes. The one exception
// is a setting's change, which checks its actor last, after it locked the
// setting's row; no change locks a key's row and then a setting's, so that
// order closes no cycle. The check of a change's actor holds its key's row
// for share, and the tenant's row when its role is on a tenant, for key
// share or for the stronger lock the change itself will take on that row, so
// no transaction asks for a stronger lock on a row it already holds; a
// revocation, a change of its grants or a removal of the tenant waits for the
// change or is waited for. The check takes its locks first and reads the key
// and its grant after, in a statement of its own: under read committed a
// statement sees what was committed when it began, and a locking statement
// that waited re-reads only the row it locked, and only if that row changed,
// which a change of grants (it locks the key's row and changes another
// table) does not do. Taken in one order, these
// locks cannot wait on each other in a cycle, which Postgres would break by
// failing one of the changes as a deadlock.
//
// # The reception pipeline's locks
//
// A tenant's pipeline (tenant.Pipeline, pipeline_*.go) runs at read
// committed, where each statement sees what was committed when it began and
// an UPDATE or a locking SELECT that waited for a row reads it again as the
// other transaction left it and tests its condition again (Postgres manual,
// "Transaction Isolation", 13.2.1). So every condition a change depends on is
// either in the statement that makes the change, with the rows it changed
// counted, or on a row the change locked before it read it; an insert that
// may meet a row another transaction is making is ON CONFLICT DO NOTHING,
// and what it met is read by a later statement, since a read inside the same
// statement would not see a row committed while it waited.
//
// Rows are locked in one order, so no two changes wait for each other in a
// cycle:
//
//  1. the tenant's configuration lock (an advisory lock for the
//     transaction), taken only by changes of storages, receivers and
//     senders, and by the creation of a profile version;
//  2. a unit of work's row, the fence of a worker's write;
//  3. a quarantine record's row;
//  4. receptions' rows, several in order of id;
//  5. a sender's row (FOR KEY SHARE by what makes a job for it, FOR
//     UPDATE by its removal), an import job's units' rows (in order of job,
//     then unit), then the import job's row, then a job's row, then a
//     job's scan's;
//  6. a storage's row;
//  7. the catalog's subject, session and scan, in that order, as their
//     inserts meet them;
//  8. instance rows, several in order of SOP Instance UID;
//  9. received files and associations, new rows of a reception held;
//  10. the running totals, last.
//
// A lookup table's counter row is locked (FOR NO KEY UPDATE) only by
// AssignLookupCodes, which locks nothing else, the tables of a batch in
// order of name. A commit reads the profile versions its instances name
// without a lock: a version is never changed or removed.
//
// CommitScan gives an import's files their outcome after its instance rows,
// so it takes rows of 5 after rows of 7 and 8: no change that holds a row of
// 5 ever locks a catalog or instance row, so that order closes no cycle. A
// cancel of an import job locks its queued units of work with SKIP LOCKED,
// then their import units in order of unit id, the import job and the job,
// in 5's order, and judges the job's state again once it holds them. Every
// change of an import job's state holds its import job's row first, so a
// file handed in reads its job's state after locking that row.
//
// A cancel of a job (CancelJob, RemoveSender) holds the job's row and then
// ends the job's queued units with SKIP LOCKED: it never waits for a
// unit's row, which a worker locks before its job's, so the two cannot wait
// for each other; a unit it skipped is told of the cancel at its worker's
// next write. A unit held as failed ends its job's scan before the
// reception step's event of the same unit, which a unit of a job never has.
//
// Each lock is the weakest that is enough, and a row is locked the first time
// a change touches it with the strongest lock that change will take on it
// (FOR NO KEY UPDATE where it updates the row later), so no change asks for a
// stronger lock on a row it holds. The statements that lock, by operation:
//
//	operation              statement and lock                                 why
//	---------------------  -------------------------------------------------  ------------------------------------------
//	Add/Set/Remove a       pg_advisory_xact_lock, per schema                  the rules across rows (names, overlaps,
//	storage or receiver                                                       ports) see no other change in flight
//	SetStorage             SELECT storage FOR NO KEY UPDATE, then reads       waits for commits and identifies holding
//	                       of what holds it                                   it FOR SHARE; later ones see the change
//	RemoveStorage          SELECT storage FOR UPDATE (its DELETE's lock)      as SetStorage
//	SetReceiver            SELECT receiver FOR NO KEY UPDATE                  the row read is the row changed
//	OpenReception          INSERT reception ON CONFLICT DO NOTHING, then a    receptions_open and receptions_arrival
//	                       read of the open one and of the next arrival       make one reception and unique arrivals
//	StepReception          UPDATE work (fence), SELECT reception FOR NO KEY   the state and last file compared cannot
//	                       UPDATE, identify: SELECT storage FOR SHARE,        change until the commit; a storage is not
//	                       UPDATE reception WHERE state = from, an end:       removed or moved under an identify; claims
//	                       DELETE its claims                                  go with the step that ends the reception
//	AddReceivedFiles       SELECT receptions WHERE receiving FOR NO KEY       a step and a batch are ordered; files and
//	                       UPDATE, in order of id; INSERT files and           counts are written on the row held
//	                       associations ON CONFLICT DO NOTHING (sets);
//	                       UPDATE reception, adding
//	ClaimInstances         UPDATE work (fence), SELECT reception FOR SHARE    claims of one reception run side by side
//	                       (identified, not ended); INSERT claims ON          and wait for a step that ends it; two
//	                       CONFLICT DO NOTHING, sorted by UID, one set;       batches wait for each other in UID order;
//	                       the UIDs that met a row read again                 the claim that won is read, never a 23505
//	CommitScan             UPDATE work (fence), SELECT reception FOR NO KEY   ends and claims of the reception wait for
//	                       UPDATE, SELECT storage FOR SHARE, subject,         it; the catalog's rows are found or made
//	                       session, scan INSERT ON CONFLICT DO NOTHING        once whoever commits at once; counts are
//	                       then read; UPDATE the reception's claims WHERE     added on rows held; the totals row is
//	                       claimed by it (one set); UPDATE scan, reception    locked last, for the shortest time
//	                       and catalog totals, adding
//	HoldFiles              SELECT receptions FOR NO KEY UPDATE, in order of   a file counted received here and a batch
//	                       id; INSERT records ON CONFLICT (id) DO NOTHING;    of it are ordered
//	                       UPDATE receptions and totals, adding
//	Release/Reject         SELECT record FOR NO KEY UPDATE; a release: the    a decision made meanwhile is seen; a file
//	                       reception it joins, SELECT ... WHERE receiving     never joins a reception closed meanwhile
//	                       FOR NO KEY UPDATE; totals last
//	Retry/Discard          UPDATE reception WHERE failed and not retried or   two of them at once make one
//	                       discarded (the mark is the compare)
//	EnqueueWork            INSERT ON CONFLICT DO NOTHING, then a read under   a key queues its work once
//	                       the unique key
//	ClaimWork              SELECT the earliest due FOR NO KEY UPDATE SKIP     two claims take different units and
//	                       LOCKED, LIMIT 1; UPDATE it; a unit given up        neither waits; a round that gives up a
//	                       fails its reception (SELECT FOR NO KEY UPDATE)     unit commits alone, so no transaction
//	                       in a transaction of its own                        holds two receptions
//	Renew/Complete/Fail    UPDATE work WHERE id, attempt, leased (the         a takeover skips the unit while it is
//	                       fence), then the change                            held, and a write after it changes nothing
//	Add/Set/Remove a       pg_advisory_xact_lock; SELECT sender FOR NO KEY    a name and a receiver's forward are checked
//	sender                 UPDATE, or FOR UPDATE to remove it                 with no other change of them in flight; a
//	                                                                          forward being made waits for a removal
//	CreateJob, RetryJob,   SELECT sender FOR KEY SHARE; RetryJob: SELECT      the sender is not removed under the job;
//	a forward              job FOR NO KEY UPDATE; INSERT job ON CONFLICT      a forward is made once per reception and
//	                       (unique_key) DO NOTHING                            sender
//	CancelJob              SELECT job FOR NO KEY UPDATE; SELECT its queued    a unit's write and a cancel are ordered
//	                       units FOR UPDATE SKIP LOCKED                       by the job's row, never by the unit's
//	A unit's progress,     UPDATE work (fence); SELECT job FOR NO KEY         two units of one job add to its counts
//	its end, a listing     UPDATE; SELECT its scan FOR NO KEY UPDATE OF js    one after the other; the cursor is compared
//
// Batches are one statement each, whatever their size: received files,
// claims, confirmations and quarantine records go as one array per column
// (unnest), so the largest statement, a batch of quarantine records, takes
// twenty parameters, far below Postgres's limit of 65535.
//
// A deadlock (SQLSTATE 40P01) or a serialization failure (40001) that
// Postgres reports anyway, and a unique violation (23505) no statement met
// with ON CONFLICT, end the transaction with nothing committed; the change
// is then run again in a new transaction, a few times with a short wait
// that grows, and never reaches the caller as an unknown error: after the
// last attempt it is Unavailable, to be tried again. The tests show the
// order makes the retry unneeded where the port promises it (claims in
// opposite orders, commits at once): no deadlock happens there at all.
//
// The clock is the caller's: every time written is a "now" the caller gave,
// never the database's own.
//
// # Ordering
//
// Text that is compared or ordered is COLLATE "C" in the schema, so names
// order byte by byte whatever collation the database was created with.
//
// # What this package must never do
//
// It must never be imported by anything but package driver and its own
// tests: nothing else may know that a database is Postgres (a lint rule
// holds it). It never puts the address, the password or the host in an
// error, a log line or an audit entry: pgx's own errors and connection
// strings can carry them, so every error from pgx is scrubbed of them before
// it joins the chain.
package postgres
