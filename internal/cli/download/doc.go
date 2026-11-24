// Package download fetches the files of a subject, a session or a scan from
// the Data service into a directory, checking each against the SHA-256 the
// server announced as it arrives, and continuing an interrupted download from
// where it stopped. The command and the interactive program both use it, so a
// download does the same in each.
//
// A file is written as <name>.part and renamed only when its length and
// SHA-256 are the announced ones: a name without the suffix is always a whole,
// checked file. Beside a .part, <name>.part.sha256 holds the SHA-256 of the
// file it is the start of, so a later run continues it only while the server
// still announces that file, and starts afresh when the file changed. A file
// is given up as soon as more bytes arrive for it than were announced, so a
// server cannot fill the disk. Names the server sends are hostile input: each must be a
// label, so no path leaves the directory. This package never deletes a file
// that is whole and correct, and never writes outside the directory it is
// given.
package download
