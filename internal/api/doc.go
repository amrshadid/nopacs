// Package api is the handlers of the services of the API contract
// (api/proto/nopacs/v1): TenantService, AccessService, SettingsService,
// AuditService and MonitorService, and the ones that act inside one tenant,
// StorageService, ReceiverService, ReceptionService, QuarantineService,
// CatalogService, MetadataService and DataService; and of the HTTP routes
// beside them: a scan's download, and DICOMweb's QIDO-RS, WADO-RS and
// STOW-RS (PS3.18 10.4 to 10.6) under /tenants/<name>/dicom-web. Each method
// turns a request into calls on the package that owns the change, and their
// answer into a response. The proto comments are
// the specification; this package is what makes each sentence in them true.
//
// # What the server does first
//
// The server makes one access check per request (access.Check) before a
// handler runs, with the requirement and the audit facts Methods gives for
// the request, and hands the Decision to the handler through the context
// (WithDecision). So the server answers UNAUTHENTICATED and the role, in the
// order the contract states; a handler then validates the request (sizes,
// names, exactly-one-of rules, page size and token) and answers
// INVALID_ARGUMENT naming the field, and only then acts.
//
// A request whose scope or tenant name is malformed cannot name a tenant to
// check a role on, so Methods asks for the same role on the installation
// instead: a caller with no right there is refused as for any tenant it holds
// no grant on, and one with that right is told the request is invalid. So a
// caller who may not call a method learns nothing about what a valid request
// looks like, and INVALID_ARGUMENT can still be answered.
//
// # Changes and reads
//
// A change goes through the package that owns it, with the request's audit
// entry (access.Decision.Entry) written in the same transaction: keys and
// grants through package access, tenants through package registry, the
// installation's settings through the master, and a tenant's settings through
// the tenant's store, which only the registry hands out. A read answers, then
// hands its entry to the access package's audit writer, which never blocks or
// fails: access.Record for the installation's log, or access.RecordTo with
// the tenant's store when the scope asked for is a tenant (the contract: "one
// entry in the log of the scope asked for"). An entry the tenant's log does
// not take is written to the installation's log, marked
// access.DetailRerouted. A request that did not complete is recorded with
// outcome failed and its real cause, in the tenant's log when its store was
// already in hand and in the installation's log otherwise; a creation of a
// tenant is the exception: the registry writes its entry itself, with the
// change that settles the creation, and says whether it did; a creation it
// refused before writing anything is recorded by the handler. A method whose
// requirement is fixed learns what its request names only after the check,
// and adds it to its entries as access.Facts.
//
// # The key at the change
//
// A handler that changes something has the access package check the
// caller's key again first (access.Confirm), before it validates the request
// or reads what it names: a key that lapsed since the check is answered
// UNAUTHENTICATED or PERMISSION_DENIED whatever it asked, never "not found",
// "already exists" or a field that is wrong. Every change then carries the caller's key as its actor, with the role the
// method needs on its scope (access.Decision.Actor): into the master's change
// for keys, grants, tenants and the installation's settings, which checks it
// in its own transaction; and for a tenant's settings, as a tenant.Guard that
// asks the master (master.Store.CheckActor), which the tenant's store runs
// last in its transaction. So a key revoked, expired or stripped of its grant
// after the check makes no change. Such a change is answered as the check
// answers such a key, UNAUTHENTICATED or PERMISSION_DENIED with the same
// message, and recorded as refused in the installation's log, never in the
// tenant's (access.RefusedAtChange). The window a tenant's change leaves is
// package tenant's to state.
//
// # Inside a tenant
//
// A request into one tenant reaches the tenant's store through the registry
// and acts on it as tenant.Pipeline, the port every driver implements. A
// refusal of the port that depends on the state of what the request names
// is answered FAILED_PRECONDITION, by its kind, mapped in package codes. Where the
// installation lets data go (its directory roots, S3 endpoints, ports and
// bind addresses) is read from its settings: a storage or a receiver is
// checked against them under a read lock, and a change of one of those
// settings is checked against every tenant's storages and receivers under
// the write lock, so neither passes the other unseen. What only another part
// of the server knows is asked through an interface (Receivers, Incoming,
// Headers), with a stand-in that says plainly what it cannot do.
//
// # Bounds of the reads
//
// A read of archived bytes holds one of the caller's open reads for as long as
// it streams: a download, a ReadFiles stream, and a DICOMweb request that
// reads the storage. A DICOMweb request holds its read from its first read to
// its last byte (holdRead), so a request that does not stream cannot pass the
// bound by asking for what does not stream. A read past the bound is refused,
// never queued, with fault.Exhausted: the installation's read.per-caller (16
// by default, checkedread.MaxOpenReads) for one caller and read.per-server
// (256 by default, checkedread.MaxServerReads) for the server
// (checkedread.Slots). Both are read live: a change applies to the next
// read, and reads already open keep their place.
//
// Each check is made as the bytes go out (checkedread.CopyChecked), so a
// mismatch cuts the answer, which the client sees as a body short of its
// length, never as a whole body that is wrong (PS3.18 10.4.1.1):
//
//   - A whole instance, by a retrieve or a download, is compared with the
//     length and SHA-256 it was archived with.
//   - A frame (PS3.18 10.4.1.1.4) is compared with the SHA-256 its line of the
//     scan's metadata file records for it. A frame of a line that holds no
//     table is found from the file's header and sent unchecked, since no
//     checksum is recorded for it.
//   - A bulk data window (a BulkDataURI, or a download with an offset and a
//     length) is compared with its length only. No checksum of a part of a file
//     is recorded, so bytes changed inside a window are found only by a
//     retrieve of the whole instance.
//   - Metadata is answered from the scan's metadata file and, for what the file
//     does not hold, from a read of the instance's header. The header is
//     checked against the size the catalog holds, not against its checksum: a
//     stored file changed without a change of size is not found by the
//     metadata service.
//
// A search (PS3.18 10.6.3) has three bounds, and holds one scan's metadata
// file open at a time:
//
//   - It passes over at most 10,000 rows (webMaxPassed): rows an index serves
//     that a key the index does not hold turns down. Past it the answer is 413.
//   - It reads at most 256 MiB of metadata files (webMaxMetadataRead), counted
//     as they come from the storage; past it the answer is 413. Making the
//     results has an allowance of its own of the same size, and a read past
//     that allowance ends the answer.
//   - An answer of instances holds at most 64 MiB of results (webMaxAnswer).
//     The rows past it are left for the next page, with the Warning that more
//     match (PS3.18 8.3.4.4).
//
// The 413 messages name the keys the catalog matches by index (a study, a
// series, a subject's PatientID, a StudyDate range, a Modality): a search with
// them is the one that keeps inside the bounds.
//
// # Errors
//
// A handler answers a *connect.Error, made from the error's fault kind
// through package codes. The server passes such an error through unchanged.
// An internal fault's cause is logged with the request's id and is never part
// of the answer, whose message is "internal error".
//
// # What this package must never do
//
// It must never read a credential or call access.Check: a handler that finds
// no Decision in its context answers INTERNAL and logs it, and never decides
// by itself. It must never undo the access rule for a tenant that does not
// exist: a later lookup that misses answers PERMISSION_DENIED, as for no
// grant, to a caller who may not list every tenant. It must never put a
// tenant's database address in an answer, an audit entry, an error or a log
// line, nor log anything a request or a response carries. It seals one
// secret itself, an S3 storage's secret access key, with the installation
// key and bound to the tenant and the storage, before it reaches the
// tenant's store, and opens it only to test that storage; it never answers,
// logs or records it. A tenant's database address is sealed by the
// registry. It never deletes a file of the incoming area: that is the
// reception pipeline's (Incoming). It starts a goroutine only inside one
// request, to check the caller's key again while a read or a stream is open
// and to hash a read beside its copy, and waits for each to end before the
// handler returns.
package api
