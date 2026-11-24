// Package sidecar is the format of a scan's metadata sidecar: one file on the
// storage beside the scan's manifest (layout.MetadataKey) that holds, for
// each instance of the scan, its header as the DICOM JSON Model (PS3.18
// Annex F) without its bulk data, and where its pixel data starts in the
// instance's file and how long it is. With it a client can search and show a
// scan, and fetch the pixels of one frame by range, without opening a DICOM
// file.
//
// # The format
//
// A sidecar is JSON, written in one form only:
//
//	{"version":2,"instances":[
//	{"sop_instance_uid":"1.2.3","key":"data/S/E/C/1.2.3.dcm","size":1234,"sha256":"<64 hex>","transfer_syntax":"1.2.840.10008.1.2.4.50","profile":"research@3","pixel_data":{"offset":1100,"length":134},"frames":[[[1120,60]],[[1188,30],[1226,8]]],"frame_sha256":["<64 hex>","<64 hex>"],"dataset":{...}},
//	...
//	]}
//
// one instance per line, its fields in that order, the instances sorted by
// SOP Instance UID with each UID once, and a final newline. transfer_syntax
// is the file's Transfer Syntax UID and is left out of a line written before
// it existed. profile, between transfer_syntax and pixel_data, is the
// profile version that de-identified the instance, "name@version", and is
// left out for an instance archived as received and of a line written
// before it existed; like transfer_syntax it was added without a new
// version, since a reader of version 2 that knows it reads every line
// written before it. pixel_data is null for an instance without pixel data and for
// one whose data set is deflated (PS3.5 A.5), where an offset counted in the
// inflated bytes is not a place in the file; its offset is the first byte of
// the element's tag in the file and its length runs to the first byte after
// the element. dataset is the data set's DICOM JSON, its members in the
// order of their tags; when that text is longer than MaxDataset it is null
// and "dataset_omitted":true follows it, so one instance with a huge header
// cannot make the sidecar unbounded or keep its scan from being archived.
//
// # Frame tables
//
// frames says where each frame of the pixel data lies in the file, frame 1
// first, as go-dicom divides it: for each frame, the [offset,length] ranges
// whose bytes joined in order are the frame as stored. For encapsulated
// pixel data (PS3.5 A.4) they are the contents of the frame's fragments,
// without the item headers between them, which is what a WADO-RS request
// for a frame as stored is answered with (PS3.18 8.7.3.3.2); for native
// pixel data, one range a frame in the file's byte order. With it a frame is
// read by range without the file's header. Its offsets count from the
// first byte of the instance's file, never from the object that holds it:
// while each instance is one object the two are the same, and a reader
// adds the instance's offset in its object (tenant.ScanInstance.Offset),
// so a layout that packs instances into one object keeps every table
// right. It is [] for an instance whose
// frames are not byte ranges of the file (a 1-bit image whose frames start
// inside a byte, pixel data go-dicom cannot divide into the frames the data
// set states) or whose table would hold more than MaxFrameRanges ranges: a
// reader of such a frame reads the file's header, as it would without a
// table. It is left out of a line without pixel data.
//
// frame_sha256 is the SHA-256 of each frame, frame 1 first, in lowercase
// hex: of the frame's ranges' bytes joined in order, read from the file
// that was archived (checked against the line's sha256 as it is read), so
// a frame answered by range is checked as it is sent, as a whole file is
// against its sha256. Every line whose frames is not empty has one
// checksum for each frame; it is left out of every other line.
//
// # Versions
//
// Version 1 is the form before frame tables, where no line has one. In
// version 2, every line with pixel data has its table, possibly []. A
// reader reads both. A merge of new instances into a sidecar of version 1
// stays version 1, since the lines it keeps have no tables; Complete is the
// rewrite that gives those lines theirs and makes it version 2, which is
// how a reader learns, from the first line alone, that a scan's tables are
// all there. The index that follows the footer is written again by every
// merge, so it names where each indexed line is after a rewrite that made
// lines longer.
//
// # Version 3
//
// Version 3 holds what a catalog made again from the storage alone needs and
// only the tenant's database held before. Its first line states the scan:
//
//	{"version":3,"scan":{"study_uid":"1.2.3","series_uid":"1.2.3.4","session_receiver":"scanner-a","receiver":"scanner-b","route":{"name":"trial","profile":"research@3"},"values":{"study_date":"20240131","modality":"MR","series_description":"T1 axial","series_number":3},"created":"2026-11-20T08:30:00.123456Z","updated":"2026-11-20T11:30:00.123456Z"},"instances":[
//
// the study and series UIDs as the catalog holds them (a line's data set may
// be omitted, so they cannot be read from one), the receiver whose reception
// first archived into the session and the one that first archived into the
// scan (each "" when the catalog holds none), the session's routing when it
// chose anything, the values the catalog keeps from the files' headers (the
// session's study date, the scan's modality, series description and series
// number, each left out when the catalog holds none), and the times of the
// first and the latest commit into the scan, in UTC to the microsecond. Every
// value of the header is the catalog's after the commit that wrote it. Each
// line then also has, after the fields of version 2 and in this order (each
// but the first two left out when it has no value): "revision" (2 or more,
// the number its key ends in, ".r2.dcm"; left out for revision 1),
// "received_sha256" (the checksum of the file as received, only when it
// differs from "sha256", which is the file as archived: a de-identified
// instance's, the catalog's keyed one), "profile_sha256" (the checksum of the
// profile version's expanded text, which names the version in whatever
// database holds it), "reception" and "held_at" (always there: the
// reception that confirmed the revision, and when the instance was first
// held, which a later revision keeps), "received_form" ("dimse"
// for a data set NoPACs wrapped in its own Part 10 header, "part10" for a file
// taken as sent), "received_syntax" (the Transfer Syntax UID it arrived in)
// and "nopacs_version" (the build that wrote the line). A sidecar of
// version 1 or 2 has none of these and none is accepted in it, so a line has
// one meaning in each version.
//
// A merge into a sidecar of version 3 writes the header its caller made from
// the catalog, never the old one's, which an attempt that did not commit may
// have written; one into a sidecar of an older version stays that version, with the
// lines it adds written without the fields it has no place for, until
// Upgrade rewrites it from the catalog. Every sidecar the archive step makes
// is of version 3.
//
// Because the form is fixed and sorted, a sidecar is written and merged as a
// stream, one line at a time: memory follows the longest line, not the scan.
//
// # What this package must never do
//
// It must never do I/O beyond the reader and writer it is given, and never
// put a value of a data set in an error's text.
package sidecar
