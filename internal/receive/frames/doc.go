// Package frames is the frame tables of a scan's metadata sidecar: where
// each frame of an instance's pixel data lies in its file (Table), recorded
// when the instance is archived, and the pass that gives them to the scans
// archived before tables existed (Run).
//
// # The table
//
// go-dicom divides pixel data into frames (filereader.DICOMFile.FrameRanges)
// and package header hands its answer here as byte ranges of the file. For
// encapsulated pixel data (PS3.5 A.4) a frame is the contents of its
// fragments joined, so its ranges are those contents, without the item
// headers between them: what a WADO-RS request for a frame as stored is
// answered with (PS3.18 8.7.3.3.2). For native pixel data a frame is one
// range of Rows x Columns x Samples per Pixel samples. Each frame's
// SHA-256 is recorded beside its ranges, so a frame answered by range is
// checked as it is sent. A file whose frames
// are not byte ranges of it gets an empty table, so a reader knows it was
// looked at and reads the file's header for such a frame instead.
//
// # The pass
//
// A sidecar written before frame tables is of version 1 (package sidecar);
// one whose every line with pixel data has its table is of version 2. The
// pass lists the tenant's scans in label order, reads the first bytes of
// each scan's sidecar, and for one of version 1 reads, by bounded ranged
// reads of the storage (package readat), the header and the pixel data's
// item headers of each instance whose line has no table, then the file
// once more, streamed through one buffer, for each frame's checksum and
// the file's own, which must be the line's (Checksums), and rewrites the
// sidecar through sidecar.Complete with each line given its table and its
// checksums. The rewrite is a Put of the whole object, which a reader
// sees whole or not at all, made under the same lock the archive step takes
// for the scan, so a reception writing the scan at the same time loses
// nothing and is not lost.
//
// Its state is the sidecars themselves: a scan done is version 2, and a
// scan not done is version 1. Nothing is kept in memory that a restart
// needs: a pass stopped midway (a crash, a stop of the server) loses the
// work of the scan it was on, at most one batch of its instances, and the
// next pass, at the next start, reads the first bytes of each scan again
// and goes on from the first scan not done. A pass that finds every scan
// done changes nothing: running it again is reading one small range per
// scan. A batch of a scan's instances too large to hold at once is written
// with sidecar.Merge, which keeps the sidecar at version 1, and the last
// batch with sidecar.Complete, so a scan is version 2 only when all of it
// is done.
//
// It never starves reception: it works one instance at a time and waits
// after each for as long as the instance took, and for many times that
// while the pipeline has a step in flight or files arriving (Config.Busy).
// Its progress is reported as events (tenant.FramesAdded): one per scan
// rewritten, and one when a pass that rewrote any ends.
//
// # The catalog's values
//
// The same pass fills in the catalog's header values (the session's Study
// Date, the scan's Modality, Series Description and Series Number), which
// the catalog did not keep for scans archived before a version that does,
// and which the searches that filter by them (QIDO-RS's StudyDate,
// ModalitiesInStudy and Modality among them) read. A scan the catalog lists
// without a modality, whatever its sidecar's version, has the header of
// the file its sidecar's first line names read, by bounded ranged reads,
// and the values made from it as the reception that archives a file makes
// them (header.CatalogText) handed to the catalog (tenant.ScanValueFill),
// which gives each only to a row that holds it empty. A scan listed with a
// modality costs nothing more, so a pass over a catalog filled in reads no
// file for it.
//
// # The rewrite to version 3
//
// A second pass, Upgrade, runs after the first in the same goroutine. A
// sidecar of version 2 has no header of its scan and no reception, time or
// received checksum in its lines, which only the tenant's database holds;
// Upgrade lists the scans in label order, reads the first bytes of each
// sidecar, and for one of version 2 reads the scan, its session, its
// session's routing and its held instances (in the order of the sidecar's
// lines, a page at a time) from the catalog, under the scan's lock, and
// rewrites the sidecar through sidecar.Upgrade as one Put of the whole
// object. A line and its row must name the same key, size and archived
// checksum, and the catalog and the sidecar the same instances: a scan where
// they do not is left as it is, counted and logged, and the pass goes on
// (a scrub reports it). A sidecar of version 1 waits for the pass of tables.
// The header holds the catalog's values of the scan and its session (the
// study date, modality, series description and series number) and its
// receivers, each empty when the catalog holds none, so a scan whose
// catalog row has no receiver is rewritten like any other, once.
// Fields the database does not hold stay empty: how the file arrived and
// the build that wrote it, and the syntax a de-identified file arrived in.
// Its state is the sidecars themselves, as the first pass's is, so a pass
// stopped midway goes on from the first scan not done, and one over scans all
// done reads the first bytes of each and writes nothing.
//
// # What this package must never do
//
// It must never hold a whole instance file in memory, never give a frame a
// checksum read from a file that is not the one its line names, never
// write a sidecar but through package sidecar, never rewrite a scan's
// sidecar without the scan's lock, never decide a frame's division itself (go-dicom does), and never
// put a value read from a file in an error, a log line or an event: events
// carry the scan's labels and counts only.
package frames
