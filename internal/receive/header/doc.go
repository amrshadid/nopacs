// Package header reads what the reception pipeline needs from a received
// file: whether it is a DICOM file at all, its transfer syntax, its SOP class
// and the three UIDs that place it (instance, series, study), where its pixel
// data is, the attributes a naming rule reads, and its data set in the DICOM
// JSON Model (PS3.18 Annex F) without its bulk data, for the scan's metadata
// sidecar.
//
// Every byte of DICOM is read by go-dicom (package filereader): this package
// decides nothing about the encoding itself. It reads a file's header with
// bulk values left in the file (ReadOptions.SkipBulkData), so the memory a
// read takes follows the header and not the image: Pixel Data in every form,
// and every binary value longer than BulkThreshold, are skipped by seeking.
//
// # What counts as a DICOM file
//
// A received file is kept as a DICOM file (PS3.10 7.1): a 128-byte preamble,
// the prefix "DICM", the file meta information with its transfer syntax, then
// the data set. A stream without the preamble and prefix is refused, though
// go-dicom can read one, since the archive's copy must be a file every DICOM
// tool opens. A data set whose last element does not end where the file ends
// was cut short, and is refused too: go-dicom reads such a file with a warning
// and drops the element that runs past the end, which is right for a viewer
// and wrong for an archive, which must not keep a file that lost its end.
// Each refusal is a *NotDICOMError carrying the byte offset where reading
// stopped, a number and never a value.
//
// # Bounded by MaxHeader
//
// go-dicom allocates a value whole once its declared length fits in what is
// left of the stream it reads, so a hostile file that declares a text value
// of a gigabyte inside a file of a gigabyte would be allocated a gigabyte.
// The header is therefore read from the file's first MaxHeader bytes, and
// the rest only when the first element past them is pixel data, which is
// skipped (Read says the order). Every value held is also bounded by the
// library's own limit (MaxValueLength), so an element after the pixel data
// is held to the same bound. A header that does not fit is a
// *TooLargeError.
//
// # go-dicom's logger
//
// What the library logs about a damaged file names tags, lengths and
// offsets of a file that may be a patient's. Each read is given a logger
// that discards, so nothing reaches the server's log, and nothing is set
// for the rest of the process.
//
// # What this package must never do
//
// It must never let go-dicom write to the process's standard streams, never
// read a header without the bound, and never put a value read from a file
// into an error's text: before
// de-identification a value can name a patient, and an error's text reaches
// logs. It must never write a file or parse DICOM with its own code.
package header
