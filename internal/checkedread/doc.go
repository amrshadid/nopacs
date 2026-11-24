// Package checkedread reads the bytes the archive holds and checks them
// against what the catalog records as they go.
//
// An instance's manifest holds its length and SHA-256. A read that passes
// bytes on without comparing them would spread damage the archive already
// knows about, so every reader of archived bytes (the data service of the
// API, the DICOMweb retrievals and a sender that passes instances to a peer)
// goes through the same three pieces:
//
//   - CopyChecked passes the bytes of one instance in pieces of a fixed size
//     and holds back the last piece until the count and the SHA-256 both
//     agree, so a body that differs from its manifest never reaches its
//     reader whole.
//   - Slots bounds how many reads one caller and the whole server hold open,
//     since each holds a storage's connection or file and two buffers.
//   - Stores opens the storages a read reaches, each once, for the tenant
//     whose pipeline it holds, and closes them at its end.
//
// The package knows nothing of HTTP, gRPC or DICOM: callers decide what to
// do with a mismatch.
//
// # What a checked read guarantees
//
// CopyChecked withholds the last piece of a body until its length and, when a
// checksum is given, its SHA-256 agree with the manifest. It does not withhold
// the pieces before it: they are passed as they are read, so a reader that has
// taken them holds the start of a damaged instance. A caller therefore ends the
// response at a mismatch, so the client sees a body short of its length and
// not a body that looks whole.
//
//   - A mismatch is ErrShort, ErrLong or ErrChecksum, and Mismatched reports
//     them. Any other error from the reader or from emit is returned as it is.
//   - With no checksum (a bulk data window) only the length is checked.
//   - At most two pieces are held: one is read into while the other is passed
//     or hashed. The hash runs on its own goroutine for a body of more than one
//     piece and is joined before CopyChecked returns, so no piece is read after
//     it returns and a caller may reuse its buffers at once.
//
// # Slots
//
// Slots counts the reads of archived bytes open now, per owner (a key, or the
// local operator) and in all. Its zero value is ready to use, with the bounds
// MaxOpenReads and MaxServerReads; the server sets Limits to read the
// installation's read.per-caller and read.per-server live. A read past the
// bound for its owner, or past the server's, is refused
// with fault.Exhausted and is not queued: a client that asks again later gets
// its read, and one that waits in line would hold a connection while it waits.
// The count is in memory, which is the whole server's count, since one process
// serves an installation (the lock on its data directory).
//
// ChunkSize is the size of one piece of ReadFiles. The bound of a server's
// memory assumes two buffers of that size for each open read, as its comment
// states; the DICOMweb and download paths size their own buffers.
package checkedread
