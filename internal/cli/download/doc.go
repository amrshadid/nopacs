// Package download fetches the files of a chosen set (subjects, sessions and
// scans named by references) from the Data service into a directory,
// checking each against the SHA-256 the server announced as it arrives, and
// continuing an interrupted download from where it stopped. The command and
// the interactive program both use it, so a download does the same in each.
//
// A run first asks the archive for the announcements of every reference (no
// file bytes), drops what is already whole on disk, and cuts the rest into
// lanes: runs of consecutive files of one scan, each fetched by one stream.
// Up to Options.Parallel streams run at once. The server refuses a caller that
// holds more streams than it serves (never queueing it), so a refusal is
// waited out with growing pauses, the number of streams lowered to what the
// server allows, and tried one higher again after a quiet stretch. One
// bandwidth bound, Options.LimitRate, covers every stream together.
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
//
// A selection file (Options.SelectionPath) records the files that are whole
// and checked, with their size and modification time, and is replaced
// atomically every few seconds and at the end. A run that finds a file as
// recorded skips it without reading it; a file that differs is read and
// hashed as without the record, so the record only saves time and a file
// changed on disk is never trusted. The layout LayoutBagIt writes a bag of
// RFC 8493 whose manifest lists the SHA-256 the server announced and the
// client checked.
package download
