// Package manifest is the format of a scan's manifest: the list, kept on the
// storage beside the scan's files, of every file of the scan with its key,
// its byte range, its size, its SHA-256 and its SOP Instance UID. With the
// manifests alone a storage can be checked, and the catalog rebuilt, without
// opening a DICOM file.
//
// It is a package of its own because the manifest is a file format that
// outlives the server that wrote it: it is versioned, its encoding is
// fixed, and the code that writes it and the code that reads it must be the
// same code.
//
// # The format
//
// A manifest is JSON, written in one form only:
//
//	{"version":1,"files":[
//	{"key":"data/S/E/C/1.2.3.dcm","offset":0,"length":1234,"size":1234,"sha256":"<64 hex>","sop_instance_uid":"1.2.3"}
//	]}
//
// one file per line, its fields in that order, the files sorted by key and
// then offset, no other space, and a final newline. The same content always
// gives the same bytes, so a manifest's checksum, kept by the catalog,
// changes only when its content does, and two copies are compared by
// checksum alone.
//
// A file's place is its key, offset and length: the instance's bytes are
// the length bytes from offset in the object at key, whose whole size is
// size, and sha256 is the SHA-256 of those bytes. In version 1 every
// instance is its own object under the layout's data/ tree, so offset is 0,
// length is size, and the key's file name is the SOP Instance UID; a later
// version may pack several instances into one object without changing the
// fields.
//
// # Reading
//
// Decode is strict, because a manifest it accepts is trusted to say where
// a scan's data is: it reads at most MaxBytes, refuses a version it does not
// know, a field it does not know, a file listed twice, a key outside the
// layout, files of different scans, and any byte form other than the one
// Encode writes (which also refuses a field given twice, since JSON
// decoders disagree on which of the two counts).
//
// # What this package must never do
//
// It must never do I/O beyond the reader it is given, read the clock, or
// import anything of NoPACs' outside the core (.golangci.yml, depguard
// "core").
package manifest
