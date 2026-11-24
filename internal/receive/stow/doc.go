// Package stow is the STOW-RS receiver of a tenant (PS3.18 10.5): the
// instances a client stores over HTTP into the tenant's DICOMweb service,
// each part of the request written to the incoming area as it arrives and
// handed to the reception pipeline through its one entry for a source
// (receive.Sink), as a DICOM listener hands a C-STORE's file.
//
// An Endpoint is one stow receiver while it runs; the supervisor starts
// and stops it as it does a DICOM listener, and a Directory is where the
// HTTP route finds the endpoint that takes a key's requests. The route
// itself, its check of the caller's key and role, and its audit entry are
// the API's (internal/api); this package reads the body and writes the
// answer.
//
// # What it guarantees
//
//   - Closed by default. A request is served only for a key the receiver
//     lists (tenant.STOWReceiver.Accepts), or one rotated from a key it
//     lists, which the route finds in the master
//     (master.FindRotatedFrom); the Directory finds no endpoint for any
//     other key.
//   - A part listed as stored in the answer is whole and synced in the
//     incoming area before the answer is written: the answer is written
//     only after every part's receive.Sink.Accept has returned, and Accept
//     returns Received only once the file is durable. A crash after the
//     answer loses nothing it listed; the pipeline's start finds each such
//     file and goes on with it.
//   - The bytes kept are the bytes sent. A part is read as a raw stream
//     (multipart.Reader.NextRawPart, no transfer decoding), its first bytes
//     held only while the header is read from them (header.Identify), and
//     then written ahead of the rest, so nothing is parsed into a data set
//     and written again.
//   - Memory per request is bounded whatever a part's size or how many
//     parts it holds: one buffer for the bytes read to identify a part (at
//     most maxPrefix), used again for every part; the multipart reader's
//     own buffer; a part's header, read to MaxPartHeader; the pipeline's
//     copy buffer; and a line of the answer for each part it lists, at
//     most MaxParts. The request is bounded by the receiver's
//     MaxRequestBytes, each part by its MaxFileBytes; a request of more
//     than MaxParts parts is refused at the first past it (413), one with
//     a part's header past MaxPartHeader at that part (400).
//   - A client that stops sending is cut: each read of the body must
//     arrive within the read stall bound (Config.ReadStall), as a client
//     that stops reading a long answer is cut by the server's write stall
//     bound.
//   - Stop ends a request at once once its drain is past, however the
//     client goes on sending: from the cut, every read of the body fails
//     without moving the read deadline, and the request is answered 503
//     with Retry-After.
//   - A part is taken as whole only when the multipart body closes it
//     with a boundary: a body that ends inside a part is answered 400 and
//     nothing of that part is kept.
//   - Each part is decided on its own (PS3.18 10.5.3): stored, or refused
//     with its Failure Reason (PS3.18 Table I.2-2); one refused part does
//     not stop the others. A part whose identity cannot be read is refused
//     and, having no UIDs to list, only counted.
//
// What the answer cannot know: the pipeline checks a stored file later,
// after the answer, exactly as a C-STORE answered success may be
// quarantined later. A different file under a SOP Instance UID the tenant
// already holds is reported stored in the answer and quarantined after
// (different_file, or uid_elsewhere under another study or series): the
// answer asks no catalog. A file whose placing UIDs read whole differ from
// those read from its first bytes, by which the part was checked against
// the request's study (a placing attribute repeated, or given after a later
// tag), is reported stored and quarantined after as uid_mismatch: each
// part's arrival carries the UIDs read from its prefix
// (receive.Arrival.StudyUID), and the pipeline compares them.
//
// # What it must never do
//
// It must never write a received byte anywhere but through the Sink, list
// a part as stored before the Sink kept it, hold a part whole in memory,
// or put a UID, a key or an address in a log line.
package stow
