// Package dimse is the DICOM listener of one receiver: an association
// acceptor (PS3.8) on the receiver's port and address that admits the
// callers the receiver lists, answers C-ECHO (PS3.4 Annex A), and hands the
// data set of every C-STORE (PS3.4 Annex B) to the reception pipeline as the
// bytes the caller sent, written as a Part 10 file (PS3.10 7.1) through the
// pipeline's one entry for a source (receive.Sink).
//
// It is the only package of the server that speaks the DICOM network
// protocol, and it does so only through the DICOM library's network package:
// it parses no PDU, decodes no data set and writes no file meta information
// with its own code.
//
// # What it guarantees
//
//   - Closed by default. An association is admitted only from a caller one
//     of the receiver's rules matches (calling AE title, address range, or
//     both), or from anyone when the receiver says "any caller" in so many
//     words; an empty list admits nobody. A stranger is rejected permanently
//     before anything is negotiated (PS3.8 9.3.4,
//     calling-AE-title-not-recognized), so it learns nothing of what the
//     receiver accepts; a caller that calls another AE title is rejected as
//     called-AE-title-not-recognized.
//   - Only the receiver's SOP classes and transfer syntaxes are negotiated,
//     with Verification and Implicit VR Little Endian always (PS3.5 10.1).
//     No query, retrieve, storage commitment or N-service is answered.
//   - A C-STORE is answered success (0000H) only once its file is whole and
//     synced in the incoming area, which receive.Sink.Accept promises. Every
//     other answer is a failure the sender keeps its copy for: out of
//     resources (A700H) when the archive cannot take the file now or the
//     file is past the receiver's largest, cannot understand (C000H) or an
//     invalid instance (0117H) when the command cannot be written as a file
//     (PS3.4 B.2.3). A data set that does not arrive whole is never kept:
//     the pipeline refuses a body whose reading ended in an error.
//   - Memory per association is bounded whatever the instance's size: the
//     library hands the data set over one PDU at a time and the pipeline
//     copies it through a fixed buffer.
//   - While the archive cannot take files (its incoming disk or the
//     receiver's storage below the reserve, the tenant over its incoming
//     limit, the tenant's database not ready), new associations are rejected
//     as transient and a store is answered out of resources; which one holds
//     is the caller's Refusal, so this package keeps no such state.
//   - Stop stops admitting, lets the stores in flight finish within a bound,
//     then ends every association; a store cut short then is not kept.
//
// # What it must never do
//
// It must never write a received byte anywhere but through the Sink, answer
// success before the Sink does, admit a caller its rules do not match, keep
// a global, let the library log anywhere but the logger its server is
// given (which discards), block the
// library's goroutine on anything but the store it serves, or put an AE
// title, an address or a UID in a log line: those go to the receiver's
// events, which carry who called and never what was sent.
package dimse
